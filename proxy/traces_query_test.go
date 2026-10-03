package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/KTCrisis/flux7-mesh/trace"
)

func TestTracesByIDAndLimit(t *testing.T) {
	handler, _ := setupHandler(t)
	handler.Traces = trace.NewStore(2000)
	handler.Traces.Record(trace.Entry{TraceID: "wanted", AgentID: "scout7", Tool: "memory.memory_store", Policy: "allow"})
	for i := 0; i < 1500; i++ {
		handler.Traces.Record(trace.Entry{AgentID: "claude", Tool: "x", Policy: "allow"})
	}
	count := func(path string) int {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newLoopbackReq("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		var entries []trace.Entry
		_ = json.Unmarshal(w.Body.Bytes(), &entries)
		return len(entries)
	}
	if n := count("/traces?trace=wanted"); n != 1 {
		t.Errorf("trace=wanted: %d entries, want 1 (found past the last 1000 calls)", n)
	}
	if n := count("/traces"); n != 100 {
		t.Errorf("default: %d, want 100", n)
	}
	if n := count("/traces?limit=500"); n != 500 {
		t.Errorf("limit=500: %d", n)
	}
	if n := count("/traces?limit=5000"); n != 1000 {
		t.Errorf("limit is capped at 1000: %d", n)
	}
}
