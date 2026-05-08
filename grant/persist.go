package grant

import (
	"database/sql"
	"log/slog"
	"time"
)

// SetDB attaches a SQLite database for durable persistence.
func (s *Store) SetDB(db *sql.DB) { s.db = db }

func (s *Store) dbSave(g *Grant) {
	if s.db == nil {
		return
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO grants (id, agent, tools, expires_at, granted_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		g.ID, g.Agent, g.Tools,
		g.ExpiresAt.UTC().Format(time.RFC3339Nano),
		g.GrantedBy,
		g.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		slog.Warn("failed to persist grant", "id", g.ID, "error", err)
	}
}

func (s *Store) dbDelete(id string) {
	if s.db == nil {
		return
	}
	s.db.Exec(`DELETE FROM grants WHERE id=?`, id)
}

func (s *Store) dbCleanup() {
	if s.db == nil {
		return
	}
	s.db.Exec(`DELETE FROM grants WHERE expires_at < ?`, time.Now().UTC().Format(time.RFC3339Nano))
}

// LoadAll restores active grants from SQLite.
func (s *Store) LoadAll() (int, error) {
	if s.db == nil {
		return 0, nil
	}
	s.dbCleanup()

	rows, err := s.db.Query(
		`SELECT id, agent, tools, expires_at, granted_by, created_at FROM grants`,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	loaded := 0
	for rows.Next() {
		var (
			g                        Grant
			expiresAt, createdAt     string
		)
		if err := rows.Scan(&g.ID, &g.Agent, &g.Tools, &expiresAt, &g.GrantedBy, &createdAt); err != nil {
			slog.Warn("failed to scan grant row", "error", err)
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, expiresAt); err == nil {
			g.ExpiresAt = t
		}
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			g.CreatedAt = t
		}
		if g.IsExpired() {
			continue
		}

		s.mu.Lock()
		s.grants = append(s.grants, &g)
		s.mu.Unlock()
		loaded++
	}
	return loaded, rows.Err()
}
