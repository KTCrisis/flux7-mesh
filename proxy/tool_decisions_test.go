package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

func TestListToolsCarriesClassification(t *testing.T) {
	handler, _ := setupHandler(t)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, newLoopbackReq("GET", "/tools", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	var tools []map[string]any
	json.NewDecoder(w.Body).Decode(&tools)
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(tools))
	}
	// Existing fields keep their place at the top level.
	if tools[0]["name"] != "get_pet" {
		t.Errorf("name = %v, want get_pet", tools[0]["name"])
	}
	c, _ := tools[0]["classification"].(map[string]any)
	if c["family"] != "named" || c["access"] != "read" {
		t.Errorf("classification = %v, want named/read", c)
	}
}

func decisionsHandler(t *testing.T) *Handler {
	t.Helper()
	reg := registry.New()
	reg.LoadManual(&registry.Tool{Name: "gmail.send_email", Source: "mcp", MCPServer: "gmail"})
	reg.LoadManual(&registry.Tool{Name: "gmail.list_emails", Source: "mcp", MCPServer: "gmail"})
	reg.LoadManual(&registry.Tool{Name: "tf.__dispatch", Source: "cli",
		CLIMeta: &registry.CLIToolMeta{Bin: "terraform", IsCatchAll: true, DefaultAction: "human_approval"}})

	pol := policy.NewEngine([]config.Policy{{Name: "claude", Agent: "claude", Rules: []config.Rule{
		{Tools: []string{"gmail.send_email"}, Action: "allow",
			Condition: &config.Condition{Field: "to", Operator: "starts_with", Value: config.CondValue{Strings: []string{"marc@"}}}},
		{Tools: []string{"gmail.*"}, Action: "human_approval"},
		{Tools: []string{"gmail.list_emails", "tf.*"}, Action: "allow"},
	}}})
	return NewHandler(reg, pol, trace.NewStore(10))
}

func TestToolDecisions(t *testing.T) {
	h := decisionsHandler(t)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newLoopbackReq("GET", "/tools/decisions?agent=claude", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var rows []struct {
		Name           string                   `json:"name"`
		Action         string                   `json:"action"`
		Rule           string                   `json:"rule"`
		Classification registry.Classification  `json:"classification"`
		Conditional    []policy.ConditionalRule `json:"conditional"`
	}
	json.NewDecoder(w.Body).Decode(&rows)
	got := map[string]int{}
	for i, r := range rows {
		got[r.Name] = i
	}

	send := rows[got["gmail.send_email"]]
	if send.Action != "human_approval" || len(send.Conditional) != 1 || send.Conditional[0].Action != "allow" {
		t.Errorf("send_email = %+v, want human_approval with one conditional allow before it", send)
	}
	// The glob on gmail.* comes first, so list_emails asks despite the later allow.
	if a := rows[got["gmail.list_emails"]].Action; a != "human_approval" {
		t.Errorf("list_emails action = %s, want human_approval (first match wins)", a)
	}
	// The dispatcher floor holds over the policy's allow.
	tf := rows[got["tf.__dispatch"]]
	if tf.Action != "human_approval" || tf.Classification.Family != registry.FamilyGeneric {
		t.Errorf("tf.__dispatch = %+v, want human_approval and generic", tf)
	}

	// An agent no policy names falls to the default deny.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, newLoopbackReq("GET", "/tools/decisions?agent=stranger", nil))
	json.NewDecoder(w.Body).Decode(&rows)
	for _, r := range rows {
		if r.Action != "deny" || r.Rule != "default" {
			t.Errorf("%s for stranger = %s/%s, want deny/default", r.Name, r.Action, r.Rule)
		}
	}
}

func TestToolDecisionsGuards(t *testing.T) {
	h := decisionsHandler(t)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newLoopbackReq("GET", "/tools/decisions", nil))
	if w.Code != 400 {
		t.Errorf("missing agent = %d, want 400", w.Code)
	}

	// It discloses the policy, so it is control plane.
	r := httptest.NewRequest("GET", "/tools/decisions?agent=claude", nil)
	r.RemoteAddr = "10.0.0.5:4000"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("non-loopback without admin token = %d, want 401", w.Code)
	}
}
