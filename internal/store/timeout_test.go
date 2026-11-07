package store

import (
	"os"
	"testing"
	"time"
)

func TestPostgresLockedNodeClaimIsBounded(t *testing.T) {
	dsn := os.Getenv("SWARM_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("real PostgreSQL not configured")
	}
	s, err := Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := "lock-test-" + ID()
	if err = s.AddNode(id, "secret", id); err != nil {
		t.Fatal(err)
	}
	epoch, err := s.Connect(id, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Create(id, "lock", "batch", id, `{}`, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	lock, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if _, err = lock.Exec(`SELECT id FROM nodes WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Claim(id, epoch, id, time.Now(), 30*time.Second); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("claim unexpectedly acquired a locked node")
		}
	case <-time.After(4 * time.Second):
		lock.Rollback()
		<-done
		t.Fatal("PostgreSQL node lock blocked the scheduler for more than four seconds")
	}
}
