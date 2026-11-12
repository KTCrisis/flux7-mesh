package policy

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KTCrisis/flux7-mesh/config"
)

func TestWatcherReloadsOnConfigChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initial := `
port: 9090
policies:
  - name: v1
    agent: "*"
    rules:
      - tools: ["*"]
        action: deny
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32
	var lastPolicies atomic.Value

	w, err := NewWatcher(cfgPath, func(policies []config.Policy) {
		reloadCount.Add(1)
		lastPolicies.Store(policies)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	time.Sleep(100 * time.Millisecond)

	updated := `
port: 9090
policies:
  - name: v2
    agent: "*"
    rules:
      - tools: ["weather.*"]
        action: allow
      - tools: ["*"]
        action: deny
`
	if err := os.WriteFile(cfgPath, []byte(updated), 0644); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	for reloadCount.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for reload callback")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	policies := lastPolicies.Load().([]config.Policy)
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(policies))
	}
	if policies[0].Name != "v2" {
		t.Errorf("policy name = %q, want v2", policies[0].Name)
	}
}

func TestWatcherReloadsOnPolicyDirChange(t *testing.T) {
	dir := t.TempDir()
	polDir := filepath.Join(dir, "policies")
	if err := os.Mkdir(polDir, 0755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := `
port: 9090
policy_dir: policies
policies:
  - name: base
    agent: "*"
    rules:
      - tools: ["*"]
        action: deny
`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32
	var lastPolicies atomic.Value

	w, err := NewWatcher(cfgPath, func(policies []config.Policy) {
		reloadCount.Add(1)
		lastPolicies.Store(policies)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	time.Sleep(100 * time.Millisecond)

	agentPolicy := `
name: scout
agent: scout7
rules:
  - tools: ["searxng.*"]
    action: allow
`
	if err := os.WriteFile(filepath.Join(polDir, "scout.yaml"), []byte(agentPolicy), 0644); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	for reloadCount.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for policy_dir reload")
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	policies := lastPolicies.Load().([]config.Policy)
	if len(policies) != 2 {
		t.Fatalf("expected 2 policies (base + scout), got %d", len(policies))
	}
}

func TestWatcherIgnoresInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initial := `
port: 9090
policies:
  - name: v1
    agent: "*"
    rules:
      - tools: ["*"]
        action: deny
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	var reloadCount atomic.Int32

	w, err := NewWatcher(cfgPath, func(policies []config.Policy) {
		reloadCount.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(cfgPath, []byte("{{invalid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	if reloadCount.Load() != 0 {
		t.Error("callback should not be called for invalid YAML")
	}
}

// OnConfigChange fires for a change of the config file, not of a policy file.
func TestWatcherTellsConfigChangesFromPolicyChanges(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	polDir := filepath.Join(dir, "policies")
	os.Mkdir(polDir, 0o755)
	polPath := filepath.Join(polDir, "a.yaml")
	os.WriteFile(cfgPath, []byte("policy_dir: policies\n"), 0o644)
	os.WriteFile(polPath, []byte("name: a\nagent: a\nrules:\n  - tools: [\"*\"]\n    action: deny\n"), 0o644)

	policyReloads := make(chan struct{}, 8)
	configChanges := make(chan struct{}, 8)
	w, err := NewWatcher(cfgPath, func([]config.Policy) { policyReloads <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.OnConfigChange = func() { configChanges <- struct{}{} }
	time.Sleep(100 * time.Millisecond)

	os.WriteFile(polPath, []byte("name: a\nagent: a\nrules:\n  - tools: [\"*\"]\n    action: allow\n"), 0o644)
	select {
	case <-policyReloads:
	case <-time.After(3 * time.Second):
		t.Fatal("a policy file change did not reload the policies")
	}
	select {
	case <-configChanges:
		t.Fatal("a policy file change was reported as a config change")
	case <-time.After(400 * time.Millisecond):
	}

	os.WriteFile(cfgPath, []byte("policy_dir: policies\nport: 9191\n"), 0o644)
	select {
	case <-configChanges:
	case <-time.After(3 * time.Second):
		t.Fatal("a config file change was not reported")
	}
}
