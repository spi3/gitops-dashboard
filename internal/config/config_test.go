package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAppliesDefaults(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
repositories:
  - name: repo
    url: https://example.invalid/repo.git
    includePaths:
      - clusters/main
    excludePaths:
      - clusters/retired
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Fatalf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Repositories[0].DefaultRef != "HEAD" {
		t.Fatalf("default ref = %q", cfg.Repositories[0].DefaultRef)
	}
	if len(cfg.Repositories[0].IncludePaths) != 1 || cfg.Repositories[0].IncludePaths[0] != "clusters/main" {
		t.Fatalf("includePaths = %#v", cfg.Repositories[0].IncludePaths)
	}
	if len(cfg.Repositories[0].ExcludePaths) != 1 || cfg.Repositories[0].ExcludePaths[0] != "clusters/retired" {
		t.Fatalf("excludePaths = %#v", cfg.Repositories[0].ExcludePaths)
	}
}

func TestLoadConfigRejectsInvalidDurations(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
monitoring:
  defaultInterval: 30s
repositories:
  - name: repo
    url: https://example.invalid/repo.git
    scanInterval: no-such-duration
runtime:
  http:
    - name: routes
      timeout: 5s
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with invalid scanInterval")
	}
}

func TestLoadConfigRejectsInvalidHTTPTimeout(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
runtime:
  http:
    - name: routes
      timeout: no-such-duration
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with invalid HTTP timeout")
	}
}

func TestLoadConfigLoadsPingRuntime(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
runtime:
  ping:
    - name: homelab
      ansibleInventory: /config/hosts.yml
      interval: 1m
      timeout: 2s
      environment: infrastructure
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Runtime.Ping) != 1 {
		t.Fatalf("ping targets = %#v", cfg.Runtime.Ping)
	}
	if cfg.Runtime.Ping[0].EffectiveName() != "homelab" {
		t.Fatalf("effective name = %q, want homelab", cfg.Runtime.Ping[0].EffectiveName())
	}
}

func TestLoadConfigExpandsEnvVars(t *testing.T) {
	t.Setenv("GITOPS_DASHBOARD_AGENT_TOKEN", "shared-agent-token")
	t.Setenv("GITOPS_DASHBOARD_ADMIN_HASH", "$2a$10$env-provided-hash")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: basic
  users:
    - username: admin
      passwordHash: "${GITOPS_DASHBOARD_ADMIN_HASH}"
  agent:
    tokens:
      - "${GITOPS_DASHBOARD_AGENT_TOKEN}"
runtime:
  docker:
    - name: host
      kind: agent
      agentToken: "${GITOPS_DASHBOARD_AGENT_TOKEN}"
agent:
  serverUrl: ws://dashboard.invalid/api/agents/connect
  target: host
  token: "${GITOPS_DASHBOARD_AGENT_TOKEN}"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Users[0].PasswordHash != "$2a$10$env-provided-hash" {
		t.Fatalf("passwordHash = %q", cfg.Auth.Users[0].PasswordHash)
	}
	if cfg.Auth.Agent.Tokens[0] != "shared-agent-token" {
		t.Fatalf("auth agent token = %q", cfg.Auth.Agent.Tokens[0])
	}
	if cfg.Runtime.Docker[0].AgentToken != "shared-agent-token" {
		t.Fatalf("runtime docker agent token = %q", cfg.Runtime.Docker[0].AgentToken)
	}
	if cfg.Agent.Token != "shared-agent-token" {
		t.Fatalf("agent token = %q", cfg.Agent.Token)
	}
}

func TestLoadConfigRejectsUnsetEnvVars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: basic
  users:
    - username: admin
      passwordHash: "${MISSING_GITOPS_DASHBOARD_HASH}"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded with an unset env var")
	}
	if !strings.Contains(err.Error(), "MISSING_GITOPS_DASHBOARD_HASH") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigLeavesLiteralDollarValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: basic
  users:
    - username: admin
      passwordHash: "$2a$10$literal-hash"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Users[0].PasswordHash != "$2a$10$literal-hash" {
		t.Fatalf("passwordHash = %q", cfg.Auth.Users[0].PasswordHash)
	}
}

func TestLoadConfigRejectsEmptyPingTarget(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
runtime:
  ping:
    - name: empty
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with empty ping target")
	}
}

func TestLoadComposeExampleConfigs(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		filepath.Join("..", "..", "examples", "compose-config", "config.yaml"),
		filepath.Join("..", "..", "examples", "compose-config", "agent.yaml"),
	} {
		if _, err := Load(path); err != nil {
			t.Fatalf("Load(%s): %v", path, err)
		}
	}
}
