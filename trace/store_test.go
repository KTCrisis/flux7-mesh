package trace

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestRecord(t *testing.T) {
	s := NewStore(100)
	s.Record(Entry{AgentID: "bot", Tool: "get_order", Policy: "allow"})

	entries := s.Query("", "", 10)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].AgentID != "bot" {
		t.Errorf("agent_id = %q", entries[0].AgentID)
	}
	if entries[0].TraceID == "" {
		t.Error("trace_id should be auto-generated")
	}
	if entries[0].Timestamp.IsZero() {
		t.Error("timestamp should be auto-set")
	}
}

func TestQueryFilterAgent(t *testing.T) {
	s := NewStore(100)
	s.Record(Entry{AgentID: "bot-a", Tool: "x", Policy: "allow"})
	s.Record(Entry{AgentID: "bot-b", Tool: "y", Policy: "allow"})
	s.Record(Entry{AgentID: "bot-a", Tool: "z", Policy: "deny"})

	entries := s.Query("bot-a", "", 10)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
}

func TestQueryFilterTool(t *testing.T) {
	s := NewStore(100)
	s.Record(Entry{AgentID: "bot", Tool: "get_order", Policy: "allow"})
	s.Record(Entry{AgentID: "bot", Tool: "delete_order", Policy: "deny"})

	entries := s.Query("", "get_order", 10)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
}

func TestQueryEmptyReturnsNonNilSlice(t *testing.T) {
	// Regression: Query used to return a nil slice when no entries matched,
	// which the JSON encoder serializes as `null` and broke TypeScript clients
	// doing `traces.map(...)`. Must return an empty slice instead.
	s := NewStore(100)
	entries := s.Query("", "", 10)
	if entries == nil {
		t.Fatal("Query on empty store returned nil, expected empty slice")
	}
	if len(entries) != 0 {
		t.Fatalf("Query on empty store returned %d entries, expected 0", len(entries))
	}

	s.Record(Entry{AgentID: "bot", Tool: "x", Policy: "allow"})
	entries = s.Query("other-bot", "", 10)
	if entries == nil {
		t.Fatal("Query with no matches returned nil, expected empty slice")
	}
	if len(entries) != 0 {
		t.Fatalf("Query with no matches returned %d entries, expected 0", len(entries))
	}
}

func TestQueryLimit(t *testing.T) {
	s := NewStore(100)
	for i := 0; i < 20; i++ {
		s.Record(Entry{AgentID: "bot", Tool: "x", Policy: "allow"})
	}
	entries := s.Query("", "", 5)
	if len(entries) != 5 {
		t.Fatalf("entries = %d, want 5", len(entries))
	}
}

func TestEviction(t *testing.T) {
	s := NewStore(10)
	for i := 0; i < 20; i++ {
		s.Record(Entry{AgentID: "bot", Tool: "x", Policy: "allow"})
	}
	stats := s.Stats()
	if stats["total"] != 10 {
		t.Errorf("total = %d, want 10 (evicted)", stats["total"])
	}
}

func TestStats(t *testing.T) {
	s := NewStore(100)
	s.Record(Entry{Policy: "allow"})
	s.Record(Entry{Policy: "allow"})
	s.Record(Entry{Policy: "deny"})
	s.Record(Entry{Policy: "human_approval"})
	s.Record(Entry{Policy: "allow", Error: "timeout"})

	stats := s.Stats()
	if stats["total"] != 5 {
		t.Errorf("total = %d", stats["total"])
	}
	if stats["allowed"] != 3 {
		t.Errorf("allowed = %d", stats["allowed"])
	}
	if stats["denied"] != 1 {
		t.Errorf("denied = %d", stats["denied"])
	}
	if stats["human_approval"] != 1 {
		t.Errorf("human_approval = %d", stats["human_approval"])
	}
	if stats["errors"] != 1 {
		t.Errorf("errors = %d", stats["errors"])
	}
}

func TestUpdate(t *testing.T) {
	s := NewStore(100)
	s.Record(Entry{AgentID: "bot", Tool: "write_file", Policy: "human_approval"})

	entries := s.Query("", "", 1)
	traceID := entries[0].TraceID

	found := s.Update(traceID, func(e *Entry) {
		e.ApprovalID = "appr-001"
		e.ApprovalStatus = "approved"
		e.ApprovedBy = "marc"
		e.ApprovalMs = 3200
	})
	if !found {
		t.Fatal("Update should find the entry")
	}

	entries = s.Query("", "", 1)
	if entries[0].ApprovalID != "appr-001" {
		t.Errorf("approval_id = %q, want appr-001", entries[0].ApprovalID)
	}
	if entries[0].ApprovalStatus != "approved" {
		t.Errorf("approval_status = %q, want approved", entries[0].ApprovalStatus)
	}
	if entries[0].ApprovedBy != "marc" {
		t.Errorf("approved_by = %q, want marc", entries[0].ApprovedBy)
	}
	if entries[0].ApprovalMs != 3200 {
		t.Errorf("approval_ms = %d, want 3200", entries[0].ApprovalMs)
	}
}

