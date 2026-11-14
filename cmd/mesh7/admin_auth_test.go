package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KTCrisis/flux7-mesh/config"
)

func TestAdminAuthReadsTheTokenFileAndScope(t *testing.T) {
	t.Setenv("MESH_ADMIN_TOKEN", "")
	f := filepath.Join(t.TempDir(), "admin.token")
	os.WriteFile(f, []byte("tok-from-file\n"), 0o600)

	tok, writes, err := adminAuth(config.AuthConfig{AdminTokenFile: f, AdminScope: "writes"})
	if err != nil || tok != "tok-from-file" || !writes {
		t.Fatalf("got (%q, %v, %v)", tok, writes, err)
	}
	if tok, _, _ := adminAuth(config.AuthConfig{AdminToken: "inline", AdminTokenFile: f}); tok != "inline" {
		t.Errorf("admin_token should win over the file, got %q", tok)
	}
	t.Setenv("MESH_ADMIN_TOKEN", "env")
	if tok, _, _ := adminAuth(config.AuthConfig{AdminTokenFile: f}); tok != "env" {
		t.Errorf("MESH_ADMIN_TOKEN should win, got %q", tok)
	}
	if _, _, err := adminAuth(config.AuthConfig{AdminScope: "reads"}); err == nil {
		t.Error("an unknown scope should be refused")
	}
	if _, _, err := adminAuth(config.AuthConfig{AdminTokenFile: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("a missing token file should be refused, not silently leave the plane open")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(empty, []byte("\n"), 0o600)
	if _, _, err := adminAuth(config.AuthConfig{AdminTokenFile: empty}); err == nil {
		t.Error("an empty token file should be refused")
	}
}
