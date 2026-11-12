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
// left alone, calls in flight included. One that changed or disappeared is
// closed first: its calls in flight fail with "client closed", and its tools
// leave the registry until the new declaration has connected. Catalogue
// pinning observes every (re)connection, so a reload never makes a new or
// changed tool callable by itself.
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
	epoch   map[string]uint64        // bumped whenever a server is stopped
	stops   map[string]chan struct{} // stops the retry loop of a server
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
	}
}

// UpstreamDiff says what a reload did, by server name.
type UpstreamDiff struct {
	Added     []string `json:"added"`
	Changed   []string `json:"changed"`
	Removed   []string `json:"removed"`
	Unchanged []string `json:"unchanged"`
}

func (d UpstreamDiff) empty() bool {
	return len(d.Added)+len(d.Changed)+len(d.Removed) == 0
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
	d := UpstreamDiff{Added: []string{}, Changed: []string{}, Removed: []string{}, Unchanged: []string{}}
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

// Apply brings the running upstreams to a new list of declarations and
// returns once every touched server has connected or entered its retry loop.
func (u *upstreams) Apply(servers []config.MCPServerConfig) (UpstreamDiff, error) {
	seen := map[string]bool{}
	for _, sc := range servers {
		if sc.Name == "" {
			return UpstreamDiff{}, fmt.Errorf("mcp_servers: a server has no name")
		}
		if seen[sc.Name] {
			return UpstreamDiff{}, fmt.Errorf("mcp_servers: duplicate name %q", sc.Name)
		}
		seen[sc.Name] = true
		if _, err := newClient(sc); err != nil {
			return UpstreamDiff{}, fmt.Errorf("mcp_servers %q: %w", sc.Name, err)
		}
	}

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

// Reload reads the declarations again and applies them. A config that does
// not load leaves the running upstreams as they are.
func (u *upstreams) Reload() (any, error) {
	if u.load == nil {
		return nil, fmt.Errorf("no config file to reload from")
	}
	servers, err := u.load()
	if err != nil {
		return nil, err
	}
	return u.Apply(servers)
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
