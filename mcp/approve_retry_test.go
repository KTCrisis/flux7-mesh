package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/proxy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// A plain agent (not a supervisor) gets an approval id, a human approves out
// of band, and the agent's retry of the same call runs once.
func TestApproveThenRetry(t *testing.T) {
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	reg := registry.New()
	reg.LoadManual(&registry.Tool{Name: "write", Source: "openapi", Method: "POST", Path: "/w", BaseURL: backend.URL,
		Params: []registry.Param{{Name: "path", In: "body", Type: "string"}}})
	pol := policy.NewEngine([]config.Policy{{Name: "p", Agent: "*", Rules: []config.Rule{
		{Tools: []string{"write"}, Action: "human_approval"}}}})
	traces := trace.NewStore(50)
	store := approval.NewStore(time.Minute)
	s := &Server{Registry: reg, Policy: pol, Traces: traces, Approvals: store,
		Handler: proxy.NewHandler(reg, pol, traces), AgentID: "claude", ApprovalChannel: "queue"}

	call := func(path string) string {
		res := sendRPC(t, s, rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
			Params: map[string]any{"name": "write", "arguments": map[string]any{"path": path}}})
		return extractText(t, res[0])
	}

	first := call("/tmp/a")
	if !strings.Contains(first, "mesh approve") || strings.Contains(first, "Use approval.resolve") {
		t.Fatalf("message must tell the human how to approve, not point the agent at an operator tool:\n%s", first)
	}
	if hits.Load() != 0 {
		t.Fatal("the call ran before approval")
	}

	pending := store.ListPending()
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if err := store.Approve(pending[0].ID, "human:marc"); err != nil {
		t.Fatal(err)
	}

	// Different arguments do not ride on that approval.
	if other := call("/tmp/b"); !strings.Contains(other, "Approval required") || hits.Load() != 0 {
		t.Fatalf("an approval for /tmp/a let /tmp/b through: %s", other)
	}
	// The same call runs, once.
	if got := call("/tmp/a"); !strings.Contains(got, `"ok"`) || hits.Load() != 1 {
		t.Fatalf("retry after approval did not run: hits=%d, %s", hits.Load(), got)
	}
	if again := call("/tmp/a"); !strings.Contains(again, "Approval required") || hits.Load() != 1 {
		t.Fatalf("one approval ran twice: hits=%d, %s", hits.Load(), again)
	}
}
