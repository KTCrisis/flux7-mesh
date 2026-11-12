package proxy

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KTCrisis/flux7-mesh/policy"
	"github.com/KTCrisis/flux7-mesh/registry"
	"github.com/KTCrisis/flux7-mesh/trace"
)

type fakeUpstreams struct {
	reloadErr error
	restarted []string
	gen       uint64
}

func (f *fakeUpstreams) Reload() (any, error) {
	if f.reloadErr != nil {
		return nil, f.reloadErr
	}
	f.gen++
	return map[string][]string{"changed": {"searxng"}}, nil
}

func (f *fakeUpstreams) Restart(name string) error {
	if name != "searxng" {
		return errors.New(`no upstream named "` + name + `"`)
	}
	f.restarted = append(f.restarted, name)
	f.gen++
	return nil
}

func (f *fakeUpstreams) Generation() uint64 { return f.gen }

func post(h *Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newLoopbackReq("POST", path, strings.NewReader("")))
	return w
}

func TestUpstreamReloadAndRestartRoutes(t *testing.T) {
	h := NewHandler(registry.New(), policy.NewEngine(nil), trace.NewStore(10))

	if w := post(h, "/mcp-servers/reload"); w.Code != 501 {
		t.Errorf("reload without upstream management = %d, want 501", w.Code)
	}

	f := &fakeUpstreams{}
	h.Upstreams = f

	w := post(h, "/mcp-servers/reload")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "searxng") {
		t.Fatalf("reload = %d %s", w.Code, w.Body)
	}
	if got := h.Traces.Query("", "mesh.upstream_reload", 10); len(got) != 1 || got[0].StatusCode != 200 {
		t.Errorf("reload not traced once: %+v", got)
	}

	if w := post(h, "/mcp-servers/searxng/restart"); w.Code != 200 || len(f.restarted) != 1 {
		t.Errorf("restart = %d, restarted %v", w.Code, f.restarted)
	}
	if w := post(h, "/mcp-servers/nobody/restart"); w.Code != 404 {
		t.Errorf("restart of an unknown upstream = %d, want 404", w.Code)
	}
	if w := post(h, "/mcp-servers/a/b/restart"); w.Code != 400 {
		t.Errorf("restart with a nested name = %d, want 400", w.Code)
	}

	// /health carries the catalogue generation
	hw := httptest.NewRecorder()
	h.ServeHTTP(hw, newLoopbackReq("GET", "/health", nil))
	var health map[string]any
	_ = json.Unmarshal(hw.Body.Bytes(), &health)
	if health["catalog"] != float64(2) {
		t.Errorf("health.catalog = %v, want 2 (one reload, one restart)", health["catalog"])
	}

	// a config that does not load is refused, traced as refused
	f.reloadErr = errors.New("yaml: line 3: did not find expected key")
	if w := post(h, "/mcp-servers/reload"); w.Code != 422 || !strings.Contains(w.Body.String(), "upstreams unchanged") {
		t.Errorf("failed reload = %d %s", w.Code, w.Body)
	}
}
