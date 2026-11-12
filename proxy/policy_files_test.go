package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func getFile(h *Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newLoopbackReq("GET", path, nil))
	return w
}

func putFile(h *Handler, path, ifMatch, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := newLoopbackReq("PUT", path, strings.NewReader(body))
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	h.ServeHTTP(w, r)
	return w
}

func TestPolicyFilesList(t *testing.T) {
	h, _ := editFixture(t)
	w := getFile(h, "/policies/files")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var files []policyFileInfo
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Name != "claude" || files[1].Name != "default" || files[0].SHA == "" {
		t.Errorf("files = %+v, want claude and default with fingerprints", files)
	}
}

func TestPolicyFileRoundTrip(t *testing.T) {
	h, file := editFixture(t)

	w := getFile(h, "/policies/files/claude")
	if w.Code != 200 || w.Body.String() != claudePolicy {
		t.Fatalf("GET status %d body %q", w.Code, w.Body)
	}
	sha := w.Header().Get("ETag")
	if sha == "" {
		t.Fatal("no ETag on GET")
	}

	edited := strings.Replace(claudePolicy, "action: allow", "action: deny", 1)
	w = putFile(h, "/policies/files/claude", sha, edited)
	if w.Code != 200 {
		t.Fatalf("PUT status %d: %s", w.Code, w.Body)
	}
	if d := h.Policy.Evaluate("claude", "gmail.send", nil); d.Action != "deny" {
		t.Errorf("after the edit gmail.send = %s, want deny (applied without waiting for the watcher)", d.Action)
	}
	if got, _ := os.ReadFile(file); string(got) != edited {
		t.Errorf("file on disk:\n%s", got)
	}
	if bak, _ := os.ReadFile(file + ".bak"); string(bak) != claudePolicy {
		t.Errorf("backup holds %q, want the previous version", bak)
	}
	if w.Header().Get("ETag") == sha {
		t.Error("ETag did not change with the content")
	}
	var traced bool
	for _, e := range h.Traces.Query("", "mesh.policy_file_edit", 10) {
		if e.Tool == "mesh.policy_file_edit" && e.Params["file"] == "claude.yaml" {
			traced = true
		}
	}
	if !traced {
		t.Error("the edit is not in the trace")
	}
}

func TestPolicyFileRejectsStaleFingerprint(t *testing.T) {
	h, file := editFixture(t)
	w := putFile(h, "/policies/files/claude", "0000", strings.Replace(claudePolicy, "allow", "deny", 1))
	if w.Code != 409 {
		t.Fatalf("status %d, want 409: %s", w.Code, w.Body)
	}
	if got, _ := os.ReadFile(file); string(got) != claudePolicy {
		t.Error("a stale edit changed the file")
	}
}

func TestPolicyFileNeedsIfMatch(t *testing.T) {
	h, _ := editFixture(t)
	if w := putFile(h, "/policies/files/claude", "", claudePolicy); w.Code != 400 {
		t.Fatalf("status %d, want 400", w.Code)
	}
}

func TestPolicyFileInvalidIsRestored(t *testing.T) {
	h, file := editFixture(t)
	sha := getFile(h, "/policies/files/claude").Header().Get("ETag")

	for name, body := range map[string]string{
		"not yaml":       "name: claude\nrules: [\n",
		"unknown action": strings.Replace(claudePolicy, "action: allow", "action: maybe", 1),
		"duplicate name": strings.Replace(claudePolicy, "name: claude", "name: default", 1),
	} {
		w := putFile(h, "/policies/files/claude", sha, body)
		if w.Code != 422 {
			t.Errorf("%s: status %d, want 422: %s", name, w.Code, w.Body)
		}
		if got, _ := os.ReadFile(file); string(got) != claudePolicy {
			t.Fatalf("%s: file not restored:\n%s", name, got)
		}
		if d := h.Policy.Evaluate("claude", "gmail.send", nil); d.Action != "allow" {
			t.Errorf("%s: running policy changed to %s", name, d.Action)
		}
	}
}

func TestPolicyFileNamesStayInTheDirectory(t *testing.T) {
	h, _ := editFixture(t)
	for _, p := range []string{
		"/policies/files/..%2Fconfig",
		"/policies/files/.hidden",
		"/policies/files/a%5Cb",
	} {
		if w := getFile(h, p); w.Code != 400 {
			t.Errorf("GET %s = %d, want 400", p, w.Code)
		}
	}
	if w := getFile(h, "/policies/files/nobody"); w.Code != 404 {
		t.Errorf("unknown file = %d, want 404", w.Code)
	}
	// no creation through the API
	if w := putFile(h, "/policies/files/nobody", "x", claudePolicy); w.Code != 404 {
		t.Errorf("PUT on a missing file = %d, want 404", w.Code)
	}
}

func TestPolicyFilePathKeepsToolEdits(t *testing.T) {
	// An agent named "files" still gets its tool edits: one more path element.
	if _, ok := parsePolicyFilePath("/policies/files/tools/gmail.send"); ok {
		t.Error("/policies/files/tools/x read as a file path")
	}
	if a, tool, ok := parsePolicyEditPath("/policies/files/tools/gmail.send"); !ok || a != "files" || tool != "gmail.send" {
		t.Error("tool edit for an agent named files no longer parses")
	}
}
