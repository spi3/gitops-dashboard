package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/routetarget"
	"github.com/example/gitops-dashboard/internal/sanitizer"
	"gopkg.in/yaml.v3"
)

var configEnvPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Auth         AuthConfig         `yaml:"auth"`
	Repositories []RepositoryConfig `yaml:"repositories"`
	Runtime      RuntimeConfig      `yaml:"runtime"`
	Monitoring   MonitoringConfig   `yaml:"monitoring"`
	Alerting     AlertingConfig     `yaml:"alerting"`
	Agent        AgentConfig        `yaml:"agent"`
}

const (
	ModeServer = "server"
	ModeAgent  = "agent"
)

type ServerConfig struct {
	Listen         string   `yaml:"listen"`
	DataDir        string   `yaml:"dataDir"`
	RepoCacheDir   string   `yaml:"repoCacheDir"`
	AllowedOrigins []string `yaml:"allowedOrigins"`
}

type AuthConfig struct {
	Mode  string       `yaml:"mode"`
	Users []AuthUser   `yaml:"users"`
	Agent AgentAuthCfg `yaml:"agent"`
}

type AuthUser struct {
	Username         string `yaml:"username"`
	PasswordHash     string `yaml:"passwordHash"`
	PasswordHashFile string `yaml:"passwordHashFile"`
	PasswordHashEnv  string `yaml:"passwordHashEnv"`
}

type AgentAuthCfg struct {
	Tokens         []string `yaml:"tokens"`
	TokenFile      string   `yaml:"tokenFile"`
	TokenEnv       string   `yaml:"tokenEnv"`
	AllowedOrigins []string `yaml:"allowedOrigins"`
}

type RepositoryConfig struct {
	Name         string   `yaml:"name"`
	URL          string   `yaml:"url"`
	DefaultRef   string   `yaml:"defaultRef"`
	Type         string   `yaml:"type"`
	TokenFile    string   `yaml:"tokenFile"`
	TokenEnv     string   `yaml:"tokenEnv"`
	SSHKeyPath   string   `yaml:"sshKeyPath"`
	KnownHosts   string   `yaml:"knownHosts"`
	ScanInterval string   `yaml:"scanInterval"`
	IncludePaths []string `yaml:"includePaths"`
	ExcludePaths []string `yaml:"excludePaths"`
}

type RuntimeConfig struct {
	Docker     []DockerTarget     `yaml:"docker"`
	Kubernetes []KubernetesTarget `yaml:"kubernetes"`
	HTTP       []HTTPRouteTarget  `yaml:"http"`
	Ping       []PingTarget       `yaml:"ping"`
}

type DockerTarget struct {
	Name           string `yaml:"name"`
	Kind           string `yaml:"kind"`
	Host           string `yaml:"host"`
	AgentToken     string `yaml:"agentToken"`
	AgentTokenFile string `yaml:"agentTokenFile"`
	AgentTokenEnv  string `yaml:"agentTokenEnv"`
	Interval       string `yaml:"interval"`
}

type KubernetesTarget struct {
	Name       string `yaml:"name"`
	Kubeconfig string `yaml:"kubeconfig"`
	Context    string `yaml:"context"`
	Interval   string `yaml:"interval"`
}

type HTTPRouteTarget struct {
	Name     string             `yaml:"name"`
	Interval string             `yaml:"interval"`
	Timeout  string             `yaml:"timeout"`
	Egress   EgressPolicyConfig `yaml:"egress"`
}

type EgressPolicyConfig struct {
	Allow EgressPolicyRules `yaml:"allow"`
	Deny  EgressPolicyRules `yaml:"deny"`
}

type EgressPolicyRules struct {
	Domains []string `yaml:"domains"`
	CIDRs   []string `yaml:"cidrs"`
}

type PingTarget struct {
	Name               string `yaml:"name"`
	Host               string `yaml:"host"`
	Repository         string `yaml:"repository"`
	AnsibleInventory   string `yaml:"ansibleInventory"`
	Interval           string `yaml:"interval"`
	Timeout            string `yaml:"timeout"`
	MinRefreshInterval string `yaml:"minRefreshInterval"`
	Environment        string `yaml:"environment"`
}

type MonitoringConfig struct {
	DefaultInterval string `yaml:"defaultInterval"`
}

type AlertingConfig struct {
	Debounce          string               `yaml:"debounce"`
	Cooldown          string               `yaml:"cooldown"`
	StabilitySamples  int                  `yaml:"stabilitySamples"`
	ResetOnMissingKey bool                 `yaml:"resetOnMissingKey"`
	ResetToken        string               `yaml:"resetToken"`
	Retry             AlertRetryConfig     `yaml:"retry"`
	Retention         AlertRetentionConfig `yaml:"retention"`
	Sinks             AlertingSinksConfig  `yaml:"sinks"`
	RedactionValues   []string             `yaml:"-"`
}

type AlertRetryConfig struct {
	MaxAttempts     int    `yaml:"maxAttempts"`
	InitialInterval string `yaml:"initialInterval"`
	MaxInterval     string `yaml:"maxInterval"`
}

// AlertRetentionConfig bounds how long terminal alert_events/alert_dispatches
// rows are kept before periodic pruning removes them. Pending/in-flight rows
// are never pruned regardless of age.
type AlertRetentionConfig struct {
	Horizon   string `yaml:"horizon"`
	Interval  string `yaml:"interval"`
	BatchSize int    `yaml:"batchSize"`
}

type AlertingSinksConfig struct {
	Webhook       WebhookAlertSinkConfig       `yaml:"webhook"`
	Discord       DiscordAlertSinkConfig       `yaml:"discord"`
	HomeAssistant HomeAssistantAlertSinkConfig `yaml:"homeAssistant"`
}

type AlertSinkFilterConfig struct {
	Services []string `yaml:"services"`
	Targets  []string `yaml:"targets"`
}

type WebhookAlertSinkConfig struct {
	Enabled           bool                  `yaml:"enabled"`
	Name              string                `yaml:"name"`
	URL               string                `yaml:"url"`
	URLEnv            string                `yaml:"urlEnv"`
	URLFile           string                `yaml:"urlFile"`
	RedactValues      []string              `yaml:"redactValues"`
	InsecureAllowHTTP bool                  `yaml:"insecureAllowHTTP"`
	Method            string                `yaml:"method"`
	Headers           map[string]string     `yaml:"headers"`
	HeaderEnv         map[string]string     `yaml:"headerEnv"`
	HeaderFile        map[string]string     `yaml:"headerFile"`
	BodyTemplate      string                `yaml:"bodyTemplate"`
	Timeout           string                `yaml:"timeout"`
	Include           AlertSinkFilterConfig `yaml:"include"`
	Exclude           AlertSinkFilterConfig `yaml:"exclude"`
}

type DiscordAlertSinkConfig struct {
	Enabled        bool                  `yaml:"enabled"`
	Name           string                `yaml:"name"`
	WebhookURL     string                `yaml:"webhookURL"`
	WebhookURLEnv  string                `yaml:"webhookURLEnv"`
	WebhookURLFile string                `yaml:"webhookURLFile"`
	RedactValues   []string              `yaml:"redactValues"`
	Timeout        string                `yaml:"timeout"`
	Include        AlertSinkFilterConfig `yaml:"include"`
	Exclude        AlertSinkFilterConfig `yaml:"exclude"`
}

type HomeAssistantAlertSinkConfig struct {
	Enabled           bool                  `yaml:"enabled"`
	Name              string                `yaml:"name"`
	BaseURL           string                `yaml:"baseURL"`
	BaseURLEnv        string                `yaml:"baseURLEnv"`
	BaseURLFile       string                `yaml:"baseURLFile"`
	RedactValues      []string              `yaml:"redactValues"`
	InsecureAllowHTTP bool                  `yaml:"insecureAllowHTTP"`
	Token             string                `yaml:"token"`
	TokenEnv          string                `yaml:"tokenEnv"`
	TokenFile         string                `yaml:"tokenFile"`
	WebhookID         string                `yaml:"webhookID"`
	WebhookIDEnv      string                `yaml:"webhookIDEnv"`
	WebhookIDFile     string                `yaml:"webhookIDFile"`
	Timeout           string                `yaml:"timeout"`
	Include           AlertSinkFilterConfig `yaml:"include"`
	Exclude           AlertSinkFilterConfig `yaml:"exclude"`
}

