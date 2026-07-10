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
	if cfg.Alerting.Enabled() {
		t.Fatalf("alerting enabled = true, want absent alerting config disabled")
	}
	if got, err := cfg.Alerting.DebounceDuration(); err != nil || got.String() != "30s" {
		t.Fatalf("alerting debounce = %v, err=%v; want 30s", got, err)
	}
}

func TestLoadConfigCanonicalizesAlertSinkNamesAndRejectsDescendingRetryBounds(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth: {mode: dev-no-auth}
alerting:
  retry: {initialInterval: 1m, maxInterval: 10s}
  sinks:
    webhook: {name: " custom-webhook "}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "maxInterval") {
		t.Fatalf("Load error = %v, want retry ordering error", err)
	}
	if err := os.WriteFile(path, []byte(`
auth: {mode: dev-no-auth}
alerting:
  sinks:
    webhook: {name: " custom-webhook "}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Alerting.Sinks.Webhook.Name != "custom-webhook" || cfg.Alerting.Sinks.Discord.Name != "discord" {
		t.Fatalf("effective sink names = %#v", cfg.Alerting.Sinks)
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

func TestLoadConfigLoadsServerAllowedOrigins(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  allowedOrigins:
    - http://127.0.0.1:5173
    - http://localhost:5173
    - http://regula1.lan:5173
auth:
  mode: dev-no-auth
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Server.AllowedOrigins, []string{"http://127.0.0.1:5173", "http://localhost:5173", "http://regula1.lan:5173"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("server allowed origins = %#v, want %#v", got, want)
	}
}

func TestLoadConfigLoadsHTTPRouteEgressPolicy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
runtime:
  http:
    - name: routes
      egress:
        allow:
          domains:
            - example.test
          cidrs:
            - 10.0.0.0/8
        deny:
          domains:
            - metadata.example.test
          cidrs:
            - 127.0.0.0/8
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := cfg.Runtime.HTTP[0].EgressPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if decision := policy.Check("https://app.example.test"); !decision.Allowed {
		t.Fatalf("domain route decision = %#v, want allowed", decision)
	}
	if decision := policy.Check("http://127.0.0.1"); decision.Allowed || !strings.Contains(decision.Rule, "deny cidr 127.0.0.0/8") {
		t.Fatalf("loopback route decision = %#v, want deny CIDR", decision)
	}
}

func TestLoadConfigRejectsInvalidHTTPRouteEgressPolicy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
runtime:
  http:
    - name: routes
      egress:
        deny:
          cidrs:
            - not-a-cidr
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "runtime.http.egress") || !strings.Contains(errText, "not-a-cidr") {
		t.Fatalf("error = %q, want egress validation context", errText)
	}
}

func TestLoadConfigLoadsPingRuntime(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
repositories:
  - name: kube
    url: https://example.invalid/kube.git
runtime:
  ping:
    - name: homelab
      repository: kube
      ansibleInventory: infrastructure/inventory/hosts.yml
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
	if cfg.Runtime.Ping[0].Repository != "kube" {
		t.Fatalf("repository = %q, want kube", cfg.Runtime.Ping[0].Repository)
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

func TestLoadConfigLoadsAlertingSinksWithEnvAndFileSecrets(t *testing.T) {
	t.Setenv("GITOPS_ALERT_WEBHOOK_URL", "https://hooks.example.test/gitops")
	t.Setenv("GITOPS_ALERT_AUTH_HEADER", "Bearer webhook-secret")
	t.Setenv("GITOPS_HOME_ASSISTANT_TOKEN", "ha-token")
	dir := t.TempDir()
	discordURLFile := filepath.Join(dir, "discord-url")
	secretHeaderFile := filepath.Join(dir, "header")
	webhookIDFile := filepath.Join(dir, "webhook-id")
	if err := os.WriteFile(discordURLFile, []byte("https://discord.example.test/api/webhooks/123/token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretHeaderFile, []byte("header-file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(webhookIDFile, []byte("ha-webhook-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`
auth:
  mode: dev-no-auth
alerting:
  debounce: 15s
  cooldown: 2m
  retry:
    maxAttempts: 4
    initialInterval: 5s
    maxInterval: 1m
  sinks:
    webhook:
      enabled: true
      urlEnv: GITOPS_ALERT_WEBHOOK_URL
      redactValues:
        - x6
      method: post
      headerEnv:
        Authorization: GITOPS_ALERT_AUTH_HEADER
      headerFile:
        X-Secret: "%s"
      bodyTemplate: '{"service":"{{ .ServiceID }}"}'
      include:
        services:
          - svc
        targets:
          - docker
    discord:
      enabled: true
      webhookURLFile: "%s"
    homeAssistant:
      enabled: true
      baseURL: http://homeassistant.local:8123
      insecureAllowHTTP: true
      tokenEnv: GITOPS_HOME_ASSISTANT_TOKEN
      webhookIDFile: "%s"
`, secretHeaderFile, discordURLFile, webhookIDFile)), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Alerting.Enabled() {
		t.Fatal("alerting enabled = false, want true")
	}
	if cfg.Alerting.Sinks.Webhook.URL != "https://hooks.example.test/gitops" {
		t.Fatalf("webhook URL = %q", cfg.Alerting.Sinks.Webhook.URL)
	}
	if cfg.Alerting.Sinks.Webhook.Method != "POST" {
		t.Fatalf("webhook method = %q, want POST", cfg.Alerting.Sinks.Webhook.Method)
	}
	if got := cfg.Alerting.Sinks.Webhook.Headers["Authorization"]; got != "Bearer webhook-secret" {
		t.Fatalf("authorization header = %q", got)
	}
	if got := cfg.Alerting.Sinks.Webhook.Headers["X-Secret"]; got != "header-file-secret" {
		t.Fatalf("file header = %q", got)
	}
	if !stringSliceContains(cfg.Alerting.RedactionValues, "x6") {
		t.Fatalf("alerting redaction values = %#v, want explicit redactValues entry", cfg.Alerting.RedactionValues)
	}
	if cfg.Alerting.Sinks.Discord.WebhookURL != "https://discord.example.test/api/webhooks/123/token" {
		t.Fatalf("discord webhook URL = %q", cfg.Alerting.Sinks.Discord.WebhookURL)
	}
	if cfg.Alerting.Sinks.HomeAssistant.Token != "ha-token" {
		t.Fatalf("home assistant token = %q", cfg.Alerting.Sinks.HomeAssistant.Token)
	}
	if cfg.Alerting.Sinks.HomeAssistant.WebhookID != "ha-webhook-secret" {
		t.Fatalf("home assistant webhook ID = %q", cfg.Alerting.Sinks.HomeAssistant.WebhookID)
	}
	if got, err := cfg.Alerting.RetryInitialIntervalDuration(); err != nil || got.String() != "5s" {
		t.Fatalf("retry initial interval = %v, err=%v; want 5s", got, err)
	}
}

func TestLoadConfigResolvesDisabledAlertingSecretsBestEffort(t *testing.T) {
	t.Setenv("GITOPS_DISABLED_HA_TOKEN", "disabled-ha-token-env")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    homeAssistant:
      enabled: false
      token: disabled-ha-token-literal
      tokenEnv: GITOPS_DISABLED_HA_TOKEN
      webhookIDEnv: MISSING_DISABLED_HA_WEBHOOK_ID
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Alerting.Enabled() {
		t.Fatal("alerting enabled = true, want disabled sink not to enable delivery")
	}
	if cfg.Alerting.Sinks.HomeAssistant.Token != "disabled-ha-token-literal" {
		t.Fatalf("disabled home assistant token = %q, want literal preserved for disabled delivery config", cfg.Alerting.Sinks.HomeAssistant.Token)
	}
	if cfg.Alerting.Sinks.HomeAssistant.WebhookID != "" {
		t.Fatalf("disabled missing webhook env resolved to %q, want non-fatal empty value", cfg.Alerting.Sinks.HomeAssistant.WebhookID)
	}
	for _, want := range []string{"disabled-ha-token-literal", "disabled-ha-token-env"} {
		if !stringSliceContains(cfg.Alerting.RedactionValues, want) {
			t.Fatalf("disabled redaction values = %#v, want %q", cfg.Alerting.RedactionValues, want)
		}
	}
}

func TestLoadConfigLoadsAlertingResetOnMissingKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  resetOnMissingKey: true
  resetToken: second-arm
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Alerting.ResetOnMissingKey {
		t.Fatal("alerting resetOnMissingKey = false, want true")
	}
	if cfg.Alerting.ResetToken != "second-arm" {
		t.Fatalf("alerting resetToken = %q, want second-arm", cfg.Alerting.ResetToken)
	}
}

func TestLoadConfigRejectsAlertingSinkNameContainingSecret(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      name: webhook-secret-name
      url: https://hooks.example.test/gitops
      redactValues:
        - secret
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded, want sink name secret validation error")
	}
	if !strings.Contains(err.Error(), "alerting.sinks.webhook.name") || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %v, want sink name secret validation", err)
	}
}

func TestLoadConfigRejectsAlertingSinkNameContainingInferredURLSecret(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      name: alerts-abc123DEF
      url: https://hooks.example.test/webhooks/abc123DEF
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded, want inferred sink name secret validation error")
	}
	if !strings.Contains(err.Error(), "alerting.sinks.webhook.name") || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %v, want inferred sink name secret validation", err)
	}
}

func TestLoadConfigRejectsAlertingSinkNameContainingEncodedSecret(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      name: alerts-tok%2Fsecret%3F
      url: https://hooks.example.test/gitops
      redactValues:
        - tok/secret?
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded, want encoded sink name secret validation error")
	}
	if !strings.Contains(err.Error(), "alerting.sinks.webhook.name") || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("error = %v, want encoded sink name secret validation", err)
	}
}

func TestLoadConfigRejectsDuplicateAlertingSinkNames(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      name: alerts
    discord:
      name: alerts
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded, want duplicate sink name validation error")
	}
	if !strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("error = %v, want duplicate sink name validation", err)
	}
}

func TestLoadConfigDoesNotTreatNonSecretAlertHeadersAsRedactionValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      url: https://hooks.example.test/gitops
      headers:
        Content-Type: application/json
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, nonSecret := range []string{"application/json", "https://hooks.example.test/gitops"} {
		if stringSliceContains(cfg.Alerting.RedactionValues, nonSecret) {
			t.Fatalf("alerting redaction values = %#v, want non-secret %q omitted", cfg.Alerting.RedactionValues, nonSecret)
		}
	}
}

func TestAlertAuthorizationRedactionValuesIncludeEverySchemeCredential(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"Basic dXNlcjpwYXNz", "Token non-bearer-secret"} {
		values := alertHeaderRedactionValues("Authorization", raw)
		parts := strings.Fields(raw)
		if !stringSliceContains(values, raw) || !stringSliceContains(values, parts[1]) {
			t.Fatalf("redaction values for %q = %#v, want complete value and credential", raw, values)
		}
	}
}

func TestLoadConfigRejectsAlertHeaderCaseInsensitiveSourceDuplicates(t *testing.T) {
	t.Setenv("GITOPS_ALERT_AUTH_HEADER", "Bearer env-token")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      url: https://hooks.example.test/gitops
      headers:
        authorization: Bearer literal-token
      headerEnv:
        Authorization: GITOPS_ALERT_AUTH_HEADER
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "Authorization") || !strings.Contains(errText, "must use only one") {
		t.Fatalf("error = %q, want case-insensitive header duplicate rejection", errText)
	}
}

func TestLoadConfigRejectsInvalidAlertHeaderName(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      url: https://hooks.example.test/gitops
      headers:
        "Bad Header": value
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "valid HTTP header name") {
		t.Fatalf("error = %q, want invalid header name rejection", errText)
	}
}

func TestLoadConfigRejectsAlertHeaderCRLFValue(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      url: https://hooks.example.test/gitops
      headers:
        X-Token: "bad\nvalue"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "CR or LF") {
		t.Fatalf("error = %q, want CR/LF header value rejection", errText)
	}
}

func TestLoadConfigRejectsInvalidAlertingURL(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      url: not-a-url
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "alerting.sinks.webhook.url") {
		t.Fatalf("error = %q, want alerting webhook URL context", errText)
	}
}

func TestLoadConfigRejectsPlainHTTPDiscordWebhook(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    discord:
      enabled: true
      webhookURL: http://discord.example.test/api/webhooks/123/token
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "alerting.sinks.discord.webhookURL must use https") {
		t.Fatalf("error = %q, want discord https requirement", errText)
	}
}

func TestLoadConfigRequiresExplicitInsecureHTTPForGenericWebhook(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      url: http://hooks.example.test/gitops
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "alerting.sinks.webhook.insecureAllowHTTP") {
		t.Fatalf("error = %q, want insecureAllowHTTP requirement", errText)
	}

	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      url: http://hooks.example.test/gitops
      insecureAllowHTTP: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with insecureAllowHTTP failed: %v", err)
	}
	if !cfg.Alerting.Sinks.Webhook.InsecureAllowHTTP || cfg.Alerting.Sinks.Webhook.URL != "http://hooks.example.test/gitops" {
		t.Fatalf("webhook config = %#v, want accepted explicit HTTP opt-in", cfg.Alerting.Sinks.Webhook)
	}
}

func TestLoadConfigRequiresExplicitInsecureHTTPForHomeAssistant(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    homeAssistant:
      enabled: true
      baseURL: http://homeassistant.local:8123
      token: ha-token
      webhookID: gitops-alert
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "alerting.sinks.homeAssistant.insecureAllowHTTP") {
		t.Fatalf("error = %q, want insecureAllowHTTP requirement", errText)
	}

	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    homeAssistant:
      enabled: true
      baseURL: http://homeassistant.local:8123
      insecureAllowHTTP: true
      token: ha-token
      webhookID: gitops-alert
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load with homeAssistant insecureAllowHTTP failed: %v", err)
	}
	if !cfg.Alerting.Sinks.HomeAssistant.InsecureAllowHTTP || cfg.Alerting.Sinks.HomeAssistant.BaseURL != "http://homeassistant.local:8123" {
		t.Fatalf("home assistant config = %#v, want accepted explicit HTTP opt-in", cfg.Alerting.Sinks.HomeAssistant)
	}

	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    homeAssistant:
      enabled: true
      baseURL: https://homeassistant.example.test
      token: ha-token
      webhookID: gitops-alert
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load with homeAssistant HTTPS failed: %v", err)
	}
	if cfg.Alerting.Sinks.HomeAssistant.BaseURL != "https://homeassistant.example.test" {
		t.Fatalf("home assistant base URL = %q, want HTTPS URL", cfg.Alerting.Sinks.HomeAssistant.BaseURL)
	}
}

func TestLoadConfigRejectsInvalidAlertingMethod(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
alerting:
  sinks:
    webhook:
      enabled: true
      url: https://hooks.example.test/gitops
      method: TRACE
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "alerting.sinks.webhook.method") {
		t.Fatalf("error = %q, want alerting webhook method context", errText)
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

func TestLoadConfigRejectsPingInventoryWithoutRepository(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
runtime:
  ping:
    - name: homelab
      ansibleInventory: infrastructure/inventory/hosts.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "ansibleInventory requires repository") {
		t.Fatalf("error = %q", errText)
	}
}

func TestLoadConfigRejectsPingInventoryOutsideRepository(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
repositories:
  - name: kube
    url: https://example.invalid/kube.git
runtime:
  ping:
    - name: homelab
      repository: kube
      ansibleInventory: ../hosts.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "must stay inside the repository") {
		t.Fatalf("error = %q", errText)
	}
}

func TestLoadConfigRejectsPingInventoryUnknownRepository(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
auth:
  mode: dev-no-auth
repositories:
  - name: kube
    url: https://example.invalid/kube.git
runtime:
  ping:
    - name: homelab
      repository: missing
      ansibleInventory: infrastructure/inventory/hosts.yml
`), 0o600); err != nil {
		t.Fatal(err)
	}
	errText := loadError(t, path)
	if !strings.Contains(errText, "is not defined in repositories") {
		t.Fatalf("error = %q", errText)
	}
}

func TestLoadComposeExampleConfigs(t *testing.T) {
	t.Setenv("GITOPS_DASHBOARD_ADMIN_HASH", "$2a$10$example-admin-hash")
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

func loadError(t *testing.T, path string) string {
	t.Helper()
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load succeeded, want error")
	}
	return err.Error()
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
