package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// PolicyEditing lets the control plane change one tool's action for one agent
// by editing the agent's policy file. Nil disables the endpoint.
type PolicyEditing struct {
	ConfigPath string                // to re-validate every policy after an edit
	Dir        string                // resolved policy_dir
	Reload     func([]config.Policy) // applies a validated policy set
	mu         sync.Mutex            // one edit at a time: read, write, validate
}

type policyEditRequest struct {
	// Action is allow, deny or human_approval, or "inherit" to remove the
	// console rule and fall back to whatever rule comes next.
	Action string `json:"action"`
	// By names who asked, for the trace. The admin token proves the right to
	// edit, not an identity, so this is a claim and recorded as one.
	By string `json:"by,omitempty"`
}

// handlePolicyEdit serves PUT /policies/{agent}/tools/{tool}.
//
// The rule is placed where it decides without disturbing the others: right
// before the rule that decides today when that rule belongs to this agent's
// policy, so conditional rules above it keep their say; at the end of the
// agent's policy otherwise, which still comes before any shared glob policy.
// A rule that already names exactly this tool is rewritten in place.
//
// Every edit is validated against the whole policy set before it is kept,
// applied at once, and recorded in the trace chain.
func (h *Handler) handlePolicyEdit(w http.ResponseWriter, r *http.Request) {
	pe := h.PolicyEditing
	if pe == nil || pe.Dir == "" {
		writeJSON(w, 501, map[string]string{"error": "policy editing needs policy_dir"})
		return
	}
	agent, tool, ok := parsePolicyEditPath(r.URL.EscapedPath())
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "want /policies/{agent}/tools/{tool}"})
		return
	}
	var req policyEditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON body"})
		return
	}
	switch req.Action {
	case "allow", "deny", "human_approval", "inherit":
	default:
		writeJSON(w, 400, map[string]string{"error": "action must be allow, deny, human_approval or inherit"})
		return
	}

	pe.mu.Lock()
	defer pe.mu.Unlock()

	before := h.Policy.Explain(agent, tool)

	// Pick the file and the position.
	file, at := "", -1
	if before.SourceFile != "" && before.PolicyAgent == agent {
		file, at = before.SourceFile, before.RuleIndex
	} else {
		for _, p := range h.Policy.Policies() {
			if p.Agent == agent && p.SourceFile != "" {
				file = p.SourceFile
				break
			}
		}
	}
	if file == "" {
		writeJSON(w, 409, map[string]string{"error": fmt.Sprintf("no policy file for agent %q in policy_dir", agent)})
		return
	}
	path := filepath.Join(pe.Dir, filepath.Base(file))
	src, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "read policy file: " + err.Error()})
		return
	}

	var out []byte
	if req.Action == "inherit" {
		var removed bool
		out, removed, err = policy.RemoveConsoleRule(src, tool)
		if err == nil && !removed {
			writeJSON(w, 409, map[string]string{"error": "no console rule for this tool in " + file + "; hand-written rules are edited in the file"})
			return
		}
	} else {
		exact, xerr := policy.ExactRule(src, tool)
		switch {
		case xerr != nil:
			err = xerr
		case exact >= 0 && (at < 0 || exact <= at):
			// The file already names this tool, and that rule is reached no
			// later than the deciding one: change it rather than stack another.
			out, err = policy.SetAction(src, exact, req.Action)
		default:
			note := time.Now().Format("2006-01-02")
			if req.By != "" {
				note += " by " + req.By
			}
			out, err = policy.InsertRule(src, tool, req.Action, at, note)
		}
	}
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}

	// Keep the previous version next to the file, then swap atomically. The
	// backup does not end in .yaml, so policy_dir never loads it.
	if err := os.WriteFile(path+".bak", src, 0o644); err != nil {
		writeJSON(w, 500, map[string]string{"error": "backup: " + err.Error()})
		return
	}
	if err := writeAtomic(path, out); err != nil {
		writeJSON(w, 500, map[string]string{"error": "write: " + err.Error()})
		return
	}
	policies, _, err := config.LoadPolicies(pe.ConfigPath)
	if err != nil {
		_ = writeAtomic(path, src)
		writeJSON(w, 422, map[string]string{"error": "edit rejected, file restored: " + err.Error()})
		return
	}
	if pe.Reload != nil {
		pe.Reload(policies)
	} else {
		h.Policy.Reload(policies)
	}

	after := h.Policy.Explain(agent, tool)
	by := req.By
	if by == "" {
		by = "admin"
	}
	h.Traces.Record(trace.Entry{
		TraceID: trace.NewID(),
		AgentID: by,
		Tool:    "mesh.policy_edit",
		Params: map[string]any{
			"agent": agent, "tool": tool, "requested": req.Action, "file": file,
			"before": before.Action + " (" + before.Rule + ")",
			"after":  after.Action + " (" + after.Rule + ")",
		},
		Policy:     "allow",
		PolicyRule: "control-plane",
		StatusCode: 200,
		Timestamp:  time.Now().UTC(), // every other trace entry is UTC
	})

	writeJSON(w, 200, map[string]any{
		"agent":  agent,
		"tool":   tool,
		"file":   file,
		"before": before,
		"after":  after,
	})
}

func parsePolicyEditPath(escaped string) (agent, tool string, ok bool) {
	rest, found := strings.CutPrefix(escaped, "/policies/")
	if !found {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[1] != "tools" {
		return "", "", false
	}
	a, err1 := url.PathUnescape(parts[0])
	t, err2 := url.PathUnescape(parts[2])
	if err1 != nil || err2 != nil || a == "" || t == "" || strings.ContainsAny(a+t, "*?\n") {
		return "", "", false
	}
	return a, t, true
}

// writeAtomic replaces path through a rename, keeping the file's mode: a
// policy file made group-writable by hand stays so.
func writeAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil { // WriteFile's mode passes through umask
		return err
	}
	return os.Rename(tmp, path)
}
