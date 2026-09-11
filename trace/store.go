package trace

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"
)

// SessionSummary groups trace entries that share a session ID.
type SessionSummary struct {
	SessionID  string    `json:"session_id"`
	AgentID    string    `json:"agent_id"` // first agent seen
	EventCount int       `json:"event_count"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	Tools      []string  `json:"tools"` // unique tool names
}

// Entry represents a single traced tool call.
type Entry struct {
	TraceID   string `json:"trace_id"`
	SessionID string `json:"session_id,omitempty"`
	AgentID   string `json:"agent_id"`
	// UserID is the human the agent acted for, when the credential carried
	// one (auth.jwt.user_claim). It is the half of the delegation that an
	// identity provider issues and then forgets; recording it here is what
	// lets a trace answer "on whose behalf", not only "by which agent".
	UserID     string         `json:"user_id,omitempty"`
	Tool       string         `json:"tool"`
	Params     map[string]any `json:"params"`
	Policy     string         `json:"policy"`      // allow, deny, human_approval
	PolicyRule string         `json:"policy_rule"` // which rule matched
	StatusCode int            `json:"status_code"` // backend response status
	LatencyMs  int64          `json:"latency_ms"`
	Error      string         `json:"error,omitempty"`
	Timestamp  time.Time      `json:"timestamp"`

	// CLI fields (populated when source = cli)
	ExitCode *int `json:"exit_code,omitempty"`

	// Lineage fields (populated when a temporal grant authorized the call).
	// GrantID is also encoded in PolicyRule as "grant:<id>", but only as a
	// string an operator would have to parse; this field is the queryable one.
	// ParentTraceID is the call that motivated the grant, when the grant
	// recorded an origin — it is what makes the causal chain walkable.
	GrantID       string `json:"grant_id,omitempty"`
	ParentTraceID string `json:"parent_trace_id,omitempty"`

	// Approval fields (populated when policy = human_approval)
	ApprovalID     string `json:"approval_id,omitempty"`
	ApprovalStatus string `json:"approval_status,omitempty"` // approved, denied, timeout
	ApprovedBy     string `json:"approved_by,omitempty"`
	ApprovalMs     int64  `json:"approval_ms,omitempty"`

	// Supervisor fields (populated when resolved by a supervisor agent)
	SupervisorReasoning  string  `json:"supervisor_reasoning,omitempty"`
	SupervisorConfidence float64 `json:"supervisor_confidence,omitempty"`

	// Token counts — real when the provider exposes them (LLM tools),
	// chars/4 estimate otherwise. TokensSource says which.
	EstimatedInputTokens  int    `json:"estimated_input_tokens,omitempty"`
	EstimatedOutputTokens int    `json:"estimated_output_tokens,omitempty"`
	TokensSource          string `json:"tokens_source,omitempty"` // "real" | "estimate"
}

// Store is a thread-safe trace store with optional JSONL file persistence.
type Store struct {
	mu      sync.RWMutex
	entries []Entry
	maxSize int

	// JSONL file persistence (nil = in-memory only)
	file        *os.File
	writer      *bufio.Writer
	filePath    string
	fileSize    int64
	maxFileSize int64 // 0 = no rotation

	// OTEL exporter (nil = disabled)
	OTEL *OTELExporter
}

// NewStore creates an in-memory trace store.
func NewStore(maxSize int) *Store {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &Store{
		entries: make([]Entry, 0, 256),
		maxSize: maxSize,
	}
}

// NewPersistentStore creates a trace store that appends to a JSONL file.
// Existing entries are loaded from the file on startup.
// maxFileBytes controls file rotation (0 = no rotation, default 10MB).
func NewPersistentStore(maxSize int, path string) (*Store, error) {
	s := NewStore(maxSize)
	s.filePath = path
	s.maxFileSize = 10 * 1024 * 1024 // 10MB default

	// Load existing entries from file
	if err := s.loadFromFile(path); err != nil {
		slog.Warn("trace: could not load existing traces", "path", path, "error", err)
	}

	// Open file for appending
	if err := s.openFile(); err != nil {
		return nil, err
	}

	return s, nil
}

func (s *Store) openFile() error {
	f, err := os.OpenFile(s.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	s.file = f
	s.writer = bufio.NewWriter(f)
	s.fileSize = info.Size()
	return nil
}

// Record adds a trace entry.
func (s *Store) Record(e Entry) {
	if e.TraceID == "" {
		e.TraceID = NewID()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries = append(s.entries, e)

	// Evict oldest if over max
	if len(s.entries) > s.maxSize {
		s.entries = s.entries[len(s.entries)-s.maxSize:]
	}

	// Export to OTEL (async to not block Record)
	if s.OTEL != nil {
		go s.OTEL.Export(e)
	}

	// Append to JSONL file
	if s.writer != nil {
		data, err := json.Marshal(e)
		if err != nil {
			slog.Error("trace: failed to marshal entry", "error", err)
			return
		}
		n, _ := s.writer.Write(data)
		s.writer.WriteByte('\n')
		s.writer.Flush()
		s.fileSize += int64(n + 1)

		// Rotate if file exceeds max size
		if s.maxFileSize > 0 && s.fileSize >= s.maxFileSize {
			s.rotate()
		}
	}
}

// Query returns the last n traces, optionally filtered by agent and/or tool.
func (s *Store) Query(agent string, tool string, limit int) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 100
	}

	result := make([]Entry, 0)
	// Iterate in reverse (most recent first)
	for i := len(s.entries) - 1; i >= 0 && len(result) < limit; i-- {
		e := s.entries[i]
		if agent != "" && e.AgentID != agent {
			continue
		}
		if tool != "" && e.Tool != tool {
			continue
		}
		result = append(result, e)
	}
	return result
}

// QuerySessions returns distinct sessions ordered by most recent activity.
// Entries without a session ID are excluded.
func (s *Store) QuerySessions(limit int) []SessionSummary {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 50
	}

	type sessionAcc struct {
		summary  SessionSummary
		toolSet  map[string]bool
		lastSeen time.Time
	}

	sessions := make(map[string]*sessionAcc)
	order := make([]string, 0) // insertion order for stable iteration

	for i := range s.entries {
		e := &s.entries[i]
		if e.SessionID == "" {
			continue
		}
		acc, ok := sessions[e.SessionID]
		if !ok {
			acc = &sessionAcc{
				summary: SessionSummary{
					SessionID: e.SessionID,
					AgentID:   e.AgentID,
					FirstSeen: e.Timestamp,
				},
				toolSet: make(map[string]bool),
			}
			sessions[e.SessionID] = acc
			order = append(order, e.SessionID)
		}
		acc.summary.EventCount++
		if e.Timestamp.After(acc.summary.LastSeen) {
			acc.summary.LastSeen = e.Timestamp
		}
		if e.Timestamp.Before(acc.summary.FirstSeen) {
			acc.summary.FirstSeen = e.Timestamp
		}
		acc.toolSet[e.Tool] = true
	}

	// Sort by last_seen descending (most recent first)
	sorted := make([]string, len(order))
	copy(sorted, order)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sessions[sorted[j]].summary.LastSeen.After(sessions[sorted[j-1]].summary.LastSeen); j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	result := make([]SessionSummary, 0, limit)
	for _, sid := range sorted {
		if len(result) >= limit {
			break
		}
		acc := sessions[sid]
		tools := make([]string, 0, len(acc.toolSet))
		for t := range acc.toolSet {
			tools = append(tools, t)
		}
		acc.summary.Tools = tools
		result = append(result, acc.summary)
	}
	return result
}

// QueryBySession returns entries for a specific session, most recent first.
func (s *Store) QueryBySession(sessionID string, limit int) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 200
	}

	result := make([]Entry, 0)
	for i := len(s.entries) - 1; i >= 0 && len(result) < limit; i-- {
		if s.entries[i].SessionID == sessionID {
			result = append(result, s.entries[i])
		}
	}
	return result
}

// Chain walks a trace back through ParentTraceID and returns the causal chain,
// oldest first, with the requested entry last. It answers "why was this
// allowed?" by naming the decision upstream of the grant that authorized it.
//
// A chain of one is the honest answer for a call nobody had to authorize, and
// for a grant issued without an origin. The walk is bounded by maxDepth and by
// a seen-set, so a cycle in the data cannot hang the caller.
func (s *Store) Chain(traceID string, maxDepth int) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if maxDepth <= 0 {
		maxDepth = 10
	}

	byID := func(id string) *Entry {
		for i := len(s.entries) - 1; i >= 0; i-- {
			if s.entries[i].TraceID == id {
				return &s.entries[i]
			}
		}
		return nil
	}

	e := byID(traceID)
	if e == nil {
		return nil
	}

	seen := map[string]bool{traceID: true}
	chain := []Entry{*e}
	for len(chain) < maxDepth {
		parentID := chain[0].ParentTraceID
		if parentID == "" || seen[parentID] {
			break
		}
		parent := byID(parentID)
		if parent == nil {
			// The ancestor has been evicted or rotated out. Stop here rather
			// than pretending the chain ends at a root.
			break
		}
		seen[parentID] = true
		chain = append([]Entry{*parent}, chain...)
	}
	return chain
}

// Update finds a trace entry by TraceID and applies fn to mutate it.
// Returns true if the entry was found and updated.
func (s *Store) Update(traceID string, fn func(*Entry)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := len(s.entries) - 1; i >= 0; i-- {
		if s.entries[i].TraceID == traceID {
			fn(&s.entries[i])
			return true
		}
	}
	return false
}

// Stats returns aggregate counts.
func (s *Store) Stats() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := map[string]int{
		"total":                   len(s.entries),
		"allowed":                 0,
		"denied":                  0,
		"human_approval":          0,
		"errors":                  0,
		"estimated_input_tokens":  0,
		"estimated_output_tokens": 0,
	}
	for _, e := range s.entries {
		switch e.Policy {
		case "allow":
			stats["allowed"]++
		case "deny":
			stats["denied"]++
		case "human_approval":
			stats["human_approval"]++
		}
		if e.Error != "" {
			stats["errors"]++
		}
		stats["estimated_input_tokens"] += e.EstimatedInputTokens
		stats["estimated_output_tokens"] += e.EstimatedOutputTokens
	}
	return stats
}

// rotate renames the current file to .old and opens a new one.
// Must be called with mu held.
func (s *Store) rotate() {
	if s.writer != nil {
		s.writer.Flush()
	}
	if s.file != nil {
		s.file.Close()
	}

	oldPath := s.filePath + ".old"
	os.Remove(oldPath)
	if err := os.Rename(s.filePath, oldPath); err != nil {
		slog.Error("trace: failed to rotate file", "error", err)
		return
	}

	slog.Info("trace: rotated", "old", oldPath, "new", s.filePath)

	if err := s.openFile(); err != nil {
		slog.Error("trace: failed to reopen after rotation", "error", err)
	}
}

// Close flushes and closes the trace file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer != nil {
		s.writer.Flush()
	}
	if s.file != nil {
		return s.file.Close()
	}
	return nil
}

// loadFromFile reads existing JSONL entries into memory.
func (s *Store) loadFromFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No file yet, start fresh
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Increase buffer for potentially large trace lines
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	loaded := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			slog.Warn("trace: skipping malformed line", "error", err)
			continue
		}
		s.entries = append(s.entries, e)
		loaded++
	}

	// Apply max size limit
	if len(s.entries) > s.maxSize {
		s.entries = s.entries[len(s.entries)-s.maxSize:]
	}

	if loaded > 0 {
		slog.Info("trace: loaded from file", "path", path, "entries", loaded, "kept", len(s.entries))
	}

	return scanner.Err()
}

// NewID generates a random 16-byte trace ID (32 hex chars, W3C compatible).
func NewID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
