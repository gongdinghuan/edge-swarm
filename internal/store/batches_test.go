package store

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestBatchGroupAtomicIdempotentAndScoped(t *testing.T) {
	testBatchGroupAtomicIdempotentAndScoped(t, database(t), "owner", "m")
}

func TestPostgresBatchGroup(t *testing.T) {
	dsn := os.Getenv("SWARM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("real PostgreSQL not configured")
	}
	s, err := Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	name := "batch-test-" + ID()
	testBatchGroupAtomicIdempotentAndScoped(t, s, name, name)
}

func testBatchGroupAtomicIdempotentAndScoped(t *testing.T, s *Store, tenant, model string) {
	t.Helper()
	now := time.Now()
	items := []BatchInput{{Model: model, Request: `{"data":"one"}`}, {Model: model, Request: `{"data":"two"}`}}
	b, existing, err := s.CreateBatchGroup(tenant, "group", items, now.Add(time.Minute))
	if err != nil || existing || len(b.Jobs) != 2 || b.Status != "queued" {
		t.Fatalf("batch: %+v existing=%v err=%v", b, existing, err)
	}
	duplicate, existing, err := s.CreateBatchGroup(tenant, "group", items, now.Add(time.Minute))
	if err != nil || !existing || duplicate.ID != b.ID || duplicate.Jobs[1].ID != b.Jobs[1].ID {
		t.Fatalf("duplicate: %+v %v", duplicate, err)
	}
	if _, _, err = s.CreateBatchGroup(tenant, "group", items[:1], now.Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed payload: %v", err)
	}
	if _, err = s.GetBatchGroup("foreign", b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign lookup: %v", err)
	}
	if err = s.CancelBatchGroup("foreign", b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign cancel: %v", err)
	}
	if err = s.CancelBatchGroup(tenant, b.ID); err != nil {
		t.Fatal(err)
	}
	found, err := s.GetBatchGroup(tenant, b.ID)
	if err != nil || found.Status != "cancelled" || found.Jobs[1].Status != "cancelled" {
		t.Fatalf("cancel: %+v %v", found, err)
	}
	for i := 0; i < 19; i++ {
		if _, _, err = s.Create(tenant, fmt.Sprint(i), "batch", model, `{}`, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err = s.CreateBatchGroup(tenant, "overflow", items, now.Add(time.Minute)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("overflow: %v", err)
	}
	if _, _, err = s.Create(tenant, "last", "batch", model, `{}`, now.Add(time.Minute)); err != nil {
		t.Fatalf("failed batch inserted partial work: %v", err)
	}
}
