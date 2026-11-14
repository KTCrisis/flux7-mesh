package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	os.Mkdir(filepath.Join(dir, "policies"), 0o755)
	pol := filepath.Join(dir, "policies", "a.yaml")
	write := func(servers, action string) {
		os.WriteFile(cfg, []byte("policy_dir: policies\nmcp_servers:\n"+servers), 0o644)
		os.WriteFile(pol, []byte("name: a\nagent: a\nrules:\n  - tools: [\"*\"]\n    action: "+action+"\n"), 0o644)
	}
	const good = "  - name: time\n    transport: stdio\n    command: uvx\n"

	var out, errw bytes.Buffer
	write(good, "deny")
	if code := runValidate([]string{"--config", cfg}, &out, &errw); code != 0 || !strings.Contains(out.String(), "1 upstream server(s)") || !strings.Contains(out.String(), "1 rule(s)") {
		t.Fatalf("sound config: exit %d\nout: %s\nerr: %s", code, out.String(), errw.String())
	}
	if code := runValidate([]string{cfg}, &out, &errw); code != 0 {
		t.Errorf("the config as a bare argument: exit %d", code)
	}

	for name, c := range map[string]struct{ servers, action, want string }{
		"misspelt action":   {good, "human_aproval", "unknown action"},
		"duplicate server":  {good + good, "deny", "duplicate name"},
		"unknown transport": {"  - name: x\n    transport: pigeon\n", "deny", "unsupported MCP transport"},
		"nameless server":   {"  - transport: stdio\n    command: uvx\n", "deny", "no name"},
	} {
		errw.Reset()
		write(c.servers, c.action)
		if code := runValidate([]string{"--config", cfg}, &out, &errw); code != 1 || !strings.Contains(errw.String(), c.want) {
			t.Errorf("%s: exit %d, stderr %q, want exit 1 naming %q", name, code, errw.String(), c.want)
		}
	}

	errw.Reset()
	if code := runValidate([]string{"--config", filepath.Join(dir, "absent.yaml")}, &out, &errw); code != 1 {
		t.Errorf("a missing file: exit %d", code)
	}
}

// A missing token file would stop the daemon and, with it, every tool a client
// gates on mesh7: validate says so before the restart.
func TestValidateChecksTheAdminTokenFile(t *testing.T) {
	t.Setenv("MESH_ADMIN_TOKEN", "")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	tok := filepath.Join(dir, "admin.token")
	write := func() {
		os.WriteFile(cfg, []byte("auth:\n  admin_token_file: "+tok+"\n  admin_scope: writes\n"), 0o644)
	}
	write()

	var out, errw bytes.Buffer
	if code := runValidate([]string{"--config", cfg}, &out, &errw); code != 1 || !strings.Contains(errw.String(), "admin_token_file") {
		t.Fatalf("missing token file: exit %d, stderr %q", code, errw.String())
	}
	os.WriteFile(tok, []byte("t0ken\n"), 0o600)
	out.Reset()
	if code := runValidate([]string{"--config", cfg}, &out, &errw); code != 0 || !strings.Contains(out.String(), "admin token for writes") {
		t.Fatalf("token present: exit %d, out %q", code, out.String())
	}
}
