package opcli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAdminTokenSources(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MESH_ADMIN_TOKEN", "")
	t.Setenv("MESH_ADMIN_TOKEN_FILE", "")
	if got := adminToken(); got != "" {
		t.Errorf("no source: %q, want empty", got)
	}

	dir := filepath.Join(home, ".config", "mesh7")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "admin.token"), []byte("from-default-file\n"), 0o600)
	// a file at the usual place is not read on its own: the CLI must not fetch
	// the token for an agent that runs it
	if got := adminToken(); got != "" {
		t.Errorf("a file found by default was read: %q", got)
	}

	other := filepath.Join(home, "other.token")
	os.WriteFile(other, []byte("  from-named-file  \nignored\n"), 0o600)
	t.Setenv("MESH_ADMIN_TOKEN_FILE", other)
	if got := adminToken(); got != "from-named-file" {
		t.Errorf("named file: %q", got)
	}

	t.Setenv("MESH_ADMIN_TOKEN", "from-env")
	if got := adminToken(); got != "from-env" {
		t.Errorf("env: %q", got)
	}
}
