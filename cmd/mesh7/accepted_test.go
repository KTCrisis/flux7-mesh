package main

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
	"github.com/KTCrisis/flux7-mesh/storage"
)

func stateDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// boot is one life of the daemon on a given state database.
func boot(t *testing.T, db *sql.DB, servers ...config.MCPServerConfig) (*upstreams, []string) {
	t.Helper()
	u := testUpstreams(t)
	store, err := newAcceptedStore(db)
	if err != nil {
		t.Fatal(err)
	}
	u.accepted = store
	held, err := u.StartDeclared(servers)
	if err != nil {
		t.Fatal(err)
	}
	return u, held
}

// A restart is not a go-ahead: a service comes back after a reboot or a crash
// with nobody watching. Only what an operator accepted is started.
func TestRestartStartsOnlyAcceptedDeclarations(t *testing.T) {
	db := stateDB(t)
	a, b, evil := newFakeUpstream(t, "ping"), newFakeUpstream(t, "search"), newFakeUpstream(t, "pwn")

	// First start ever: the file is trusted, once.
	u1, held := boot(t, db, a.as("a"))
	if len(held) != 0 || !toolNames(u1, "a")["a.ping"] {
		t.Fatalf("first start: held %v, tools %v", held, toolNames(u1, "a"))
	}

	// While it runs, the file gains a server and a's declaration is rewritten.
	// Both are held; then the machine reboots before anyone accepted anything.
	u1.Propose([]config.MCPServerConfig{evil.as("a"), b.as("b")})
	u1.Close()

	u2, held := boot(t, db, evil.as("a"), b.as("b"))
	if len(held) != 2 || held[0] != "a" || held[1] != "b" {
		t.Fatalf("restart: held %v, want a and b", held)
	}
	if evil.inits.Load() != 0 || b.inits.Load() != 0 {
		t.Fatalf("a restart started declarations nobody accepted (handshakes: rewritten a %d, b %d)", evil.inits.Load(), b.inits.Load())
	}
	if len(toolNames(u2, "a"))+len(toolNames(u2, "b")) != 0 {
		t.Error("held declarations have tools after a restart")
	}
	if n := len(u2.Held().([]HeldUpstream)); n != 2 {
		t.Errorf("%d held after restart, want 2", n)
	}

	// The operator accepts b. That survives the next restart; a is still held.
	if err := u2.Accept("b"); err != nil {
		t.Fatal(err)
	}
	u2.Close()
	u3, held := boot(t, db, evil.as("a"), b.as("b"))
	if len(held) != 1 || held[0] != "a" || !toolNames(u3, "b")["b.search"] {
		t.Errorf("third start: held %v, b tools %v; want a held and b started", held, toolNames(u3, "b"))
	}
	if evil.inits.Load() != 0 {
		t.Error("the rewritten declaration of a ended up started")
	}

	// Setting a back to the accepted declaration needs no new acceptance.
	u3.Close()
	u4, held := boot(t, db, a.as("a"), b.as("b"))
	if len(held) != 0 || !toolNames(u4, "a")["a.ping"] {
		t.Errorf("fourth start with the accepted declarations: held %v", held)
	}
}

// Emptying the list must not bring back the trust of a first start.
func TestRestartAfterEverythingWasRemovedIsNotAFirstUse(t *testing.T) {
	db := stateDB(t)
	a, evil := newFakeUpstream(t, "ping"), newFakeUpstream(t, "pwn")

	u1, _ := boot(t, db, a.as("a"))
	u1.Propose(nil) // every declaration removed from the file: applied at once
	u1.Close()

	u2, held := boot(t, db, evil.as("x"))
	if len(held) != 1 || evil.inits.Load() != 0 || len(toolNames(u2, "x")) != 0 {
		t.Fatalf("after an emptied list a new declaration was trusted: held %v, handshakes %d", held, evil.inits.Load())
	}
	// and a, removed then declared again, is new too
	u2.Close()
	_, held = boot(t, db, a.as("a"))
	if len(held) != 1 || a.inits.Load() != 1 {
		t.Errorf("a removed then declared again: held %v, handshakes %d; want held, not restarted", held, a.inits.Load())
	}
}

func TestDeclFingerprint(t *testing.T) {
	base := config.MCPServerConfig{Name: "s", Transport: "stdio", Command: "uvx", Args: []string{"pkg@1"},
		Env: map[string]string{"A": "1"}, Headers: map[string]string{"Authorization": "Bearer one"}}
	same := base
	same.Headers = map[string]string{"Authorization": "Bearer two"} // a rotated token
	if declFingerprint(base) != declFingerprint(same) {
		t.Error("a header value changes the fingerprint: a rotated token would hold the server at every start")
	}
	for name, mutate := range map[string]func(*config.MCPServerConfig){
		"command":   func(c *config.MCPServerConfig) { c.Command = "sh" },
		"argument":  func(c *config.MCPServerConfig) { c.Args = []string{"pkg@2"} },
		"env value": func(c *config.MCPServerConfig) { c.Env = map[string]string{"A": "2"} },
		"env key": func(c *config.MCPServerConfig) {
			c.Env = map[string]string{"A": "1", "NODE_OPTIONS": "--require /tmp/x.js"}
		},
		"url":        func(c *config.MCPServerConfig) { c.URL = "https://elsewhere" },
		"transport":  func(c *config.MCPServerConfig) { c.Transport = "sse" },
		"header key": func(c *config.MCPServerConfig) { c.Headers = map[string]string{"X-Other": "v"} },
	} {
		changed := base
		mutate(&changed)
		if declFingerprint(changed) == declFingerprint(base) {
			t.Errorf("a changed %s keeps the fingerprint", name)
		}
	}
}

// Without a database nothing is remembered: every start is a first use.
func TestNoStorageTrustsEveryStart(t *testing.T) {
	a := newFakeUpstream(t, "ping")
	u, held := boot(t, nil, a.as("a"))
	if len(held) != 0 || !toolNames(u, "a")["a.ping"] {
		t.Errorf("held %v", held)
	}
	u.Close()
	if _, held := boot(t, nil, a.as("a")); len(held) != 0 {
		t.Errorf("second start without storage held %v", held)
	}
}
