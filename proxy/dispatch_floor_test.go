package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// setupFloorHandler builds a handler whose policy allows everything by glob —
// the case the floor exists to catch. `terraform.*: allow` is what an operator
// writes when they mean "the read-only commands I declared".
func setupFloorHandler(t *testing.T, defaultAction string) *Handler {
	t.Helper()

	reg := registry.New()
	reg.LoadCLI([]config.CLIToolConfig{
		{
			Name:          "terraform",
			Bin:           "terraform",
			DefaultAction: defaultAction,
			Commands: map[string]config.CLICommandConfig{
				"show": {},
			},
		},
	})

	pol := policy.NewEngine([]config.Policy{
		{Name: "wide-open", Agent: "*", Rules: []config.Rule{
			{Tools: []string{"*"}, Action: "allow"},
		}},
	})

	return NewHandler(reg, pol, trace.NewStore(100))
}

func decideAction(t *testing.T, h *Handler, tool string) (string, string) {
	t.Helper()

	body := `{"agent":"tester","tool":"` + tool + `","arguments":{"command":"destroy"}}`
	req := httptest.NewRequest("POST", "/decide", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var resp struct {
		Action string `json:"action"`
		Rule   string `json:"rule"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode /decide response: %v", err)
	}
	return resp.Action, resp.Rule
}

func TestDecideAppliesDispatchFloor(t *testing.T) {
	h := setupFloorHandler(t, "deny")

	// The declared command keeps the policy's allow.
	if action, _ := decideAction(t, h, "terraform.show"); action != "allow" {
		t.Errorf("terraform.show = %q, want allow — declaring a command is the grant", action)
	}

	// The dispatcher is refused despite `*: allow`.
	action, rule := decideAction(t, h, "terraform.__dispatch")
	if action != "deny" {
		t.Errorf("terraform.__dispatch = %q, want deny", action)
	}
	if rule != "default_action" {
		t.Errorf("rule = %q, want default_action", rule)
	}

	// The real bypass: an undeclared subcommand resolves to the dispatcher,
	// so /decide must answer what the call path would actually enforce.
	if action, _ := decideAction(t, h, "terraform.destroy"); action != "deny" {
		t.Errorf("terraform.destroy = %q, want deny", action)
	}
}

func TestDecideFloorAllowIsNoOp(t *testing.T) {
	h := setupFloorHandler(t, "allow")

	for _, tool := range []string{"terraform.show", "terraform.__dispatch", "terraform.destroy"} {
		if action, _ := decideAction(t, h, tool); action != "allow" {
			t.Errorf("%s = %q, want allow — an allow floor must never restrict", tool, action)
		}
	}
}

func TestToolCallAppliesDispatchFloor(t *testing.T) {
	h := setupFloorHandler(t, "deny")

	// The execution path must refuse before the binary is ever reached.
	req := httptest.NewRequest("POST", "/tool/terraform.destroy", strings.NewReader(`{"command":"destroy"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 403 {
		t.Errorf("POST /tool/terraform.destroy = %d, want 403", w.Code)
	}
}
