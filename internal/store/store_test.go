package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func database(t *testing.T) *Store {
	t.Helper()
	s, err := Open("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func node(t *testing.T, s *Store) int64 {
	t.Helper()
	if err := s.AddNode("n1", "secret", "m"); err != nil {
		t.Fatal(err)
	}
	e, err := s.Connect("n1", "secret")
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func task(t *testing.T, s *Store, kind string) Task {
	t.Helper()
	j, _, err := s.Create("tenant", "key", kind, "m", `{"model":"m"}`, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestIdempotencyAndTenantIsolation(t *testing.T) {
	s := database(t)
	a := task(t, s, "batch")
	b, exists, err := s.Create("tenant", "key", "batch", "m", `{"model":"m"}`, time.Now().Add(time.Minute))
	if err != nil || !exists || a.ID != b.ID {
		t.Fatalf("duplicate: %+v %v %v", b, exists, err)
	}
	_, _, err = s.Create("tenant", "key", "batch", "m", `{"model":"changed"}`, time.Now().Add(time.Minute))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict: %v", err)
	}
	if _, err = s.Get("other", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant leaked: %v", err)
	}
}
func TestExclusiveClaimAndExpiredResult(t *testing.T) {
	s := database(t)
	e := node(t, s)
	task(t, s, "batch")
	now := time.Now().Truncate(time.Millisecond)
	var wg sync.WaitGroup
	claims := make(chan Task, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, err := s.Claim("n1", e, "m", now, time.Second)
			if err == nil {
				claims <- j
			}
		}()
	}
	wg.Wait()
	close(claims)
	var got []Task
	for j := range claims {
		got = append(got, j)
	}
	if len(got) != 1 {
		t.Fatalf("claims=%d", len(got))
	}
	j := got[0]
	if err := s.Complete(j.ID, "n1", e, j.Fence, "late", now.Add(2*time.Second)); !errors.Is(err, ErrStale) {
		t.Fatalf("late result accepted: %v", err)
	}
	if err := s.Reap(now.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	newer, err := s.Claim("n1", e, "m", now.Add(3*time.Second), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if newer.Fence <= j.Fence {
		t.Fatal("fence did not increase")
	}
	if err := s.Complete(j.ID, "n1", e, j.Fence, "old", now.Add(3*time.Second)); !errors.Is(err, ErrStale) {
		t.Fatal("old fence accepted")
	}
}
func TestReconnectAndRevocationFenceExecution(t *testing.T) {
	s := database(t)
	e := node(t, s)
	task(t, s, "batch")
	j, err := s.Claim("n1", e, "m", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.Connect("n1", "secret")
	if err != nil || newer <= e {
		t.Fatal("no new connection epoch")
	}
	if err = s.Complete(j.ID, "n1", e, j.Fence, "bad", time.Now()); !errors.Is(err, ErrStale) {
		t.Fatal("old connection accepted")
	}
	if err = s.Revoke("n1"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Connect("n1", "secret"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked node connected")
	}
}
func TestOutputGateCancelAndDurability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "durable.db")
	s, err := Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	e := node(t, s)
	a := task(t, s, "chat")
	j, err := s.Claim("n1", e, "m", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.MarkOutput(j.ID, "n1", e, j.Fence, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.Cancel("other", a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross tenant cancel")
	}
	if err = s.Cancel("tenant", a.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(j.ID, "n1", e, j.Fence, "bad", time.Now()); !errors.Is(err, ErrStale) {
		t.Fatal("cancelled result accepted")
	}
	s.Close()
	s, err = Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	found, err := s.Get("tenant", a.ID)
	if err != nil || found.Status != "cancelled" || !found.OutputStarted {
		t.Fatalf("not durable: %+v %v", found, err)
	}
}

func TestTenantQueueIsBoundedAndDuplicatesStillResolve(t *testing.T) {
	s := database(t)
	for i := 0; i < 20; i++ {
		if _, _, err := s.Create("tenant", fmt.Sprint(i), "batch", "m", `{}`, time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Create("tenant", "overflow", "batch", "m", `{}`, time.Now().Add(time.Minute)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unbounded queue: %v", err)
	}
	if _, exists, err := s.Create("tenant", "0", "batch", "m", `{}`, time.Now().Add(time.Minute)); err != nil || !exists {
		t.Fatalf("duplicate blocked by capacity: %v", err)
	}
}

func TestPostgresAuthority(t *testing.T) {
	dsn := os.Getenv("SWARM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("real PostgreSQL not configured")
	}
	s, err := Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := ID()
	tenant := "test-" + id
	defer s.db.Exec(`DELETE FROM tasks WHERE tenant=$1`, tenant)
	defer s.db.Exec(`DELETE FROM nodes WHERE id=$1`, id)
	if err = s.AddNode(id, "test-secret", "m"); err != nil {
		t.Fatal(err)
	}
	epoch, err := s.Connect(id, "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := s.Create(tenant, "one", "batch", "m", `{}`, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(id, epoch, "m", time.Now(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != j.ID {
		t.Fatal("use a dedicated test database without other queued m jobs")
	}
	if err = s.Complete(j.ID, id, epoch, claimed.Fence, "ok", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(j.ID, id, epoch, claimed.Fence, "overwrite", time.Now()); !errors.Is(err, ErrStale) {
		t.Fatal("duplicate result accepted")
	}
}
