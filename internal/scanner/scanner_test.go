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
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/sanitizer"
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
		if service.Runtime == "compose" && service.ComposeProject != "prod-stack" {
			t.Fatalf("compose project = %q, want prod-stack", service.ComposeProject)
		}
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

func TestScanAllDiscoversPingHostsFromConfiguredRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := createFixtureRepo(t)
	writeFile(t, filepath.Join(source, "infrastructure", "inventory", "hosts.yml"), `
all:
  hosts:
    serenity:
      ansible_host: serenity.lan
`)
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "add inventory")

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
		Runtime: config.RuntimeConfig{
			Ping: []config.PingTarget{{
				Name:             "homelab",
				Repository:       "fixture",
				AnsibleInventory: "infrastructure/inventory/hosts.yml",
			}},
		},
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
	var host core.Service
	for _, service := range summary.Services {
		if service.Runtime == "host" {
			host = service
			break
		}
	}
	if host.Name != "serenity" || host.Repository != "fixture" || host.SourcePath != "infrastructure/inventory/hosts.yml" || host.ResourceName != "serenity.lan" {
		t.Fatalf("host service = %#v", host)
	}
	if host.SourceCommit == "" {
		t.Fatal("host SourceCommit is empty")
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

func TestGitAuthUsesTokenFreeRemoteAndEnvScopedHeader(t *testing.T) {
	t.Setenv("GITOPS_TEST_TOKEN", "secret-token")
	cfg := config.Config{}
	scanner := New(cfg, nil, slog.Default())
	repo := config.RepositoryConfig{
		Name:     "repo",
		URL:      "https://github.com/example/repo.git",
		TokenEnv: "GITOPS_TEST_TOKEN",
	}
	auth, err := scanner.gitAuth(repo)
	if err != nil {
		t.Fatal(err)
	}
	if auth.remoteURL != "https://github.com/example/repo.git" {
		t.Fatalf("remoteURL = %q", auth.remoteURL)
	}
	env := strings.Join(auth.env, "\n")
	if strings.Contains(env, "secret-token") {
		t.Fatalf("env contains raw token: %#v", auth.env)
	}
	if !strings.Contains(env, "GIT_CONFIG_KEY_0=http.https://github.com/example/repo.git.extraHeader") {
		t.Fatalf("extraHeader key missing: %#v", auth.env)
	}
	if !strings.Contains(env, "GIT_CONFIG_VALUE_0=Authorization: Basic ") {
		t.Fatalf("extraHeader value missing: %#v", auth.env)
	}
	if repo.URL != "https://github.com/example/repo.git" {
		t.Fatalf("repo URL mutated: %q", repo.URL)
	}
}

func TestGitAuthPreservesEmbeddedCredentialsWithoutTokenAndRedacts(t *testing.T) {
	secret := "embedded-secret-token"
	cfg := config.Config{}
	scanner := New(cfg, nil, slog.Default())
	repo := config.RepositoryConfig{
		Name: "repo",
		URL:  "https://deploy:" + secret + "@github.com/example/repo.git",
	}
	auth, err := scanner.gitAuth(repo)
	if err != nil {
		t.Fatal(err)
	}
	if auth.remoteURL != repo.URL {
		t.Fatalf("remoteURL = %q, want credentialed config URL", auth.remoteURL)
	}
	if auth.stripRemoteUserinfo {
		t.Fatal("stripRemoteUserinfo = true, want false without replacement token auth")
	}
	redacted := auth.redactor.Redact("git clone " + repo.URL + " failed with deploy:" + secret)
	assertNoToken(t, "redacted auth error", redacted, secret)
	if strings.Contains(redacted, "deploy:"+secret) {
		t.Fatalf("redacted auth error contains userinfo: %q", redacted)
	}
}

func TestScanAllFailedCloneWithTokenDoesNotLeak(t *testing.T) {
	token := "clone-secret-token-t008"
	t.Setenv("GITOPS_TEST_TOKEN", token)
	ctx := context.Background()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "dashboard.db")
	store, err := storage.Open(dbPath)
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
			Name:       "private",
			URL:        "https://127.0.0.1:1/private/repo.git",
			DefaultRef: "main",
			TokenEnv:   "GITOPS_TEST_TOKEN",
		}},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	err = scanner.ScanAll(ctx)
	if err == nil {
		t.Fatal("ScanAll succeeded, want clone failure")
	}
	assertNoToken(t, "returned error", err.Error(), token)
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertSummaryNoToken(t, summary, token)
	assertNoTokenInTree(t, dataDir, token)
}