type AgentConfig struct {
	ServerURL string       `yaml:"serverUrl"`
	Target    string       `yaml:"target"`
	Token     string       `yaml:"token"`
	TokenFile string       `yaml:"tokenFile"`
	TokenEnv  string       `yaml:"tokenEnv"`
	Interval  string       `yaml:"interval"`
	Docker    DockerTarget `yaml:"docker"`
}

func Load(path string) (Config, error) {
	return LoadForMode(path, ModeServer)
}

func LoadForMode(path, mode string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	data, err = expandConfigEnv(data)
	if err != nil {
		return Config{}, fmt.Errorf("expand config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	switch mode {
	case ModeServer:
		cfg.applyServerDefaults()
		if err := cfg.resolveServerSecrets(); err != nil {
			return Config{}, err
		}
		if err := cfg.Validate(); err != nil {
			return Config{}, err
		}
	case ModeAgent:
		if err := cfg.resolveAgentSecrets(); err != nil {
			return Config{}, err
		}
		if err := cfg.ValidateAgent(); err != nil {
			return Config{}, err
		}
	default:
		return Config{}, fmt.Errorf("mode must be server or agent")
	}
	return cfg, nil
}

func expandConfigEnv(data []byte) ([]byte, error) {
	missing := map[string]struct{}{}
	expanded := configEnvPattern.ReplaceAllStringFunc(string(data), func(match string) string {
		parts := configEnvPattern.FindStringSubmatch(match)
		name := parts[1]
		value, ok := os.LookupEnv(name)
		if !ok {
			missing[name] = struct{}{}
			return match
		}
		return value
	})
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unset env vars: %s", strings.Join(names, ", "))
	}
	return []byte(expanded), nil
}

func (cfg *Config) applyServerDefaults() {
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = ":8080"
	}
	if cfg.Server.DataDir == "" {
		cfg.Server.DataDir = "data"
	}
	if cfg.Server.RepoCacheDir == "" {
		cfg.Server.RepoCacheDir = filepath.Join(cfg.Server.DataDir, "repos")
	}
	if cfg.Auth.Mode == "" {
		cfg.Auth.Mode = "basic"
	}
	if cfg.Monitoring.DefaultInterval == "" {
		cfg.Monitoring.DefaultInterval = "30s"
	}
	cfg.applyAlertingDefaults()
	for i := range cfg.Repositories {
		if cfg.Repositories[i].DefaultRef == "" {
			cfg.Repositories[i].DefaultRef = "HEAD"
		}
	}
}

func (cfg *Config) applyAlertingDefaults() {
	if cfg.Alerting.StabilitySamples == 0 {
		cfg.Alerting.StabilitySamples = 2
	}
	if cfg.Alerting.Debounce == "" {
		cfg.Alerting.Debounce = "30s"
	}
	if cfg.Alerting.Cooldown == "" {
		cfg.Alerting.Cooldown = "5m"
	}
	if cfg.Alerting.Retry.MaxAttempts == 0 {
		cfg.Alerting.Retry.MaxAttempts = 3
	}
	if cfg.Alerting.Retry.InitialInterval == "" {
		cfg.Alerting.Retry.InitialInterval = "10s"
	}
	if cfg.Alerting.Retry.MaxInterval == "" {
		cfg.Alerting.Retry.MaxInterval = "5m"
	}
	if cfg.Alerting.Retention.Horizon == "" {
		cfg.Alerting.Retention.Horizon = "720h"
	}
	if cfg.Alerting.Retention.Interval == "" {
		cfg.Alerting.Retention.Interval = "1h"
	}
	if cfg.Alerting.Retention.BatchSize == 0 {
		cfg.Alerting.Retention.BatchSize = 500
	}
	if cfg.Alerting.Sinks.Webhook.Method == "" {
		cfg.Alerting.Sinks.Webhook.Method = "POST"
	} else {
		cfg.Alerting.Sinks.Webhook.Method = strings.ToUpper(cfg.Alerting.Sinks.Webhook.Method)
	}
	if cfg.Alerting.Sinks.Webhook.Timeout == "" {
		cfg.Alerting.Sinks.Webhook.Timeout = "10s"
	}
	if cfg.Alerting.Sinks.Discord.Timeout == "" {
		cfg.Alerting.Sinks.Discord.Timeout = "10s"
	}
	if cfg.Alerting.Sinks.HomeAssistant.Timeout == "" {
		cfg.Alerting.Sinks.HomeAssistant.Timeout = "10s"
	}
	// Persist the effective identity so every caller uses the same validated,
	// canonical sink name rather than having to repeat defaulting rules.
	cfg.Alerting.Sinks.Webhook.Name = alertSinkName("webhook", cfg.Alerting.Sinks.Webhook.Name)
	cfg.Alerting.Sinks.Discord.Name = alertSinkName("discord", cfg.Alerting.Sinks.Discord.Name)
	cfg.Alerting.Sinks.HomeAssistant.Name = alertSinkName("home-assistant", cfg.Alerting.Sinks.HomeAssistant.Name)
}

func (cfg *Config) resolveServerSecrets() error {
	for i := range cfg.Auth.Users {
		passwordHash, err := resolveSecret(
			cfg.Auth.Users[i].PasswordHash,
			cfg.Auth.Users[i].PasswordHashEnv,
			cfg.Auth.Users[i].PasswordHashFile,
			fmt.Sprintf("auth.users[%d].passwordHash", i),
			fmt.Sprintf("auth.users[%d].passwordHashEnv", i),
			fmt.Sprintf("auth.users[%d].passwordHashFile", i),
		)
		if err != nil {
			return err
		}
		cfg.Auth.Users[i].PasswordHash = passwordHash
	}

	tokens := make([]string, 0, len(cfg.Auth.Agent.Tokens)+2)
	tokens = append(tokens, cfg.Auth.Agent.Tokens...)
	if cfg.Auth.Agent.TokenEnv != "" {
		token, err := resolveEnvSecret("auth.agent.tokenEnv", cfg.Auth.Agent.TokenEnv)
		if err != nil {
			return err
		}
		tokens = append(tokens, token)
	}
	if cfg.Auth.Agent.TokenFile != "" {
		token, err := resolveFileSecret("auth.agent.tokenFile", cfg.Auth.Agent.TokenFile)
		if err != nil {
			return err
		}
		tokens = append(tokens, token)
	}
	cfg.Auth.Agent.Tokens = tokens

	for i := range cfg.Runtime.Docker {
		token, err := resolveSecret(
			cfg.Runtime.Docker[i].AgentToken,
			cfg.Runtime.Docker[i].AgentTokenEnv,
			cfg.Runtime.Docker[i].AgentTokenFile,
			fmt.Sprintf("runtime.docker[%d].agentToken", i),
			fmt.Sprintf("runtime.docker[%d].agentTokenEnv", i),
			fmt.Sprintf("runtime.docker[%d].agentTokenFile", i),
		)
		if err != nil {
			return err
		}
		cfg.Runtime.Docker[i].AgentToken = token
	}
	if err := cfg.resolveAlertingSecrets(); err != nil {
		return err
	}
	return nil
}