func TestUpdateNotFound(t *testing.T) {
	s := NewStore(100)
	found := s.Update("nonexistent", func(e *Entry) {
		e.ApprovalStatus = "approved"
	})
	if found {
		t.Error("Update should return false for nonexistent ID")
	}
}

func TestPersistentStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")

	// Create store, write entries
	s, err := NewPersistentStore(100, path)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	s.Record(Entry{AgentID: "bot", Tool: "get_order", Policy: "allow"})
	s.Record(Entry{AgentID: "bot", Tool: "delete_order", Policy: "deny"})
	s.Close()

	// Verify file exists and has content
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("trace file is empty")
	}

	// Reopen store — should reload entries
	s2, err := NewPersistentStore(100, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	entries := s2.Query("", "", 10)
	if len(entries) != 2 {
		t.Fatalf("reloaded entries = %d, want 2", len(entries))
	}

	// Most recent first
	if entries[0].Tool != "delete_order" {
		t.Errorf("first entry = %q, want delete_order (most recent)", entries[0].Tool)
	}
	if entries[1].Tool != "get_order" {
		t.Errorf("second entry = %q, want get_order", entries[1].Tool)
	}
}

func TestPersistentStoreAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")

	// Session 1: write 2 entries
	s1, _ := NewPersistentStore(100, path)
	s1.Record(Entry{AgentID: "bot", Tool: "a", Policy: "allow"})
	s1.Record(Entry{AgentID: "bot", Tool: "b", Policy: "allow"})
	s1.Close()

	// Session 2: write 1 more entry
	s2, _ := NewPersistentStore(100, path)
	s2.Record(Entry{AgentID: "bot", Tool: "c", Policy: "deny"})
	s2.Close()

	// Session 3: should see all 3
	s3, _ := NewPersistentStore(100, path)
	defer s3.Close()

	entries := s3.Query("", "", 10)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
}

func TestPersistentStoreEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")

	// Write 20 entries to file
	s1, _ := NewPersistentStore(100, path)
	for i := 0; i < 20; i++ {
		s1.Record(Entry{AgentID: "bot", Tool: "x", Policy: "allow"})
	}
	s1.Close()

	// Reload with maxSize 10 — should keep only last 10
	s2, _ := NewPersistentStore(10, path)
	defer s2.Close()

	stats := s2.Stats()
	if stats["total"] != 10 {
		t.Errorf("total = %d, want 10 (evicted on load)", stats["total"])
	}
}

func TestPersistentStoreNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new-traces.jsonl")

	// File doesn't exist yet — should create it
	s, err := NewPersistentStore(100, path)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	defer s.Close()

	s.Record(Entry{AgentID: "bot", Tool: "x", Policy: "allow"})

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("trace file should have been created")
	}
}

func TestPersistentStoreRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")

	s, _ := NewPersistentStore(10000, path)
	s.maxFileSize = 500 // rotate after 500 bytes

	// Write entries until rotation triggers
	for i := 0; i < 20; i++ {
		s.Record(Entry{AgentID: "bot", Tool: "tool_with_long_name", Policy: "allow"})
	}
	s.Close()

	// The .old file should exist
	oldPath := path + ".old"
	if _, err := os.Stat(oldPath); os.IsNotExist(err) {
		t.Fatal("rotated .old file should exist")
	}

	// Current file should be smaller than max
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() > 500 {
		t.Errorf("current file size = %d, should be < 500 after rotation", info.Size())
	}
}

func TestSessionID(t *testing.T) {
	s := NewStore(100)
	s.Record(Entry{AgentID: "bot", Tool: "x", Policy: "allow", SessionID: "sess-1"})

	entries := s.Query("", "", 10)
	if entries[0].SessionID != "sess-1" {
		t.Errorf("session_id = %q, want sess-1", entries[0].SessionID)
	}
}

func TestQuerySessions(t *testing.T) {
	s := NewStore(100)
	s.Record(Entry{AgentID: "bot-a", Tool: "read", Policy: "allow", SessionID: "sess-1"})
	s.Record(Entry{AgentID: "bot-a", Tool: "write", Policy: "allow", SessionID: "sess-1"})
	s.Record(Entry{AgentID: "bot-b", Tool: "delete", Policy: "deny", SessionID: "sess-2"})
	// Entry without session — should be excluded
	s.Record(Entry{AgentID: "bot-c", Tool: "list", Policy: "allow"})

	sessions := s.QuerySessions(50)
	if len(sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(sessions))
	}

	// Find sess-1
	var s1 *SessionSummary
	for i := range sessions {
		if sessions[i].SessionID == "sess-1" {
			s1 = &sessions[i]
			break
		}
	}
	if s1 == nil {
		t.Fatal("sess-1 not found")
	}
	if s1.EventCount != 2 {
		t.Errorf("sess-1 event_count = %d, want 2", s1.EventCount)
	}
	if s1.AgentID != "bot-a" {
		t.Errorf("sess-1 agent_id = %q, want bot-a", s1.AgentID)
	}
	if len(s1.Tools) != 2 {
		t.Errorf("sess-1 tools = %d, want 2", len(s1.Tools))
	}
}

