package trace

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testKey = []byte("test-trace-key")

func writeChain(t *testing.T, key []byte, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "traces.jsonl")
	s, err := NewPersistentStore(100, path)
	if err != nil {
		t.Fatal(err)
	}
	s.SetKey(key)
	for i := 0; i < n; i++ {
		// params carry a "seq" key on purpose: the envelope must still be found
		s.Record(Entry{TraceID: NewID(), Tool: "t", Policy: "allow", Params: map[string]any{"seq": i, "amount": 12.5}})
	}
	s.Close()
	return path
}

func lines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
}

func rewrite(t *testing.T, path string, ls [][]byte) {
	t.Helper()
	if err := os.WriteFile(path, append(bytes.Join(ls, []byte("\n")), '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}

func mustVerify(t *testing.T, key []byte, paths ...string) Report {
	t.Helper()
	r, err := Verify(paths, key)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestChainIntact(t *testing.T) {
	path := writeChain(t, testKey, 5)
	r := mustVerify(t, testKey, path)
	if r.Break != nil || r.Chained != 5 || r.FirstSeq != 1 || r.LastSeq != 5 || r.Alg != AlgHMACSHA256 {
		t.Fatalf("report = %+v break=%+v", r, r.Break)
	}
}

func TestChainDetectsEditedLine(t *testing.T) {
	path := writeChain(t, testKey, 5)
	ls := lines(t, path)
	ls[2] = bytes.Replace(ls[2], []byte(`"policy":"allow"`), []byte(`"policy":"deny"`), 1)
	rewrite(t, path, ls)
	r := mustVerify(t, testKey, path)
	if r.Break == nil || r.Break.Line != 3 || !strings.Contains(r.Break.Reason, "hash") {
		t.Fatalf("break = %+v", r.Break)
	}
}

func TestChainDetectsDeletedLine(t *testing.T) {
	path := writeChain(t, testKey, 5)
	ls := lines(t, path)
	rewrite(t, path, append(ls[:2:2], ls[3:]...))
	r := mustVerify(t, testKey, path)
	if r.Break == nil || !strings.Contains(r.Break.Reason, "seq") {
		t.Fatalf("break = %+v", r.Break)
	}
}

// Without the key, a forger can only produce plain SHA-256 lines; a verifier
// holding the key refuses them.
func TestChainRefusesDowngrade(t *testing.T) {
	path := writeChain(t, nil, 3)
	if r := mustVerify(t, nil, path); r.Break != nil || r.Alg != AlgSHA256 {
		t.Fatalf("plain chain should verify without key: %+v", r.Break)
	}
	r := mustVerify(t, testKey, path)
	if r.Break == nil || !strings.Contains(r.Break.Reason, "required") {
		t.Fatalf("break = %+v", r.Break)
	}
}

func TestChainWrongKey(t *testing.T) {
	path := writeChain(t, testKey, 3)
	if r := mustVerify(t, []byte("other"), path); r.Break == nil {
		t.Fatal("wrong key verified")
	}
	if r := mustVerify(t, nil, path); r.Break == nil || !strings.Contains(r.Break.Reason, "MESH_TRACE_KEY") {
		t.Fatalf("no key: break = %+v", r.Break)
	}
}

// Lines written before the chain existed are counted, not rejected; an
// unchained line slipped in after the chain started is.
func TestChainLegacyPrefixAndInsertion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")
	legacy := []byte(`{"trace_id":"old","agent_id":"a","tool":"t","params":null,"policy":"allow","policy_rule":"","status_code":0,"latency_ms":0,"timestamp":"2026-01-01T00:00:00Z"}`)
	rewrite(t, path, [][]byte{legacy})
	s, _ := NewPersistentStore(100, path)
	s.SetKey(testKey)
	s.Record(Entry{Tool: "t", Policy: "allow"})
	s.Record(Entry{Tool: "t", Policy: "allow"})
	s.Close()

	r := mustVerify(t, testKey, path)
	if r.Break != nil || r.Unchained != 1 || r.Chained != 2 {
		t.Fatalf("report = %+v break=%+v", r, r.Break)
	}
	ls := lines(t, path)
	rewrite(t, path, [][]byte{ls[0], ls[1], legacy, ls[2]})
	if r := mustVerify(t, testKey, path); r.Break == nil || !strings.Contains(r.Break.Reason, "unchained") {
		t.Fatalf("insertion: break = %+v", r.Break)
	}
}

// The chain carries on across a restart and across a rotation.
func TestChainContinuesAcrossRestartAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")
	s, _ := NewPersistentStore(100, path)
	s.SetKey(testKey)
	s.Record(Entry{Tool: "t", Policy: "allow"})
	s.Close()

	s, _ = NewPersistentStore(100, path)
	s.SetKey(testKey)
	s.maxFileSize = 1 // rotate after every line
	s.Record(Entry{Tool: "t", Policy: "allow"})
	s.maxFileSize = 0
	s.Record(Entry{Tool: "t", Policy: "allow"})
	seq, head := s.Head()
	s.Close()

	r := mustVerify(t, testKey, path+".old", path)
	if r.Break != nil || r.FirstSeq != 1 || r.LastSeq != 3 || r.Head != head || seq != 3 {
		t.Fatalf("report = %+v break=%+v", r, r.Break)
	}
	// The current file alone verifies from its anchor.
	r = mustVerify(t, testKey, path)
	if r.Break != nil || r.FirstSeq != 3 || r.Anchor == "" {
		t.Fatalf("current file alone: %+v break=%+v", r, r.Break)
	}
}

// Revisions are chained like any other line, and the loader still reads
// chained lines as plain entries.
func TestChainedRevisionsLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traces.jsonl")
	s, _ := NewPersistentStore(100, path)
	s.SetKey(testKey)
	s.Record(Entry{TraceID: "t1", Tool: "pay", Policy: "human_approval", Params: map[string]any{"seq": 7}})
	s.Update("t1", func(e *Entry) { e.ApprovalStatus = "approved" })
	s.Close()

	if r := mustVerify(t, testKey, path); r.Break != nil || r.Chained != 2 {
		t.Fatalf("report = %+v break=%+v", r, r.Break)
	}
	s2, _ := NewPersistentStore(100, path)
	defer s2.Close()
	got := s2.Query("", "", 10)
	if len(got) != 1 || got[0].ApprovalStatus != "approved" || got[0].Params["seq"] != float64(7) {
		t.Fatalf("loaded = %+v", got)
	}
}
