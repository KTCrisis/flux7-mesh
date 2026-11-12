package opcli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeMesh answers the control-plane routes the upstream and pin commands use
// and records what it was sent.
func fakeMesh(t *testing.T) (calls *[]string, bodies *[]string) {
	t.Helper()
	calls, bodies = &[]string{}, &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*calls = append(*calls, r.Method+" "+r.URL.Path)
		*bodies = append(*bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /mcp-servers":
			w.Write([]byte(`[{"name":"time","transport":"stdio","status":"ready","tools":["a","b","c"]},{"name":"fetch","transport":"stdio","status":"retrying","error":"exit 1","tools":[]}]`))
		case "POST /mcp-servers/reload":
			w.Write([]byte(`{"diff":{"added":[],"changed":[],"removed":["old"],"unchanged":["osm","memory"],"held":["fetch","time"]}}`))
		case "GET /mcp-servers/held":
			w.Write([]byte(`[{"name":"fetch","transport":"stdio","command":"uvx","args":["mcp-server-fetch@2026.11.11"],"env_keys":["HTTP_PROXY"],"replaces":false},{"name":"time","transport":"stdio","command":"uvx","args":["mcp-server-time@2026.11.11"],"replaces":true}]`))
		case "POST /mcp-servers/fetch/accept":
			w.Write([]byte(`{"server":"fetch"}`))
		case "POST /mcp-servers/time/restart":
			w.Write([]byte(`{"server":"time"}`))
		case "GET /tools/pins":
			w.Write([]byte(`[{"tool":"time.now","server":"time","status":"changed"}]`))
		case "POST /tools/pins/accept":
			w.Write([]byte(`{}`))
		default:
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]string{"error": `no upstream named "nobody"`})
		}
	}))
	t.Cleanup(srv.Close)
	old := meshURL
	meshURL = srv.URL
	t.Cleanup(func() { meshURL = old })
	return calls, bodies
}

func TestUpstreamCommands(t *testing.T) {
	calls, _ := fakeMesh(t)
	var out, errw bytes.Buffer

	if code := cmdUpstream([]string{"list"}, &out, &errw); code != 0 {
		t.Fatalf("list exit %d: %s", code, errw.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "fetch") || !strings.Contains(lines[1], "retrying") || !strings.Contains(lines[2], "ready") {
		t.Errorf("list output, sorted by name with status:\n%s", out.String())
	}

	out.Reset()
	if code := cmdUpstream([]string{"reload"}, &out, &errw); code != 0 {
		t.Fatalf("reload exit %d", code)
	}
	for _, want := range []string{
		"removed  old", "untouched 2",
		"held, nothing started: fetch, time",
		"runs     uvx mcp-server-fetch@2026.11.11", "env      HTTP_PROXY",
		"new server", "replaces the running declaration",
		"upstream accept <name>",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("reload output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "added") || strings.Contains(out.String(), "changed ") {
		t.Errorf("reload output names an empty group:\n%s", out.String())
	}

	out.Reset()
	if code := cmdUpstream([]string{"held"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "runs     uvx mcp-server-time@2026.11.11") {
		t.Errorf("held exit %d:\n%s", code, out.String())
	}
	out.Reset()
	if code := cmdUpstream([]string{"accept", "fetch"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "fetch accepted and started") {
		t.Errorf("accept exit %d out %q", code, out.String())
	}
	if code := cmdUpstream([]string{"accept", "nobody"}, &out, &errw); code != 1 {
		t.Errorf("accept of nothing held: exit %d, want 1", code)
	}

	out.Reset()
	if code := cmdUpstream([]string{"restart", "time"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "time restarted") {
		t.Errorf("restart exit %d out %q", code, out.String())
	}

	// the mesh's own reason reaches the operator
	errw.Reset()
	if code := cmdUpstream([]string{"restart", "nobody"}, &out, &errw); code != 1 || !strings.Contains(errw.String(), `no upstream named "nobody"`) {
		t.Errorf("unknown upstream: exit %d, stderr %q", code, errw.String())
	}

	// usage errors do not reach the mesh
	n := len(*calls)
	for _, args := range [][]string{{}, {"restart"}, {"accept"}, {"frobnicate"}} {
		if code := cmdUpstream(args, &out, &errw); code != 1 {
			t.Errorf("upstream %v exit %d, want 1", args, code)
		}
	}
	if len(*calls) != n {
		t.Errorf("a usage error called the mesh: %v", (*calls)[n:])
	}
}

func TestPinCommands(t *testing.T) {
	calls, bodies := fakeMesh(t)
	var out, errw bytes.Buffer

	if code := cmdPin([]string{"list"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "time.now") || !strings.Contains(out.String(), "changed") {
		t.Fatalf("pin list exit %d:\n%s", code, out.String())
	}

	if code := cmdPin([]string{"accept", "time.now", "time.ago"}, &out, &errw); code != 0 {
		t.Fatalf("accept by name exit %d: %s", code, errw.String())
	}
	var sent map[string]any
	json.Unmarshal([]byte((*bodies)[len(*bodies)-1]), &sent)
	if tools, _ := sent["tools"].([]any); len(tools) != 2 || sent["server"] != nil || sent["by"] == "" {
		t.Errorf("accept by name sent %v", sent)
	}

	if code := cmdPin([]string{"accept", "--server", "time"}, &out, &errw); code != 0 {
		t.Fatalf("accept by server exit %d", code)
	}
	sent = nil
	json.Unmarshal([]byte((*bodies)[len(*bodies)-1]), &sent)
	if sent["server"] != "time" || sent["tools"] != nil {
		t.Errorf("accept by server sent %v", sent)
	}

	// accepting nothing in particular is refused here, not sent
	n := len(*calls)
	for _, args := range [][]string{{"accept"}, {"accept", "--server"}, {"accept", "--all"}} {
		if code := cmdPin(args, &out, &errw); code != 1 {
			t.Errorf("pin %v exit %d, want 1", args, code)
		}
	}
	if len(*calls) != n {
		t.Errorf("a usage error called the mesh: %v", (*calls)[n:])
	}
}
