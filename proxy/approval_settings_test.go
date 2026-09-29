package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/approval"
	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

const settingsConfig = `# mesh config
approval:
  channel: queue   # daemon
  timeout_seconds: 300
  wait_seconds: 3

memory:
  url: http://localhost:9070

supervisor:
  enabled: false
`

func settingsFixture(t *testing.T) (*Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte(settingsConfig), 0o600)
	h := NewHandler(registry.New(), policy.NewEngine(nil), trace.NewStore(10))
	h.Approvals = approval.NewStore(5 * time.Minute)
	h.Approvals.MemoryReader = approval.NewMemoryReader("http://localhost:1", "", 3)
	h.Approvals.Apply(approval.Settings{TimeoutSeconds: 300, WaitSeconds: 3, AutoApprove: true, MinApprovals: 3})
	h.ApprovalSettings = &ApprovalSettingsEditing{ConfigPath: path}
	return h, path
}

func TestApprovalSettingsEditAppliesAndPersists(t *testing.T) {
	h, path := settingsFixture(t)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newLoopbackReq("GET", "/approvals/settings", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"wait_seconds":3`) {
		t.Fatalf("GET: %d %s", w.Code, w.Body)
	}

	body := `{"timeout_seconds":600,"wait_seconds":2,"auto_approve":true,"min_approvals":5,"auto_approve_writes":false,"by":"console"}`
	w = httptest.NewRecorder()
	h.ServeHTTP(w, newLoopbackReq("PUT", "/approvals/settings", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	st := h.Approvals.Settings()
	if st.WaitSeconds != 2 || st.TimeoutSeconds != 600 || st.MinApprovals != 5 || h.Approvals.Wait() != 2*time.Second {
		t.Fatalf("not applied at once: %+v", st)
	}
	file, _ := os.ReadFile(path)
	for _, want := range []string{"# mesh config", "channel: queue   # daemon", "timeout_seconds: 600", "wait_seconds: 2", "min_approvals: 5"} {
		if !strings.Contains(string(file), want) {
			t.Fatalf("file missing %q:\n%s", want, file)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode changed to %v", fi.Mode().Perm())
	}
	var resp struct {
		Changed []string `json:"changed"`
		Backup  string   `json:"backup"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Changed) != 3 || resp.Backup == "" {
		t.Fatalf("changed/backup: %+v", resp)
	}
	if b, err := os.ReadFile(resp.Backup); err != nil || !strings.Contains(string(b), "wait_seconds: 3") {
		t.Fatal("the backup must hold the previous version")
	}
	if e := h.Traces.Query("", "mesh.approval_settings_edit", 10); len(e) != 1 || e[0].AgentID != "console" {
		t.Fatalf("the edit was not traced: %+v", e)
	}
}

func TestApprovalSettingsRefusesOutOfRange(t *testing.T) {
	h, path := settingsFixture(t)
	before, _ := os.ReadFile(path)
	for _, body := range []string{
		`{"timeout_seconds":300,"wait_seconds":60,"auto_approve":true,"min_approvals":3}`,
		`{"timeout_seconds":5,"wait_seconds":3,"auto_approve":true,"min_approvals":3}`,
		`{"timeout_seconds":300,"wait_seconds":3,"auto_approve":true,"min_approvals":0}`,
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, newLoopbackReq("PUT", "/approvals/settings", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("%s: want 400, got %d %s", body, w.Code, w.Body)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("a refused edit touched the file")
	}
}
