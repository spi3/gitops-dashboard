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
	servicesByName := map[string][]string{}
	for _, service := range summary.Services {
		servicesByName[service.Name] = service.Exposure
	}
	if !contains(servicesByName["web"], "https://web.example.test") {
		t.Fatalf("web exposure = %v, want traefik route", servicesByName["web"])
	}
	if !contains(servicesByName["api"], "https://api.example.test/") {
		t.Fatalf("api exposure = %v, want ingress route", servicesByName["api"])
	}
	if !contains(servicesByName["api"], "https://api-alt.example.test") {
		t.Fatalf("api exposure = %v, want traefik route", servicesByName["api"])
	}
}

func TestScanAllUsesRelativeRepoCacheFromWorkingDirectory(t *testing.T) {
	ctx := context.Background()
	source := createFixtureRepo(t)
	workDir := t.TempDir()
	t.Chdir(workDir)
	store, err := storage.Open(filepath.Join("data", "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Config{
		Server: config.ServerConfig{
			DataDir:      "data",
			RepoCacheDir: filepath.Join("data", "repos"),
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
	if _, err := os.Stat(filepath.Join(workDir, "data", "repos", "fixture", ".git")); err != nil {
		t.Fatalf("expected clone in configured cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "data", "repos", "data")); err == nil {
		t.Fatalf("scanner nested relative repo cache under itself")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestScanAllHonorsRepositoryPathFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := createFixtureRepo(t)
	writeFile(t, filepath.Join(source, "dev", "compose.yaml"), `
services:
  dev:
    image: example/dev:v1
`)
	writeFile(t, filepath.Join(source, "prod", "ignored", "compose.yaml"), `
services:
  ignored:
    image: example/ignored:v1
`)
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "add filtered services")

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
			Name:         "fixture",
			URL:          "file://" + source,
			DefaultRef:   "main",
			IncludePaths: []string{"prod"},
			ExcludePaths: []string{"prod/ignored", "prod/dynamic.yaml"},
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
	servicesByName := map[string][]string{}
	for _, service := range summary.Services {
		servicesByName[service.Name] = service.Exposure
	}
	if _, ok := servicesByName["dev"]; ok {
		t.Fatalf("dev service was scanned despite includePaths: %#v", servicesByName)
	}
	if _, ok := servicesByName["ignored"]; ok {
		t.Fatalf("ignored service was scanned despite excludePaths: %#v", servicesByName)
	}
	if !contains(servicesByName["web"], "https://web.example.test") {
		t.Fatalf("web exposure = %v, want included compose route", servicesByName["web"])
	}
	if contains(servicesByName["api"], "https://api-alt.example.test") {
		t.Fatalf("api exposure = %v, excluded traefik file still contributed route", servicesByName["api"])
	}
}

func TestRepositoryPathFilters(t *testing.T) {
	t.Parallel()
	repo := config.RepositoryConfig{
		IncludePaths: []string{"docker_files/serenity", "clusters/main/**/*.yaml"},
		ExcludePaths: []string{"docker_files/serenity/retired", "**/gotk-components.yaml"},
	}
	cases := []struct {
		path string
		want bool
	}{
		{path: "docker_files/serenity/gt/docker-compose.yml", want: true},
		{path: "docker_files/hd3-docker/gt/docker-compose.yml", want: false},
		{path: "docker_files/serenity/retired/app/docker-compose.yml", want: false},
		{path: "clusters/main/default/app.yaml", want: true},
		{path: "clusters/main/flux-system/gotk-components.yaml", want: false},
	}
	for _, tc := range cases {
		if got := shouldScanRepoPath(repo, tc.path); got != tc.want {
			t.Fatalf("shouldScanRepoPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
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
    labels:
      - "traefik.http.routers.web.rule=Host('web.example.test')"
`)
	writeFile(t, filepath.Join(dir, "prod", "app.yaml"), `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
  labels:
    app: api
spec:
  selector:
    matchLabels:
      app: api
  template:
    metadata:
      labels:
        app: api
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
---
apiVersion: v1
kind: Service
metadata:
  name: api
  namespace: prod
spec:
  selector:
    app: api
  ports:
    - port: 80
      targetPort: 8080
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: api
  namespace: prod
spec:
  rules:
    - host: api.example.test
      http:
        paths:
          - path: /
            backend:
              service:
                name: api
`)
	writeFile(t, filepath.Join(dir, "prod", "dynamic.yaml"), `
http:
  routers:
    api:
      rule: Host(`+"`"+`api-alt.example.test`+"`"+`)
      service: api
`)
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "fixture")
	return dir
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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