func TestQueryBySession(t *testing.T) {
	s := NewStore(100)
	s.Record(Entry{AgentID: "bot", Tool: "a", Policy: "allow", SessionID: "sess-1"})
	s.Record(Entry{AgentID: "bot", Tool: "b", Policy: "allow", SessionID: "sess-2"})
	s.Record(Entry{AgentID: "bot", Tool: "c", Policy: "allow", SessionID: "sess-1"})

	entries := s.QueryBySession("sess-1", 100)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// Most recent first
	if entries[0].Tool != "c" {
		t.Errorf("first entry tool = %q, want c", entries[0].Tool)
	}
	if entries[1].Tool != "a" {
		t.Errorf("second entry tool = %q, want a", entries[1].Tool)
	}
}

func TestQuerySessionsEmpty(t *testing.T) {
	s := NewStore(100)
	sessions := s.QuerySessions(50)
	if sessions == nil {
		t.Fatal("QuerySessions on empty store returned nil, expected empty slice")
	}
	if len(sessions) != 0 {
		t.Fatalf("QuerySessions on empty store returned %d, expected 0", len(sessions))
	}
}

func TestQueryBySessionEmpty(t *testing.T) {
	s := NewStore(100)
	entries := s.QueryBySession("nonexistent", 100)
	if entries == nil {
		t.Fatal("QueryBySession returned nil, expected empty slice")
	}
	if len(entries) != 0 {
		t.Fatalf("QueryBySession returned %d, expected 0", len(entries))
	}
}

func TestPersistentStoreSessionID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")

	s, err := NewPersistentStore(100, path)
	if err != nil {
		t.Fatalf("NewPersistentStore: %v", err)
	}
	s.Record(Entry{AgentID: "bot", Tool: "x", Policy: "allow", SessionID: "sess-42"})
	s.Close()

	// Reopen — session_id should survive round-trip
	s2, err := NewPersistentStore(100, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	entries := s2.Query("", "", 10)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].SessionID != "sess-42" {
		t.Errorf("session_id = %q, want sess-42", entries[0].SessionID)
	}
}

func TestCloseIdempotent(t *testing.T) {
	s := NewStore(100)
	// Close on in-memory store should not panic
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("double Close: %v", err)
	}
}

// Chain walks the authority behind a call: the approved call, the grant it
// produced, and every call that grant later waved through.
func TestChainWalksParents(t *testing.T) {
	s := NewStore(100)

	root := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mid := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	leaf := "cccccccccccccccccccccccccccccccc"

	s.Record(Entry{TraceID: root, AgentID: "claude", Tool: "fs.write", Policy: "human_approval"})
	s.Record(Entry{TraceID: mid, AgentID: "claude", Tool: "fs.write", Policy: "allow",
		GrantID: "g1", ParentTraceID: root})
	s.Record(Entry{TraceID: leaf, AgentID: "claude", Tool: "fs.delete", Policy: "allow",
		GrantID: "g1", ParentTraceID: mid})

	chain := s.Chain(leaf, 10)
	if len(chain) != 3 {
		t.Fatalf("expected a chain of 3, got %d", len(chain))
	}
	// Oldest first, requested entry last.
	if chain[0].TraceID != root || chain[2].TraceID != leaf {
		t.Errorf("chain out of order: %s ... %s", chain[0].TraceID, chain[2].TraceID)
	}
}

// A call nobody had to authorize is a chain of one, not an error.
func TestChainWithoutParentIsSingleton(t *testing.T) {
	s := NewStore(100)
	id := "dddddddddddddddddddddddddddddddd"
	s.Record(Entry{TraceID: id, AgentID: "claude", Tool: "fs.read", Policy: "allow"})

	chain := s.Chain(id, 10)
	if len(chain) != 1 {
		t.Fatalf("expected a chain of 1, got %d", len(chain))
	}
}

func TestChainUnknownTraceReturnsNil(t *testing.T) {
	s := NewStore(100)
	if chain := s.Chain("nope", 10); chain != nil {
		t.Errorf("expected nil for an unknown trace, got %d entries", len(chain))
	}
}

// depth bounds the walk, so a long chain cannot flood a caller.
func TestChainRespectsDepth(t *testing.T) {
	s := NewStore(100)
	prev := ""
	last := ""
	for i := range 6 {
		id := fmt.Sprintf("%032x", i+1)
		s.Record(Entry{TraceID: id, AgentID: "claude", Tool: "fs.write", ParentTraceID: prev})
		prev, last = id, id
	}
	if chain := s.Chain(last, 3); len(chain) != 3 {
		t.Errorf("expected depth to cap the chain at 3, got %d", len(chain))
	}
}

// A cycle in the data must not hang the walk.
func TestChainStopsOnCycle(t *testing.T) {
	s := NewStore(100)
	a := "11111111111111111111111111111111"
	b := "22222222222222222222222222222222"
	s.Record(Entry{TraceID: a, ParentTraceID: b})
	s.Record(Entry{TraceID: b, ParentTraceID: a})

	chain := s.Chain(a, 10)
	if len(chain) != 2 {
		t.Fatalf("expected the walk to stop at the cycle, got %d entries", len(chain))
	}
}
