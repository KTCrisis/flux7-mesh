package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadExpandsEnvInOTELHeadersOnly(t *testing.T) {
	t.Setenv("OTEL_TEST_TOKEN", "tok-123")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `
port: 9191
otel_endpoint: https://collector.example/otlp
otel_headers:
  Authorization: "Bearer ${OTEL_TEST_TOKEN}"
  X-Scope-OrgID: "tenant-$OTEL_TEST_TOKEN"
otel_ca_cert: /etc/ssl/private-ca.pem
policies:
  - name: literal
    agent: "*"
    rules:
      - tools: ["shell.run"]
        action: deny
        condition:
          field: command
          operator: contains
          value: "$HOME"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.OTELHeaders["Authorization"]; got != "Bearer tok-123" {
		t.Errorf("Authorization = %q", got)
	}
	if got := cfg.OTELHeaders["X-Scope-OrgID"]; got != "tenant-tok-123" {
		t.Errorf("X-Scope-OrgID = %q", got)
	}
	if cfg.OTELCACert != "/etc/ssl/private-ca.pem" {
		t.Errorf("OTELCACert = %q", cfg.OTELCACert)
	}
	// A "$" anywhere else in the file is left alone: expansion is scoped to
	// header values, never to policies.
	if len(cfg.Policies) != 1 || len(cfg.Policies[0].Rules) != 1 || cfg.Policies[0].Rules[0].Condition == nil {
		t.Fatalf("policy not loaded as expected: %+v", cfg.Policies)
	}
	if got := cfg.Policies[0].Rules[0].Condition.Value.Strings; len(got) != 1 || got[0] != "$HOME" {
		t.Errorf("policy condition value was expanded: %v", got)
	}
}

func TestLoadExpandsUpstreamHeadersAndMemoryToken(t *testing.T) {
	t.Setenv("MEM7_TEST_TOKEN", "m7-secret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := `
memory:
  url: http://localhost:9070
  token: "${MEM7_TEST_TOKEN}"
mcp_servers:
  - name: memory
    transport: streamable-http
    url: http://localhost:9070/mcp
    forward_identity: true
    headers:
      Authorization: "Bearer ${MEM7_TEST_TOKEN}"
  - name: remote
    transport: streamable-http
    url: https://example.org/mcp
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Memory.Token != "m7-secret" {
		t.Errorf("memory.token = %q", cfg.Memory.Token)
	}
	if got := cfg.MCPServers[0].Headers["Authorization"]; got != "Bearer m7-secret" {
		t.Errorf("upstream header = %q", got)
	}
	if !cfg.MCPServers[0].ForwardIdentity || cfg.MCPServers[1].ForwardIdentity {
		t.Errorf("forward_identity: %v, %v", cfg.MCPServers[0].ForwardIdentity, cfg.MCPServers[1].ForwardIdentity)
	}
}
