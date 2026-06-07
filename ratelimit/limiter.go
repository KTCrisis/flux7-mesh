package ratelimit

import (
	"fmt"
	"sync"
	"time"
)

// Limit defines rate constraints for a policy.
type Limit struct {
	MaxPerMinute int
	MaxTotal     int
}

// Limiter tracks call counts per agent and detects loops.
type Limiter struct {
	mu      sync.Mutex
	agents  map[string]*agentState
	limits  map[string]Limit // policy name → limit
	cleanup *time.Ticker
}

type agentState struct {
	calls    []time.Time // timestamps for sliding window
	total    int
	recent   []recentCall // last N calls for loop detection
	lastSeen time.Time    // for idle eviction (bounds map under self-declared IDs)
}

// maxAgents bounds the tracked-agent map. Agent IDs are self-declared in
// non-JWT mode, so without a cap an attacker could enumerate unique IDs and
// grow the map until OOM. When full, the least-recently-seen agent is evicted.
const maxAgents = 50000

type recentCall struct {
	tool   string
	params string // serialized params for comparison
	at     time.Time
}

func New() *Limiter {
	l := &Limiter{
		agents: make(map[string]*agentState),
		limits: make(map[string]Limit),
	}
	// Cleanup old entries every minute
	l.cleanup = time.NewTicker(1 * time.Minute)
	go func() {
		for range l.cleanup.C {
			l.gc()
		}
	}()
	return l
}

// Close stops the background cleanup goroutine.
func (l *Limiter) Close() {
	l.cleanup.Stop()
}

// SetLimit registers a rate limit for a policy name.
func (l *Limiter) SetLimit(policyName string, limit Limit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits[policyName] = limit
}

// ReplaceLimits atomically replaces all rate limits.
// Existing per-agent counters are kept — only the limit thresholds change.
func (l *Limiter) ReplaceLimits(limits map[string]Limit) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits = limits
}

// Check verifies if the agent can make a call. Returns nil if OK, error if denied.
// policyName is the matched policy name from the engine.
func (l *Limiter) Check(agentID, policyName, toolName, paramsKey string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	limit, ok := l.limits[policyName]
	if !ok {
		return nil // no limit configured
	}

	state := l.getOrCreate(agentID)
	now := time.Now()
	state.lastSeen = now

	// 1. Check total budget
	if limit.MaxTotal > 0 && state.total >= limit.MaxTotal {
		return fmt.Errorf("rate_limit: agent %q exceeded total budget (%d calls)", agentID, limit.MaxTotal)
	}

	// 2. Check per-minute rate
	if limit.MaxPerMinute > 0 {
		cutoff := now.Add(-1 * time.Minute)
		count := 0
		for _, t := range state.calls {
			if t.After(cutoff) {
				count++
			}
		}
		if count >= limit.MaxPerMinute {
			return fmt.Errorf("rate_limit: agent %q exceeded %d calls/min", agentID, limit.MaxPerMinute)
		}
	}

	// 3. Loop detection: same tool+params > 3 times in 10s
	loopCount := 0
	cutoff10s := now.Add(-10 * time.Second)
	for _, rc := range state.recent {
		if rc.at.After(cutoff10s) && rc.tool == toolName && rc.params == paramsKey {
			loopCount++
		}
	}
	if loopCount >= 3 {
		return fmt.Errorf("loop_detected: agent %q called %s with same params %d times in 10s", agentID, toolName, loopCount)
	}

	return nil
}

// Record marks a call as made. Call after Check succeeds.
func (l *Limiter) Record(agentID, toolName, paramsKey string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	state := l.getOrCreate(agentID)
	now := time.Now()
	state.lastSeen = now

	state.calls = append(state.calls, now)
	state.total++
	state.recent = append(state.recent, recentCall{
		tool:   toolName,
		params: paramsKey,
		at:     now,
	})

	// Keep recent list bounded
	if len(state.recent) > 50 {
		state.recent = state.recent[len(state.recent)-50:]
	}
}

// Stats returns current state for an agent (for /health or traces).
func (l *Limiter) Stats(agentID, policyName string) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()

	state, ok := l.agents[agentID]
	if !ok {
		return nil
	}

	limit := l.limits[policyName]
	now := time.Now()
	cutoff := now.Add(-1 * time.Minute)
	callsLastMin := 0
	for _, t := range state.calls {
		if t.After(cutoff) {
			callsLastMin++
		}
	}

	stats := map[string]any{
		"total_calls":    state.total,
		"calls_last_min": callsLastMin,
	}
	if limit.MaxPerMinute > 0 {
		stats["max_per_minute"] = limit.MaxPerMinute
		stats["remaining_per_minute"] = limit.MaxPerMinute - callsLastMin
	}
	if limit.MaxTotal > 0 {
		stats["max_total"] = limit.MaxTotal
		stats["remaining_total"] = limit.MaxTotal - state.total
	}
	return stats
}

func (l *Limiter) getOrCreate(agentID string) *agentState {
	state, ok := l.agents[agentID]
	if !ok {
		if len(l.agents) >= maxAgents {
			l.evictOldest()
		}
		state = &agentState{}
		l.agents[agentID] = state
	}
	return state
}

// evictOldest removes the least-recently-seen agent. Caller holds l.mu.
func (l *Limiter) evictOldest() {
	var oldestID string
	var oldest time.Time
	for id, st := range l.agents {
		if oldestID == "" || st.lastSeen.Before(oldest) {
			oldestID, oldest = id, st.lastSeen
		}
	}
	if oldestID != "" {
		delete(l.agents, oldestID)
	}
}

// gc removes call timestamps older than 2 minutes.
func (l *Limiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := time.Now().Add(-2 * time.Minute)
	for id, state := range l.agents {
		fresh := state.calls[:0]
		for _, t := range state.calls {
			if t.After(cutoff) {
				fresh = append(fresh, t)
			}
		}
		state.calls = fresh

		recentCutoff := time.Now().Add(-10 * time.Second)
		freshRecent := state.recent[:0]
		for _, rc := range state.recent {
			if rc.at.After(recentCutoff) {
				freshRecent = append(freshRecent, rc)
			}
		}
		state.recent = freshRecent

		// Evict agents idle past the cutoff, regardless of their total counter.
		// The previous `state.total == 0` guard meant any agent that ever made
		// a call was kept forever — an unbounded-growth DoS under self-declared
		// agent IDs. A returning idle agent simply gets a fresh budget.
		if state.lastSeen.Before(cutoff) {
			delete(l.agents, id)
		}
	}
}
