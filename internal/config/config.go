package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var configEnvPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Auth         AuthConfig         `yaml:"auth"`
	Repositories []RepositoryConfig `yaml:"repositories"`
	Runtime      RuntimeConfig      `yaml:"runtime"`
	Monitoring   MonitoringConfig   `yaml:"monitoring"`
	Agent        AgentConfig        `yaml:"agent"`
}

const (
	ModeServer = "server"
	ModeAgent  = "agent"
)

type ServerConfig struct {
	Listen       string `yaml:"listen"`
	DataDir      string `yaml:"dataDir"`
	RepoCacheDir string `yaml:"repoCacheDir"`
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
	Tokens    []string `yaml:"tokens"`
	TokenFile string   `yaml:"tokenFile"`
	TokenEnv  string   `yaml:"tokenEnv"`
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
	Name     string `yaml:"name"`
	Interval string `yaml:"interval"`
	Timeout  string `yaml:"timeout"`
}

type PingTarget struct {
	Name             string `yaml:"name"`
	Host             string `yaml:"host"`
	Repository       string `yaml:"repository"`
	AnsibleInventory string `yaml:"ansibleInventory"`
	Interval         string `yaml:"interval"`
	Timeout          string `yaml:"timeout"`
	Environment      string `yaml:"environment"`
}

type MonitoringConfig struct {
	DefaultInterval string `yaml:"defaultInterval"`
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
	for i := range cfg.Repositories {
		if cfg.Repositories[i].DefaultRef == "" {
			cfg.Repositories[i].DefaultRef = "HEAD"
		}
	}
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
	return nil
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
