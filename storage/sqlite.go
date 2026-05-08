package storage

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	var version int
	db.QueryRow("PRAGMA user_version").Scan(&version)

	if version < 1 {
		stmts := []string{
			`CREATE TABLE IF NOT EXISTS approvals (
				id TEXT PRIMARY KEY,
				agent_id TEXT NOT NULL,
				tool TEXT NOT NULL,
				params TEXT DEFAULT '{}',
				policy_rule TEXT DEFAULT '',
				trace_id TEXT DEFAULT '',
				callback_url TEXT DEFAULT '',
				status TEXT NOT NULL DEFAULT 'pending',
				created_at TEXT NOT NULL,
				resolved_by TEXT DEFAULT '',
				resolved_at TEXT DEFAULT '',
				reasoning TEXT DEFAULT '',
				confidence REAL DEFAULT 0
			)`,
			`CREATE INDEX IF NOT EXISTS idx_approvals_status ON approvals(status)`,
			`CREATE INDEX IF NOT EXISTS idx_approvals_agent ON approvals(agent_id)`,
			`CREATE TABLE IF NOT EXISTS grants (
				id TEXT PRIMARY KEY,
				agent TEXT NOT NULL,
				tools TEXT NOT NULL,
				expires_at TEXT NOT NULL,
				granted_by TEXT DEFAULT '',
				created_at TEXT NOT NULL
			)`,
			`PRAGMA user_version = 1`,
		}
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				return fmt.Errorf("schema v1: %w", err)
			}
		}
	}
	return nil
}