func (cfg *Config) resolveAlertingSecrets() error {
	// Webhook.RedactValues is resolved against the URL below (once it is
	// known) so declared path/query secrets also cover their raw-escaped
	// form as it appears in the URL; see appendAlertDeclaredURLSecretValues.
	cfg.Alerting.RedactionValues = appendAlertRedactValues(cfg.Alerting.RedactionValues, cfg.Alerting.Sinks.Discord.RedactValues...)
	cfg.Alerting.RedactionValues = appendAlertRedactValues(cfg.Alerting.RedactionValues, cfg.Alerting.Sinks.HomeAssistant.RedactValues...)
	if cfg.Alerting.Sinks.Webhook.Enabled {
		url, err := resolveSecret(
			cfg.Alerting.Sinks.Webhook.URL,
			cfg.Alerting.Sinks.Webhook.URLEnv,
			cfg.Alerting.Sinks.Webhook.URLFile,
			"alerting.sinks.webhook.url",
			"alerting.sinks.webhook.urlEnv",
			"alerting.sinks.webhook.urlFile",
		)
		if err != nil {
			return err
		}
		cfg.Alerting.Sinks.Webhook.URL = url
		cfg.Alerting.Sinks.Webhook.URLEnv = ""
		cfg.Alerting.Sinks.Webhook.URLFile = ""
		cfg.Alerting.RedactionValues = appendAlertURLRedactValues(cfg.Alerting.RedactionValues, url)
		cfg.Alerting.RedactionValues = appendAlertDeclaredURLSecretValues(cfg.Alerting.RedactionValues, url, cfg.Alerting.Sinks.Webhook.RedactValues)
		headers, err := resolveSecretMap(
			cfg.Alerting.Sinks.Webhook.Headers,
			cfg.Alerting.Sinks.Webhook.HeaderEnv,
			cfg.Alerting.Sinks.Webhook.HeaderFile,
			"alerting.sinks.webhook.headers",
			"alerting.sinks.webhook.headerEnv",
			"alerting.sinks.webhook.headerFile",
		)
		if err != nil {
			return err
		}
		cfg.Alerting.Sinks.Webhook.Headers = headers
		cfg.Alerting.Sinks.Webhook.HeaderEnv = nil
		cfg.Alerting.Sinks.Webhook.HeaderFile = nil
		for name, value := range headers {
			cfg.Alerting.RedactionValues = appendAlertHeaderRedactValues(cfg.Alerting.RedactionValues, name, value)
		}
	} else {
		url := resolveSecretBestEffort(
			cfg.Alerting.Sinks.Webhook.URL,
			cfg.Alerting.Sinks.Webhook.URLEnv,
			cfg.Alerting.Sinks.Webhook.URLFile,
			"alerting.sinks.webhook.url",
			"alerting.sinks.webhook.urlEnv",
			"alerting.sinks.webhook.urlFile",
		)
		cfg.Alerting.Sinks.Webhook.URL = url.Value
		cfg.Alerting.RedactionValues = appendAlertURLRedactValues(cfg.Alerting.RedactionValues, url.RedactionValues...)
		cfg.Alerting.RedactionValues = appendAlertDeclaredURLSecretValues(cfg.Alerting.RedactionValues, url.Value, cfg.Alerting.Sinks.Webhook.RedactValues)
		headers, err := resolveSecretMapBestEffort(
			cfg.Alerting.Sinks.Webhook.Headers,
			cfg.Alerting.Sinks.Webhook.HeaderEnv,
			cfg.Alerting.Sinks.Webhook.HeaderFile,
			"alerting.sinks.webhook.headers",
			"alerting.sinks.webhook.headerEnv",
			"alerting.sinks.webhook.headerFile",
		)
		if err != nil {
			return err
		}
		cfg.Alerting.Sinks.Webhook.Headers = headers.Values
		cfg.Alerting.RedactionValues = append(cfg.Alerting.RedactionValues, headers.RedactionValues...)
	}
	if cfg.Alerting.Sinks.Discord.Enabled {
		webhookURL, err := resolveSecret(
			cfg.Alerting.Sinks.Discord.WebhookURL,
			cfg.Alerting.Sinks.Discord.WebhookURLEnv,
			cfg.Alerting.Sinks.Discord.WebhookURLFile,
			"alerting.sinks.discord.webhookURL",
			"alerting.sinks.discord.webhookURLEnv",
			"alerting.sinks.discord.webhookURLFile",
		)
		if err != nil {
			return err
		}
		cfg.Alerting.Sinks.Discord.WebhookURL = webhookURL
		cfg.Alerting.Sinks.Discord.WebhookURLEnv = ""
		cfg.Alerting.Sinks.Discord.WebhookURLFile = ""
		cfg.Alerting.RedactionValues = appendAlertRedactValues(cfg.Alerting.RedactionValues, webhookURL)
		cfg.Alerting.RedactionValues = appendAlertURLRedactValues(cfg.Alerting.RedactionValues, webhookURL)
		cfg.Alerting.RedactionValues = appendAlertURLTrailingPathSecretValues(cfg.Alerting.RedactionValues, webhookURL, 2)
	} else {
		webhookURL := resolveSecretBestEffort(
			cfg.Alerting.Sinks.Discord.WebhookURL,
			cfg.Alerting.Sinks.Discord.WebhookURLEnv,
			cfg.Alerting.Sinks.Discord.WebhookURLFile,
			"alerting.sinks.discord.webhookURL",
			"alerting.sinks.discord.webhookURLEnv",
			"alerting.sinks.discord.webhookURLFile",
		)
		cfg.Alerting.Sinks.Discord.WebhookURL = webhookURL.Value
		cfg.Alerting.RedactionValues = appendAlertRedactValues(cfg.Alerting.RedactionValues, webhookURL.RedactionValues...)
		cfg.Alerting.RedactionValues = appendAlertURLRedactValues(cfg.Alerting.RedactionValues, webhookURL.RedactionValues...)
		cfg.Alerting.RedactionValues = appendAlertURLTrailingPathSecretValues(cfg.Alerting.RedactionValues, webhookURL.Value, 2)
	}
	if cfg.Alerting.Sinks.HomeAssistant.Enabled {
		baseURL, err := resolveSecret(
			cfg.Alerting.Sinks.HomeAssistant.BaseURL,
			cfg.Alerting.Sinks.HomeAssistant.BaseURLEnv,
			cfg.Alerting.Sinks.HomeAssistant.BaseURLFile,
			"alerting.sinks.homeAssistant.baseURL",
			"alerting.sinks.homeAssistant.baseURLEnv",
			"alerting.sinks.homeAssistant.baseURLFile",
		)
		if err != nil {
			return err
		}
		cfg.Alerting.Sinks.HomeAssistant.BaseURL = baseURL
		cfg.Alerting.Sinks.HomeAssistant.BaseURLEnv = ""
		cfg.Alerting.Sinks.HomeAssistant.BaseURLFile = ""
		cfg.Alerting.RedactionValues = appendAlertURLRedactValues(cfg.Alerting.RedactionValues, baseURL)
		token, err := resolveSecret(
			cfg.Alerting.Sinks.HomeAssistant.Token,
			cfg.Alerting.Sinks.HomeAssistant.TokenEnv,
			cfg.Alerting.Sinks.HomeAssistant.TokenFile,
			"alerting.sinks.homeAssistant.token",
			"alerting.sinks.homeAssistant.tokenEnv",
			"alerting.sinks.homeAssistant.tokenFile",
		)
		if err != nil {
			return err
		}
		cfg.Alerting.Sinks.HomeAssistant.Token = token
		cfg.Alerting.Sinks.HomeAssistant.TokenEnv = ""
		cfg.Alerting.Sinks.HomeAssistant.TokenFile = ""
		cfg.Alerting.RedactionValues = appendAlertRedactValues(cfg.Alerting.RedactionValues, token)
		webhookID, err := resolveSecret(
			cfg.Alerting.Sinks.HomeAssistant.WebhookID,
			cfg.Alerting.Sinks.HomeAssistant.WebhookIDEnv,
			cfg.Alerting.Sinks.HomeAssistant.WebhookIDFile,
			"alerting.sinks.homeAssistant.webhookID",
			"alerting.sinks.homeAssistant.webhookIDEnv",
			"alerting.sinks.homeAssistant.webhookIDFile",
		)
		if err != nil {
			return err
		}
		cfg.Alerting.Sinks.HomeAssistant.WebhookID = webhookID
		cfg.Alerting.Sinks.HomeAssistant.WebhookIDEnv = ""
		cfg.Alerting.Sinks.HomeAssistant.WebhookIDFile = ""
		cfg.Alerting.RedactionValues = appendAlertRedactValues(cfg.Alerting.RedactionValues, webhookID)
	} else {
		baseURL := resolveSecretBestEffort(
			cfg.Alerting.Sinks.HomeAssistant.BaseURL,
			cfg.Alerting.Sinks.HomeAssistant.BaseURLEnv,
			cfg.Alerting.Sinks.HomeAssistant.BaseURLFile,
			"alerting.sinks.homeAssistant.baseURL",
			"alerting.sinks.homeAssistant.baseURLEnv",
			"alerting.sinks.homeAssistant.baseURLFile",
		)
		cfg.Alerting.Sinks.HomeAssistant.BaseURL = baseURL.Value
		cfg.Alerting.RedactionValues = appendAlertURLRedactValues(cfg.Alerting.RedactionValues, baseURL.RedactionValues...)
		token := resolveSecretBestEffort(
			cfg.Alerting.Sinks.HomeAssistant.Token,
			cfg.Alerting.Sinks.HomeAssistant.TokenEnv,
			cfg.Alerting.Sinks.HomeAssistant.TokenFile,
			"alerting.sinks.homeAssistant.token",
			"alerting.sinks.homeAssistant.tokenEnv",
			"alerting.sinks.homeAssistant.tokenFile",
		)
		cfg.Alerting.Sinks.HomeAssistant.Token = token.Value
		cfg.Alerting.RedactionValues = append(cfg.Alerting.RedactionValues, token.RedactionValues...)
		webhookID := resolveSecretBestEffort(
			cfg.Alerting.Sinks.HomeAssistant.WebhookID,
			cfg.Alerting.Sinks.HomeAssistant.WebhookIDEnv,
			cfg.Alerting.Sinks.HomeAssistant.WebhookIDFile,
			"alerting.sinks.homeAssistant.webhookID",
			"alerting.sinks.homeAssistant.webhookIDEnv",
			"alerting.sinks.homeAssistant.webhookIDFile",
		)
		cfg.Alerting.Sinks.HomeAssistant.WebhookID = webhookID.Value
		cfg.Alerting.RedactionValues = append(cfg.Alerting.RedactionValues, webhookID.RedactionValues...)
	}
	return nil
}

