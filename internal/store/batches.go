package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type BatchInput struct{ Model, Request string }
type BatchGroup struct {
	ID      string `json:"id"`
	Created int64  `json:"created_ms"`
	Status  string `json:"status"`
	Jobs    []Task `json:"jobs"`
}

func (s *Store) migrateBatchGroups(ctx context.Context, tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS batch_groups(id TEXT PRIMARY KEY,tenant TEXT NOT NULL,idem TEXT NOT NULL,payload_hash TEXT NOT NULL,created BIGINT NOT NULL,UNIQUE(tenant,idem))`,
		`CREATE TABLE IF NOT EXISTS batch_group_items(batch_id TEXT NOT NULL REFERENCES batch_groups(id),ordinal INTEGER NOT NULL,task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id),PRIMARY KEY(batch_id,ordinal))`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// CreateBatchGroup uses the same per-tenant PostgreSQL advisory lock as Create.
// In SQLite the single database connection serializes the writer. All children
// and their parent commit together; an oversized group does not enqueue a prefix.
func (s *Store) CreateBatchGroup(tenant, key string, items []BatchInput, deadline time.Time) (BatchGroup, bool, error) {
	if len(items) == 0 || len(items) > 20 {
		return BatchGroup{}, false, ErrCapacity
	}
	if key == "" {
		key = ID()
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return BatchGroup{}, false, err
	}
	digest := hash(string(encoded))
	now := time.Now().UnixMilli()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return BatchGroup{}, false, err
	}
	defer tx.Rollback()
	if s.postgres {
		if _, err = tx.ExecContext(ctx, s.q(`SELECT pg_advisory_xact_lock(hashtext(?))`), tenant); err != nil {
			return BatchGroup{}, false, err
		}
	}
	var existing, digestFound string
	err = tx.QueryRowContext(ctx, s.q(`SELECT id,payload_hash FROM batch_groups WHERE tenant=? AND idem=?`), tenant, key).Scan(&existing, &digestFound)
	if err == nil {
		if digestFound != digest {
			return BatchGroup{}, true, ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return BatchGroup{}, true, err
		}
		b, err := s.GetBatchGroup(tenant, existing)
		return b, true, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BatchGroup{}, false, err
	}
	var queued int
	if err = tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM tasks WHERE tenant=? AND status IN ('queued','running')`), tenant).Scan(&queued); err != nil {
		return BatchGroup{}, false, err
	}
	if queued+len(items) > 20 {
		return BatchGroup{}, false, ErrCapacity
	}
	id := ID()
	if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO batch_groups(id,tenant,idem,payload_hash,created) VALUES(?,?,?,?,?)`), id, tenant, key, digest, now); err != nil {
		return BatchGroup{}, false, err
	}
	for idx, item := range items {
		taskID := ID()
		taskKey := fmt.Sprintf("@batch:%s:%d", id, idx)
		if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO tasks(id,tenant,idem,payload_hash,kind,model,request,deadline,created) VALUES(?,?,?,?,'batch',?,?,?,?)`), taskID, tenant, taskKey, hash(item.Request), item.Model, item.Request, deadline.UnixMilli(), now); err != nil {
			return BatchGroup{}, false, err
		}
		if _, err = tx.ExecContext(ctx, s.q(`INSERT INTO batch_group_items(batch_id,ordinal,task_id) VALUES(?,?,?)`), id, idx, taskID); err != nil {
			return BatchGroup{}, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return BatchGroup{}, false, err
	}
	b, err := s.GetBatchGroup(tenant, id)
	return b, false, err
}

func (s *Store) GetBatchGroup(tenant, id string) (BatchGroup, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b := BatchGroup{ID: id, Jobs: []Task{}}
	err := s.db.QueryRowContext(ctx, s.q(`SELECT created FROM batch_groups WHERE tenant=? AND id=?`), tenant, id).Scan(&b.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return BatchGroup{}, ErrNotFound
	}
	if err != nil {
		return BatchGroup{}, err
	}
	cols := "t." + strings.ReplaceAll(summaryColumns, ",", ",t.")
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+cols+` FROM batch_group_items AS i JOIN tasks AS t ON t.id=i.task_id WHERE i.batch_id=? AND t.tenant=? ORDER BY i.ordinal`), id, tenant)
	if err != nil {
		return BatchGroup{}, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		job, e := scanTask(rows, false)
		if e != nil {
			return BatchGroup{}, e
		}
		b.Jobs = append(b.Jobs, job)
		counts[job.Status]++
	}
	if err = rows.Err(); err != nil {
		return BatchGroup{}, err
	}
	switch {
	case counts["succeeded"] == len(b.Jobs):
		b.Status = "succeeded"
	case counts["cancelled"] == len(b.Jobs):
		b.Status = "cancelled"
	case counts["queued"] == len(b.Jobs):
		b.Status = "queued"
	case counts["running"]+counts["queued"] > 0:
		b.Status = "running"
	default:
		b.Status = "completed_with_errors"
	}
	return b, nil
}

func (s *Store) CancelBatchGroup(tenant, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var found string
	err := s.db.QueryRowContext(ctx, s.q(`SELECT id FROM batch_groups WHERE tenant=? AND id=?`), tenant, id).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.q(`UPDATE tasks SET status='cancelled',lease=0,available_at=0,error='' WHERE tenant=? AND status IN ('queued','running') AND id IN (SELECT task_id FROM batch_group_items WHERE batch_id=?)`), tenant, id)
	return err
}
