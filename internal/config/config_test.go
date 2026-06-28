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
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with invalid scanInterval")
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