func appendAlertRedactValues(values []string, extra ...string) []string {
	for _, value := range extra {
		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func appendAlertURLRedactValues(values []string, urls ...string) []string {
	for _, raw := range urls {
		values = appendAlertRedactValues(values, alertURLRedactionValues(raw)...)
	}
	return values
}

func appendAlertHeaderRedactValues(values []string, name, raw string) []string {
	return appendAlertRedactValues(values, alertHeaderRedactionValues(name, raw)...)
}

func AlertingRedactionValues(alerting AlertingConfig) []string {
	values := []string{}
	values = appendAlertURLRedactValues(values, alerting.Sinks.Webhook.URL)
	for name, value := range alerting.Sinks.Webhook.Headers {
		values = appendAlertHeaderRedactValues(values, name, value)
	}
	values = appendAlertDeclaredURLSecretValues(values, alerting.Sinks.Webhook.URL, alerting.Sinks.Webhook.RedactValues)
	values = appendAlertURLRedactValues(values, alerting.Sinks.Discord.WebhookURL)
	// The Discord webhook URL's trailing id/token path segments are the
	// credential by protocol definition (.../api/webhooks/{id}/{token}), and
	// the URL as a whole is a single-purpose credential too.
	values = appendAlertRedactValues(values, alerting.Sinks.Discord.WebhookURL)
	values = appendAlertURLTrailingPathSecretValues(values, alerting.Sinks.Discord.WebhookURL, 2)
	values = appendAlertRedactValues(values, alerting.Sinks.Discord.RedactValues...)
	values = appendAlertURLRedactValues(values, alerting.Sinks.HomeAssistant.BaseURL)
	values = appendAlertRedactValues(values, alerting.Sinks.HomeAssistant.Token, alerting.Sinks.HomeAssistant.WebhookID)
	values = appendAlertRedactValues(values, alerting.Sinks.HomeAssistant.RedactValues...)
	values = appendAlertRedactValues(values, alerting.RedactionValues...)
	return values
}

// alertURLRedactionValues extracts redaction candidates that can be derived
// deterministically from a URL: embedded userinfo credentials and query
// parameters whose name is a recognized secret name (token, key, secret,
// etc; see isAlertSecretParameterName). It intentionally does not attempt to
// guess which path segments are secret-bearing by inspecting their contents
// (e.g. "looks random") — that kind of entropy heuristic is unreliable (a
// purely numeric or purely lowercase secret looks identical to a benign path
// word) and has historically both missed real secrets and risked redacting
// benign words. Path-embedded secrets for the generic webhook sink must be
// declared explicitly; see appendAlertDeclaredURLSecretValues.
func alertURLRedactionValues(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	values := []string{}
	values = appendAlertRedactValues(values, sanitizer.URLUserinfoValues(raw)...)
	values = appendAlertURLQueryRedactValues(values, parsed.RawQuery)
	for key, queryValues := range parsed.Query() {
		if !isAlertSecretParameterName(key) {
			continue
		}
		for _, value := range queryValues {
			values = appendAlertRedactValues(values, alertEscapedSecretVariants(url.QueryEscape(value), value)...)
		}
	}
	return values
}

// appendAlertURLTrailingPathSecretValues registers the last count path
// segments of a URL as redaction candidates unconditionally, regardless of
// their content. This is not content-based guessing: for a fixed-shape
// webhook URL such as Discord's `.../api/webhooks/{id}/{token}`, the
// trailing segments are secret by protocol definition, so every occurrence
// is a "complete secret-bearing path component" the operator never has to
// declare.
func appendAlertURLTrailingPathSecretValues(values []string, rawURL string, count int) []string {
	if count <= 0 {
		return values
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return values
	}
	segments := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	start := len(segments) - count
	if start < 0 {
		start = 0
	}
	for _, escaped := range segments[start:] {
		if escaped == "" {
			continue
		}
		if decoded, err := url.PathUnescape(escaped); err == nil {
			values = appendAlertRedactValues(values, alertEscapedSecretVariants(escaped, decoded)...)
		}
	}
	return values
}

// appendAlertDeclaredURLSecretValues registers explicitly declared secret
// values (alerting.sinks.webhook.redactValues) as redaction candidates. Each
// declared value is registered as-is plus its path/query-escaped variants; if
// the value also appears (in decoded form) as a literal path or query
// component of rawURL, the exact raw (still-escaped) substring from the URL
// is registered too, so logs containing either the encoded or decoded form
// are covered. This is the explicit, deterministic replacement for automatic
// path-segment secret detection: the operator states what is secret rather
// than the code guessing from character composition.
func appendAlertDeclaredURLSecretValues(values []string, rawURL string, declared []string) []string {
	declaredSet := map[string]struct{}{}
	for _, value := range declared {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		declaredSet[value] = struct{}{}
		values = appendAlertRedactValues(values, alertEscapedSecretVariants(value, value)...)
	}
	if len(declaredSet) == 0 {
		return values
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return values
	}
	for _, escapedPath := range alertEscapedPathVariants(parsed) {
		for _, part := range strings.Split(escapedPath, "/") {
			if part == "" {
				continue
			}
			unescaped, err := url.PathUnescape(part)
			if err != nil {
				continue
			}
			if _, ok := declaredSet[unescaped]; ok {
				values = appendAlertRedactValues(values, alertEscapedSecretVariants(part, unescaped)...)
			}
		}
	}
	for _, part := range strings.Split(parsed.RawQuery, "&") {
		if part == "" {
			continue
		}
		_, valuePart, _ := strings.Cut(part, "=")
		unescaped, err := url.QueryUnescape(valuePart)
		if err != nil {
			continue
		}
		if _, ok := declaredSet[unescaped]; ok {
			values = appendAlertRedactValues(values, alertEscapedSecretVariants(valuePart, unescaped)...)
		}
	}
	return values
}

func alertEscapedPathVariants(parsed *url.URL) []string {
	variants := []string{}
	for _, value := range []string{parsed.RawPath, parsed.EscapedPath()} {
		if value == "" {
			continue
		}
		seen := false
		for _, existing := range variants {
			if existing == value {
				seen = true
				break
			}
		}
		if !seen {
			variants = append(variants, value)
		}
	}
	return variants
}

func appendAlertURLQueryRedactValues(values []string, rawQuery string) []string {
	for _, part := range strings.Split(rawQuery, "&") {
		if part == "" {
			continue
		}
		keyPart, valuePart, _ := strings.Cut(part, "=")
		key, err := url.QueryUnescape(keyPart)
		if err != nil || !isAlertSecretParameterName(key) {
			continue
		}
		value, err := url.QueryUnescape(valuePart)
		if err != nil {
			value = valuePart
		}
		values = appendAlertRedactValues(values, alertEscapedSecretVariants(valuePart, value)...)
	}
	return values
}

func alertEscapedSecretVariants(escaped, decoded string) []string {
	variants := []string{decoded}
	if escaped != "" {
		variants = append(variants, escaped)
	}
	if decoded != "" {
		variants = append(variants, url.PathEscape(decoded), url.QueryEscape(decoded))
	}
	return variants
}

func alertHeaderRedactionValues(name, raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	name = textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
	fields := strings.Fields(raw)
	if strings.EqualFold(name, "Authorization") && len(fields) == 2 {
		return appendAlertRedactValues(nil, raw, fields[1])
	}
	if isAlertSecretParameterName(name) {
		return appendAlertRedactValues(nil, raw)
	}
	return nil
}

func isAlertSecretParameterName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	normalized = strings.ReplaceAll(normalized, "_", "-")
	switch normalized {
	case "access-token", "api-key", "apikey", "auth", "authorization", "bearer", "client-secret", "key", "password", "secret", "signature", "sig", "token", "webhook-token":
		return true
	default:
		return strings.HasSuffix(normalized, "-token") ||
			strings.HasSuffix(normalized, "-secret") ||
			strings.HasSuffix(normalized, "-key")
	}
}

func (cfg *Config) resolveAgentSecrets() error {
	token, err := resolveSecret(
		cfg.Agent.Token,
		cfg.Agent.TokenEnv,
		cfg.Agent.TokenFile,
		"agent.token",
		"agent.tokenEnv",
		"agent.tokenFile",
	)
	if err != nil {
		return err
	}
	cfg.Agent.Token = token
	return nil
}

func resolveSecret(value, envName, filePath, valueField, envField, fileField string) (string, error) {
	sourceCount := 0
	if value != "" {
		sourceCount++
	}
	if envName != "" {
		sourceCount++
	}
	if filePath != "" {
		sourceCount++
	}
	if sourceCount > 1 {
		return "", fmt.Errorf("%s must use only one of %s, %s, or %s", valueField, valueField, envField, fileField)
	}
	if envName != "" {
		return resolveEnvSecret(envField, envName)
	}
	if filePath != "" {
		return resolveFileSecret(fileField, filePath)
	}
	return value, nil
}

type bestEffortSecret struct {
	Value           string
	RedactionValues []string
}

func resolveSecretBestEffort(value, envName, filePath, valueField, envField, fileField string) bestEffortSecret {
	result := bestEffortSecret{Value: value}
	addRedactionValue := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw != "" {
			result.RedactionValues = append(result.RedactionValues, raw)
		}
	}
	addRedactionValue(value)
	if envName != "" {
		if resolved, err := resolveEnvSecret(envField, envName); err == nil {
			if result.Value == "" {
				result.Value = resolved
			}
			addRedactionValue(resolved)
		} else {
			slog.Default().Warn("disabled alerting redaction source unavailable", "field", envField, "source", envName, "error", err)
		}
	}
	if filePath != "" {
		if resolved, err := resolveFileSecret(fileField, filePath); err == nil {
			if result.Value == "" {
				result.Value = resolved
			}
			addRedactionValue(resolved)
		} else {
			slog.Default().Warn("disabled alerting redaction source unavailable", "field", fileField, "source", filePath, "error", err)
		}
	}
	return result
}

