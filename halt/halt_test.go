package halt

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/grant"
	"github.com/KTCrisis/flux7-mesh/storage"
)

func TestScopes(t *testing.T) {
	s, _ := NewStore(nil)

	if _, _, err := s.Start(ScopeAgent, "", "", "op"); err == nil {
		t.Fatal("an agent halt without target must be refused")
	}
	if _, _, err := s.Start("planet", "x", "", "op"); err == nil {
		t.Fatal("an unknown scope must be refused")
	}

	h, created, err := s.Start(ScopeAgent, "scout7", "loops on search", "op")
	if err != nil || !created {
		t.Fatalf("start: %v %v", created, err)
	}
	if got := s.Match("scout7", "s1"); got == nil || got.ID != h.ID {
		t.Fatal("scout7 must be halted")
	}
	if s.Match("claude", "s1") != nil {
		t.Fatal("another agent must not be halted")
	}

	sess, _, _ := s.Start(ScopeSession, "s2", "", "op")
	if got := s.Match("claude", "s2"); got == nil || got.ID != sess.ID {
		t.Fatal("session s2 must be halted")
	}
	if s.Match("claude", "") != nil {
		t.Fatal("a call without session must not match a session halt")
	}
}

func TestBroadestHaltWinsAndStartIsIdempotent(t *testing.T) {
	s, _ := NewStore(nil)
	s.Start(ScopeAgent, "claude", "", "op")
	all, _, _ := s.Start(ScopeAll, "ignored", "incident", "op")
	if all.Target != "" {
		t.Fatal("a global halt has no target")
	}
	if got := s.Match("claude", ""); got.ID != all.ID {
		t.Fatal("the global halt must be reported first")
	}
	again, created, _ := s.Start(ScopeAll, "", "", "op2")
	if created || again.ID != all.ID {
		t.Fatal("starting the same halt twice must return the active one")
	}
}

func TestResumeByPrefixKeepsRevokedGrants(t *testing.T) {
	s, _ := NewStore(nil)
	h, _, _ := s.Start(ScopeAgent, "claude", "", "op")
	g := grant.Grant{ID: "g1", Agent: "claude", Tools: "*", ExpiresAt: time.Now().Add(time.Hour)}
	s.RecordRevoked(h.ID, []grant.Grant{g})

	resumed, err := s.Resume(h.ID[:6], "op")
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.RevokedGrants) != 1 || resumed.RevokedGrants[0].ID != "g1" {
		t.Fatal("resume must hand back the revoked grants")
	}
	if s.Match("claude", "") != nil {
		t.Fatal("a resumed halt must stop applying")
	}
	if _, err := s.Resume(h.ID, "op"); err != ErrNotFound {
		t.Fatal("resuming twice must report not found")
	}
}

// Two processes sharing the state database: a halt started by one is seen
// by the other at its next sync, and so is its resumption.
func TestHaltIsSharedAcrossProcesses(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	daemon, _ := NewStore(db)
	client, _ := NewStore(db) // a `mesh7 --mcp` client started before the halt

	h, _, _ := daemon.Start(ScopeAll, "", "incident", "op")
	daemon.RecordRevoked(h.ID, []grant.Grant{{ID: "g1", Agent: "claude", Tools: "*", ExpiresAt: time.Now().Add(time.Hour)}})

	client.lastSync = time.Time{} // as if SyncEvery had passed
	got := client.Match("claude", "")
	if got == nil || got.ID != h.ID || got.Reason != "incident" {
		t.Fatalf("the client must see the daemon's halt, got %+v", got)
	}
	if len(got.RevokedGrants) != 1 {
		t.Fatal("the revoked grants travel with the halt")
	}

	if _, err := client.Resume(h.ID, "op"); err != nil {
		t.Fatal(err)
	}
	daemon.lastSync = time.Time{}
	if daemon.Match("claude", "") != nil {
		t.Fatal("the daemon must see the resumption")
	}

	reopened, _ := NewStore(db)
	if len(reopened.Active()) != 0 {
		t.Fatal("a resumed halt must not come back after a restart")
	}
}

// A database that fails after a halt was started must not lift it: the store
// keeps the last known halts rather than failing open.
func TestHaltSurvivesDatabaseFailure(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := NewStore(db)
	h, _, err := s.Start(ScopeAgent, "claude", "incident", "op")
	if err != nil {
		t.Fatal(err)
	}

	db.Close()               // the database goes away
	s.lastSync = time.Time{} // and the next Match tries to reload from it
	if got := s.Match("claude", ""); got == nil || got.ID != h.ID {
		t.Fatalf("halt lifted by a database failure, got %+v", got)
	}
}

// Resume needs the full ID or a prefix of at least 4 characters: a short
// prefix could lift the wrong stop.
func TestResumeRefusesShortPrefix(t *testing.T) {
	s, _ := NewStore(nil)
	h, _, _ := s.Start(ScopeAll, "", "", "op")
	if _, err := s.Resume(h.ID[:3], "op"); err != ErrNotFound {
		t.Errorf("3-character prefix: err = %v, want ErrNotFound", err)
	}
	if len(s.Active()) != 1 {
		t.Fatal("halt lifted by a short prefix")
	}
	if _, err := s.Resume("not-an-id", "op"); err != ErrNotFound {
		t.Errorf("unknown ID: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Resume(h.ID[:4], "op"); err != nil {
		t.Errorf("4-character prefix: %v", err)
	}
}

// Starting a halt with an unknown scope is refused, so no stop is recorded
// that would cover nothing.
func TestStartRefusesUnknownScope(t *testing.T) {
	s, _ := NewStore(nil)
	if _, _, err := s.Start(Scope("tenant"), "acme", "", "op"); err == nil {
		t.Fatal("unknown scope accepted")
	}
	if len(s.Active()) != 0 {
		t.Fatal("a halt with an unknown scope was recorded")
	}
}
