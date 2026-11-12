package main

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/mcp"
	"github.com/KTCrisis/flux7-mesh/pin"
	"github.com/KTCrisis/flux7-mesh/registry"
)

// upstreams owns the life of the upstream MCP servers: it connects them at
// start, retries the ones that are down, and applies a changed `mcp_servers`
// list to the running mesh by touching only the servers that changed.
//
// A server is identified by its name. One whose declaration is unchanged is
// left alone, calls in flight included.
//
// A declaration is a command line: starting it runs a program as the mesh.
// The file that holds it can be written by anything that can write files,
// agents included, so a declaration that appears or changes while the mesh
// runs is not started. It is held until an operator accepts it, and the
// declaration it would replace keeps running meanwhile. Only the declarations
// read when the daemon starts are trusted as they are: starting the daemon is
// an operator's act. A declaration that disappears is applied at once, since
// removing a server can only restrict.
//
// When a server is replaced or removed its calls in flight fail with "client
// closed". Catalogue pinning observes every (re)connection.
type upstreams struct {
	mgr  *mcp.Manager
	reg  *registry.Registry
	pins *pin.Store // nil unless pin_tools

	// load reads the declared servers again, from the config file.
	load func() ([]config.MCPServerConfig, error)

	connectTimeout time.Duration
	retryFirst     time.Duration
	retryMax       time.Duration

	mu      sync.Mutex
	current map[string]config.MCPServerConfig
	epoch   map[string]uint64                 // bumped whenever a server is stopped
	stops   map[string]chan struct{}          // stops the retry loop of a server
	held    map[string]config.MCPServerConfig // declarations awaiting an operator
	closed  bool

	// generation counts the changes of the tool catalogue caused by upstreams
	// coming, going or reconnecting. Clients compare it to learn that the
	// tool list is worth reading again.
	generation atomic.Uint64
}

func newUpstreams(mgr *mcp.Manager, reg *registry.Registry, pins *pin.Store) *upstreams {
	return &upstreams{
		mgr: mgr, reg: reg, pins: pins,
		connectTimeout: 30 * time.Second,
		retryFirst:     5 * time.Second,
		retryMax:       5 * time.Minute,
		current:        map[string]config.MCPServerConfig{},
		epoch:          map[string]uint64{},
		stops:          map[string]chan struct{}{},
		held:           map[string]config.MCPServerConfig{},
	}
}

// UpstreamDiff says what a reload did, by server name.
type UpstreamDiff struct {
	Added     []string `json:"added"`
	Changed   []string `json:"changed"`
	Removed   []string `json:"removed"`
	Unchanged []string `json:"unchanged"`
	// Held names the declarations that are new or changed and wait for an
	// operator: nothing was started for them.
	Held []string `json:"held"`
	// newlyHeld is the part of Held this proposal added or altered.
	newlyHeld []string
}

func (d UpstreamDiff) empty() bool {
	return len(d.Added)+len(d.Changed)+len(d.Removed) == 0
}

// HeldUpstream is a declaration awaiting acceptance, shown to the operator who
// decides: what would run, and whether it replaces a server that runs now.
// Values of env and headers are not shown, they are where secrets live.
type HeldUpstream struct {
	Name       string   `json:"name"`
	Transport  string   `json:"transport"`
	Command    string   `json:"command,omitempty"`
	Args       []string `json:"args,omitempty"`
	URL        string   `json:"url,omitempty"`
	EnvKeys    []string `json:"env_keys,omitempty"`
	HeaderKeys []string `json:"header_keys,omitempty"`
	Replaces   bool     `json:"replaces"` // a server of this name runs with another declaration
}

func newClient(sc config.MCPServerConfig) (*mcp.MCPClient, error) {
	var client *mcp.MCPClient
	switch sc.Transport {
	case "stdio":
		client = mcp.NewStdioClient(sc.Name, sc.Command, sc.Args, sc.Env)
	case "sse":
		client = mcp.NewSSEClient(sc.Name, sc.URL, sc.Headers)
	case "streamable-http":
		client = mcp.NewStreamableHTTPClient(sc.Name, sc.URL, sc.Headers)
	default:
		return nil, fmt.Errorf("unsupported MCP transport %q", sc.Transport)
	}
	client.ForwardIdentity = sc.ForwardIdentity
	return client, nil
}

