package scanner

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/storage"
)

func TestScanAllClonesAndParsesFixtureRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := createFixtureRepo(t)
	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Config{
		Server: config.ServerConfig{
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(dataDir, "repos"),
		},
		Auth: config.AuthConfig{Mode: "dev-no-auth"},
		Repositories: []config.RepositoryConfig{{
			Name:       "fixture",
			URL:        "file://" + source,
			DefaultRef: "main",
		}},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	if err := scanner.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Repositories) != 1 || summary.Repositories[0].LastCommit == "" {
		t.Fatalf("repositories = %#v", summary.Repositories)
	}
	if len(summary.Services) != 2 {
		t.Fatalf("services = %#v, want compose and kubernetes services", summary.Services)
	}
	runtimes := map[string]bool{}
	for _, service := range summary.Services {
		runtimes[service.Runtime] = true
		if service.Environment != "production" {
			t.Fatalf("service %s environment = %q, want production", service.Name, service.Environment)
		}
	}
	if !runtimes["compose"] || !runtimes["kubernetes"] {
		t.Fatalf("runtimes = %#v", runtimes)
	}
}

func TestCloneURLInjectsTokenWithoutMutatingConfig(t *testing.T) {
	t.Setenv("GITOPS_TEST_TOKEN", "secret-token")
	cfg := config.Config{}
	scanner := New(cfg, nil, slog.Default())
	repo := config.RepositoryConfig{
		Name:     "repo",
		URL:      "https://github.com/example/repo.git",
		TokenEnv: "GITOPS_TEST_TOKEN",
	}
	cloneURL, err := scanner.cloneURL(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cloneURL, "x-access-token:secret-token") {
		t.Fatalf("cloneURL = %q", cloneURL)
	}
	if repo.URL != "https://github.com/example/repo.git" {
		t.Fatalf("repo URL mutated: %q", repo.URL)
	}
}

func TestGitEnvUsesSSHKey(t *testing.T) {
	env := gitEnv(config.RepositoryConfig{
		SSHKeyPath: "/tmp/key",
		KnownHosts: "/tmp/known_hosts",
	})
	if len(env) != 1 || !strings.Contains(env[0], "GIT_SSH_COMMAND=ssh") {
		t.Fatalf("env = %#v", env)
	}
	if !strings.Contains(env[0], "UserKnownHostsFile") {
		t.Fatalf("known hosts missing: %#v", env)
	}
}

func createFixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	writeFile(t, filepath.Join(dir, "prod", "compose.yaml"), `
services:
  web:
    image: example/web:v1
    ports:
      - "8080:80"
`)
	writeFile(t, filepath.Join(dir, "prod", "app.yaml"), `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
        - name: api
          image: example/api:v1
          readinessProbe:
            httpGet:
              path: /health
              port: 8080
          livenessProbe:
            httpGet:
              path: /health
              port: 8080
`)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "fixture")
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
}
