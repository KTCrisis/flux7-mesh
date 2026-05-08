package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenAndMigrate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var version int
	db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 1 {
		t.Errorf("expected user_version 1, got %d", version)
	}

	// Verify tables exist
	for _, table := range []string{"approvals", "grants"} {
		var name string
		err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s not found: %v", table, err)
		}
	}
}

func TestOpenIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db1.Exec(`INSERT INTO approvals (id, agent_id, tool, status, created_at) VALUES ('a1', 'claude', 'fs.write', 'pending', '2026-01-01T00:00:00Z')`)
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	var count int
	db2.QueryRow("SELECT COUNT(*) FROM approvals").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 approval after reopen, got %d", count)
	}
}

func TestFileCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "state.db")
	os.MkdirAll(filepath.Dir(path), 0o755)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Error("database file was not created")
	}
}