func resolveSecretMap(values, envNames, filePaths map[string]string, valueField, envField, fileField string) (map[string]string, error) {
	resolved := map[string]string{}
	sources := map[string]string{}
	for key, value := range values {
		canonicalKey, err := canonicalAlertHeaderName(valueField, key)
		if err != nil {
			return nil, err
		}
		if err := validateAlertHeaderValue(valueField, canonicalKey, value); err != nil {
			return nil, err
		}
		if previous := sources[canonicalKey]; previous != "" {
			return nil, fmt.Errorf("%s[%s] duplicates %s", valueField, key, previous)
		}
		sources[canonicalKey] = fmt.Sprintf("%s[%s]", valueField, key)
		resolved[canonicalKey] = value
	}
	for key, envName := range envNames {
		canonicalKey, err := canonicalAlertHeaderName(envField, key)
		if err != nil {
			return nil, err
		}
		if previous := sources[canonicalKey]; previous != "" {
			return nil, fmt.Errorf("%s[%s] must use only one of %s, %s, or %s (already set by %s)", envField, key, valueField, envField, fileField, previous)
		}
		value, err := resolveEnvSecret(fmt.Sprintf("%s[%s]", envField, key), envName)
		if err != nil {
			return nil, err
		}
		if err := validateAlertHeaderValue(envField, canonicalKey, value); err != nil {
			return nil, err
		}
		sources[canonicalKey] = fmt.Sprintf("%s[%s]", envField, key)
		resolved[canonicalKey] = value
	}
	for key, filePath := range filePaths {
		canonicalKey, err := canonicalAlertHeaderName(fileField, key)
		if err != nil {
			return nil, err
		}
		if previous := sources[canonicalKey]; previous != "" {
			return nil, fmt.Errorf("%s[%s] must use only one of %s, %s, or %s (already set by %s)", fileField, key, valueField, envField, fileField, previous)
		}
		value, err := resolveFileSecret(fmt.Sprintf("%s[%s]", fileField, key), filePath)
		if err != nil {
			return nil, err
		}
		if err := validateAlertHeaderValue(fileField, canonicalKey, value); err != nil {
			return nil, err
		}
		sources[canonicalKey] = fmt.Sprintf("%s[%s]", fileField, key)
		resolved[canonicalKey] = value
	}
	if len(resolved) == 0 {
		return nil, nil
	}
	return resolved, nil
}

func canonicalAlertHeaderName(field, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%s contains an empty key", field)
	}
	for i := 0; i < len(name); i++ {
		if !isAlertHeaderNameByte(name[i]) {
			return "", fmt.Errorf("%s[%s] is not a valid HTTP header name", field, name)
		}
	}
	return textproto.CanonicalMIMEHeaderKey(name), nil
}

func isAlertHeaderNameByte(value byte) bool {
	return (value >= 'A' && value <= 'Z') ||
		(value >= 'a' && value <= 'z') ||
		(value >= '0' && value <= '9') ||
		strings.ContainsRune("!#$%&'*+-.^_`|~", rune(value))
}

func validateAlertHeaderValue(field, name, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s[%s] must not contain CR or LF characters", field, name)
	}
	return nil
}

type bestEffortSecretMap struct {
	Values          map[string]string
	RedactionValues []string
}

