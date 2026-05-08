package approval

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/KTCrisis/agent-mesh/storage"
)

func newTestStoreWithDB(t *testing.T) *Store {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	s := NewStore(5 * time.Minute)
	s.Notifier = NewNotifier("")
	s.SetDB(db)
	return s
}

func TestPersistSubmitAndReload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	// Store 1: submit an approval
	db1, _ := storage.Open(dbPath)
	s1 := NewStore(5 * time.Minute)
	s1.Notifier = NewNotifier("")
	s1.SetDB(db1)

	pa := s1.Submit("claude", "fs.write", "human_approval", map[string]any{"path": "/tmp/x"}, "")
	db1.Close()

	// Store 2: reload from disk
	db2, _ := storage.Open(dbPath)
	s2 := NewStore(5 * time.Minute)
	s2.Notifier = NewNotifier("")
	s2.SetDB(db2)
	defer db2.Close()

	n, err := s2.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 loaded, got %d", n)
	}

	got := s2.Get(pa.ID)
	if got == nil {
		t.Fatal("approval not found after reload")
	}
	if got.AgentID != "claude" || got.Tool != "fs.write" {
		t.Errorf("wrong data: agent=%s tool=%s", got.AgentID, got.Tool)
	}
	if got.Status != StatusPending {
		t.Errorf("expected pending, got %s", got.Status)
	}
	if got.Result == nil {
		t.Error("pending approval should have a Result channel after reload")
	}
}

func TestPersistResolveAndReload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db1, _ := storage.Open(dbPath)
	s1 := NewStore(5 * time.Minute)
	s1.Notifier = NewNotifier("")
	s1.SetDB(db1)

	pa := s1.Submit("claude", "fs.write", "human_approval", nil, "")
	s1.Resolve(pa.ID, StatusApproved, ResolveOpts{ResolvedBy: "user:marc", Reasoning: "ok"})
	db1.Close()

	db2, _ := storage.Open(dbPath)
	s2 := NewStore(5 * time.Minute)
	s2.Notifier = NewNotifier("")
	s2.SetDB(db2)
	defer db2.Close()

	n, _ := s2.LoadAll()
	if n != 1 {
		t.Fatalf("expected 1 loaded, got %d", n)
	}

	got := s2.Get(pa.ID)
	if got.Status != StatusApproved {
		t.Errorf("expected approved, got %s", got.Status)
	}
	if got.ResolvedBy != "user:marc" {
		t.Errorf("expected user:marc, got %s", got.ResolvedBy)
	}
}

func TestPersistExpiredPendingBecomesTimeout(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db1, _ := storage.Open(dbPath)
	s1 := NewStore(1 * time.Millisecond)
	s1.Notifier = NewNotifier("")
	s1.SetDB(db1)

	s1.Submit("claude", "fs.write", "human_approval", nil, "")
	time.Sleep(50 * time.Millisecond) // let it persist before closing
	db1.Close()

	// Reload with a store that has 1ms timeout — the approval is already expired
	db2, _ := storage.Open(dbPath)
	s2 := NewStore(1 * time.Millisecond)
	s2.Notifier = NewNotifier("")
	s2.SetDB(db2)
	defer db2.Close()

	s2.LoadAll()

	list := s2.List()
	if len(list) != 1 {
		t.Fatalf("expected 1, got %d", len(list))
	}
	if list[0].Status != StatusTimeout {
		t.Errorf("expected timeout, got %s", list[0].Status)
	}
}

func TestPersistParams(t *testing.T) {
	s := newTestStoreWithDB(t)

	params := map[string]any{"path": "/tmp/x", "content": "hello"}
	pa := s.Submit("claude", "fs.write", "human_approval", params, "")

	// Reload
	s2 := NewStore(5 * time.Minute)
	s2.Notifier = NewNotifier("")
	s2.SetDB(s.db)

	s2.LoadAll()
	got := s2.Get(pa.ID)
	if got == nil {
		t.Fatal("not found")
	}
	if got.Params["path"] != "hello" && got.Params["content"] != "hello" {
		// just check params were restored
	}
	if len(got.Params) != 2 {
		t.Errorf("expected 2 params, got %d", len(got.Params))
	}
}

func TestNoDB(t *testing.T) {
	s := NewStore(5 * time.Minute)
	s.Notifier = NewNotifier("")
	// no SetDB — should work as pure in-memory
	pa := s.Submit("claude", "fs.write", "human_approval", nil, "")
	if pa == nil {
		t.Fatal("submit should work without DB")
	}
	n, err := s.LoadAll()
	if err != nil || n != 0 {
		t.Errorf("LoadAll without DB should return 0, got %d err=%v", n, err)
	}
}
