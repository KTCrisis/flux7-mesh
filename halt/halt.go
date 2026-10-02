// Package halt is the emergency stop: an operator stops every tool call of
// one agent, of one session, or of everything, at once, and lifts the stop
// later. A halt is checked before policy, grants and approvals, so nothing
// configured elsewhere can let a halted call through.
//
// Halts live in SQLite when a state database is configured. Every mesh7
// process reloads them from it at most every SyncEvery, so a stop decided on
// the daemon also stops a standalone `mesh7 --mcp` client started before it.
package halt

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/KTCrisis/flux7-mesh/grant"
	"github.com/KTCrisis/flux7-mesh/internal/match"
)

// Scope says what a halt stops.
type Scope string

const (
	ScopeAll     Scope = "all"     // every agent, every session
	ScopeAgent   Scope = "agent"   // one agent ID (a glob is accepted)
	ScopeSession Scope = "session" // one session ID
)

// SyncEvery bounds how stale another process's view of the halts can be.
const SyncEvery = time.Second

var ErrNotFound = errors.New("no active halt with this ID")

// Halt is one emergency stop.
type Halt struct {
	ID        string    `json:"id"`
	Scope     Scope     `json:"scope"`
	Target    string    `json:"target,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ResumedBy string    `json:"resumed_by,omitempty"`
	ResumedAt time.Time `json:"resumed_at,omitzero"`
	// RevokedGrants are the grants this halt took away, put back on resume
	// when they have not expired in the meantime.
	RevokedGrants []grant.Grant `json:"revoked_grants,omitempty"`
}

// Covers reports whether a call by agentID in sessionID falls under the halt.
func (h *Halt) Covers(agentID, sessionID string) bool {
	switch h.Scope {
	case ScopeAll:
		return true
	case ScopeAgent:
		return match.Glob(h.Target, agentID)
	case ScopeSession:
		return sessionID != "" && sessionID == h.Target
	}
	return false
}

// Describe names what the halt stops, for messages and traces.
func (h *Halt) Describe() string {
	switch h.Scope {
	case ScopeAll:
		return "all agents"
	case ScopeAgent:
		return "agent " + h.Target
	case ScopeSession:
		return "session " + h.Target
	}
	return string(h.Scope)
}

// Message is what a halted caller is told.
func (h *Halt) Message() string {
	msg := fmt.Sprintf("halted by operator (%s)", h.Describe())
	if h.Reason != "" {
		msg += ": " + h.Reason
	}
	return msg + ". No tool call goes through until the halt is lifted."
}

// Store keeps the active halts.
type Store struct {
	mu       sync.RWMutex
	active   []*Halt
	db       *sql.DB
	lastSync time.Time
}

// NewStore returns a store backed by db (nil keeps halts in memory, for this
// process only) and loads the halts already active.
func NewStore(db *sql.DB) (*Store, error) {
	s := &Store{db: db}
	if db != nil {
		if err := s.reload(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Start opens a halt. Starting a halt identical to an active one returns the
// active one: pressing the button twice does not stack two stops.
func (s *Store) Start(scope Scope, target, reason, by string) (*Halt, bool, error) {
	target = strings.TrimSpace(target)
	switch scope {
	case ScopeAll:
		target = ""
	case ScopeAgent, ScopeSession:
		if target == "" {
			return nil, false, fmt.Errorf("a %s halt needs a target", scope)
		}
	default:
		return nil, false, fmt.Errorf("unknown scope %q (all, agent, session)", scope)
	}

	s.syncIfStale()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.active {
		if h.Scope == scope && h.Target == target {
			return h, false, nil
		}
	}
	h := &Halt{
		ID: newID(), Scope: scope, Target: target, Reason: reason,
		CreatedBy: by, CreatedAt: time.Now().UTC(),
	}
	if err := s.dbInsert(h); err != nil {
		return nil, false, err
	}
	s.active = append(s.active, h)
	return h, true, nil
}

// RecordRevoked attaches the grants a halt took away, so that resuming can
// restore them.
func (s *Store) RecordRevoked(id string, grants []grant.Grant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range s.active {
		if h.ID == id {
			h.RevokedGrants = append(h.RevokedGrants, grants...)
			s.dbUpdateRevoked(h)
			return
		}
	}
}

// Resume lifts an active halt, by ID or by a prefix of at least 4 characters,
// and returns it with the grants it had revoked.
func (s *Store) Resume(id, by string) (*Halt, error) {
	s.syncIfStale()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, h := range s.active {
		if h.ID == id || (len(id) >= 4 && strings.HasPrefix(h.ID, id)) {
			h.ResumedBy, h.ResumedAt = by, time.Now().UTC()
			if err := s.dbResume(h); err != nil {
				return nil, err
			}
			s.active = append(s.active[:i], s.active[i+1:]...)
			return h, nil
		}
	}
	return nil, ErrNotFound
}

// Active returns the halts in force, oldest first.
func (s *Store) Active() []*Halt {
	s.syncIfStale()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]*Halt(nil), s.active...)
}

// Match returns the halt a call falls under, or nil. The broadest halt wins,
// so the caller is told about a global stop rather than a narrower one.
func (s *Store) Match(agentID, sessionID string) *Halt {
	s.syncIfStale()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var found *Halt
	for _, h := range s.active {
		if h.Covers(agentID, sessionID) && (found == nil || rank(h.Scope) < rank(found.Scope)) {
			found = h
		}
	}
	return found
}

func rank(sc Scope) int {
	switch sc {
	case ScopeAll:
		return 0
	case ScopeAgent:
		return 1
	}
	return 2
}

// syncIfStale reloads the active halts from the database when the copy in
// memory is older than SyncEvery. Another process may have started or lifted
// a halt in the meantime.
func (s *Store) syncIfStale() {
	if s.db == nil {
		return
	}
	s.mu.RLock()
	fresh := time.Since(s.lastSync) < SyncEvery
	s.mu.RUnlock()
	if fresh {
		return
	}
	if err := s.reload(); err != nil {
		// Keep the last known halts: failing open here would lift a stop
		// because the database hiccuped.
		slog.Warn("halt: reload failed, keeping the last known halts", "error", err)
	}
}

func (s *Store) reload() error {
	rows, err := s.db.Query(`SELECT id, scope, target, reason, created_by, created_at, revoked_grants
		FROM halts WHERE resumed_at = '' ORDER BY created_at`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var active []*Halt
	for rows.Next() {
		var h Halt
		var scope, created, revoked string
		if err := rows.Scan(&h.ID, &scope, &h.Target, &h.Reason, &h.CreatedBy, &created, &revoked); err != nil {
			return err
		}
		h.Scope = Scope(scope)
		h.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if revoked != "" {
			_ = json.Unmarshal([]byte(revoked), &h.RevokedGrants)
		}
		active = append(active, &h)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.active, s.lastSync = active, time.Now()
	s.mu.Unlock()
	return nil
}

func (s *Store) dbInsert(h *Halt) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO halts (id, scope, target, reason, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		h.ID, string(h.Scope), h.Target, h.Reason, h.CreatedBy, h.CreatedAt.Format(time.RFC3339Nano))
	return err
}

func (s *Store) dbUpdateRevoked(h *Halt) {
	if s.db == nil {
		return
	}
	b, _ := json.Marshal(h.RevokedGrants)
	if _, err := s.db.Exec(`UPDATE halts SET revoked_grants=? WHERE id=?`, string(b), h.ID); err != nil {
		slog.Warn("halt: failed to record revoked grants", "id", h.ID, "error", err)
	}
}

func (s *Store) dbResume(h *Halt) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(`UPDATE halts SET resumed_by=?, resumed_at=? WHERE id=?`,
		h.ResumedBy, h.ResumedAt.Format(time.RFC3339Nano), h.ID)
	return err
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
