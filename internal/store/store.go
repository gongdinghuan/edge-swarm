// Package store owns durable execution authority. All worker mutations are fenced
// by task generation, connection epoch, node revocation and server-side lease.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("idempotency conflict")
	ErrUnauthorized = errors.New("unauthorized node")
	ErrStale        = errors.New("stale execution authority")
	ErrCapacity     = errors.New("tenant queue full")
)

type Store struct {
	db       *sql.DB
	postgres bool
}
type Node struct {
	ID       string `json:"id"`
	Model    string `json:"model"`
	Epoch    int64  `json:"epoch"`
	Revoked  bool   `json:"revoked"`
	Draining bool   `json:"draining"`
}
type Task struct {
	ID            string `json:"id"`
	Tenant        string `json:"-"`
	Kind          string `json:"kind"`
	Model         string `json:"model"`
	Request       string `json:"-"`
	Status        string `json:"status"`
	Node          string `json:"node_id,omitempty"`
	Epoch         int64  `json:"-"`
	Fence         int64  `json:"-"`
	Attempts      int    `json:"attempts"`
	Lease         int64  `json:"-"`
	Deadline      int64  `json:"deadline_ms"`
	Created       int64  `json:"created_ms"`
	AvailableAt   int64  `json:"available_at_ms"`
	OutputStarted bool   `json:"output_started"`
	Result        string `json:"result,omitempty"`
	Error         string `json:"error,omitempty"`
	FinishReason  string `json:"finish_reason,omitempty"`
}

