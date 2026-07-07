package config

import (
	"fmt"
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

func TestLoadConfigResolvesServerSecretSources(t *testing.T) {
	t.Setenv("GITOPS_DASHBOARD_ADMIN_HASH", "$2a$10$env-provided-hash")
	t.Setenv("GITOPS_DASHBOARD_AUTH_AGENT_TOKEN", "auth-agent-env-token")
	t.Setenv("GITOPS_DASHBOARD_DOCKER_AGENT_TOKEN", "docker-agent-env-token")
	dir := t.TempDir()
	authAgentTokenFile := filepath.Join(dir, "auth-agent-token")
	dockerAgentTokenFile := filepath.Join(dir, "docker-agent-token")
	if err := os.WriteFile(authAgentTokenFile, []byte("auth-agent-file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dockerAgentTokenFile, []byte("docker-agent-file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`
auth:
  mode: basic
  users:
    - username: admin
      passwordHashEnv: GITOPS_DASHBOARD_ADMIN_HASH
  agent:
    tokenEnv: GITOPS_DASHBOARD_AUTH_AGENT_TOKEN
    tokenFile: "%s"
runtime:
  docker:
    - name: host-env
      kind: agent
      agentTokenEnv: GITOPS_DASHBOARD_DOCKER_AGENT_TOKEN
    - name: host-file
      kind: agent
      agentTokenFile: "%s"
`, authAgentTokenFile, dockerAgentTokenFile)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Users[0].PasswordHash != "$2a$10$env-provided-hash" {
		t.Fatalf("passwordHash = %q", cfg.Auth.Users[0].PasswordHash)
	}
	if got, want := cfg.Auth.Agent.Tokens, []string{"auth-agent-env-token", "auth-agent-file-token"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("auth agent tokens = %#v, want %#v", got, want)
	}
	if cfg.Runtime.Docker[0].AgentToken != "docker-agent-env-token" {
		t.Fatalf("env docker agent token = %q", cfg.Runtime.Docker[0].AgentToken)
	}
	if cfg.Runtime.Docker[1].AgentToken != "docker-agent-file-token" {
		t.Fatalf("file docker agent token = %q", cfg.Runtime.Docker[1].AgentToken)
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

func TestLoadConfigRejectsUnsetSecretEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: basic
  users:
    - username: admin
      passwordHashEnv: MISSING_GITOPS_DASHBOARD_HASH
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded with an unset passwordHashEnv")
	}
	if !strings.Contains(err.Error(), "auth.users[0].passwordHashEnv") {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "MISSING_GITOPS_DASHBOARD_HASH") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConfigRejectsConflictingSecretSources(t *testing.T) {
	t.Setenv("GITOPS_DASHBOARD_ADMIN_HASH", "$2a$10$env-provided-hash")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: basic
  users:
    - username: admin
      passwordHash: "$2a$10$literal-hash"
      passwordHashEnv: GITOPS_DASHBOARD_ADMIN_HASH
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded with conflicting password hash sources")
	}
	if !strings.Contains(err.Error(), "must use only one") {
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

func TestLoadAgentConfigAcceptsMinimalEnvBackedConfig(t *testing.T) {
	t.Setenv("HD3_DOCKER_GITOPS_DASHBOARD_AGENT_TOKEN", "agent-token")
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(`
agent:
  serverUrl: ws://gitops-dashboard.lan:8080/api/agents/connect
  target: hd3-docker
  tokenEnv: HD3_DOCKER_GITOPS_DASHBOARD_AGENT_TOKEN
  interval: 30s
  docker:
    host: unix:///var/run/docker.sock
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadForMode(path, ModeAgent)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Mode != "" {
		t.Fatalf("auth mode = %q, want empty in agent mode", cfg.Auth.Mode)
	}
	if cfg.Agent.Token != "agent-token" {
		t.Fatalf("agent token = %q", cfg.Agent.Token)
	}
	if cfg.Agent.Target != "hd3-docker" {
		t.Fatalf("agent target = %q", cfg.Agent.Target)
	}
}

func TestLoadAgentConfigRejectsInvalidAgentDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(`
agent:
  serverUrl: ws://dashboard.invalid/api/agents/connect
  target: docker
  token: token
  interval: no-such-duration
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadForMode(path, ModeAgent)
	if err == nil {
		t.Fatal("LoadForMode succeeded with invalid agent interval")
	}
	if !strings.Contains(err.Error(), "agent.interval") {
		t.Fatalf("error = %v", err)
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
	t.Setenv("GITOPS_DASHBOARD_AGENT_TOKEN", "example-agent-token")
	serverConfig := filepath.Join("..", "..", "examples", "compose-config", "config.yaml")
	if _, err := Load(serverConfig); err != nil {
		t.Fatalf("Load(%s): %v", serverConfig, err)
	}
	agentConfig := filepath.Join("..", "..", "examples", "compose-config", "agent.yaml")
	if _, err := LoadForMode(agentConfig, ModeAgent); err != nil {
		t.Fatalf("LoadForMode(%s, %s): %v", agentConfig, ModeAgent, err)
	}
}