func resolveSecretMapBestEffort(values, envNames, filePaths map[string]string, valueField, envField, fileField string) (bestEffortSecretMap, error) {
	result := bestEffortSecretMap{Values: map[string]string{}}
	addRedactionValue := func(key, raw string) {
		result.RedactionValues = appendAlertHeaderRedactValues(result.RedactionValues, key, raw)
	}
	sources := map[string]string{}
	for key, value := range values {
		canonicalKey, err := canonicalAlertHeaderName(valueField, key)
		if err != nil {
			return result, err
		}
		if err := validateAlertHeaderValue(valueField, canonicalKey, value); err != nil {
			return result, err
		}
		if previous := sources[canonicalKey]; previous != "" {
			return result, fmt.Errorf("%s[%s] duplicates %s", valueField, key, previous)
		}
		sources[canonicalKey] = fmt.Sprintf("%s[%s]", valueField, key)
		result.Values[canonicalKey] = value
		addRedactionValue(canonicalKey, value)
	}
	for key, envName := range envNames {
		canonicalKey, err := canonicalAlertHeaderName(envField, key)
		if err != nil {
			return result, err
		}
		if previous := sources[canonicalKey]; previous != "" {
			return result, fmt.Errorf("%s[%s] must use only one of %s, %s, or %s (already set by %s)", envField, key, valueField, envField, fileField, previous)
		}
		if value, err := resolveEnvSecret(fmt.Sprintf("%s[%s]", envField, key), envName); err == nil {
			if err := validateAlertHeaderValue(envField, canonicalKey, value); err != nil {
				return result, err
			}
			if _, ok := result.Values[canonicalKey]; !ok {
				result.Values[canonicalKey] = value
			}
			sources[canonicalKey] = fmt.Sprintf("%s[%s]", envField, key)
			addRedactionValue(canonicalKey, value)
		} else {
			slog.Default().Warn("disabled alerting redaction source unavailable", "field", fmt.Sprintf("%s[%s]", envField, key), "source", envName, "error", err)
		}
	}
	for key, filePath := range filePaths {
		canonicalKey, err := canonicalAlertHeaderName(fileField, key)
		if err != nil {
			return result, err
		}
		if previous := sources[canonicalKey]; previous != "" {
			return result, fmt.Errorf("%s[%s] must use only one of %s, %s, or %s (already set by %s)", fileField, key, valueField, envField, fileField, previous)
		}
		if value, err := resolveFileSecret(fmt.Sprintf("%s[%s]", fileField, key), filePath); err == nil {
			if err := validateAlertHeaderValue(fileField, canonicalKey, value); err != nil {
				return result, err
			}
			if _, ok := result.Values[canonicalKey]; !ok {
				result.Values[canonicalKey] = value
			}
			sources[canonicalKey] = fmt.Sprintf("%s[%s]", fileField, key)
			addRedactionValue(canonicalKey, value)
		} else {
			slog.Default().Warn("disabled alerting redaction source unavailable", "field", fmt.Sprintf("%s[%s]", fileField, key), "source", filePath, "error", err)
		}
	}
	if len(result.Values) == 0 {
		result.Values = nil
	}
	return result, nil
}

func resolveEnvSecret(field, envName string) (string, error) {
	value, ok := os.LookupEnv(envName)
	if !ok || value == "" {
		return "", fmt.Errorf("%s references unset env %s", field, envName)
	}
	return value, nil
}

func resolveFileSecret(field, filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("read %s %s: %w", field, filePath, err)
	}
	value := string(bytesTrimSpace(data))
	if value == "" {
		return "", fmt.Errorf("%s %s is empty", field, filePath)
	}
	return value, nil
}

func (cfg Config) Validate() error {
	if cfg.Auth.Mode != "basic" && cfg.Auth.Mode != "dev-no-auth" {
		return fmt.Errorf("auth.mode must be basic or dev-no-auth")
	}
	if cfg.Auth.Mode == "basic" && len(cfg.Auth.Users) == 0 {
		return fmt.Errorf("auth.users must be configured when basic auth is enabled")
	}
	for _, user := range cfg.Auth.Users {
		if user.Username == "" || user.PasswordHash == "" {
			return fmt.Errorf("auth users require username and passwordHash")
		}
	}
	for _, repo := range cfg.Repositories {
		if repo.Name == "" || repo.URL == "" {
			return fmt.Errorf("repositories require name and url")
		}
		if repo.TokenEnv != "" && os.Getenv(repo.TokenEnv) == "" {
			return fmt.Errorf("repository %s references unset token env %s", repo.Name, repo.TokenEnv)
		}
		if _, err := repo.ScanDuration(); err != nil {
			return err
		}
	}
	if _, err := cfg.DefaultInterval(); err != nil {
		return err
	}
	for _, target := range cfg.Runtime.Docker {
		if _, err := target.IntervalDuration(); err != nil {
			return err
		}
	}
	for _, target := range cfg.Runtime.Kubernetes {
		if _, err := target.IntervalDuration(); err != nil {
			return err
		}
	}
	for _, target := range cfg.Runtime.HTTP {
		if _, err := target.IntervalDuration(); err != nil {
			return err
		}
		if _, err := target.TimeoutDuration(); err != nil {
			return err
		}
		if _, err := target.EgressPolicy(); err != nil {
			return err
		}
	}
	repositoryNames := map[string]struct{}{}
	for _, repo := range cfg.Repositories {
		repositoryNames[repo.Name] = struct{}{}
	}
	for _, target := range cfg.Runtime.Ping {
		if target.Host == "" && target.AnsibleInventory == "" {
			return fmt.Errorf("runtime.ping target requires host or ansibleInventory")
		}
		if target.Repository != "" && target.AnsibleInventory == "" {
			return fmt.Errorf("runtime.ping repository requires ansibleInventory")
		}
		if target.AnsibleInventory != "" {
			if target.Repository == "" {
				return fmt.Errorf("runtime.ping ansibleInventory requires repository")
			}
			if _, ok := repositoryNames[target.Repository]; !ok {
				return fmt.Errorf("runtime.ping repository %q is not defined in repositories", target.Repository)
			}
			if err := validateRepoRelativePath("runtime.ping.ansibleInventory", target.AnsibleInventory); err != nil {
				return err
			}
		}
		if _, err := target.IntervalDuration(); err != nil {
			return err
		}
		if _, err := target.TimeoutDuration(); err != nil {
			return err
		}
		if _, err := target.MinRefreshDuration(); err != nil {
			return err
		}
	}
	if err := cfg.validateUniqueMonitorTargetNames(); err != nil {
		return err
	}
	if err := cfg.Alerting.Validate(); err != nil {
		return err
	}
	return nil
}

func (cfg Config) validateUniqueMonitorTargetNames() error {
	seen := map[string]string{}
	register := func(kind, name string) error {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("runtime monitor target identity is empty")
		}
		if strings.Contains(name, ": ") {
			return fmt.Errorf("runtime monitor target identity uses reserved delimiter")
		}
		if previous, ok := seen[name]; ok {
			sum := sha256.Sum256([]byte(name))
			return fmt.Errorf("runtime monitor target identity duplicates %s target (digest=%s length=%d)", previous, hex.EncodeToString(sum[:4]), len(name))
		}
		seen[name] = kind
		return nil
	}
	for _, target := range cfg.Runtime.Docker {
		if err := register("docker", target.Name); err != nil {
			return err
		}
	}
	for _, target := range cfg.Runtime.Kubernetes {
		if err := register("kubernetes", target.Name); err != nil {
			return err
		}
	}
	for _, target := range cfg.Runtime.HTTP {
		name := target.Name
		if name == "" {
			name = "routes"
		}
		if err := register("http", name); err != nil {
			return err
		}
	}
	for _, target := range cfg.Runtime.Ping {
		if err := register("ping", target.EffectiveName()); err != nil {
			return err
		}
	}
	return nil
}

func (cfg Config) ValidateAgent() error {
	if cfg.Agent.ServerURL == "" {
		return fmt.Errorf("agent.serverUrl is required")
	}
	if cfg.Agent.Target == "" {
		return fmt.Errorf("agent.target is required")
	}
	if cfg.Agent.Token == "" {
		return fmt.Errorf("agent.token, agent.tokenEnv, or agent.tokenFile is required")
	}
	if _, err := cfg.Agent.IntervalDuration(); err != nil {
		return err
	}
	return nil
}

func (cfg Config) DefaultInterval() (time.Duration, error) {
	interval, err := time.ParseDuration(cfg.Monitoring.DefaultInterval)
	if err != nil {
		return 0, fmt.Errorf("monitoring.defaultInterval: %w", err)
	}
	return interval, nil
}

func (cfg AlertingConfig) Enabled() bool {
	return cfg.Sinks.Webhook.Enabled || cfg.Sinks.Discord.Enabled || cfg.Sinks.HomeAssistant.Enabled
}

