package storage

import (
	"database/sql"
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
	if version != schemaVersion {
		t.Errorf("expected user_version %d, got %d", schemaVersion, version)
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

// A database created before the lineage columns existed must gain them without
// losing its rows: an operator upgrading mesh7 keeps their live grants.
func TestMigrateV1ToV2KeepsRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	v1 := []string{
		`CREATE TABLE grants (
			id TEXT PRIMARY KEY,
			agent TEXT NOT NULL,
			tools TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			granted_by TEXT DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`INSERT INTO grants (id, agent, tools, expires_at, created_at)
		 VALUES ('g1', 'claude', 'fs.*', '2099-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`PRAGMA user_version = 1`,
	}
	for _, s := range v1 {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	up, err := Open(path)
	if err != nil {
		t.Fatalf("migrating a v1 database failed: %v", err)
	}
	defer up.Close()

	var version int
	up.QueryRow("PRAGMA user_version").Scan(&version)
	if version != schemaVersion {
		t.Errorf("expected user_version %d after upgrade, got %d", schemaVersion, version)
	}

	// The pre-existing grant survives, with an empty origin rather than a
	// fabricated one.
	var id, approvalID, originTraceID string
	err = up.QueryRow(
		`SELECT id, COALESCE(approval_id, ''), COALESCE(origin_trace_id, '') FROM grants`,
	).Scan(&id, &approvalID, &originTraceID)
	if err != nil {
		t.Fatalf("legacy grant lost after migration: %v", err)
	}
	if id != "g1" {
		t.Errorf("expected grant g1, got %q", id)
	}
	if approvalID != "" || originTraceID != "" {
		t.Errorf("expected empty origin on a legacy grant, got %q/%q", approvalID, originTraceID)
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
