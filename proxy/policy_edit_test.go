package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

const claudePolicy = `name: claude
agent: "claude"
rules:
  # mail
  - tools: ["gmail.*"]
    action: allow
`

func editFixture(t *testing.T) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfg, []byte("policy_dir: policies\n"), 0o644)
	os.Mkdir(filepath.Join(dir, "policies"), 0o755)
	os.WriteFile(filepath.Join(dir, "policies", "claude.yaml"), []byte(claudePolicy), 0o644)
	os.WriteFile(filepath.Join(dir, "policies", "default.yaml"),
		[]byte("name: default\nagent: \"*\"\nrules:\n  - tools: [\"*\"]\n    action: deny\n"), 0o644)

	pols, pdir, err := config.LoadPolicies(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(registry.New(), policy.NewEngine(pols), trace.NewStore(10))
	h.PolicyEditing = &PolicyEditing{ConfigPath: cfg, Dir: pdir}
	return h, filepath.Join(pdir, "claude.yaml")
}

func put(h *Handler, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newLoopbackReq("PUT", path, strings.NewReader(body)))
	return w
}

func TestPolicyEditRoundTrip(t *testing.T) {
	h, file := editFixture(t)

	// Deny one tool the glob allows: an exact rule lands before the glob.
	w := put(h, "/policies/claude/tools/gmail.send", `{"action":"deny","by":"console"}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if d := h.Policy.Evaluate("claude", "gmail.send", nil); d.Action != "deny" {
		t.Errorf("after edit, gmail.send = %s, want deny (applied without waiting for the watcher)", d.Action)
	}
	if d := h.Policy.Evaluate("claude", "gmail.list", nil); d.Action != "allow" {
		t.Errorf("gmail.list = %s, the glob must still allow the others", d.Action)
	}
	body, _ := os.ReadFile(file)
	if !strings.Contains(string(body), "# mail") || !strings.Contains(string(body), "by console") {
		t.Errorf("file lost its comment or the marker:\n%s", body)
	}
	if _, err := os.Stat(file + ".bak"); err != nil {
		t.Error("no backup written")
	}

	// Second edit rewrites the same rule instead of stacking another.
	put(h, "/policies/claude/tools/gmail.send", `{"action":"human_approval"}`)
	body, _ = os.ReadFile(file)
	if strings.Count(string(body), `"gmail.send"`) != 1 {
		t.Errorf("rule duplicated:\n%s", body)
	}
	if d := h.Policy.Evaluate("claude", "gmail.send", nil); d.Action != "human_approval" {
		t.Errorf("gmail.send = %s, want human_approval", d.Action)
	}

	// Inherit removes it and the file is back to what the operator wrote.
	if w := put(h, "/policies/claude/tools/gmail.send", `{"action":"inherit"}`); w.Code != 200 {
		t.Fatalf("inherit status %d: %s", w.Code, w.Body)
	}
	body, _ = os.ReadFile(file)
	if string(body) != claudePolicy {
		t.Errorf("file not restored:\n%s", body)
	}

	// Every edit is in the trace.
	edits := h.Traces.Query("", "mesh.policy_edit", 10)
	if len(edits) != 3 {
		t.Errorf("trace has %d policy edits, want 3", len(edits))
	}
}

func TestPolicyEditAppendsWhenAGlobPolicyDecides(t *testing.T) {
	h, file := editFixture(t)
	// net.get is decided by the shared default deny: the rule goes at the end
	// of claude's own policy, which is evaluated first.
	if w := put(h, "/policies/claude/tools/net.get", `{"action":"allow"}`); w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if d := h.Policy.Evaluate("claude", "net.get", nil); d.Action != "allow" {
		t.Errorf("net.get = %s, want allow", d.Action)
	}
	body, _ := os.ReadFile(file)
	if !strings.HasPrefix(string(body), claudePolicy) {
		t.Errorf("rule not appended at the end:\n%s", body)
	}
}

func TestPolicyEditRefusals(t *testing.T) {
	h, _ := editFixture(t)
	cases := []struct {
		path, body string
		code       int
	}{
		{"/policies/claude/tools/gmail.send", `{"action":"maybe"}`, 400},
		{"/policies/claude/tools/gmail.*", `{"action":"deny"}`, 400},       // globs are not a tool
		{"/policies/stranger/tools/gmail.send", `{"action":"deny"}`, 409},  // no file for that agent
		{"/policies/claude/tools/gmail.list", `{"action":"inherit"}`, 409}, // nothing set from the console
	}
	for _, c := range cases {
		if w := put(h, c.path, c.body); w.Code != c.code {
			t.Errorf("%s %s = %d, want %d (%s)", c.path, c.body, w.Code, c.code, w.Body)
		}
	}

	// Control plane: not from the network without the admin token.
	r := httptest.NewRequest("PUT", "/policies/claude/tools/gmail.send", strings.NewReader(`{"action":"deny"}`))
	r.RemoteAddr = "10.0.0.5:4000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("non-loopback = %d, want 401", w.Code)
	}

	var body map[string]any
	json.NewDecoder(put(h, "/policies/claude/tools/x", `{"action":"deny"}`).Body).Decode(&body)
	if body["file"] != "claude.yaml" {
		t.Errorf("response file = %v", body["file"])
	}
}
