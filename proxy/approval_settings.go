package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// ApprovalSettingsEditing lets the console change the approval knobs
// (timeout, wait, auto-approval from precedents) without a restart: the
// config file is rewritten line by line, reloaded to check it, and the new
// values are applied to the approval store at once.
type ApprovalSettingsEditing struct {
	ConfigPath string
	mu         sync.Mutex
}

type approvalSettingsRequest struct {
	approval.Settings
	By string `json:"by"` // who asked, a claim recorded in the trace
}

func (h *Handler) handleGetApprovalSettings(w http.ResponseWriter, r *http.Request) {
	if h.Approvals == nil {
		writeJSON(w, 409, map[string]string{"error": "no approval store"})
		return
	}
	path := ""
	if h.ApprovalSettings != nil {
		path = h.ApprovalSettings.ConfigPath
	}
	writeJSON(w, 200, map[string]any{
		"settings":  h.Approvals.Settings(),
		"config":    path,
		"editable":  h.ApprovalSettings != nil,
		"precedent": h.Approvals.MemoryReader != nil,
	})
}

func (h *Handler) handlePutApprovalSettings(w http.ResponseWriter, r *http.Request) {
	se := h.ApprovalSettings
	if se == nil || h.Approvals == nil {
		writeJSON(w, 501, map[string]string{"error": "approval settings are not editable here (no config file)"})
		return
	}
	var req approvalSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "expected JSON approval settings"})
		return
	}
	next := req.Settings
	if err := next.Validate(); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if next.AutoApprove && h.Approvals.MemoryReader == nil {
		writeJSON(w, 400, map[string]string{"error": "auto_approve needs memory.url (mem7) in the config"})
		return
	}

	se.mu.Lock()
	defer se.mu.Unlock()
	before := h.Approvals.Settings()
	src, err := os.ReadFile(se.ConfigPath)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "read config: " + err.Error()})
		return
	}
	var updates []config.Scalar
	add := func(section, key, value string, changed bool) {
		if changed {
			updates = append(updates, config.Scalar{Section: section, Key: key, Value: value})
		}
	}
	add("approval", "timeout_seconds", strconv.Itoa(next.TimeoutSeconds), next.TimeoutSeconds != before.TimeoutSeconds)
	add("approval", "wait_seconds", strconv.FormatFloat(next.WaitSeconds, 'f', -1, 64), next.WaitSeconds != before.WaitSeconds)
	add("supervisor", "auto_approve", strconv.FormatBool(next.AutoApprove), next.AutoApprove != before.AutoApprove)
	add("supervisor", "min_approvals", strconv.Itoa(next.MinApprovals), next.MinApprovals != before.MinApprovals)
	add("supervisor", "auto_approve_writes", strconv.FormatBool(next.AutoApproveWrites), next.AutoApproveWrites != before.AutoApproveWrites)
	if len(updates) == 0 {
		writeJSON(w, 200, map[string]any{"settings": before, "changed": []string{}})
		return
	}
	out, err := config.SetScalars(src, updates)
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}

	// back up next to the file (keeping its mode), swap atomically, then
	// reload the whole config: a file that no longer loads is restored
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(se.ConfigPath); err == nil {
		mode = fi.Mode().Perm()
	}
	backup, err := uniqueBackup(se.ConfigPath)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "backup: " + err.Error()})
		return
	}
	if err := os.WriteFile(backup, src, mode); err != nil {
		writeJSON(w, 500, map[string]string{"error": "backup: " + err.Error()})
		return
	}
	if err := writeAtomic(se.ConfigPath, out); err != nil {
		writeJSON(w, 500, map[string]string{"error": "write: " + err.Error()})
		return
	}
	cfg, err := config.Load(se.ConfigPath)
	if err == nil && (cfg.Approval.WaitSeconds != next.WaitSeconds || cfg.Approval.TimeoutSeconds != next.TimeoutSeconds ||
		cfg.Supervisor.IsAutoApproveEnabled() != next.AutoApprove || cfg.Supervisor.GetMinApprovals() != next.MinApprovals ||
		cfg.Supervisor.AutoApproveWrites != next.AutoApproveWrites) {
		err = fmt.Errorf("the file does not read back as requested")
	}
	if err != nil {
		_ = writeAtomic(se.ConfigPath, src)
		writeJSON(w, 422, map[string]string{"error": "edit rejected, file restored: " + err.Error()})
		return
	}
	h.Approvals.Apply(next)

	changed := make([]string, 0, len(updates))
	for _, u := range updates {
		changed = append(changed, u.Section+"."+u.Key)
	}
	by := req.By
	if by == "" {
		by = "admin"
	}
	h.Traces.Record(trace.Entry{
		TraceID:    trace.NewID(),
		AgentID:    by,
		Tool:       "mesh.approval_settings_edit",
		Params:     map[string]any{"changed": changed, "before": before, "after": next, "backup": backup},
		Policy:     "allow",
		PolicyRule: "control-plane",
		StatusCode: 200,
		Timestamp:  time.Now().UTC(), // every other trace entry is UTC
	})
	writeJSON(w, 200, map[string]any{"settings": next, "changed": changed, "backup": backup})
}

// uniqueBackup names a backup that does not exist yet: two edits within the
// same second must not overwrite each other's backup (seen on 2026-09-29,
// the original version was lost).
func uniqueBackup(path string) (string, error) {
	base := path + ".bak-" + time.Now().UTC().Format("20060102-150405")
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		if _, err := os.Stat(name); os.IsNotExist(err) {
			return name, nil
		}
	}
	return "", fmt.Errorf("no free backup name next to %s", path)
}
