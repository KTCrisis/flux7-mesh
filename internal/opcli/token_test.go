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
	if got := adminToken(); got != "from-default-file" {
		t.Errorf("default file: %q", got)
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
