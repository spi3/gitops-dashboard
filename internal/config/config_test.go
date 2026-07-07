package config

import (
	"os"
	"path/filepath"
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
