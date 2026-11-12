package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/trace"
)

// maxPolicyFileBytes bounds a policy file sent over the control plane. The
// largest hand-written policy in use is a few hundred lines.
const maxPolicyFileBytes = 1 << 20

// policyFileInfo is one entry of GET /policies/files.
type policyFileInfo struct {
	Name string `json:"name"` // file name without .yaml
	File string `json:"file"` // file name in policy_dir
	SHA  string `json:"sha"`  // fingerprint of the content, the ETag of the file
}

func fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// policyFilePath resolves {name} to a file of policy_dir. A name is a single
// path element, with or without .yaml; anything that could leave the
// directory, or name a hidden file, is refused.
func (pe *PolicyEditing) policyFilePath(name string) (string, bool) {
	name = strings.TrimSuffix(name, ".yaml")
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) || name != filepath.Base(name) {
		return "", false
	}
	return filepath.Join(pe.Dir, name+".yaml"), true
}

// parsePolicyFilePath reads /policies/files/{name}. The second result tells a
// file path from the tool-edit path /policies/{agent}/tools/{tool}, which has
// one more element, so an agent called "files" keeps its tool edits.
func parsePolicyFilePath(escaped string) (string, bool) {
	rest, found := strings.CutPrefix(escaped, "/policies/files/")
	if !found || rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	name, err := url.PathUnescape(rest)
	if err != nil {
		return "", false
	}
	return name, true
}

// handlePolicyFiles serves GET /policies/files: the policy files of
// policy_dir, by name, each with the fingerprint of its content.
func (h *Handler) handlePolicyFiles(w http.ResponseWriter, _ *http.Request) {
	pe := h.PolicyEditing
	if pe == nil || pe.Dir == "" {
		writeJSON(w, 501, map[string]string{"error": "policy files need policy_dir"})
		return
	}
	entries, err := os.ReadDir(pe.Dir)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "read policy_dir: " + err.Error()})
		return
	}
	files := []policyFileInfo{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(pe.Dir, e.Name()))
		if err != nil {
			continue
		}
		files = append(files, policyFileInfo{
			Name: strings.TrimSuffix(e.Name(), ".yaml"),
			File: e.Name(),
			SHA:  fingerprint(b),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	writeJSON(w, 200, files)
}

// handlePolicyFileGet serves GET /policies/files/{name}: the file as text,
// with its fingerprint in ETag.
func (h *Handler) handlePolicyFileGet(w http.ResponseWriter, r *http.Request) {
	pe := h.PolicyEditing
	if pe == nil || pe.Dir == "" {
		writeJSON(w, 501, map[string]string{"error": "policy files need policy_dir"})
		return
	}
	name, ok := parsePolicyFilePath(r.URL.EscapedPath())
	path, ok2 := "", false
	if ok {
		path, ok2 = pe.policyFilePath(name)
	}
	if !ok || !ok2 {
		writeJSON(w, 400, map[string]string{"error": "want /policies/files/{name}"})
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "no policy file " + filepath.Base(path)})
		return
	}
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("ETag", fingerprint(b))
	w.WriteHeader(200)
	w.Write(b)
}

// handlePolicyFilePut serves PUT /policies/files/{name}: replace a policy
// file with the text sent.
//
// It is the whole-file sibling of the tool edit and takes the same path: one
// edit at a time, the previous version kept next to the file, the whole policy
// set validated before the edit is kept, the file restored when it is not,
// the set applied at once, and the edit recorded in the trace chain.
//
// The caller sends in If-Match the fingerprint it read, so two editors cannot
// overwrite each other unseen. Only an existing file is replaced: a new file
// would add a policy, and two files declaring the same name both load.
func (h *Handler) handlePolicyFilePut(w http.ResponseWriter, r *http.Request) {
	pe := h.PolicyEditing
	if pe == nil || pe.Dir == "" {
		writeJSON(w, 501, map[string]string{"error": "policy files need policy_dir"})
		return
	}
	name, ok := parsePolicyFilePath(r.URL.EscapedPath())
	path, ok2 := "", false
	if ok {
		path, ok2 = pe.policyFilePath(name)
	}
	if !ok || !ok2 {
		writeJSON(w, 400, map[string]string{"error": "want /policies/files/{name}"})
		return
	}
	ifMatch := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if ifMatch == "" {
		writeJSON(w, 400, map[string]string{"error": "If-Match is required: send the fingerprint read with GET (its ETag)"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPolicyFileBytes+1))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "read body: " + err.Error()})
		return
	}
	if len(body) > maxPolicyFileBytes {
		writeJSON(w, 413, map[string]string{"error": "policy file larger than 1 MiB"})
		return
	}
	if strings.TrimSpace(string(body)) == "" {
		writeJSON(w, 400, map[string]string{"error": "empty body"})
		return
	}

	pe.mu.Lock()
	defer pe.mu.Unlock()

	file := filepath.Base(path)
	src, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "no policy file " + file + "; create it in policy_dir first"})
		return
	}
	before := fingerprint(src)
	if ifMatch != before {
		writeJSON(w, 409, map[string]string{"error": file + " changed since it was read (now " + before + "); reload it first"})
		return
	}
	after := fingerprint(body)
	if after == before {
		w.Header().Set("ETag", before)
		writeJSON(w, 200, map[string]any{"file": file, "sha": before, "changed": false})
		return
	}

	if err := os.WriteFile(path+".bak", src, 0o644); err != nil {
		writeJSON(w, 500, map[string]string{"error": "backup: " + err.Error()})
		return
	}
	if err := writeAtomic(path, body); err != nil {
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

	by := strings.TrimSpace(r.Header.Get("X-Edited-By"))
	if by == "" {
		by = "admin"
	}
	h.Traces.Record(trace.Entry{
		TraceID: trace.NewID(),
		AgentID: by,
		Tool:    "mesh.policy_file_edit",
		Params: map[string]any{
			"file": file, "before_sha": before, "after_sha": after,
			"before_bytes": len(src), "after_bytes": len(body),
		},
		Policy:     "allow",
		PolicyRule: "control-plane",
		StatusCode: 200,
		Timestamp:  time.Now().UTC(),
	})

	w.Header().Set("ETag", after)
	writeJSON(w, 200, map[string]any{"file": file, "sha": after, "changed": true, "backup": file + ".bak"})
}

func isPolicyFilePath(r *http.Request) bool {
	_, ok := parsePolicyFilePath(r.URL.EscapedPath())
	return ok
}
