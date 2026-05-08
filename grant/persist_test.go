package grant

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/KTCrisis/agent-mesh/storage"
)

func TestPersistGrantAndReload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db1, _ := storage.Open(dbPath)
	s1 := NewStore()
	s1.SetDB(db1)

	g := s1.Add("claude", "filesystem.*", "user:marc", 1*time.Hour)
	db1.Close()

	db2, _ := storage.Open(dbPath)
	s2 := NewStore()
	s2.SetDB(db2)
	defer db2.Close()

	n, err := s2.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 loaded, got %d", n)
	}

	list := s2.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 active grant, got %d", len(list))
	}
	if list[0].ID != g.ID {
		t.Errorf("expected ID %s, got %s", g.ID, list[0].ID)
	}
	if list[0].Agent != "claude" || list[0].Tools != "filesystem.*" {
		t.Errorf("wrong data: agent=%s tools=%s", list[0].Agent, list[0].Tools)
	}
}

func TestPersistRevokeAndReload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db1, _ := storage.Open(dbPath)
	s1 := NewStore()
	s1.SetDB(db1)

	g := s1.Add("claude", "filesystem.*", "user:marc", 1*time.Hour)
	s1.Revoke(g.ID)
	db1.Close()

	db2, _ := storage.Open(dbPath)
	s2 := NewStore()
	s2.SetDB(db2)
	defer db2.Close()

	n, _ := s2.LoadAll()
	if n != 0 {
		t.Errorf("expected 0 after revoke, got %d", n)
	}
}

func TestPersistExpiredGrantNotLoaded(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db1, _ := storage.Open(dbPath)
	s1 := NewStore()
	s1.SetDB(db1)

	s1.Add("claude", "filesystem.*", "user:marc", 1*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	db1.Close()

	db2, _ := storage.Open(dbPath)
	s2 := NewStore()
	s2.SetDB(db2)
	defer db2.Close()

	n, _ := s2.LoadAll()
	if n != 0 {
		t.Errorf("expired grant should not be loaded, got %d", n)
	}
}

func TestGrantNoDB(t *testing.T) {
	s := NewStore()
	g := s.Add("claude", "filesystem.*", "", 1*time.Hour)
	if g == nil {
		t.Fatal("Add should work without DB")
	}
	n, err := s.LoadAll()
	if err != nil || n != 0 {
		t.Errorf("LoadAll without DB should return 0, got %d err=%v", n, err)
	}
}

func TestPersistCheckAfterReload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	db1, _ := storage.Open(dbPath)
	s1 := NewStore()
	s1.SetDB(db1)
	s1.Add("claude", "filesystem.*", "user:marc", 1*time.Hour)
	db1.Close()

	db2, _ := storage.Open(dbPath)
	s2 := NewStore()
	s2.SetDB(db2)
	defer db2.Close()
	s2.LoadAll()

	if s2.Check("claude", "filesystem.write_file") == nil {
		t.Error("grant should match after reload")
	}
	if s2.Check("worker", "filesystem.write_file") != nil {
		t.Error("grant should not match different agent")
	}
}