func TestScanAllFailedCloneWithEmbeddedCredentialsDoesNotLeak(t *testing.T) {
	token := "embedded-clone-secret-t008"
	ctx := context.Background()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "dashboard.db")
	store, err := storage.Open(dbPath)
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
			Name:       "private",
			URL:        "https://deploy:" + token + "@127.0.0.1:1/private/repo.git",
			DefaultRef: "main",
		}},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	err = scanner.ScanAll(ctx)
	if err == nil {
		t.Fatal("ScanAll succeeded, want clone failure")
	}
	assertNoToken(t, "returned error", err.Error(), token)
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertSummaryNoToken(t, summary, token)
	assertNoTokenInTree(t, dataDir, token)
}

func TestScanAllFailedFetchMigratesCredentialedRemoteAndDoesNotLeak(t *testing.T) {
	token := "fetch-secret-token-t008"
	t.Setenv("GITOPS_TEST_TOKEN", token)
	ctx := context.Background()
	source := createFixtureRepo(t)
	dataDir := t.TempDir()
	repoPath := filepath.Join(dataDir, "repos", "fixture")
	runGit(t, dataDir, "clone", "file://"+source, repoPath)
	credentialedRemote := "https://x-access-token:" + token + "@127.0.0.1:1/private/repo.git"
	runGit(t, repoPath, "remote", "set-url", "origin", credentialedRemote)
	configPath := filepath.Join(repoPath, ".git", "config")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), token) {
		t.Fatalf("test setup did not create credentialed remote: %s", before)
	}

	dbPath := filepath.Join(dataDir, "dashboard.db")
	store, err := storage.Open(dbPath)
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
			URL:        "https://127.0.0.1:1/private/repo.git",
			DefaultRef: "main",
			TokenEnv:   "GITOPS_TEST_TOKEN",
		}},
		Monitoring: config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	err = scanner.ScanAll(ctx)
	if err == nil {
		t.Fatal("ScanAll succeeded, want fetch failure")
	}
	assertNoToken(t, "returned error", err.Error(), token)
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, ".git/config", string(after), token)
	if !strings.Contains(string(after), "https://127.0.0.1:1/private/repo.git") {
		t.Fatalf("remote was not rewritten token-free: %s", after)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertSummaryNoToken(t, summary, token)
	assertNoTokenInTree(t, dataDir, token)
}

func TestMigrateRemoteLeavesSSHOriginUnchanged(t *testing.T) {
	ctx := context.Background()
	repoPath := t.TempDir()
	runGit(t, repoPath, "init", "-b", "main")
	origin := "ssh://git@github.com/org/repo.git"
	runGit(t, repoPath, "remote", "add", "origin", origin)
	auth := gitAuth{
		redactor:            sanitizer.New("token"),
		stripRemoteUserinfo: true,
	}
	if err := migrateRemote(ctx, repoPath, auth); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(gitOutputForTest(t, repoPath, "remote", "get-url", "origin"))
	if got != origin {
		t.Fatalf("origin = %q, want %q", got, origin)
	}
}

func TestMigrateRemoteStripsHTTPSCredentialedOrigin(t *testing.T) {
	ctx := context.Background()
	token := "migrate-secret-token-t008"
	repoPath := t.TempDir()
	runGit(t, repoPath, "init", "-b", "main")
	runGit(t, repoPath, "remote", "add", "origin", "https://x-access-token:"+token+"@github.com/org/repo.git")
	auth := gitAuth{
		redactor:            sanitizer.New(token),
		stripRemoteUserinfo: true,
	}
	if err := migrateRemote(ctx, repoPath, auth); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(gitOutputForTest(t, repoPath, "remote", "get-url", "origin"))
	if got != "https://github.com/org/repo.git" {
		t.Fatalf("origin = %q, want token-free HTTPS origin", got)
	}
	assertNoToken(t, "origin", got, token)
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
name: prod-stack
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

func assertSummaryNoToken(t *testing.T, summary core.DashboardSummary, token string) {
	t.Helper()
	for _, repo := range summary.Repositories {
		assertNoToken(t, "repository error", repo.Error, token)
	}
	for _, scan := range summary.Scans {
		assertNoToken(t, "scan error", scan.Error, token)
	}
	for _, status := range summary.Statuses {
		assertNoToken(t, "status message", status.Message, token)
	}
	for _, uptime := range summary.Uptime {
		for _, sample := range uptime.Samples {
			assertNoToken(t, "uptime sample message", sample.Message, token)
		}
	}
}

func assertNoTokenInTree(t *testing.T, root, token string) {
	t.Helper()
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return
	} else if err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertNoToken(t, path, string(data), token)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertNoToken(t *testing.T, label, value, token string) {
	t.Helper()
	if strings.Contains(value, token) {
		t.Fatalf("%s contains token %q: %q", label, token, value)
	}
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

func gitOutputForTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v failed: %v", args, err)
	}
	return string(output)
}
