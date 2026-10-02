package grant

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"sync"
	"time"

	"github.com/KTCrisis/flux7-mesh/internal/match"
)

// Origin records what motivated a grant. Both fields are optional: a grant
// issued out of the blue has none. When they are set, every call the grant
// later authorizes can be walked back to the decision that created it —
// without this, a grant is an orphan and "why was this allowed?" stops at
// "because a grant existed".
type Origin struct {
	ApprovalID string `json:"approval_id,omitempty"` // the approval that led to it
	TraceID    string `json:"trace_id,omitempty"`    // the call that led to it
}

// Grant is a temporary permission override.
type Grant struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`      // agent pattern (* = any)
	Tools     string    `json:"tools"`      // tool glob pattern
	ExpiresAt time.Time `json:"expires_at"`
	GrantedBy string    `json:"granted_by"`
	CreatedAt time.Time `json:"created_at"`
	Origin    Origin    `json:"origin,omitzero"`
}

// IsExpired returns true if the grant has passed its expiration.
func (g *Grant) IsExpired() bool {
	return time.Now().After(g.ExpiresAt)
}

// Remaining returns how much time is left on this grant.
func (g *Grant) Remaining() time.Duration {
	r := time.Until(g.ExpiresAt)
	if r < 0 {
		return 0
	}
	return r
}

// Store manages active temporal grants.
type Store struct {
	mu     sync.RWMutex
	grants []*Grant
	db     *sql.DB
}

func NewStore() *Store {
	return &Store{}
}

// Add creates a new temporal grant with no recorded origin.
func (s *Store) Add(agent, tools, grantedBy string, duration time.Duration) *Grant {
	return s.AddWithOrigin(agent, tools, grantedBy, duration, Origin{})
}

// AddWithOrigin creates a new temporal grant and records what motivated it.
func (s *Store) AddWithOrigin(agent, tools, grantedBy string, duration time.Duration, origin Origin) *Grant {
	now := time.Now().UTC()
	g := &Grant{
		ID:        newID(),
		Agent:     agent,
		Tools:     tools,
		ExpiresAt: now.Add(duration),
		GrantedBy: grantedBy,
		CreatedAt: now,
		Origin:    origin,
	}
	s.mu.Lock()
	s.grants = append(s.grants, g)
	s.mu.Unlock()
	s.dbSave(g)
	return g
}

// Check returns true if an active grant covers this agent+tool combination.
func (s *Store) Check(agentID, toolName string) *Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, g := range s.grants {
		if g.IsExpired() {
			continue
		}
		if !matchPattern(g.Agent, agentID) {
			continue
		}
		if !matchPattern(g.Tools, toolName) {
			continue
		}
		return g
	}
	return nil
}

// Revoke removes a grant by ID or prefix.
func (s *Store) Revoke(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, g := range s.grants {
		if g.ID == id || (len(id) >= 4 && len(g.ID) >= len(id) && g.ID[:len(id)] == id) {
			s.grants = append(s.grants[:i], s.grants[i+1:]...)
			s.dbDelete(g.ID)
			return true
		}
	}
	return false
}

// RevokeWhere removes every active grant the predicate selects and returns
// copies of them, so that the caller can put them back later (an emergency
// stop revokes an agent's grants, and resuming restores those still valid).
func (s *Store) RevokeWhere(selects func(*Grant) bool) []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()

	var revoked []Grant
	kept := s.grants[:0]
	for _, g := range s.grants {
		if !g.IsExpired() && selects(g) {
			revoked = append(revoked, *g)
			s.dbDelete(g.ID)
			continue
		}
		kept = append(kept, g)
	}
	s.grants = kept
	return revoked
}

// Restore puts back a grant revoked earlier, with its ID, expiry and origin
// unchanged. An expired grant is not restored: it would no longer be in force
// anyway. It returns whether the grant was restored.
func (s *Store) Restore(g Grant) bool {
	if g.IsExpired() {
		return false
	}
	s.mu.Lock()
	for _, existing := range s.grants {
		if existing.ID == g.ID {
			s.mu.Unlock()
			return false
		}
	}
	restored := g
	s.grants = append(s.grants, &restored)
	s.mu.Unlock()
	s.dbSave(&restored)
	return true
}

// List returns all active (non-expired) grants.
func (s *Store) List() []*Grant {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var active []*Grant
	for _, g := range s.grants {
		if !g.IsExpired() {
			active = append(active, g)
		}
	}
	return active
}

// Cleanup removes expired grants.
func (s *Store) Cleanup() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	fresh := s.grants[:0]
	removed := 0
	for _, g := range s.grants {
		if g.IsExpired() {
			removed++
		} else {
			fresh = append(fresh, g)
		}
	}
	s.grants = fresh
	return removed
}

func matchPattern(pattern, value string) bool {
	return match.Glob(pattern, value)
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