// connect builds the client of one upstream, connects it and registers its
// tools. epoch is the server's epoch when the attempt was decided: if the
// server was stopped or redeclared while the connection was being made, the
// fresh client is closed and nothing is registered.
func (u *upstreams) connect(ctx context.Context, sc config.MCPServerConfig, epoch uint64) error {
	client, err := newClient(sc)
	if err != nil {
		return err
	}
	if err := client.Connect(ctx); err != nil { // Connect closes the client on failure
		return err
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.epoch[sc.Name] != epoch {
		client.Close()
		return nil // superseded: not a failure, and nothing to retry
	}
	u.mgr.Add(client)
	defs := convertMCPTools(client.Tools())
	u.reg.RemoveByServer(sc.Name) // a tool the server no longer offers must not linger
	u.reg.LoadMCP(sc.Name, defs)
	if u.pins != nil {
		if err := u.pins.Observe(sc.Name, u.reg.ByServer(sc.Name)); err != nil {
			slog.Error("pin catalogue", "server", sc.Name, "error", err)
		}
		for _, v := range u.pins.Pending() {
			if v.Server == sc.Name {
				slog.Warn("upstream tool held back until accepted", "tool", v.Tool, "status", v.Status)
			}
		}
	}
	for _, d := range defs {
		slog.Info("  MCP tool registered", "name", sc.Name+"."+d.Name, "server", sc.Name)
	}
	u.generation.Add(1)
	return nil
}

// start connects one server, and hands it to the retry loop when it does not
// answer. The caller holds no lock.
func (u *upstreams) start(sc config.MCPServerConfig) {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	u.current[sc.Name] = sc
	epoch := u.epoch[sc.Name]
	stop := make(chan struct{})
	u.stops[sc.Name] = stop
	u.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), u.connectTimeout)
	err := u.connect(ctx, sc, epoch)
	cancel()
	if err == nil {
		return
	}
	slog.Error("failed to connect MCP server, retrying in the background", "name", sc.Name, "error", err)

	u.mu.Lock()
	superseded := u.closed || u.epoch[sc.Name] != epoch
	if !superseded {
		u.mgr.SetRetrying(sc.Name, sc.Transport, err)
	}
	u.mu.Unlock()
	if superseded {
		return
	}
	go mcp.RetryConnect(stop, sc.Name, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), u.connectTimeout)
		defer cancel()
		return u.connect(ctx, sc, epoch)
	}, u.retryFirst, u.retryMax, func(err error) {
		u.mu.Lock()
		if !u.closed && u.epoch[sc.Name] == epoch {
			u.mgr.SetRetrying(sc.Name, sc.Transport, err)
		}
		u.mu.Unlock()
	})
}

// stop takes one server out: its retry loop ends, its client is closed, its
// tools leave the registry.
func (u *upstreams) stop(name string) {
	u.mu.Lock()
	u.epoch[name]++
	if ch, ok := u.stops[name]; ok {
		close(ch)
		delete(u.stops, name)
	}
	delete(u.current, name)
	client := u.mgr.Remove(name)
	u.reg.RemoveByServer(name)
	u.generation.Add(1)
	u.mu.Unlock()
	if client != nil {
		client.Close()
	}
}

// StartAll connects the servers declared at start, in parallel, and returns
// when each has either connected or been handed to its retry loop.
func (u *upstreams) StartAll(servers []config.MCPServerConfig) {
	var wg sync.WaitGroup
	for _, sc := range servers {
		wg.Add(1)
		go func(sc config.MCPServerConfig) {
			defer wg.Done()
			u.start(sc)
		}(sc)
	}
	wg.Wait()
}

// diff compares the running declarations with a new list.
func (u *upstreams) diff(servers []config.MCPServerConfig) (UpstreamDiff, map[string]config.MCPServerConfig) {
	want := make(map[string]config.MCPServerConfig, len(servers))
	for _, sc := range servers {
		want[sc.Name] = sc
	}
	d := UpstreamDiff{Added: []string{}, Changed: []string{}, Removed: []string{}, Unchanged: []string{}, Held: []string{}}
	u.mu.Lock()
	for name, sc := range want {
		old, ok := u.current[name]
		switch {
		case !ok:
			d.Added = append(d.Added, name)
		case !reflect.DeepEqual(old, sc):
			d.Changed = append(d.Changed, name)
		default:
			d.Unchanged = append(d.Unchanged, name)
		}
	}
	for name := range u.current {
		if _, ok := want[name]; !ok {
			d.Removed = append(d.Removed, name)
		}
	}
	u.mu.Unlock()
	sort.Strings(d.Added)
	sort.Strings(d.Changed)
	sort.Strings(d.Removed)
	sort.Strings(d.Unchanged)
	return d, want
}

func checkDeclarations(servers []config.MCPServerConfig) error {
	seen := map[string]bool{}
	for _, sc := range servers {
		if sc.Name == "" {
			return fmt.Errorf("mcp_servers: a server has no name")
		}
		if seen[sc.Name] {
			return fmt.Errorf("mcp_servers: duplicate name %q", sc.Name)
		}
		seen[sc.Name] = true
		if _, err := newClient(sc); err != nil {
			return fmt.Errorf("mcp_servers %q: %w", sc.Name, err)
		}
	}
	return nil
}

