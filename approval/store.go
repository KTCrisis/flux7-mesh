package approval

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"
)

// Status represents the resolution state of an approval.
type Status string

const (
	StatusPending  Status = "pending"
	StatusApproved Status = "approved"
	StatusDenied   Status = "denied"
	StatusTimeout  Status = "timeout"
)

// Resolution is the outcome sent through the channel.
type Resolution struct {
	Status     Status
	ResolvedBy string
	ResolvedAt time.Time
	Reasoning  string
	Confidence float64
}

// ResolveOpts contains optional fields for resolving an approval.
type ResolveOpts struct {
	ResolvedBy string
	Reasoning  string
	Confidence float64
}

// PendingApproval represents a tool call waiting for human decision.
type PendingApproval struct {
	ID          string         `json:"id"`
	AgentID     string         `json:"agent_id"`
	Tool        string         `json:"tool"`
	Params      map[string]any `json:"params"`
	PolicyRule  string         `json:"policy_rule"`
	TraceID     string         `json:"trace_id,omitempty"`
	CallbackURL string         `json:"callback_url,omitempty"`
	Status      Status         `json:"status"`
	CreatedAt   time.Time      `json:"created_at"`
	ResolvedBy  string         `json:"resolved_by,omitempty"`
	ResolvedAt  time.Time      `json:"resolved_at,omitempty"`
	Reasoning   string         `json:"reasoning,omitempty"`
	Confidence  float64        `json:"confidence,omitempty"`
	Result      chan Resolution `json:"-"`
}