const summaryColumns = `id,tenant,kind,model,status,node,epoch,fence,attempts,lease,deadline,created,available_at,output_started,error,finish_reason`
const columns = summaryColumns + `,request,result`

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func hash(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func Open(driver, dsn string) (*Store, error) {
	if driver != "sqlite" && driver != "pgx" {
		return nil, errors.New("driver must be sqlite or pgx")
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, postgres: driver == "pgx"}
	if driver == "sqlite" {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(10)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err == nil {
		err = s.migrate(ctx)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

const operationTimeout = 2 * time.Second

func operationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), operationTimeout)
}
func (s *Store) exec(query string, args ...any) (sql.Result, error) {
	ctx, cancel := operationContext()
	defer cancel()
	return s.db.ExecContext(ctx, query, args...)
}
func (s *Store) queryTask(query string, args ...any) (Task, error) {
	ctx, cancel := operationContext()
	defer cancel()
	return scan(s.db.QueryRowContext(ctx, query, args...))
}
func (s *Store) q(q string) string {
	if !s.postgres {
		return q
	}
	n := 0
	return replaceParams(q, &n)
}
func replaceParams(q string, n *int) string {
	var b strings.Builder
	for _, c := range q {
		if c == '?' {
			*n++
			fmt.Fprintf(&b, "$%d", *n)
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}
func (s *Store) beginWrite(ctx context.Context) (*sql.Tx, error) {
	if s.postgres {
		// Statements after a lock must see the preceding holder's commits.
		return s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	}
	return s.db.BeginTx(ctx, nil)
}
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if s.postgres {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(4815162342)`); err != nil {
			return err
		}
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS nodes(id TEXT PRIMARY KEY,secret TEXT NOT NULL,model TEXT NOT NULL,epoch BIGINT NOT NULL DEFAULT 0,revoked INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY,tenant TEXT NOT NULL,idem TEXT NOT NULL,payload_hash TEXT NOT NULL,kind TEXT NOT NULL,model TEXT NOT NULL,request TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'queued',node TEXT NOT NULL DEFAULT '',epoch BIGINT NOT NULL DEFAULT 0,fence BIGINT NOT NULL DEFAULT 0,attempts INTEGER NOT NULL DEFAULT 0,lease BIGINT NOT NULL DEFAULT 0,deadline BIGINT NOT NULL,output_started INTEGER NOT NULL DEFAULT 0,result TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',created BIGINT NOT NULL,UNIQUE(tenant,idem))`,
		`CREATE INDEX IF NOT EXISTS tasks_schedule ON tasks(status,model,created)`,
		`CREATE INDEX IF NOT EXISTS tasks_node ON tasks(node,status)`,
	}
	for _, q := range statements {
		if _, err = tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	for _, c := range []struct{ table, name, definition string }{
		{"nodes", "draining", "INTEGER NOT NULL DEFAULT 0"},
		{"tasks", "available_at", "BIGINT NOT NULL DEFAULT 0"},
		{"tasks", "finish_reason", "TEXT NOT NULL DEFAULT ''"},
	} {
		prefix := "ALTER TABLE " + c.table + " ADD COLUMN "
		if s.postgres {
			prefix += "IF NOT EXISTS "
		} else {
			var exists int
			err = tx.QueryRowContext(ctx, `SELECT 1 FROM pragma_table_info(?) WHERE name=?`, c.table, c.name).Scan(&exists)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, prefix+c.name+" "+c.definition); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS tasks_tenant_kind_created ON tasks(tenant,kind,created,id)`); err != nil {
		return err
	}
	if err = s.migrateBatchGroups(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) AddNode(id, secret, model string) error {
	_, err := s.exec(s.q(`INSERT INTO nodes(id,secret,model) VALUES(?,?,?)`), id, hash(secret), model)
	return err
}
func (s *Store) Connect(id, secret string) (int64, error) {
	var epoch int64
	ctx, cancel := operationContext()
	defer cancel()
	err := s.db.QueryRowContext(ctx, s.q(`UPDATE nodes SET epoch=epoch+1 WHERE id=? AND secret=? AND revoked=0 RETURNING epoch`), id, hash(secret)).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrUnauthorized
	}
	return epoch, err
}

// Disconnect fences a lost session. A newer connection keeps its authority.
func (s *Store) Disconnect(id string, epoch int64) error {
	_, err := s.exec(s.q(`UPDATE nodes SET epoch=epoch+1 WHERE id=? AND epoch=? AND revoked=0`), id, epoch)
	return err
}

func (s *Store) NodeModel(id string, epoch int64) (string, error) {
	if epoch <= 0 {
		return "", ErrUnauthorized
	}
	var model string
	ctx, cancel := operationContext()
	defer cancel()
	err := s.db.QueryRowContext(ctx, s.q(`SELECT model FROM nodes WHERE id=? AND epoch=? AND revoked=0`), id, epoch).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrUnauthorized
	}
	return model, err
}
func (s *Store) Revoke(id string) error {
	res, err := s.exec(s.q(`UPDATE nodes SET revoked=1,epoch=epoch+1 WHERE id=?`), id)
	return affected(res, err, ErrNotFound)
}
func (s *Store) SetDraining(id string, drain bool) error {
	value := 0
	if drain {
		value = 1
	}
	res, err := s.exec(s.q(`UPDATE nodes SET draining=? WHERE id=?`), value, id)
	return affected(res, err, ErrNotFound)
}
func (s *Store) ListNodes() ([]Node, error) {
	ctx, cancel := operationContext()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT id,model,epoch,revoked,draining FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := []Node{}
	for rows.Next() {
		var n Node
		var revoked, draining int
		if err = rows.Scan(&n.ID, &n.Model, &n.Epoch, &revoked, &draining); err != nil {
			return nil, err
		}
		n.Revoked, n.Draining = revoked != 0, draining != 0
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}
func (s *Store) Create(tenant, key, kind, model, request string, deadline time.Time) (Task, bool, error) {
	if key == "" {
		key = ID()
	}
	id := ID()
	digest := hash(kind + "\n" + model + "\n" + request)
	ctx, cancel := operationContext()
	defer cancel()
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback()
	if s.postgres {
		if _, err = tx.ExecContext(ctx, s.q(`SELECT pg_advisory_xact_lock(hashtext(?))`), tenant); err != nil {
			return Task{}, false, err
		}
	}
	res, err := tx.ExecContext(ctx, s.q(`INSERT INTO tasks(id,tenant,idem,payload_hash,kind,model,request,deadline,created) SELECT ?,?,?,?,?,?,?,?,? WHERE (SELECT COUNT(*) FROM tasks WHERE tenant=? AND status IN ('queued','running'))<20 ON CONFLICT(tenant,idem) DO NOTHING`), id, tenant, key, digest, kind, model, request, deadline.UnixMilli(), time.Now().UnixMilli(), tenant)
	if err != nil {
		return Task{}, false, err
	}
	rows, _ := res.RowsAffected()
	var existingID, existingHash string
	err = tx.QueryRowContext(ctx, s.q(`SELECT id,payload_hash FROM tasks WHERE tenant=? AND idem=?`), tenant, key).Scan(&existingID, &existingHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, false, ErrCapacity
	}
	if err != nil {
		return Task{}, false, err
	}
	if existingHash != digest {
		return Task{}, true, ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return Task{}, false, err
	}
	j, err := s.Get(tenant, existingID)
	return j, rows == 0, err
}

type scanner interface{ Scan(...any) error }

func scan(row scanner) (Task, error) {
	return scanTask(row, true)
}
func scanTask(row scanner, payload bool) (Task, error) {
	var t Task
	var output int
	fields := []any{&t.ID, &t.Tenant, &t.Kind, &t.Model, &t.Status, &t.Node, &t.Epoch, &t.Fence, &t.Attempts, &t.Lease, &t.Deadline, &t.Created, &t.AvailableAt, &output, &t.Error, &t.FinishReason}
	if payload {
		fields = append(fields, &t.Request, &t.Result)
	}
	err := row.Scan(fields...)
	t.OutputStarted = output != 0
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return t, err
}
func (s *Store) Get(tenant, id string) (Task, error) {
	return s.queryTask(s.q(`SELECT `+columns+` FROM tasks WHERE tenant=? AND id=?`), tenant, id)
}
func (s *Store) Lookup(id string) (Task, error) {
	return s.queryTask(s.q(`SELECT `+columns+` FROM tasks WHERE id=?`), id)
}
func (s *Store) ListTasks(tenant, kind string, limit int) ([]Task, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	query := `SELECT ` + summaryColumns + ` FROM tasks WHERE tenant=?`
	args := []any{tenant}
	if kind != "" {
		query += ` AND kind=?`
		args = append(args, kind)
	}
	ctx, cancel := operationContext()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY created DESC,id DESC LIMIT ?`), append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		j, err := scanTask(rows, false)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, j)
	}
	return tasks, rows.Err()
}

const readyTasks = ` FROM tasks WHERE status='queued' AND model=? AND deadline>? AND available_at<=? ORDER BY created,id LIMIT 1`

func (s *Store) NextQueued(model string, now time.Time) (Task, error) {
	return s.queryTask(s.q(`SELECT `+columns+readyTasks), model, now.UnixMilli(), now.UnixMilli())
}
func (s *Store) Claim(node string, epoch int64, model string, now time.Time, lease time.Duration) (Task, error) {
	if lease.Milliseconds() <= 0 {
		return Task{}, ErrNotFound
	}
	candidate := `SELECT id` + readyTasks
	if s.postgres {
		candidate += ` FOR UPDATE SKIP LOCKED`
	}
	var claimed Task
	err := s.nodeOperation(node, epoch, ErrNotFound, func(ctx context.Context, db taskDB) error {
		leaseUntil := now.Add(lease).UnixMilli()
		var err error
		claimed, err = scan(db.QueryRowContext(ctx, s.q(`UPDATE tasks SET status='running',node=?,epoch=?,fence=fence+1,attempts=attempts+1,
 lease=CASE WHEN deadline<? THEN deadline ELSE ? END,available_at=0,error=''
 WHERE id=(`+candidate+`) AND status='queued'
 AND EXISTS(SELECT 1 FROM nodes WHERE id=? AND epoch=? AND revoked=0 AND draining=0 AND model=?)
 AND NOT EXISTS(SELECT 1 FROM tasks WHERE node=? AND status='running') RETURNING `+columns),
			node, epoch, leaseUntil, leaseUntil, model, now.UnixMilli(), now.UnixMilli(), node, epoch, model, node))
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return claimed, nil
}

type taskDB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// PostgreSQL locks the node before testing task authority. This orders claims
// and worker writes against concurrent connects, revocations and drain changes.
// SQLite already serializes writes and rechecks the node inside the UPDATE.
func (s *Store) nodeOperation(node string, epoch int64, invalid error, operation func(context.Context, taskDB) error) error {
	if epoch <= 0 {
		return invalid
	}
	ctx, cancel := operationContext()
	defer cancel()
	if !s.postgres {
		return operation(ctx, s.db)
	}
	tx, err := s.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRowContext(ctx, s.q(`SELECT 1 FROM nodes WHERE id=? AND epoch=? AND revoked=0 FOR UPDATE`), node, epoch).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid
	}
	if err != nil {
		return err
	}
	if err = operation(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func authority() string {
	return `id=? AND node=? AND epoch=? AND fence=? AND status='running' AND lease>? AND deadline>? AND EXISTS(SELECT 1 FROM nodes WHERE nodes.id=tasks.node AND nodes.epoch=tasks.epoch AND revoked=0)`
}
func args(id, node string, epoch, fence int64, now time.Time) []any {
	return []any{id, node, epoch, fence, now.UnixMilli(), now.UnixMilli()}
}
func affected(res sql.Result, err, errorNone error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errorNone
	}
	return nil
}
func (s *Store) MarkOutput(id, node string, epoch, fence int64, now time.Time) error {
	return s.workerUpdate(node, epoch, `output_started=1`, args(id, node, epoch, fence, now))
}
func (s *Store) Renew(id, node string, epoch, fence int64, now time.Time, lease time.Duration) error {
	if lease.Milliseconds() <= 0 {
		return ErrStale
	}
	until := now.Add(lease).UnixMilli()
	a := append([]any{until, until, until}, args(id, node, epoch, fence, now)...)
	return s.workerUpdate(node, epoch, `lease=CASE WHEN deadline<=? THEN deadline WHEN lease>? THEN lease ELSE ? END`, a)
}
func (s *Store) Complete(id, node string, epoch, fence int64, result string, now time.Time) error {
	return s.CompleteWithReason(id, node, epoch, fence, result, "", now)
}
func (s *Store) CompleteWithReason(id, node string, epoch, fence int64, result, reason string, now time.Time) error {
	a := append([]any{result, reason}, args(id, node, epoch, fence, now)...)
	return s.workerUpdate(node, epoch, `status='succeeded',result=?,finish_reason=?,error='',lease=0`, a)
}

// An unpublished batch may retry twice. The delay is persisted alongside the
// transition, so a controller restart cannot reset the retry schedule.
const retryAt = `CAST(? AS BIGINT)+CASE WHEN attempts=1 THEN 1000 ELSE 2000 END`
const retryable = `kind='batch' AND attempts<3 AND output_started=0 AND deadline>` + retryAt

func (s *Store) Fail(id, node string, epoch, fence int64, message string, now time.Time) error {
	ms := now.UnixMilli()
	a := append([]any{ms, ms, ms, message}, args(id, node, epoch, fence, now)...)
	return s.workerUpdate(node, epoch, `status=CASE WHEN `+retryable+` THEN 'queued' WHEN output_started=1 THEN 'interrupted' ELSE 'failed' END,
 available_at=CASE WHEN `+retryable+` THEN `+retryAt+` ELSE 0 END,error=?,lease=0`, a)
}
func (s *Store) workerUpdate(node string, epoch int64, assignments string, values []any) error {
	return s.nodeOperation(node, epoch, ErrStale, func(ctx context.Context, db taskDB) error {
		r, err := db.ExecContext(ctx, s.q(`UPDATE tasks SET `+assignments+` WHERE `+authority()), values...)
		return affected(r, err, ErrStale)
	})
}
func (s *Store) Cancel(tenant, id string) error {
	r, e := s.exec(s.q(`UPDATE tasks SET status=CASE WHEN status IN ('queued','running') THEN 'cancelled' ELSE status END,
 lease=0,available_at=0,error=CASE WHEN status IN ('queued','running') THEN '' ELSE error END WHERE tenant=? AND id=?`), tenant, id)
	return affected(r, e, ErrNotFound)
}
func (s *Store) Reap(now time.Time) error {
	ms := now.UnixMilli()
	_, err := s.exec(s.q(`UPDATE tasks SET status=CASE WHEN status='queued' THEN 'failed' WHEN `+retryable+` THEN 'queued'
 WHEN output_started=1 OR kind='chat' THEN 'interrupted' ELSE 'failed' END,
 available_at=CASE WHEN status='running' AND `+retryable+` THEN `+retryAt+` ELSE 0 END,lease=0,
 error=CASE WHEN deadline<=? THEN 'task deadline expired' ELSE 'execution authority expired' END
 WHERE (status='running' AND (lease<=? OR deadline<=? OR NOT EXISTS(SELECT 1 FROM nodes WHERE nodes.id=tasks.node AND nodes.epoch=tasks.epoch AND revoked=0)))
 OR (status='queued' AND deadline<=?)`), ms, ms, ms, ms, ms, ms, ms)
	return err
}
