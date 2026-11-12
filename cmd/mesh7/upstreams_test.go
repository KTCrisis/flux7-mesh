package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/mcp"
	"github.com/KTCrisis/flux7-mesh/registry"
)

// fakeUpstream is a streamable-HTTP MCP server offering the given tools. It
// counts handshakes, can refuse its first requests, and can be slow to answer.
type fakeUpstream struct {
	*httptest.Server
	inits   atomic.Int64
	refuse  atomic.Int64 // requests still to answer with 503
	latency atomic.Int64 // nanoseconds slept before answering initialize
}

func newFakeUpstream(t *testing.T, tools ...string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.refuse.Load() > 0 {
			f.refuse.Add(-1)
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any = map[string]any{}
		switch req.Method {
		case "initialize":
			f.inits.Add(1)
			time.Sleep(time.Duration(f.latency.Load()))
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake"}}
		case "tools/list":
			list := []any{}
			for _, name := range tools {
				list = append(list, map[string]any{"name": name, "description": name, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
			}
			result = map[string]any{"tools": list}
		case "tools/call":
			result = map[string]any{"content": []any{}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) as(name string) config.MCPServerConfig {
	return config.MCPServerConfig{Name: name, Transport: "streamable-http", URL: f.URL}
}

func testUpstreams(t *testing.T) *upstreams {
	t.Helper()
	u := newUpstreams(mcp.NewManager(), registry.New(), nil)
	u.connectTimeout = 3 * time.Second
	u.retryFirst = 20 * time.Millisecond
	u.retryMax = 40 * time.Millisecond
	t.Cleanup(u.Close)
	return u
}

func toolNames(u *upstreams, server string) map[string]bool {
	out := map[string]bool{}
	for _, tool := range u.reg.ByServer(server) {
		out[tool.Name] = true
	}
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never happened: %s", what)
}

func TestUpstreamsApplyTouchesOnlyWhatChanged(t *testing.T) {
	a, b := newFakeUpstream(t, "ping"), newFakeUpstream(t, "search")
	u := testUpstreams(t)
	u.StartAll([]config.MCPServerConfig{a.as("a")})
	if !toolNames(u, "a")["a.ping"] {
		t.Fatalf("a.ping not registered at start: %v", toolNames(u, "a"))
	}
	clientA := u.mgr.Get("a")
	gen := u.Generation()

	// Add b: a is left alone, not even reconnected.
	d, err := u.Apply([]config.MCPServerConfig{a.as("a"), b.as("b")})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 1 || d.Added[0] != "b" || len(d.Unchanged) != 1 || len(d.Changed)+len(d.Removed) != 0 {
		t.Errorf("diff = %+v, want b added and a unchanged", d)
	}
	if u.mgr.Get("a") != clientA || a.inits.Load() != 1 {
		t.Errorf("an unchanged upstream was reconnected (handshakes: %d)", a.inits.Load())
	}
	if !toolNames(u, "b")["b.search"] {
		t.Errorf("b.search not registered: %v", toolNames(u, "b"))
	}
	if u.Generation() == gen {
		t.Error("the catalogue changed but its generation did not")
	}

	// The same list again changes nothing.
	gen = u.Generation()
	if d, _ := u.Apply([]config.MCPServerConfig{a.as("a"), b.as("b")}); !d.empty() || u.Generation() != gen {
		t.Errorf("a second identical apply did something: %+v", d)
	}

	// Redeclare a onto another server: its old tools go, the new ones come.
	a2 := newFakeUpstream(t, "pong")
	d, _ = u.Apply([]config.MCPServerConfig{a2.as("a"), b.as("b")})
	if len(d.Changed) != 1 || d.Changed[0] != "a" {
		t.Errorf("diff = %+v, want a changed", d)
	}
	if names := toolNames(u, "a"); names["a.ping"] || !names["a.pong"] {
		t.Errorf("after the change a offers %v, want only a.pong", names)
	}
	if b.inits.Load() != 1 {
		t.Errorf("b was reconnected while a changed (handshakes: %d)", b.inits.Load())
	}

	// Drop a: its tools and its client are gone, calls no longer reach it.
	d, _ = u.Apply([]config.MCPServerConfig{b.as("b")})
	if len(d.Removed) != 1 || d.Removed[0] != "a" {
		t.Errorf("diff = %+v, want a removed", d)
	}
	if len(toolNames(u, "a")) != 0 || u.mgr.Get("a") != nil {
		t.Error("a removed upstream still has tools or a client")
	}
	if !toolNames(u, "b")["b.search"] {
		t.Error("removing a took b's tools away")
	}
}

func TestUpstreamsApplyRefusesABadListWhole(t *testing.T) {
	a := newFakeUpstream(t, "ping")
	u := testUpstreams(t)
	u.StartAll([]config.MCPServerConfig{a.as("a")})

	for name, list := range map[string][]config.MCPServerConfig{
		"duplicate name":    {a.as("a"), a.as("a")},
		"no name":           {a.as("")},
		"unknown transport": {{Name: "x", Transport: "carrier-pigeon"}},
	} {
		if _, err := u.Apply(list); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if !toolNames(u, "a")["a.ping"] || a.inits.Load() != 1 {
			t.Fatalf("%s: a refused list changed the running upstreams", name)
		}
	}
}

func TestUpstreamsRetryThenConnect(t *testing.T) {
	a := newFakeUpstream(t, "ping")
	a.refuse.Store(3)
	u := testUpstreams(t)
	u.StartAll([]config.MCPServerConfig{a.as("a")})

	if len(toolNames(u, "a")) != 0 {
		t.Fatal("tools registered from an upstream that refused the handshake")
	}
	eventually(t, "the upstream connects once it answers", func() bool { return toolNames(u, "a")["a.ping"] })
	statuses := u.mgr.ServerStatuses().([]mcp.ServerStatus)
	if len(statuses) != 1 || statuses[0].Status == "retrying" {
		t.Errorf("statuses after the retry = %+v, want one connected upstream", statuses)
	}
}

func TestUpstreamsRemovedWhileConnectingStaysRemoved(t *testing.T) {
	a := newFakeUpstream(t, "ping")
	a.latency.Store(int64(300 * time.Millisecond))
	u := testUpstreams(t)

	done := make(chan struct{})
	go func() { u.StartAll([]config.MCPServerConfig{a.as("a")}); close(done) }()
	eventually(t, "the handshake starts", func() bool { return a.inits.Load() == 1 })
	if _, err := u.Apply(nil); err != nil { // the declaration is withdrawn mid-handshake
		t.Fatal(err)
	}
	<-done
	if len(toolNames(u, "a")) != 0 || u.mgr.Get("a") != nil {
		t.Error("an upstream removed while it was connecting came back")
	}
}

func TestUpstreamsRestart(t *testing.T) {
	a := newFakeUpstream(t, "ping")
	u := testUpstreams(t)
	u.StartAll([]config.MCPServerConfig{a.as("a")})
	before := u.mgr.Get("a")

	if err := u.Restart("a"); err != nil {
		t.Fatal(err)
	}
	if a.inits.Load() != 2 || u.mgr.Get("a") == before || !toolNames(u, "a")["a.ping"] {
		t.Errorf("restart: handshakes %d, tools %v", a.inits.Load(), toolNames(u, "a"))
	}
	if err := u.Restart("nobody"); err == nil {
		t.Error("restarting an unknown upstream answered no error")
	}
}

func TestUpstreamsReloadKeepsWhatRunsWhenTheConfigFails(t *testing.T) {
	a := newFakeUpstream(t, "ping")
	u := testUpstreams(t)
	u.StartAll([]config.MCPServerConfig{a.as("a")})
	u.load = func() ([]config.MCPServerConfig, error) { return nil, io.ErrUnexpectedEOF }

	if _, err := u.Reload(); err == nil {
		t.Fatal("a config that does not load was applied")
	}
	if !toolNames(u, "a")["a.ping"] {
		t.Error("a failed reload dropped the running upstream")
	}
}

// A declaration that appears or changes while the mesh runs starts nothing
// until an operator accepts it; the one it would replace keeps running.
func TestUpstreamsProposeHoldsNewAndChangedDeclarations(t *testing.T) {
	a, b, a2 := newFakeUpstream(t, "ping"), newFakeUpstream(t, "search"), newFakeUpstream(t, "pong")
	u := testUpstreams(t)
	u.StartAll([]config.MCPServerConfig{a.as("a")})
	gen := u.Generation()

	// The hot path is Reload: it must go through Propose, never Apply.
	changedA := a2.as("a")
	changedA.Env = map[string]string{"API_TOKEN": "s3cret"}
	u.load = func() ([]config.MCPServerConfig, error) {
		return []config.MCPServerConfig{changedA, b.as("b")}, nil
	}
	res, err := u.Reload()
	if err != nil {
		t.Fatal(err)
	}
	d := res.(UpstreamDiff)
	if len(d.Held) != 2 || d.Held[0] != "a" || d.Held[1] != "b" || len(d.Added)+len(d.Changed)+len(d.Removed) != 0 {
		t.Fatalf("diff = %+v, want a and b held and nothing applied", d)
	}
	if b.inits.Load() != 0 || a2.inits.Load() != 0 {
		t.Fatalf("a held declaration was started (handshakes: b %d, new a %d)", b.inits.Load(), a2.inits.Load())
	}
	if !toolNames(u, "a")["a.ping"] || a.inits.Load() != 1 || len(toolNames(u, "b")) != 0 {
		t.Errorf("the running declaration of a was disturbed, or b has tools: a %v b %v", toolNames(u, "a"), toolNames(u, "b"))
	}
	if u.Generation() != gen {
		t.Error("the catalogue generation moved although nothing was started")
	}

	// The operator sees what would run, without the values of env.
	held := u.Held().([]HeldUpstream)
	if len(held) != 2 || !held[0].Replaces || held[1].Replaces || held[0].URL != a2.URL {
		t.Errorf("held = %+v", held)
	}
	if len(held[0].EnvKeys) != 1 || held[0].EnvKeys[0] != "API_TOKEN" {
		t.Errorf("env keys = %v, want the key without its value", held[0].EnvKeys)
	}
	if raw, _ := json.Marshal(held); bytes.Contains(raw, []byte("s3cret")) {
		t.Error("a held declaration discloses an env value")
	}

	// Accept starts it, replacing what ran.
	if err := u.Accept("a"); err != nil {
		t.Fatal(err)
	}
	if names := toolNames(u, "a"); names["a.ping"] || !names["a.pong"] || a2.inits.Load() != 1 {
		t.Errorf("after accept a offers %v", names)
	}
	if b.inits.Load() != 0 {
		t.Error("accepting a started b")
	}
	if err := u.Accept("b"); err != nil || !toolNames(u, "b")["b.search"] {
		t.Errorf("accept b: %v, tools %v", err, toolNames(u, "b"))
	}
	if err := u.Accept("b"); err == nil {
		t.Error("accepting twice answered no error")
	}
	if len(u.Held().([]HeldUpstream)) != 0 {
		t.Error("accepted declarations are still held")
	}
}

func TestUpstreamsProposeWithdrawnOrRevertedIsNoLongerHeld(t *testing.T) {
	a, b, a2 := newFakeUpstream(t, "ping"), newFakeUpstream(t, "search"), newFakeUpstream(t, "pong")
	u := testUpstreams(t)
	u.StartAll([]config.MCPServerConfig{a.as("a")})

	u.Propose([]config.MCPServerConfig{a2.as("a"), b.as("b")})
	if n := len(u.Held().([]HeldUpstream)); n != 2 {
		t.Fatalf("%d held, want 2", n)
	}
	// a set back to what runs, b withdrawn
	d, _ := u.Propose([]config.MCPServerConfig{a.as("a")})
	if len(d.Held) != 0 || len(u.Held().([]HeldUpstream)) != 0 {
		t.Errorf("still held after revert and withdrawal: %+v", d)
	}
	if err := u.Accept("b"); err == nil {
		t.Error("a withdrawn declaration could still be accepted")
	}
	if a.inits.Load() != 1 || b.inits.Load() != 0 || a2.inits.Load() != 0 {
		t.Error("proposing and withdrawing started or restarted something")
	}
}

// Removing a declaration needs no acceptance: it can only restrict.
func TestUpstreamsProposeRemovesAtOnce(t *testing.T) {
	a, b := newFakeUpstream(t, "ping"), newFakeUpstream(t, "search")
	u := testUpstreams(t)
	u.StartAll([]config.MCPServerConfig{a.as("a"), b.as("b")})

	d, err := u.Propose([]config.MCPServerConfig{b.as("b")})
	if err != nil || len(d.Removed) != 1 || d.Removed[0] != "a" {
		t.Fatalf("diff = %+v err %v, want a removed", d, err)
	}
	if len(toolNames(u, "a")) != 0 || u.mgr.Get("a") != nil || !toolNames(u, "b")["b.search"] {
		t.Error("removal not applied at once, or it touched b")
	}
}