// Remaining returns how much time is left before timeout.
func (p *PendingApproval) Remaining(timeout time.Duration) time.Duration {
	remaining := timeout - time.Since(p.CreatedAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// Store manages pending approvals with channel-based blocking.
type Store struct {
	mu           sync.RWMutex
	pending      map[string]*PendingApproval
	timeout      time.Duration
	Notifier     *Notifier
	MemoryWriter *MemoryWriter
	MemoryReader *MemoryReader
}

// TryAutoResolve checks mem7 for past decisions and returns an auto-resolution
// if the pattern is clear (enough consistent approvals). Returns nil if the
// request should be escalated to human/supervisor.
// When auto-approved, the decision is also written to mem7 for future queries.
func (s *Store) TryAutoResolve(agentID, tool string) *Resolution {
	if s == nil || s.MemoryReader == nil {
		return nil
	}
	ar := s.MemoryReader.AutoResolve(tool, agentID)
	if ar.Action != "approve" {
		return nil
	}
	res := &Resolution{
		Status:     StatusApproved,
		ResolvedBy: "supervisor:mem7",
		ResolvedAt: time.Now().UTC(),
		Reasoning:  ar.Reason,
		Confidence: ar.Confidence,
	}
	s.MemoryWriter.WriteDecision(
		&PendingApproval{ID: newID(), AgentID: agentID, Tool: tool},
		*res,
	)
	return res
}

// NewStore creates an approval store with the given default timeout.
func NewStore(timeout time.Duration) *Store {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &Store{
		pending: make(map[string]*PendingApproval),
		timeout: timeout,
	}
}

// Timeout returns the configured default timeout.
func (s *Store) Timeout() time.Duration {
	return s.timeout
}

// Submit creates a pending approval and starts a timeout goroutine.
// The caller should block on the returned PendingApproval.Result channel.
// callbackURL is optional — set from X-Callback-URL header for HTTP agents.
func (s *Store) Submit(agentID, tool, policyRule string, params map[string]any, callbackURL string) *PendingApproval {
	pa := &PendingApproval{
		ID:          newID(),
		AgentID:     agentID,
		Tool:        tool,
		Params:      params,
		PolicyRule:  policyRule,
		CallbackURL: callbackURL,
		Status:      StatusPending,
		CreatedAt:   time.Now().UTC(),
		Result:      make(chan Resolution, 1),
	}

	s.mu.Lock()
	s.pending[pa.ID] = pa
	s.mu.Unlock()

	// Notify webhook (new pending → human)
	s.Notifier.OnSubmit(pa)

	// Timeout goroutine
	go func() {
		time.Sleep(s.timeout)
		s.mu.Lock()
		defer s.mu.Unlock()
		if pa.Status != StatusPending {
			return // already resolved
		}
		pa.Status = StatusTimeout
		pa.ResolvedAt = time.Now().UTC()
		res := Resolution{
			Status:     StatusTimeout,
			ResolvedBy: "system:timeout",
			ResolvedAt: pa.ResolvedAt,
		}
		pa.Result <- res
		// Callback agent on timeout too
		s.Notifier.OnResolve(pa, res)
		s.MemoryWriter.WriteDecision(pa, res)
	}()

	return pa
}

var (
	ErrNotFound        = errors.New("approval not found")
	ErrAlreadyResolved = errors.New("approval already resolved")
)

// Resolve sets the status of a pending approval and unblocks the handler.
// Supports prefix matching: if id is not an exact match, it tries to find
// a unique entry whose ID starts with the given prefix.
func (s *Store) Resolve(id string, status Status, opts ResolveOpts) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	pa := s.findLocked(id)
	if pa == nil {
		return ErrNotFound
	}
	if pa.Status != StatusPending {
		return ErrAlreadyResolved
	}

	now := time.Now().UTC()
	pa.Status = status
	pa.ResolvedBy = opts.ResolvedBy
	pa.ResolvedAt = now
	pa.Reasoning = opts.Reasoning
	pa.Confidence = opts.Confidence
	res := Resolution{
		Status:     status,
		ResolvedBy: opts.ResolvedBy,
		ResolvedAt: now,
		Reasoning:  opts.Reasoning,
		Confidence: opts.Confidence,
	}
	pa.Result <- res
	// Callback agent (if X-Callback-URL was set)
	s.Notifier.OnResolve(pa, res)
	// Persist decision to mem7 (if configured)
	s.MemoryWriter.WriteDecision(pa, res)
	return nil
}

// Approve is a convenience wrapper for Resolve with StatusApproved.
func (s *Store) Approve(id, resolvedBy string) error {
	return s.Resolve(id, StatusApproved, ResolveOpts{ResolvedBy: resolvedBy})
}

// Deny is a convenience wrapper for Resolve with StatusDenied.
func (s *Store) Deny(id, resolvedBy string) error {
	return s.Resolve(id, StatusDenied, ResolveOpts{ResolvedBy: resolvedBy})
}

// Get returns a pending approval by ID or prefix, or nil if not found.
func (s *Store) Get(id string) *PendingApproval {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findLocked(id)
}

// findLocked finds an approval by exact ID or unique prefix match.
// Caller must hold s.mu (read or write).
func (s *Store) findLocked(id string) *PendingApproval {
	// Exact match first
	if pa, ok := s.pending[id]; ok {
		return pa
	}
	// Prefix match — must be unique
	var match *PendingApproval
	for key, pa := range s.pending {
		if strings.HasPrefix(key, id) {
			if match != nil {
				return nil // ambiguous prefix
			}
			match = pa
		}
	}
	return match
}

// List returns all approvals (pending + resolved), most recent first.
func (s *Store) List() []*PendingApproval {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectLocked(nil)
}

// ListPending returns only pending approvals, most recent first.
func (s *Store) ListPending() []*PendingApproval {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.collectLocked(func(pa *PendingApproval) bool {
		return pa.Status == StatusPending
	})
}

// collectLocked gathers approvals matching filter (nil = all), sorted most recent first.
// Caller must hold s.mu.
func (s *Store) collectLocked(filter func(*PendingApproval) bool) []*PendingApproval {
	result := make([]*PendingApproval, 0, len(s.pending))
	for _, pa := range s.pending {
		if filter == nil || filter(pa) {
			result = append(result, pa)
		}
	}
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].CreatedAt.After(result[i].CreatedAt) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	return result
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