func (cfg AlertingConfig) Validate() error {
	if cfg.StabilitySamples <= 0 {
		return fmt.Errorf("alerting.stabilitySamples must be greater than zero")
	}
	if _, err := cfg.DebounceDuration(); err != nil {
		return err
	}
	if _, err := cfg.CooldownDuration(); err != nil {
		return err
	}
	if cfg.Retry.MaxAttempts <= 0 {
		return fmt.Errorf("alerting.retry.maxAttempts must be greater than zero")
	}
	initial, err := cfg.RetryInitialIntervalDuration()
	if err != nil {
		return err
	}
	maximum, err := cfg.RetryMaxIntervalDuration()
	if err != nil {
		return err
	}
	if maximum < initial {
		return fmt.Errorf("alerting.retry.maxInterval must be greater than or equal to alerting.retry.initialInterval")
	}
	if _, err := cfg.RetentionHorizonDuration(); err != nil {
		return err
	}
	if _, err := cfg.RetentionIntervalDuration(); err != nil {
		return err
	}
	if cfg.Retention.BatchSize <= 0 {
		return fmt.Errorf("alerting.retention.batchSize must be greater than zero")
	}
	if err := cfg.validateSinkNames(); err != nil {
		return err
	}
	if cfg.Sinks.Webhook.Enabled {
		if err := cfg.Sinks.Webhook.Validate(); err != nil {
			return err
		}
	}
	if cfg.Sinks.Discord.Enabled {
		if err := cfg.Sinks.Discord.Validate(); err != nil {
			return err
		}
	}
	if cfg.Sinks.HomeAssistant.Enabled {
		if err := cfg.Sinks.HomeAssistant.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// EnabledSinkNames returns validated, canonical identities declared by the
// configured sink set. Disabled sinks remain valid historical identities; the
// delivery layer decides whether they are active.
func (cfg AlertingConfig) EnabledSinkNames() []string {
	return []string{
		alertSinkName("webhook", cfg.Sinks.Webhook.Name),
		alertSinkName("discord", cfg.Sinks.Discord.Name),
		alertSinkName("home-assistant", cfg.Sinks.HomeAssistant.Name),
	}
}

// ActiveSinkNames returns only configured delivery destinations. Historical
// disabled identities remain in EnabledSinkNames for storage validation.
func (cfg AlertingConfig) ActiveSinkNames() []string {
	active := []string{}
	if cfg.Sinks.Webhook.Enabled {
		active = append(active, alertSinkName("webhook", cfg.Sinks.Webhook.Name))
	}
	if cfg.Sinks.Discord.Enabled {
		active = append(active, alertSinkName("discord", cfg.Sinks.Discord.Name))
	}
	if cfg.Sinks.HomeAssistant.Enabled {
		active = append(active, alertSinkName("home-assistant", cfg.Sinks.HomeAssistant.Name))
	}
	return active
}

func (cfg AlertingConfig) validateSinkNames() error {
	sinks := []struct {
		field       string
		defaultName string
		name        string
	}{
		{field: "alerting.sinks.webhook.name", defaultName: "webhook", name: cfg.Sinks.Webhook.Name},
		{field: "alerting.sinks.discord.name", defaultName: "discord", name: cfg.Sinks.Discord.Name},
		{field: "alerting.sinks.homeAssistant.name", defaultName: "home-assistant", name: cfg.Sinks.HomeAssistant.Name},
	}
	seen := map[string]string{}
	for _, sink := range sinks {
		name := alertSinkName(sink.defaultName, sink.name)
		if name == "" {
			return fmt.Errorf("%s must not be empty", sink.field)
		}
		if previous, ok := seen[name]; ok {
			return fmt.Errorf("%s %q duplicates %s", sink.field, name, previous)
		}
		seen[name] = sink.field
		if secret := alertSinkNameSecretMatch(name, cfg.RedactionValues); secret != "" {
			return fmt.Errorf("%s must not contain configured secret values", sink.field)
		}
	}
	return nil
}

func alertSinkName(defaultName, configured string) string {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return defaultName
	}
	return configured
}

func alertSinkNameSecretMatch(name string, secrets []string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret == "" {
			continue
		}
		for _, variant := range alertSecretNameVariants(secret) {
			if variant != "" && strings.Contains(name, variant) {
				return secret
			}
		}
	}
	return ""
}

func alertSecretNameVariants(secret string) []string {
	hexValue := hex.EncodeToString([]byte(secret))
	variants := []string{
		secret,
		url.PathEscape(secret),
		strings.ToLower(url.PathEscape(secret)),
		strings.ToUpper(url.PathEscape(secret)),
		url.QueryEscape(secret),
		strings.ToLower(url.QueryEscape(secret)),
		strings.ToUpper(url.QueryEscape(secret)),
		base64.StdEncoding.EncodeToString([]byte(secret)),
		base64.RawStdEncoding.EncodeToString([]byte(secret)),
		base64.URLEncoding.EncodeToString([]byte(secret)),
		base64.RawURLEncoding.EncodeToString([]byte(secret)),
		hexValue,
		strings.ToUpper(hexValue),
	}
	seen := map[string]struct{}{}
	deduped := make([]string, 0, len(variants))
	for _, variant := range variants {
		if variant == "" {
			continue
		}
		if _, ok := seen[variant]; ok {
			continue
		}
		seen[variant] = struct{}{}
		deduped = append(deduped, variant)
	}
	return deduped
}

func (cfg AlertingConfig) DebounceDuration() (time.Duration, error) {
	return requiredPositiveDuration(cfg.Debounce, "alerting.debounce")
}

func (cfg AlertingConfig) CooldownDuration() (time.Duration, error) {
	return requiredPositiveDuration(cfg.Cooldown, "alerting.cooldown")
}

func (cfg AlertingConfig) RetryInitialIntervalDuration() (time.Duration, error) {
	return requiredPositiveDuration(cfg.Retry.InitialInterval, "alerting.retry.initialInterval")
}

func (cfg AlertingConfig) RetryMaxIntervalDuration() (time.Duration, error) {
	return requiredPositiveDuration(cfg.Retry.MaxInterval, "alerting.retry.maxInterval")
}

func (cfg AlertingConfig) RetentionHorizonDuration() (time.Duration, error) {
	return requiredPositiveDuration(cfg.Retention.Horizon, "alerting.retention.horizon")
}

func (cfg AlertingConfig) RetentionIntervalDuration() (time.Duration, error) {
	return requiredPositiveDuration(cfg.Retention.Interval, "alerting.retention.interval")
}

func (sink WebhookAlertSinkConfig) Validate() error {
	if err := validateHTTPURL("alerting.sinks.webhook.url", sink.URL); err != nil {
		return err
	}
	if parsed, _ := url.Parse(sink.URL); parsed != nil && parsed.Scheme == "http" && !sink.InsecureAllowHTTP {
		return fmt.Errorf("alerting.sinks.webhook.url must use https unless alerting.sinks.webhook.insecureAllowHTTP is true")
	}
	switch sink.Method {
	case "GET", "POST", "PUT", "PATCH":
	default:
		return fmt.Errorf("alerting.sinks.webhook.method must be one of GET, POST, PUT, or PATCH")
	}
	if _, err := sink.TimeoutDuration(); err != nil {
		return err
	}
	if err := validateAlertHeaderSources(sink.Headers, sink.HeaderEnv, sink.HeaderFile, "alerting.sinks.webhook.headers", "alerting.sinks.webhook.headerEnv", "alerting.sinks.webhook.headerFile"); err != nil {
		return err
	}
	return nil
}

