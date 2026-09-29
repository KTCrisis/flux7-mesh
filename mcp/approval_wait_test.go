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

// waitServer is a non-blocking MCP server with an approval wait, in front of
// a backend that counts how many times the tool really ran.
func waitServer(t *testing.T, wait time.Duration) (*Server, *approval.Store, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(backend.Close)
	reg := registry.New()
	reg.LoadManual(&registry.Tool{Name: "write", Source: "openapi", Method: "POST", Path: "/w", BaseURL: backend.URL,
		Params: []registry.Param{{Name: "path", In: "body", Type: "string"}}})
	pol := policy.NewEngine([]config.Policy{{Name: "p", Agent: "*", Rules: []config.Rule{
		{Tools: []string{"write"}, Action: "human_approval"}}}})
	traces := trace.NewStore(50)
	store := approval.NewStore(time.Minute)
	s := &Server{Registry: reg, Policy: pol, Traces: traces, Approvals: store,
		Handler: proxy.NewHandler(reg, pol, traces), AgentID: "claude", ApprovalChannel: "queue",
		ApprovalWait: wait}
	return s, store, &hits
}

// resolveWhenPending plays the supervisor: it resolves the first pending
// approval after `after`, the way sup7 does about half a second after submit.
func resolveWhenPending(t *testing.T, store *approval.Store, after time.Duration, status approval.Status) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if p := store.ListPending(); len(p) == 1 {
				time.Sleep(after)
				store.Resolve(p[0].ID, status, approval.ResolveOpts{ResolvedBy: "supervisor:supervisor", Reasoning: "jev"})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func callWrite(t *testing.T, s *Server, path string) string {
	res := sendRPC(t, s, rpcRequest{JSONRPC: "2.0", ID: float64(1), Method: "tools/call",
		Params: map[string]any{"name": "write", "arguments": map[string]any{"path": path}}})
	return extractText(t, res[0])
}

// Decided within the wait, the call runs in the same request: no retry.
// Its approval is used up, so a retry of the same call does not run it again.
func TestWaitRunsTheCallWhenDecidedInTime(t *testing.T) {
	s, store, hits := waitServer(t, time.Second)
	resolveWhenPending(t, store, 50*time.Millisecond, approval.StatusApproved)
	if got := callWrite(t, s, "/tmp/a"); !strings.Contains(got, `"ok"`) || hits.Load() != 1 {
		t.Fatalf("approved within the wait, the call did not run: hits=%d, %s", hits.Load(), got)
	}
	if again := callWrite(t, s, "/tmp/a"); !strings.Contains(again, "Approval required") || hits.Load() != 1 {
		t.Fatalf("a retry ran the call a second time on the same approval: hits=%d, %s", hits.Load(), again)
	}
}

func TestWaitAnswersARefusalInTime(t *testing.T) {
	s, store, hits := waitServer(t, time.Second)
	resolveWhenPending(t, store, 50*time.Millisecond, approval.StatusDenied)
	if got := callWrite(t, s, "/tmp/a"); !strings.Contains(got, "Denied") || hits.Load() != 0 {
		t.Fatalf("denied within the wait, the agent was not told: hits=%d, %s", hits.Load(), got)
	}
}

// Too late for the wait (a human, or a slow supervisor): the usual flow, the
// agent gets the approval id and its retry runs once.
func TestWaitExpiresThenRetryRuns(t *testing.T) {
	s, store, hits := waitServer(t, 50*time.Millisecond)
	start := time.Now()
	if got := callWrite(t, s, "/tmp/a"); !strings.Contains(got, "Approval required") {
		t.Fatalf("no decision within the wait, want the approval id: %s", got)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("the call waited %s, far beyond the configured 50ms", elapsed)
	}
	p := store.ListPending()
	if len(p) != 1 {
		t.Fatalf("pending = %d, want 1", len(p))
	}
	store.Approve(p[0].ID, "human:marc")
	if got := callWrite(t, s, "/tmp/a"); !strings.Contains(got, `"ok"`) || hits.Load() != 1 {
		t.Fatalf("retry after a late approval did not run once: hits=%d, %s", hits.Load(), got)
	}
}
