package approval

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"
)

func (s *Store) dbSave(pa *PendingApproval) {
	if s.db == nil {
		return
	}
	paramsJSON, _ := json.Marshal(pa.Params)
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO approvals (id, agent_id, tool, params, policy_rule, trace_id, callback_url, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pa.ID, pa.AgentID, pa.Tool, string(paramsJSON), pa.PolicyRule,
		pa.TraceID, pa.CallbackURL, string(pa.Status),
		pa.CreatedAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		slog.Warn("failed to persist approval", "id", pa.ID, "error", err)
	}
}

func (s *Store) dbUpdate(pa *PendingApproval) {
	if s.db == nil {
		return
	}
	resolvedAt := ""
	if !pa.ResolvedAt.IsZero() {
		resolvedAt = pa.ResolvedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.Exec(
		`UPDATE approvals SET status=?, resolved_by=?, resolved_at=?, reasoning=?, confidence=? WHERE id=?`,
		string(pa.Status), pa.ResolvedBy, resolvedAt, pa.Reasoning, pa.Confidence, pa.ID,
	)
	if err != nil {
		slog.Warn("failed to update approval", "id", pa.ID, "error", err)
	}
}

// LoadAll restores approvals from SQLite into the in-memory map.
// Pending approvals get new channels and timeout goroutines with remaining time.
func (s *Store) LoadAll() (int, error) {
	if s.db == nil {
		return 0, nil
	}
	rows, err := s.db.Query(
		`SELECT id, agent_id, tool, params, policy_rule, trace_id, callback_url,
		        status, created_at, resolved_by, resolved_at, reasoning, confidence
		 FROM approvals ORDER BY created_at DESC`,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	loaded := 0
	for rows.Next() {
		pa, err := scanApproval(rows)
		if err != nil {
			slog.Warn("failed to scan approval row", "error", err)
			continue
		}

		s.mu.Lock()
		s.pending[pa.ID] = pa
		s.mu.Unlock()

		if pa.Status == StatusPending {
			remaining := s.timeout - time.Since(pa.CreatedAt)
			if remaining <= 0 {
				s.expireLoaded(pa)
			} else {
				pa.Result = make(chan Resolution, 1)
				go s.timeoutAfter(pa, remaining)
			}
		}
		loaded++
	}
	return loaded, rows.Err()
}

func (s *Store) expireLoaded(pa *PendingApproval) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pa.Status = StatusTimeout
	pa.ResolvedAt = time.Now().UTC()
	s.dbUpdate(pa)
	s.MemoryWriter.WriteDecision(pa, Resolution{
		Status: StatusTimeout, ResolvedBy: "system:timeout", ResolvedAt: pa.ResolvedAt,
	})
}

func scanApproval(rows *sql.Rows) (*PendingApproval, error) {
	var (
		pa                             PendingApproval
		params, status                 string
		createdAt, resolvedBy          string
		resolvedAt, reasoning          string
		confidence                     float64
	)
	err := rows.Scan(
		&pa.ID, &pa.AgentID, &pa.Tool, &params, &pa.PolicyRule,
		&pa.TraceID, &pa.CallbackURL, &status, &createdAt,
		&resolvedBy, &resolvedAt, &reasoning, &confidence,
	)
	if err != nil {
		return nil, err
	}
	pa.Status = Status(status)
	pa.ResolvedBy = resolvedBy
	pa.Reasoning = reasoning
	pa.Confidence = confidence

	if params != "" {
		json.Unmarshal([]byte(params), &pa.Params)
	}
	if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		pa.CreatedAt = t
	}
	if resolvedAt != "" {
		if t, err := time.Parse(time.RFC3339Nano, resolvedAt); err == nil {
			pa.ResolvedAt = t
		}
	}
	return &pa, nil
}