func validateAlertHeaderSources(values, envNames, filePaths map[string]string, valueField, envField, fileField string) error {
	seen := map[string]string{}
	for key, value := range values {
		canonicalKey, err := canonicalAlertHeaderName(valueField, key)
		if err != nil {
			return err
		}
		if err := validateAlertHeaderValue(valueField, canonicalKey, value); err != nil {
			return err
		}
		if previous := seen[canonicalKey]; previous != "" {
			return fmt.Errorf("%s[%s] duplicates %s", valueField, key, previous)
		}
		seen[canonicalKey] = fmt.Sprintf("%s[%s]", valueField, key)
	}
	for key := range envNames {
		canonicalKey, err := canonicalAlertHeaderName(envField, key)
		if err != nil {
			return err
		}
		if previous := seen[canonicalKey]; previous != "" {
			return fmt.Errorf("%s[%s] must use only one of %s, %s, or %s (already set by %s)", envField, key, valueField, envField, fileField, previous)
		}
		seen[canonicalKey] = fmt.Sprintf("%s[%s]", envField, key)
	}
	for key := range filePaths {
		canonicalKey, err := canonicalAlertHeaderName(fileField, key)
		if err != nil {
			return err
		}
		if previous := seen[canonicalKey]; previous != "" {
			return fmt.Errorf("%s[%s] must use only one of %s, %s, or %s (already set by %s)", fileField, key, valueField, envField, fileField, previous)
		}
		seen[canonicalKey] = fmt.Sprintf("%s[%s]", fileField, key)
	}
	return nil
}

func (sink WebhookAlertSinkConfig) TimeoutDuration() (time.Duration, error) {
	return requiredPositiveDuration(sink.Timeout, "alerting.sinks.webhook.timeout")
}

func (sink DiscordAlertSinkConfig) Validate() error {
	if err := validateHTTPURL("alerting.sinks.discord.webhookURL", sink.WebhookURL); err != nil {
		return err
	}
	parsed, _ := url.Parse(sink.WebhookURL)
	if parsed != nil && parsed.Scheme != "https" {
		return fmt.Errorf("alerting.sinks.discord.webhookURL must use https")
	}
	if _, err := sink.TimeoutDuration(); err != nil {
		return err
	}
	return nil
}

func (sink DiscordAlertSinkConfig) TimeoutDuration() (time.Duration, error) {
	return requiredPositiveDuration(sink.Timeout, "alerting.sinks.discord.timeout")
}

func (sink HomeAssistantAlertSinkConfig) Validate() error {
	if err := validateHTTPURL("alerting.sinks.homeAssistant.baseURL", sink.BaseURL); err != nil {
		return err
	}
	if parsed, _ := url.Parse(sink.BaseURL); parsed != nil && parsed.Scheme == "http" && !sink.InsecureAllowHTTP {
		return fmt.Errorf("alerting.sinks.homeAssistant.baseURL must use https unless alerting.sinks.homeAssistant.insecureAllowHTTP is true")
	}
	if strings.TrimSpace(sink.Token) == "" {
		return fmt.Errorf("alerting.sinks.homeAssistant.token, tokenEnv, or tokenFile is required")
	}
	if strings.TrimSpace(sink.WebhookID) == "" {
		return fmt.Errorf("alerting.sinks.homeAssistant.webhookID, webhookIDEnv, or webhookIDFile is required")
	}
	if _, err := sink.TimeoutDuration(); err != nil {
		return err
	}
	return nil
}

func (sink HomeAssistantAlertSinkConfig) TimeoutDuration() (time.Duration, error) {
	return requiredPositiveDuration(sink.Timeout, "alerting.sinks.homeAssistant.timeout")
}

func (target DockerTarget) CheckInterval(defaultInterval time.Duration) time.Duration {
	interval, err := target.IntervalDuration()
	if err != nil || interval == 0 {
		return defaultInterval
	}
	return interval
}

func (target DockerTarget) IntervalDuration() (time.Duration, error) {
	return optionalPositiveDuration(target.Interval, "runtime.docker.interval")
}

func (target KubernetesTarget) CheckInterval(defaultInterval time.Duration) time.Duration {
	interval, err := target.IntervalDuration()
	if err != nil || interval == 0 {
		return defaultInterval
	}
	return interval
}

func (target KubernetesTarget) IntervalDuration() (time.Duration, error) {
	return optionalPositiveDuration(target.Interval, "runtime.kubernetes.interval")
}

func (target HTTPRouteTarget) CheckInterval(defaultInterval time.Duration) time.Duration {
	interval, err := target.IntervalDuration()
	if err != nil || interval == 0 {
		return defaultInterval
	}
	return interval
}

func (target HTTPRouteTarget) IntervalDuration() (time.Duration, error) {
	return optionalPositiveDuration(target.Interval, "runtime.http.interval")
}

func (target HTTPRouteTarget) TimeoutDuration() (time.Duration, error) {
	return optionalPositiveDuration(target.Timeout, "runtime.http.timeout")
}

func (target HTTPRouteTarget) EgressPolicy() (routetarget.EgressPolicy, error) {
	policy, err := routetarget.NewEgressPolicy(routetarget.EgressPolicyConfig{
		AllowDomains: target.Egress.Allow.Domains,
		AllowCIDRs:   target.Egress.Allow.CIDRs,
		DenyDomains:  target.Egress.Deny.Domains,
		DenyCIDRs:    target.Egress.Deny.CIDRs,
	})
	if err != nil {
		return routetarget.EgressPolicy{}, fmt.Errorf("runtime.http.egress: %w", err)
	}
	return policy, nil
}

func (target PingTarget) EffectiveName() string {
	if target.Name != "" {
		return target.Name
	}
	if target.Host != "" {
		return target.Host
	}
	if target.AnsibleInventory != "" {
		name := filepath.Base(filepath.FromSlash(target.AnsibleInventory))
		if name != "." && name != string(filepath.Separator) {
			return name
		}
	}
	return "hosts"
}

func (target PingTarget) CheckInterval(defaultInterval time.Duration) time.Duration {
	interval, err := target.IntervalDuration()
	if err != nil || interval == 0 {
		return defaultInterval
	}
	return interval
}

func (target PingTarget) IntervalDuration() (time.Duration, error) {
	return optionalPositiveDuration(target.Interval, "runtime.ping.interval")
}

func (target PingTarget) TimeoutDuration() (time.Duration, error) {
	return optionalPositiveDuration(target.Timeout, "runtime.ping.timeout")
}

func (target PingTarget) MinRefreshDuration() (time.Duration, error) {
	return optionalPositiveDuration(target.MinRefreshInterval, "runtime.ping.minRefreshInterval")
}

func (cfg AgentConfig) IntervalDuration() (time.Duration, error) {
	return optionalPositiveDuration(cfg.Interval, "agent.interval")
}

func (cfg RepositoryConfig) ScanDuration() (time.Duration, error) {
	return optionalPositiveDuration(cfg.ScanInterval, fmt.Sprintf("repository %s scanInterval", cfg.Name))
}

func optionalPositiveDuration(value, field string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	if interval <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", field)
	}
	return interval, nil
}

func requiredPositiveDuration(value, field string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, fmt.Errorf("%s is required", field)
	}
	return optionalPositiveDuration(value, field)
}

func validateHTTPURL(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%s must be a valid http or https URL", field)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s must be a valid http or https URL", field)
	}
	return nil
}

func validateRepoRelativePath(field, value string) error {
	if filepath.IsAbs(value) {
		return fmt.Errorf("%s must be a repository-relative path", field)
	}
	cleaned := filepath.Clean(filepath.FromSlash(value))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s must stay inside the repository", field)
	}
	return nil
}

func (cfg RepositoryConfig) Token() (string, error) {
	if cfg.TokenEnv != "" {
		return os.Getenv(cfg.TokenEnv), nil
	}
	if cfg.TokenFile == "" {
		return "", nil
	}
	data, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read token file for repository %s: %w", cfg.Name, err)
	}
	return string(bytesTrimSpace(data)), nil
}

func bytesTrimSpace(data []byte) []byte {
	start := 0
	for start < len(data) && (data[start] == ' ' || data[start] == '\n' || data[start] == '\r' || data[start] == '\t') {
		start++
	}
	end := len(data)
	for end > start && (data[end-1] == ' ' || data[end-1] == '\n' || data[end-1] == '\r' || data[end-1] == '\t') {
		end--
	}
	return data[start:end]
}
