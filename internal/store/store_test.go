package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func newInstance(t *testing.T, s *Store, id, user string) Instance {
	t.Helper()
	now := time.Now().UTC()
	in, err := s.CreateInstance(context.Background(), Instance{ID: id, UserID: user, State: StateStopped, CreatedAt: now, LastActivityAt: now})
	if err != nil {
		t.Fatal(err)
	}
	return in
}

func TestOneInstancePerUser(t *testing.T) {
	s, _ := open(t)
	newInstance(t, s, "inst_a", "u1")
	now := time.Now()
	got, err := s.CreateInstance(context.Background(), Instance{ID: "inst_b", UserID: "u1", State: StateStopped, CreatedAt: now, LastActivityAt: now})
	if !errors.Is(err, ErrConflict) || got.ID != "inst_a" {
		t.Fatalf("expected conflict pointing to inst_a, got %v %v", got.ID, err)
	}
}

func TestHistoryKeepsLatest20(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	newInstance(t, s, "inst_a", "u1")
	for i := 1; i <= 21; i++ {
		if err := s.InsertRun(ctx, Run{ID: fmt.Sprintf("run_%02d", i), InstanceID: "inst_a", Agent: "claude", Prompt: "p", Status: RunSucceeded, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.ListRuns(ctx, "inst_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != HistoryLimit || runs[0].ID != "run_21" || runs[len(runs)-1].ID != "run_02" {
		t.Fatalf("got %d runs, first %s last %s", len(runs), runs[0].ID, runs[len(runs)-1].ID)
	}
	if _, err := s.GetRun(ctx, "inst_a", "run_01"); !errors.Is(err, ErrNotFound) {
		t.Fatal("oldest run should be removed from storage")
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&total); err != nil || total != HistoryLimit {
		t.Fatalf("storage holds %d rows", total)
	}
}

func TestHistoryConcurrentInserts(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	newInstance(t, s, "inst_a", "u1")
	newInstance(t, s, "inst_b", "u2")
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inst := "inst_a"
			if i%2 == 1 {
				inst = "inst_b"
			}
			if err := s.InsertRun(ctx, Run{ID: fmt.Sprintf("run_%d", i), InstanceID: inst, Agent: "codex", Prompt: "p", Status: RunRunning, StartedAt: time.Now()}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for _, inst := range []string{"inst_a", "inst_b"} {
		runs, _ := s.ListRuns(ctx, inst)
		if len(runs) != HistoryLimit {
			t.Fatalf("%s has %d runs", inst, len(runs))
		}
		for _, r := range runs {
			if r.InstanceID != inst {
				t.Fatalf("run %s leaked into %s", r.ID, inst)
			}
		}
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	s, dir := open(t)
	ctx := context.Background()
	newInstance(t, s, "inst_a", "u1")
	_ = s.InsertRun(ctx, Run{ID: "run_1", InstanceID: "inst_a", Agent: "claude", Prompt: "hello", Status: RunRunning, StartedAt: time.Now()})
	_ = s.UpsertConnection(ctx, Connection{UserID: "u1", Provider: "github", Status: ConnConnected, Secret: "v1:x", UpdatedAt: time.Now()})
	s.Close()

	s2, err := OpenPath(ctx, filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.GetInstance(ctx, "inst_a"); err != nil {
		t.Fatal(err)
	}
	if c, err := s2.GetConnection(ctx, "u1", "github"); err != nil || c.Status != ConnConnected {
		t.Fatalf("connection lost: %v", err)
	}
	n, err := s2.InterruptRunningRuns(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("interrupt: %d %v", n, err)
	}
	r, _ := s2.GetRun(ctx, "inst_a", "run_1")
	if r.Status != RunInterrupted || r.ErrorCode != "gateway_restarted" {
		t.Fatalf("run status %s", r.Status)
	}
}

func TestDeleteInstanceCascadesRuns(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	newInstance(t, s, "inst_a", "u1")
	_ = s.InsertRun(ctx, Run{ID: "run_1", InstanceID: "inst_a", Agent: "claude", Prompt: "p", Status: RunSucceeded, StartedAt: time.Now()})
	if err := s.DeleteInstance(ctx, "inst_a"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n)
	if n != 0 {
		t.Fatalf("runs left: %d", n)
	}
}

func TestOAuthStateSingleUse(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.PutOAuthState(ctx, OAuthState{StateHash: "h", UserID: "u1", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	st, err := s.TakeOAuthState(ctx, "h")
	if err != nil || st.UserID != "u1" {
		t.Fatalf("take: %v", err)
	}
	if _, err := s.TakeOAuthState(ctx, "h"); !errors.Is(err, ErrNotFound) {
		t.Fatal("state reused")
	}
}
