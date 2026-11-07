package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLifecycleSQLite(t *testing.T) {
	testLifecycle(t, "sqlite", filepath.Join(t.TempDir(), "tasks.db"))
}

func TestLifecyclePostgres(t *testing.T) {
	dsn := os.Getenv("SWARM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("real PostgreSQL not configured")
	}
	testLifecycle(t, "pgx", dsn)
}

func testLifecycle(t *testing.T, driver, dsn string) {
	t.Helper()
	s, err := Open(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	if err = s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	nodeID, tenant, model := ID(), ID(), ID()
	defer func() {
		s.db.Exec(s.q(`DELETE FROM tasks WHERE tenant=?`), tenant)
		s.db.Exec(s.q(`DELETE FROM nodes WHERE id=?`), nodeID)
	}()
	if err = s.AddNode(nodeID, "secret", model); err != nil {
		t.Fatal(err)
	}
	epoch, err := s.Connect(nodeID, "secret")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Millisecond)
	create := func(key, kind string, deadline time.Time) Task {
		t.Helper()
		j, _, err := s.Create(tenant, key, kind, model, `{"private":true}`, deadline)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	requireNoClaim := func(at time.Time) {
		t.Helper()
		if j, err := s.Claim(nodeID, epoch, model, at, time.Second); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unexpected claim at %v: %+v %v", at, j, err)
		}
	}
	claim := func(at time.Time) Task {
		t.Helper()
		j, err := s.Claim(nodeID, epoch, model, at, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	reopen := func() {
		t.Helper()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(driver, dsn)
		if err != nil {
			t.Fatal(err)
		}
		s = reopened
	}
	first := create("first", "batch", now.Add(time.Minute))
	if j, err := s.NextQueued(model, now); err != nil || j.ID != first.ID || j.Status != "queued" || j.Attempts != 0 {
		t.Fatalf("preview changed the task: %+v %v", j, err)
	}
	create("second", "batch", now.Add(time.Minute))
	var wg sync.WaitGroup
	claims := make(chan Task, 12)
	for range cap(claims) {
		wg.Go(func() {
			j, err := s.Claim(nodeID, epoch, model, now, time.Second)
			if err == nil {
				claims <- j
			} else if !errors.Is(err, ErrNotFound) {
				t.Errorf("claim failed: %v", err)
			}
		})
	}
	wg.Wait()
	close(claims)
	if len(claims) != 1 {
		t.Fatalf("same node claimed %d tasks", len(claims))
	}
	running := <-claims
	if err := s.SetDraining(nodeID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteWithReason(running.ID, nodeID, epoch, running.Fence, "private-result", "length", now); err != nil {
		t.Fatal(err)
	}
	requireNoClaim(now)
	reopen()
	nodes, err := s.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range nodes {
		if n.ID == nodeID {
			found = n.Draining && !n.Revoked && n.Model == model && n.Epoch == epoch
		}
	}
	if !found {
		t.Fatalf("node drain lost across reopen: %+v", nodes)
	}
	listed, err := s.ListTasks(tenant, "batch", 2)
	if err != nil || len(listed) != 2 {
		t.Fatalf("task listing: %+v %v", listed, err)
	}
	for _, j := range listed {
		if j.Request != "" || j.Result != "" || (j.ID == running.ID && j.FinishReason != "length") {
			t.Fatalf("invalid task summary: %+v", j)
		}
	}
	if other, err := s.ListTasks(tenant+"-other", "batch", 2); err != nil || len(other) != 0 {
		t.Fatalf("tenant isolation in listing: %+v %v", other, err)
	}
	if err := s.SetDraining(nodeID, false); err != nil {
		t.Fatal(err)
	}
	retried := claim(now)
	if err := s.Fail(retried.ID, nodeID, epoch, retried.Fence, "backend down", now); err != nil {
		t.Fatal(err)
	}
	requireNoClaim(now.Add(100 * time.Millisecond))
	if j, err := s.NextQueued(model, now.Add(100*time.Millisecond)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("preview returned delayed task: %+v %v", j, err)
	}
	secondAttempt := claim(now.Add(time.Second))
	if secondAttempt.ID != retried.ID || secondAttempt.Attempts != 2 || secondAttempt.Fence <= retried.Fence {
		t.Fatalf("invalid retry authority: %+v", secondAttempt)
	}
	if err := s.Complete(secondAttempt.ID, nodeID, epoch, secondAttempt.Fence, "late", now.Add(2*time.Second)); !errors.Is(err, ErrStale) {
		t.Fatalf("accepted result at lease boundary: %v", err)
	}
	if err := s.Reap(now.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	requireNoClaim(now.Add(3 * time.Second))
	third := claim(now.Add(4 * time.Second))
	if err := s.Fail(third.ID, nodeID, epoch, third.Fence, "backend down", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Get(tenant, third.ID); err != nil || j.Status != "failed" || j.Attempts != 3 || j.Lease != 0 {
		t.Fatalf("retry limit ignored: %+v %v", j, err)
	}
	queued := create("expired", "chat", now.Add(5*time.Second))
	if err := s.Reap(now.Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Get(tenant, queued.ID); err != nil || j.Status != "failed" || j.Attempts != 0 {
		t.Fatalf("queued job expired without running: %+v %v", j, err)
	}
	chat := create("chat", "chat", now.Add(time.Minute))
	chatRun := claim(now.Add(5 * time.Second))
	if err := s.MarkOutput(chat.ID, nodeID, epoch, chatRun.Fence, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(tenant, chat.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(tenant, chat.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Renew(chat.ID, nodeID, epoch, chatRun.Fence, now.Add(5*time.Second), time.Second); !errors.Is(err, ErrStale) {
		t.Fatalf("cancelled lease renewed: %v", err)
	}
	if j, err := s.Get(tenant, chat.ID); err != nil || j.Status != "cancelled" || j.Lease != 0 || !j.OutputStarted {
		t.Fatalf("cancel lost state: %+v %v", j, err)
	}

	recoveryTime := now.Add(6 * time.Second)
	create("recovery", "batch", now.Add(time.Minute))
	old := claim(recoveryTime)
	oldEpoch := epoch
	reopen()
	if j, err := s.Get(tenant, old.ID); err != nil || j.Status != "running" || j.Fence != old.Fence || j.Lease != old.Lease {
		t.Fatalf("running authority lost across restart: %+v %v", j, err)
	}
	epoch, err = s.Connect(nodeID, "secret")
	if err != nil || epoch <= oldEpoch {
		t.Fatalf("connection epoch lost across restart: %d %v", epoch, err)
	}
	if err := s.Disconnect(nodeID, oldEpoch); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(old.ID, nodeID, oldEpoch, old.Fence, "stale", recoveryTime); !errors.Is(err, ErrStale) {
		t.Fatalf("old epoch completed after reconnect: %v", err)
	}
	if err := s.Reap(recoveryTime); err != nil {
		t.Fatal(err)
	}
	reopen()
	requireNoClaim(recoveryTime.Add(999 * time.Millisecond))
	resumed := claim(recoveryTime.Add(time.Second))
	if resumed.ID != old.ID || resumed.Fence <= old.Fence || resumed.Epoch != epoch || resumed.Attempts != 2 || resumed.Error != "" {
		t.Fatalf("retry lost durable generation: %+v", resumed)
	}
	if err := s.Complete(resumed.ID, nodeID, epoch, resumed.Fence, "recovered", recoveryTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	deadline := now.Add(8500 * time.Millisecond)
	short := create("short", "batch", deadline)
	shortRun := claim(now.Add(8 * time.Second))
	if shortRun.Lease != deadline.UnixMilli() {
		t.Fatalf("claim lease %d extends past deadline %d", shortRun.Lease, deadline.UnixMilli())
	}
	if err := s.Renew(short.ID, nodeID, epoch, shortRun.Fence, deadline.Add(-time.Millisecond), time.Minute); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Get(tenant, short.ID); err != nil || j.Lease != deadline.UnixMilli() {
		t.Fatalf("renewal extended beyond deadline: %+v %v", j, err)
	}
	if err := s.Complete(short.ID, nodeID, epoch, shortRun.Fence, "late", deadline); !errors.Is(err, ErrStale) {
		t.Fatalf("accepted result at deadline: %v", err)
	}
	if err := s.Reap(deadline); err != nil {
		t.Fatal(err)
	}
	if j, err := s.Get(tenant, short.ID); err != nil || j.Status != "failed" || j.Attempts != 1 || j.Lease != 0 {
		t.Fatalf("expired task retried: %+v %v", j, err)
	}
}

func TestUnconnectedNodeCannotClaim(t *testing.T) {
	s := database(t)
	if err := s.AddNode("unconnected", "secret", "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NodeModel("unconnected", 0); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("epoch zero authenticated: %v", err)
	}
	task(t, s, "batch")
	if j, err := s.Claim("unconnected", 0, "m", time.Now(), time.Second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("task assigned before node authenticated: %+v %v", j, err)
	}
}

func TestSQLiteUpgradesExistingTaskRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	for _, query := range []string{
		`CREATE TABLE nodes(id TEXT PRIMARY KEY,secret TEXT NOT NULL,model TEXT NOT NULL,epoch BIGINT NOT NULL DEFAULT 0,revoked INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE tasks(id TEXT PRIMARY KEY,tenant TEXT NOT NULL,idem TEXT NOT NULL,payload_hash TEXT NOT NULL,kind TEXT NOT NULL,model TEXT NOT NULL,request TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'queued',node TEXT NOT NULL DEFAULT '',epoch BIGINT NOT NULL DEFAULT 0,fence BIGINT NOT NULL DEFAULT 0,attempts INTEGER NOT NULL DEFAULT 0,lease BIGINT NOT NULL DEFAULT 0,deadline BIGINT NOT NULL,output_started INTEGER NOT NULL DEFAULT 0,result TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',created BIGINT NOT NULL,UNIQUE(tenant,idem))`,
		`INSERT INTO nodes(id,secret,model,epoch) VALUES('old-node','hash','m',4)`,
		`INSERT INTO tasks(id,tenant,idem,payload_hash,kind,model,request,status,result,deadline,created) VALUES('old-task','tenant','key','hash','batch','m','{}','succeeded','existing-result',2000,1000)`,
	} {
		if _, err := old.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if j, err := s.Get("tenant", "old-task"); err != nil || j.Result != "existing-result" || j.FinishReason != "" || j.AvailableAt != 0 {
		t.Fatalf("migration lost legacy task: %+v %v", j, err)
	}
	if nodes, err := s.ListNodes(); err != nil || len(nodes) != 1 || nodes[0].Epoch != 4 || nodes[0].Draining {
		t.Fatalf("migration lost legacy node: %+v %v", nodes, err)
	}
}
