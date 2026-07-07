package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server       ServerConfig       `yaml:"server"`
	Auth         AuthConfig         `yaml:"auth"`
	Repositories []RepositoryConfig `yaml:"repositories"`
	Runtime      RuntimeConfig      `yaml:"runtime"`
	Monitoring   MonitoringConfig   `yaml:"monitoring"`
	Agent        AgentConfig        `yaml:"agent"`
}

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
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"passwordHash"`
}

type AgentAuthCfg struct {
	Tokens []string `yaml:"tokens"`
}

type RepositoryConfig struct {
	Name         string `yaml:"name"`
	URL          string `yaml:"url"`
	DefaultRef   string `yaml:"defaultRef"`
	Type         string `yaml:"type"`
	TokenFile    string `yaml:"tokenFile"`
	TokenEnv     string `yaml:"tokenEnv"`
	SSHKeyPath   string `yaml:"sshKeyPath"`
	KnownHosts   string `yaml:"knownHosts"`
	ScanInterval string `yaml:"scanInterval"`
}

type RuntimeConfig struct {
	Docker     []DockerTarget     `yaml:"docker"`
	Kubernetes []KubernetesTarget `yaml:"kubernetes"`
	HTTP       []HTTPRouteTarget  `yaml:"http"`
}

type DockerTarget struct {
	Name       string `yaml:"name"`
	Kind       string `yaml:"kind"`
	Host       string `yaml:"host"`
	AgentToken string `yaml:"agentToken"`
	Interval   string `yaml:"interval"`
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

type MonitoringConfig struct {
	DefaultInterval string `yaml:"defaultInterval"`
}

type AgentConfig struct {
	ServerURL string       `yaml:"serverUrl"`
	Target    string       `yaml:"target"`
	Token     string       `yaml:"token"`
	Interval  string       `yaml:"interval"`
	Docker    DockerTarget `yaml:"docker"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (cfg *Config) applyDefaults() {
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
