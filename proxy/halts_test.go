package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/grant"
	"github.com/KTCrisis/flux7-mesh/halt"
)

func setupHaltHandler(t *testing.T) *Handler {
	t.Helper()
	h, _ := setupHandler(t)
	h.Halts, _ = halt.NewStore(nil)
	h.Grants = grant.NewStore()
	h.Approvals = approval.NewStore(time.Minute)
	return h
}

func callTool(h *Handler, agent, session string) *httptest.ResponseRecorder {
	req := newLoopbackReq("POST", "/tool/get_pet", strings.NewReader(`{"params":{}}`))
	req.Header.Set("Authorization", "Bearer agent:"+agent)
	if session != "" {
		req.Header.Set("X-Session-Id", session)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func postJSON(h *Handler, path, body string) *httptest.ResponseRecorder {
	req := newLoopbackReq("POST", path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAgentHaltStopsOnlyThatAgent(t *testing.T) {
	h := setupHaltHandler(t)

	w := postJSON(h, "/halts", `{"scope":"agent","target":"scout7","reason":"loops on search","by":"marc"}`)
	if w.Code != 201 {
		t.Fatalf("POST /halts = %d: %s", w.Code, w.Body.String())
	}

	w = callTool(h, "scout7", "")
	if w.Code != 403 {
		t.Fatalf("halted agent: status %d, want 403", w.Code)
	}
	var resp ToolCallResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Policy != "halted" || !strings.Contains(resp.Error, "loops on search") {
		t.Fatalf("unexpected response %+v", resp)
	}

	if w := callTool(h, "claude", ""); w.Code != 200 {
		t.Fatalf("another agent must still work, got %d", w.Code)
	}

	// The refused call and the operator's action are both in the trace chain.
	var halted, action bool
	for _, e := range h.Traces.Query("", "", 50) {
		if e.Policy == "halted" && e.AgentID == "scout7" {
			halted = true
		}
		if e.Tool == "mesh.halt" && e.AgentID == "marc" {
			action = true
		}
	}
	if !halted || !action {
		t.Fatalf("traces: halted call %v, halt action %v", halted, action)
	}
}

func TestSessionHalt(t *testing.T) {
	h := setupHaltHandler(t)
	postJSON(h, "/halts", `{"scope":"session","target":"s-42"}`)

	if w := callTool(h, "claude", "s-42"); w.Code != 403 {
		t.Fatalf("call in the halted session: %d, want 403", w.Code)
	}
	if w := callTool(h, "claude", "s-43"); w.Code != 200 {
		t.Fatalf("call in another session: %d, want 200", w.Code)
	}
}

func TestGlobalHaltRevokesGrantsAndResumeRestoresThem(t *testing.T) {
	h := setupHaltHandler(t)
	g := h.Grants.Add("claude", "get_*", "marc", time.Hour)

	w := postJSON(h, "/halts", `{"scope":"all","reason":"incident"}`)
	var created struct {
		Halt          halt.Halt `json:"halt"`
		RevokedGrants int       `json:"revoked_grants"`
	}
	json.NewDecoder(w.Body).Decode(&created)
	if created.RevokedGrants != 1 || h.Grants.Check("claude", "get_pet") != nil {
		t.Fatal("a global halt must revoke the grants")
	}
	if w := callTool(h, "anyone", ""); w.Code != 403 {
		t.Fatalf("global halt: %d, want 403", w.Code)
	}

	// Pressing the button twice does not stack a second stop.
	w = postJSON(h, "/halts", `{"scope":"all"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"already_active":true`) {
		t.Fatalf("second identical halt: %d %s", w.Code, w.Body.String())
	}

	w = postJSON(h, "/halts/"+created.Halt.ID[:6]+"/resume", `{"by":"marc"}`)
	if w.Code != 200 {
		t.Fatalf("resume = %d: %s", w.Code, w.Body.String())
	}
	if back := h.Grants.Check("claude", "get_pet"); back == nil || back.ID != g.ID {
		t.Fatal("resume must restore the revoked grant with its ID")
	}
	if w := callTool(h, "anyone", ""); w.Code != 200 {
		t.Fatalf("after resume: %d, want 200", w.Code)
	}
	if w := postJSON(h, "/halts/"+created.Halt.ID+"/resume", `{}`); w.Code != 404 {
		t.Fatalf("resuming twice: %d, want 404", w.Code)
	}
}

func TestHaltDeniesPendingApprovals(t *testing.T) {
	h := setupHaltHandler(t)
	pa := h.Approvals.Submit("scout7", "get_pet", "ask", nil, "")
	other := h.Approvals.Submit("claude", "get_pet", "ask", nil, "")

	postJSON(h, "/halts", `{"scope":"agent","target":"scout7"}`)

	select {
	case res := <-pa.Result:
		if res.Status != approval.StatusDenied || !strings.HasPrefix(res.ResolvedBy, "halt:") {
			t.Fatalf("pending approval resolved as %+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("the halted agent's pending approval must be denied at once")
	}
	if got := h.Approvals.Get(other.ID); got.Status != approval.StatusPending {
		t.Fatal("another agent's approval must stay pending")
	}
}

func TestDecideAnswersDenyWhenHalted(t *testing.T) {
	h := setupHaltHandler(t)
	postJSON(h, "/halts", `{"scope":"agent","target":"claude"}`)

	w := postJSON(h, "/decide", `{"agent":"claude","tool":"get_pet"}`)
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp)
	if w.Code != 403 || resp["action"] != "deny" || resp["halted"] != true {
		t.Fatalf("/decide while halted: %d %v", w.Code, resp)
	}
}

func TestHaltValidationAndAdminGate(t *testing.T) {
	h := setupHaltHandler(t)
	if w := postJSON(h, "/halts", `{"scope":"agent"}`); w.Code != 400 {
		t.Fatalf("agent halt without target: %d, want 400", w.Code)
	}

	req := httptest.NewRequest("POST", "/halts", strings.NewReader(`{"scope":"all"}`))
	req.RemoteAddr = "203.0.113.9:4000" // not loopback, no admin token
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("remote caller without admin token: %d, want 401", w.Code)
	}
}
