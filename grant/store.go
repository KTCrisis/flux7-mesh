package grant

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"sync"
	"time"

	"github.com/KTCrisis/flux7-mesh/internal/match"
)

// Grant is a temporary permission override.
type Grant struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`      // agent pattern (* = any)
	Tools     string    `json:"tools"`      // tool glob pattern
	ExpiresAt time.Time `json:"expires_at"`
	GrantedBy string    `json:"granted_by"`
	CreatedAt time.Time `json:"created_at"`
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

// Add creates a new temporal grant.
func (s *Store) Add(agent, tools, grantedBy string, duration time.Duration) *Grant {
	now := time.Now().UTC()
	g := &Grant{
		ID:        newID(),
		Agent:     agent,
		Tools:     tools,
		ExpiresAt: now.Add(duration),
		GrantedBy: grantedBy,
		CreatedAt: now,
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