// Apply brings the running upstreams to a list of declarations the caller
// vouches for, and returns once every touched server has connected or entered
// its retry loop. A list read from the config file of a running mesh is not
// such a list: that one goes through Propose.
func (u *upstreams) Apply(servers []config.MCPServerConfig) (UpstreamDiff, error) {
	if err := checkDeclarations(servers); err != nil {
		return UpstreamDiff{}, err
	}
	u.mu.Lock()
	u.held = map[string]config.MCPServerConfig{} // the whole list is trusted here
	u.mu.Unlock()

	d, want := u.diff(servers)
	for _, name := range d.Removed {
		u.stop(name)
	}
	for _, name := range d.Changed {
		u.stop(name)
	}
	var wg sync.WaitGroup
	for _, name := range append(append([]string{}, d.Added...), d.Changed...) {
		wg.Add(1)
		go func(sc config.MCPServerConfig) {
			defer wg.Done()
			u.start(sc)
		}(want[name])
	}
	wg.Wait()
	if !d.empty() {
		slog.Info("upstream hot-reload", "added", d.Added, "changed", d.Changed, "removed", d.Removed, "unchanged", len(d.Unchanged))
	}
	return d, nil
}

// Propose compares a list of declarations nobody vouched for with what runs.
// Servers that disappeared are stopped. Declarations that are new or changed
// are held, not started: the running declaration of a changed server stays in
// service until Accept. A held declaration that is withdrawn, or set back to
// what runs, stops being held.
func (u *upstreams) Propose(servers []config.MCPServerConfig) (UpstreamDiff, error) {
	if err := checkDeclarations(servers); err != nil {
		return UpstreamDiff{}, err
	}
	d, want := u.diff(servers)
	for _, name := range d.Removed {
		u.stop(name)
	}

	u.mu.Lock()
	held := map[string]config.MCPServerConfig{}
	for _, name := range append(append([]string{}, d.Added...), d.Changed...) {
		held[name] = want[name]
		d.Held = append(d.Held, name)
	}
	fresh := []string{} // held now and not held, or held differently, before
	for name, sc := range held {
		if old, ok := u.held[name]; !ok || !reflect.DeepEqual(old, sc) {
			fresh = append(fresh, name)
		}
	}
	u.held = held
	u.mu.Unlock()

	sort.Strings(d.Held)
	sort.Strings(fresh)
	// nothing was started for these: the diff must not say they were applied
	d.Added, d.Changed = []string{}, []string{}
	if len(d.Removed) > 0 {
		slog.Info("upstream hot-reload", "removed", d.Removed, "unchanged", len(d.Unchanged))
	}
	d.newlyHeld = fresh
	if len(fresh) > 0 {
		slog.Warn("upstream declarations held until accepted: nothing was started", "held", fresh,
			"accept", "mesh7 upstream accept <name>")
	}
	return d, nil
}

// Held lists the declarations awaiting acceptance, by name.
func (u *upstreams) Held() any {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]HeldUpstream, 0, len(u.held))
	for name, sc := range u.held {
		h := HeldUpstream{Name: name, Transport: sc.Transport, Command: sc.Command, Args: sc.Args, URL: sc.URL}
		for k := range sc.Env {
			h.EnvKeys = append(h.EnvKeys, k)
		}
		for k := range sc.Headers {
			h.HeaderKeys = append(h.HeaderKeys, k)
		}
		sort.Strings(h.EnvKeys)
		sort.Strings(h.HeaderKeys)
		_, h.Replaces = u.current[name]
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Accept starts a held declaration: the operator has read it and vouches for
// it. The server it replaces, if any, is closed first.
func (u *upstreams) Accept(name string) error {
	u.mu.Lock()
	sc, ok := u.held[name]
	if ok {
		delete(u.held, name)
	}
	_, running := u.current[name]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("no held declaration named %q", name)
	}
	if running {
		u.stop(name)
	}
	u.start(sc)
	slog.Info("upstream declaration accepted", "name", name, "replaced", running)
	return nil
}

// Reload reads the declarations again from the config file and proposes them.
// A config that does not load leaves the running upstreams as they are.
func (u *upstreams) Reload() (any, error) {
	if u.load == nil {
		return nil, fmt.Errorf("no config file to reload from")
	}
	servers, err := u.load()
	if err != nil {
		return nil, err
	}
	return u.Propose(servers)
}

// Restart closes one server and connects it again from its current
// declaration: the way to pick up a new build of the same command.
func (u *upstreams) Restart(name string) error {
	u.mu.Lock()
	sc, ok := u.current[name]
	u.mu.Unlock()
	if !ok {
		return fmt.Errorf("no upstream named %q", name)
	}
	u.stop(name)
	u.start(sc)
	return nil
}

// Generation is the current count of catalogue changes.
func (u *upstreams) Generation() uint64 { return u.generation.Load() }

// Close stops every retry loop and closes every client.
func (u *upstreams) Close() {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return
	}
	u.closed = true
	for name, ch := range u.stops {
		close(ch)
		delete(u.stops, name)
	}
	u.mu.Unlock()
	u.mgr.CloseAll()
}
