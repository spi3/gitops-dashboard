package scanner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestRouteTargetReplacementsRefusesAmbiguousPorts(t *testing.T) {
	previous := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"http://10.10.10.127"}}}
	current := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"http://10.10.10.127:8080", "http://10.10.10.127:9090"}}}
	replacements, ambiguous := routeTargetReplacements(previous, current, "repo", nil)
	if len(replacements) != 0 {
		t.Fatalf("replacements = %#v, want none", replacements)
	}
	if len(ambiguous) != 1 || ambiguous[0].ServiceID != "svc" || ambiguous[0].OldRoute != "http://10.10.10.127" {
		t.Fatalf("ambiguous = %#v", ambiguous)
	}
}

func TestRouteTargetReplacementsRequiresSetDifferenceAndBijection(t *testing.T) {
	previous := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"https://app.example.test", "https://app.example.test:8443"}}}
	current := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"https://app.example.test", "https://app.example.test:9443"}}}
	replacements, exclusions := routeTargetReplacements(previous, current, "repo", nil)
	if len(replacements) != 1 || replacements[0].OldRoute != "https://app.example.test:8443" || replacements[0].NewRoute != "https://app.example.test:9443" {
		t.Fatalf("replacements = %#v", replacements)
	}
	if len(exclusions) != 0 {
		t.Fatalf("exclusions = %#v", exclusions)
	}

	// A still-present portless identity is not an old set difference and cannot
	// be guessed as the replacement for a newly discovered portful route.
	previous = []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"https://app.example.test"}}}
	current = []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"https://app.example.test", "https://app.example.test:8443"}}}
	replacements, exclusions = routeTargetReplacements(previous, current, "repo", nil)
	if len(replacements) != 0 || len(exclusions) != 0 {
		t.Fatalf("present identity replacements/exclusions = %#v/%#v", replacements, exclusions)
	}

	// Two old identities competing for one new identity are not a bijection;
	// retain both rather than allowing either to consume the replacement.
	previous = []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"https://app.example.test:8080", "https://app.example.test:9090"}}}
	current = []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"https://app.example.test:9443"}}}
	replacements, exclusions = routeTargetReplacements(previous, current, "repo", nil)
	if len(replacements) != 0 || len(exclusions) != 2 {
		t.Fatalf("non-bijective replacements/exclusions = %#v/%#v", replacements, exclusions)
	}
}

func TestRouteTargetReplacementsMigratesSinglePortEvidence(t *testing.T) {
	previous := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"http://10.10.10.127"}}}
	current := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"http://10.10.10.127:8080"}}}
	replacements, ambiguous := routeTargetReplacements(previous, current, "repo", nil)
	if len(ambiguous) != 0 || len(replacements) != 1 || replacements[0].OldRoute != "http://10.10.10.127" || replacements[0].NewRoute != "http://10.10.10.127:8080" {
		t.Fatalf("replacements/ambiguous = %#v/%#v", replacements, ambiguous)
	}
}

func TestRouteTargetReplacementsReconsidersPriorAmbiguity(t *testing.T) {
	oldRoute, firstCandidate, retainedCandidate := "https://app.example.test", "https://app.example.test:8443", "https://app.example.test:9443"
	first := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{oldRoute}}}
	second := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{firstCandidate, retainedCandidate}}}
	replacements, exclusions := routeTargetReplacements(first, second, "repo", nil)
	if len(replacements) != 0 || len(exclusions) != 1 {
		t.Fatalf("ambiguous transition = replacements:%#v exclusions:%#v", replacements, exclusions)
	}
	third := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{firstCandidate}}}
	replacements, exclusions = routeTargetReplacements(second, third, "repo", exclusions)
	if len(replacements) != 1 || replacements[0] != (storage.RouteTargetReplacement{ServiceID: "svc", OldRoute: oldRoute, NewRoute: firstCandidate}) || len(exclusions) != 0 {
		t.Fatalf("resolved transition = replacements:%#v exclusions:%#v", replacements, exclusions)
	}
}

// TestRouteTargetReplacementsRetiresStaleFabricatedTCPRouteWithoutReplacement
// covers T-055's coordination with T-031: rescanning a service whose Exposure
// used to hold an invented HTTP route for what is actually a TCP backend
// (e.g. a pre-fix "https://db:5432") must drop that route outright once the
// same evidence reparses as "tcp/db:5432" TCP inventory, not attempt to
// "replace" it — replacement is for genuine port renumbering of a route that
// is still a route, and a stale invented route has no HTTP-shaped candidate
// in the new set to match against.
func TestRouteTargetReplacementsRetiresStaleFabricatedTCPRouteWithoutReplacement(t *testing.T) {
	previous := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"https://db:5432"}}}
	current := []core.Service{{ID: "svc", Repository: "repo", Exposure: []string{"tcp/db:5432"}}}
	replacements, exclusions := routeTargetReplacements(previous, current, "repo", nil)
	if len(replacements) != 0 {
		t.Fatalf("replacements = %#v, want none: a TCP endpoint is not a route replacement candidate", replacements)
	}
	if len(exclusions) != 0 {
		t.Fatalf("exclusions = %#v, want none: the stale route retires cleanly", exclusions)
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

func TestScanAllModelsTraefikTCPEndpointsSeparatelyFromHTTPRoutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := createFixtureRepo(t)
	writeFile(t, filepath.Join(source, "prod", "db", "compose.yaml"), `
services:
  db:
    image: postgres:16
    healthcheck:
      test: ["CMD", "pg_isready"]
`)
	writeFile(t, filepath.Join(source, "prod", "dynamic-tcp.yaml"), `
tcp:
  routers:
    db:
      rule: HostSNI(`+"`db.example.test`"+`)
      service: db
  services:
    db:
      loadBalancer:
        servers:
          - address: "db:5432"
`)
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "add mixed http/tcp fixture")

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
	servicesByName := map[string][]string{}
	for _, service := range summary.Services {
		servicesByName[service.Name] = service.Exposure
	}
	dbExposure := servicesByName["db"]
	if len(dbExposure) != 2 || !contains(dbExposure, "tcp/db.example.test") || !contains(dbExposure, "tcp/db:5432") {
		t.Fatalf("db exposure = %v, want SNI and backend address TCP evidence only", dbExposure)
	}
	for _, route := range dbExposure {
		if strings.HasPrefix(route, "http://") || strings.HasPrefix(route, "https://") {
			t.Fatalf("db exposure = %v, TCP evidence must never become an HTTP route", dbExposure)
		}
	}
	if !contains(servicesByName["web"], "https://web.example.test") {
		t.Fatalf("web exposure = %v, unaffected by the added TCP fixture", servicesByName["web"])
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

func TestCompiledRepositoryPathFilterMatchesLegacySemantics(t *testing.T) {
	t.Parallel()
	repo := config.RepositoryConfig{
		IncludePaths: []string{"clusters/**/apps/*.yaml", "docker_files/serenity", "apps/[ab]*/**"},
		ExcludePaths: []string{"**/gotk-components.yaml", "clusters/**/retired", "apps/**/tmp"},
	}
	filter := newRepoPathFilter(repo)
	for _, rel := range []string{
		"clusters/main/apps/web.yaml",
		"clusters/main/apps/nested/web.yaml",
		"clusters/main/flux-system/gotk-components.yaml",
		"clusters/main/retired/app.yaml",
		"docker_files/serenity/web/docker-compose.yml",
		"docker_files/other/web/docker-compose.yml",
		"apps/api/prod/deploy.yaml",
		"apps/batch/tmp/job.yaml",
		"README.md",
	} {
		if got, want := filter.shouldScan(rel), legacyShouldScanRepoPath(repo, rel); got != want {
			t.Fatalf("compiled shouldScan(%q) = %v, want legacy %v", rel, got, want)
		}
		if got, want := filter.shouldSkipDir(rel), legacyShouldSkipRepoDir(repo, rel); got != want {
			t.Fatalf("compiled shouldSkipDir(%q) = %v, want legacy %v", rel, got, want)
		}
	}
}

func TestConcurrentScanAllCoalescesRepositoryScan(t *testing.T) {
	ctx := context.Background()
	source := createFixtureRepo(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	gitShim := filepath.Join(binDir, "git")
	if err := os.WriteFile(gitShim, []byte("#!/bin/sh\nif [ \"$1\" = \"clone\" ]; then sleep 0.2; fi\nexec "+shellQuote(realGit)+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

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
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- scanner.ScanAll(ctx)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Scans) != 1 {
		t.Fatalf("scans = %#v, want one coalesced scan", summary.Scans)
	}
}

func TestDetachedRepoSyncLeaderTimeoutDoesNotCancelJoiningScan(t *testing.T) {
	source := createFixtureRepo(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	signalDir := t.TempDir()
	cloneStarted := filepath.Join(signalDir, "clone-started")
	releaseClone := filepath.Join(signalDir, "release-clone")
	binDir := t.TempDir()
	gitShim := filepath.Join(binDir, "git")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"clone\" ]; then\n" +
		"  touch " + shellQuote(cloneStarted) + "\n" +
		"  while [ ! -f " + shellQuote(releaseClone) + " ]; do sleep 0.01; done\n" +
		"fi\n" +
		"exec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(gitShim, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

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

	leaderCtx, cancelLeader := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelLeader()
	leaderDone := make(chan error, 1)
	go func() {
		_, err := scanner.SyncRepo(leaderCtx, cfg.Repositories[0])
		leaderDone <- err
	}()
	waitForFile(t, cloneStarted)
	select {
	case err := <-leaderDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("leader SyncRepo error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for leader SyncRepo deadline")
	}

	scanDone := make(chan error, 1)
	go func() {
		scanDone <- scanner.ScanAll(context.Background())
	}()
	waitForRunningScan(t, store, "fixture")
	if err := os.WriteFile(releaseClone, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-scanDone:
		if err != nil {
			t.Fatalf("joining ScanAll failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for joining scan")
	}

	summary, err := store.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Services) != 2 {
		t.Fatalf("services = %#v, want scan to complete after joining detached sync", summary.Services)
	}
	if len(summary.Scans) != 1 || summary.Scans[0].Status != "ok" {
		t.Fatalf("scans = %#v, want successful joining scan", summary.Scans)
	}
}

func legacyShouldScanRepoPath(repo config.RepositoryConfig, rel string) bool {
	rel = normalizeRepoPath(rel)
	if rel == "." {
		return true
	}
	if legacyMatchesAnyRepoPath(repo.ExcludePaths, rel) {
		return false
	}
	if len(repo.IncludePaths) == 0 {
		return true
	}
	return legacyMatchesAnyRepoPath(repo.IncludePaths, rel)
}

func legacyShouldSkipRepoDir(repo config.RepositoryConfig, rel string) bool {
	rel = normalizeRepoPath(rel)
	return rel != "." && legacyMatchesAnyRepoPath(repo.ExcludePaths, rel)
}

func legacyMatchesAnyRepoPath(patterns []string, rel string) bool {
	for _, pattern := range patterns {
		if legacyMatchesRepoPath(pattern, rel) {
			return true
		}
	}
	return false
}

func legacyMatchesRepoPath(pattern, rel string) bool {
	pattern = normalizeRepoPath(pattern)
	rel = normalizeRepoPath(rel)
	if pattern == "." || rel == "." {
		return false
	}
	if !hasGlob(pattern) {
		return rel == pattern || strings.HasPrefix(rel, pattern+"/")
	}
	if legacyRepoGlobMatch(pattern, rel) {
		return true
	}
	for ancestor := path.Dir(rel); ancestor != "." && ancestor != "/"; ancestor = path.Dir(ancestor) {
		if legacyRepoGlobMatch(pattern, ancestor) {
			return true
		}
	}
	return false
}

func legacyRepoGlobMatch(pattern, rel string) bool {
	if !strings.Contains(pattern, "**") {
		ok, err := path.Match(pattern, rel)
		return err == nil && ok
	}
	ok, err := regexp.MatchString(globRegex(pattern), rel)
	return err == nil && ok
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

func TestGitAuthRejectsEmbeddedCredentialsWithoutToken(t *testing.T) {
	secret := "embedded-secret-token"
	cfg := config.Config{}
	scanner := New(cfg, nil, slog.Default())
	repo := config.RepositoryConfig{
		Name: "repo",
		URL:  "https://deploy:" + secret + "@github.com/example/repo.git",
	}
	_, err := scanner.gitAuth(repo)
	if err == nil {
		t.Fatal("gitAuth succeeded, want rejection of embedded configured URL userinfo")
	}
	if !strings.Contains(err.Error(), "repo") {
		t.Fatalf("error = %q, want it to name the repository", err)
	}
	assertNoToken(t, "gitAuth error", err.Error(), secret)
	if strings.Contains(err.Error(), repo.URL) || strings.Contains(err.Error(), "deploy:") {
		t.Fatalf("error leaks the configured URL or userinfo: %q", err)
	}
}

func TestGitAuthRejectsEmbeddedCredentialsWithToken(t *testing.T) {
	t.Setenv("GITOPS_TEST_TOKEN", "secret-token")
	secret := "embedded-secret-token-with-tokenenv"
	cfg := config.Config{}
	scanner := New(cfg, nil, slog.Default())
	repo := config.RepositoryConfig{
		Name:     "repo",
		URL:      "https://deploy:" + secret + "@github.com/example/repo.git",
		TokenEnv: "GITOPS_TEST_TOKEN",
	}
	_, err := scanner.gitAuth(repo)
	if err == nil {
		t.Fatal("gitAuth succeeded, want rejection of embedded configured URL userinfo even with tokenEnv set")
	}
	assertNoToken(t, "gitAuth error", err.Error(), secret)
	assertNoToken(t, "gitAuth error", err.Error(), "secret-token")
}

func TestGitAuthRejectsHTTPTokenAuth(t *testing.T) {
	t.Setenv("GITOPS_TEST_TOKEN", "secret-token")
	cfg := config.Config{}
	scanner := New(cfg, nil, slog.Default())
	repo := config.RepositoryConfig{
		Name:     "repo",
		URL:      "http://github.example.test/example/repo.git",
		TokenEnv: "GITOPS_TEST_TOKEN",
	}
	_, err := scanner.gitAuth(repo)
	if err == nil {
		t.Fatal("gitAuth succeeded, want rejection of plain HTTP token authentication")
	}
	assertNoToken(t, "gitAuth error", err.Error(), "secret-token")
}

func TestGitAuthAllowsCredentialFreeHTTPWithoutToken(t *testing.T) {
	cfg := config.Config{}
	scanner := New(cfg, nil, slog.Default())
	repo := config.RepositoryConfig{
		Name: "repo",
		URL:  "http://github.example.test/example/repo.git",
	}
	auth, err := scanner.gitAuth(repo)
	if err != nil {
		t.Fatalf("gitAuth failed, want credential-free HTTP left unchanged: %v", err)
	}
	if auth.remoteURL != repo.URL {
		t.Fatalf("remoteURL = %q, want unchanged %q", auth.remoteURL, repo.URL)
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
	// Capture logs so the credential scrub/reject path is asserted over all
	// four leak channels the spec names: returned errors, logs, persisted
	// fields, and .git/config. ScanAll logs every failed repository scan
	// with the error value (see scanOne's caller in ScanAll), which is
	// exactly the path this fetch failure drives.
	var logs bytes.Buffer
	scanner := New(cfg, store, slog.New(slog.NewTextHandler(&logs, nil)))
	err = scanner.ScanAll(ctx)
	if err == nil {
		t.Fatal("ScanAll succeeded, want fetch failure")
	}
	assertNoToken(t, "returned error", err.Error(), token)
	if !strings.Contains(logs.String(), "repository scan failed") || !strings.Contains(logs.String(), "fixture") {
		t.Fatalf("captured logs do not show the expected failed-scan entry, want the log channel actually exercised: %s", logs.String())
	}
	assertNoToken(t, "captured logs", logs.String(), token)
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

func TestOriginReconciliationLeavesMatchingSingleURLUnchanged(t *testing.T) {
	ctx := context.Background()
	repoPath := t.TempDir()
	runGit(t, repoPath, "init", "-b", "main")
	origin := "https://example.test/org/repo.git"
	runGit(t, repoPath, "remote", "add", "origin", origin)
	if err := reconcileOrigin(ctx, repoPath, sanitizer.New("token"), nil, origin); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(gitOutputForTest(t, repoPath, "remote", "get-url", "origin"))
	if got != origin {
		t.Fatalf("origin = %q, want unchanged %q", got, origin)
	}
}

func TestOriginReconciliationRepairsMismatchedURL(t *testing.T) {
	ctx := context.Background()
	repoPath := t.TempDir()
	runGit(t, repoPath, "init", "-b", "main")
	runGit(t, repoPath, "remote", "add", "origin", "https://old.example.test/org/repo.git")
	want := "https://new.example.test/org/repo.git"
	if err := reconcileOrigin(ctx, repoPath, sanitizer.New("token"), nil, want); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(gitOutputForTest(t, repoPath, "remote", "get-url", "origin"))
	if got != want {
		t.Fatalf("origin = %q, want reconciled to %q", got, want)
	}
}

func TestCredentialFreeRemoteURL(t *testing.T) {
	for _, tt := range []struct {
		name        string
		raw         string
		wantClean   string
		wantStrip   bool
		wantErr     bool
		errContains string
	}{
		{name: "https no userinfo unchanged", raw: "https://github.com/org/repo.git", wantClean: "https://github.com/org/repo.git"},
		{name: "http no userinfo unchanged", raw: "http://github.example.test/org/repo.git", wantClean: "http://github.example.test/org/repo.git"},
		{name: "https strips userinfo", raw: "https://x-access-token:secret@github.com/org/repo.git", wantClean: "https://github.com/org/repo.git", wantStrip: true},
		{name: "HTTPS scheme case insensitive strips userinfo", raw: "HTTPS://deploy:secret@GitHub.example.test/org/repo.git", wantStrip: true},
		{name: "http missing host is invalid", raw: "http:///no-host", wantErr: true},
		{name: "ssh url unchanged", raw: "ssh://git@github.com/org/repo.git", wantClean: "ssh://git@github.com/org/repo.git"},
		{name: "git url unchanged", raw: "git://github.com/org/repo.git", wantClean: "git://github.com/org/repo.git"},
		{name: "file url unchanged", raw: "file:///srv/repos/repo.git", wantClean: "file:///srv/repos/repo.git"},
		{name: "file url without path is invalid", raw: "file://", wantErr: true},
		{name: "scp-like with user unchanged", raw: "git@github.com:org/repo.git", wantClean: "git@github.com:org/repo.git"},
		{name: "scp-like without user unchanged", raw: "github.com:org/repo.git", wantClean: "github.com:org/repo.git"},
		{name: "absolute filesystem path unchanged", raw: "/srv/repos/repo.git", wantClean: "/srv/repos/repo.git"},
		{name: "relative filesystem path unchanged", raw: "repos/repo.git", wantClean: "repos/repo.git"},
		{name: "empty is invalid", raw: "", wantErr: true},
		{name: "dot is invalid", raw: ".", wantErr: true},
		{name: "control character is invalid", raw: "https://github.com/org/repo\n.git", wantErr: true},
		{name: "whitespace is invalid", raw: "not a remote at all here", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clean, stripped, err := credentialFreeRemoteURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("credentialFreeRemoteURL(%q) succeeded, want error", tt.raw)
				}
				if strings.Contains(err.Error(), tt.raw) && tt.raw != "" {
					t.Fatalf("error echoes raw remote: %q", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("credentialFreeRemoteURL(%q) failed: %v", tt.raw, err)
			}
			if stripped != tt.wantStrip {
				t.Fatalf("stripped = %v, want %v", stripped, tt.wantStrip)
			}
			if tt.wantClean != "" && clean != tt.wantClean {
				t.Fatalf("clean = %q, want %q", clean, tt.wantClean)
			}
			if !tt.wantStrip && clean != tt.raw {
				t.Fatalf("clean = %q, want byte-for-byte unchanged %q", clean, tt.raw)
			}
		})
	}
}

func TestScrubCachedOriginCredentialsAllFetchAndPushURLs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repoPath := t.TempDir()
	runGit(t, repoPath, "init", "-b", "main")
	fetchToken := "scrub-fetch-secret-t060"
	pushToken := "scrub-push-secret-t060"
	runGit(t, repoPath, "config", "--add", "remote.origin.url", "https://x-access-token:"+fetchToken+"@example.test/repo-a.git")
	runGit(t, repoPath, "config", "--add", "remote.origin.url", "ssh://git@example.test/repo-b.git")
	runGit(t, repoPath, "config", "--add", "remote.origin.pushurl", "https://x-access-token:"+pushToken+"@example.test/repo-a.git")
	runGit(t, repoPath, "config", "--add", "remote.origin.pushurl", "https://x-access-token:"+pushToken+"@example.test/repo-c.git")

	redactor := sanitizer.New(fetchToken, pushToken)
	if err := scrubCachedOriginCredentials(ctx, repoPath, redactor, nil); err != nil {
		t.Fatal(err)
	}

	wantFetch := []string{"https://example.test/repo-a.git", "ssh://git@example.test/repo-b.git"}
	gotFetch := splitGitConfigLines(gitOutputForTest(t, repoPath, "config", "--get-all", "remote.origin.url"))
	if !reflect.DeepEqual(gotFetch, wantFetch) {
		t.Fatalf("fetch urls = %#v, want %#v", gotFetch, wantFetch)
	}
	wantPush := []string{"https://example.test/repo-a.git", "https://example.test/repo-c.git"}
	gotPush := splitGitConfigLines(gitOutputForTest(t, repoPath, "config", "--get-all", "remote.origin.pushurl"))
	if !reflect.DeepEqual(gotPush, wantPush) {
		t.Fatalf("push urls = %#v, want %#v", gotPush, wantPush)
	}

	configData, err := os.ReadFile(filepath.Join(repoPath, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	assertNoToken(t, ".git/config", string(configData), fetchToken)
	assertNoToken(t, ".git/config", string(configData), pushToken)
}

func TestCachedOriginScrubPrecedesCredentialValidation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "policy-secret-t060"
	dataDir := t.TempDir()
	repoCacheDir := filepath.Join(dataDir, "repos")
	if err := os.MkdirAll(repoCacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoCacheDir, "init", "-b", "main", "private")
	repoPath := filepath.Join(repoCacheDir, "private")
	runGit(t, repoPath, "remote", "add", "origin", "https://x-access-token:"+token+"@example.invalid/private/repo.git")

	repo := config.RepositoryConfig{
		Name:       "private",
		URL:        "https://user:" + token + "@example.invalid/private/repo.git",
		DefaultRef: "main",
	}
	cfg := config.Config{
		Server:       config.ServerConfig{DataDir: dataDir, RepoCacheDir: repoCacheDir},
		Auth:         config.AuthConfig{Mode: "dev-no-auth"},
		Repositories: []config.RepositoryConfig{repo},
		Monitoring:   config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, nil, slog.Default())
	if _, err := scanner.SyncRepo(ctx, repo); err == nil {
		t.Fatal("SyncRepo succeeded, want rejection of embedded configured URL userinfo")
	} else {
		assertNoToken(t, "returned error", err.Error(), token)
	}

	got := strings.TrimSpace(gitOutputForTest(t, repoPath, "remote", "get-url", "origin"))
	if got != "https://example.invalid/private/repo.git" {
		t.Fatalf("cached origin = %q, want scrubbed even though the configured URL was rejected", got)
	}
}

// TestSyncRepoScrubsMultipleCachedOriginURLsBeforeNetworkAttempt is the
// representative pre-change fixture: multiple fetch URLs, multiple push
// URLs, credentials in at least one of each, and one credential-free
// non-HTTP(S) URL. It proves synchronization removes every HTTP(S) userinfo
// value before the first network attempt (an unreachable host here stands in
// for "network"), preserving push URL count and ordering; the fetch URL list
// is then reconciled (T-061) down to exactly the single configured URL,
// since three cached fetch URLs are themselves a mismatch.
func TestSyncRepoScrubsMultipleCachedOriginURLsBeforeNetworkAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dataDir := t.TempDir()
	repoCacheDir := filepath.Join(dataDir, "repos")
	if err := os.MkdirAll(repoCacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit(t, repoCacheDir, "init", "-b", "main", "multi")
	repoPath := filepath.Join(repoCacheDir, "multi")
	fetchToken := "multi-fetch-secret-t060"
	pushToken := "multi-push-secret-t060"
	runGit(t, repoPath, "config", "--add", "remote.origin.url", "https://x-access-token:"+fetchToken+"@127.0.0.1:1/private/repo-a.git")
	runGit(t, repoPath, "config", "--add", "remote.origin.url", "https://x-access-token:"+fetchToken+"@127.0.0.1:1/private/repo-b.git")
	runGit(t, repoPath, "config", "--add", "remote.origin.url", "ssh://git@127.0.0.1/private/repo-c.git")
	runGit(t, repoPath, "config", "--add", "remote.origin.pushurl", "https://x-access-token:"+pushToken+"@127.0.0.1:1/private/repo-a.git")
	runGit(t, repoPath, "config", "--add", "remote.origin.pushurl", "https://x-access-token:"+pushToken+"@127.0.0.1:1/private/repo-b.git")

	dbPath := filepath.Join(dataDir, "dashboard.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := config.RepositoryConfig{
		Name:       "multi",
		URL:        "https://127.0.0.1:1/private/repo-a.git",
		DefaultRef: "main",
	}
	cfg := config.Config{
		Server:       config.ServerConfig{DataDir: dataDir, RepoCacheDir: repoCacheDir},
		Auth:         config.AuthConfig{Mode: "dev-no-auth"},
		Repositories: []config.RepositoryConfig{repo},
		Monitoring:   config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	if _, err := scanner.SyncRepo(ctx, repo); err == nil {
		t.Fatal("SyncRepo succeeded, want a network fetch failure against an unreachable host")
	} else {
		assertNoToken(t, "returned error", err.Error(), fetchToken)
		assertNoToken(t, "returned error", err.Error(), pushToken)
	}

	// T-061 origin reconciliation runs after scrub and before fetch: three
	// cached fetch URLs can never match the single configured URL, so it
	// repairs the list down to exactly that one value.
	wantFetch := []string{"https://127.0.0.1:1/private/repo-a.git"}
	gotFetch := splitGitConfigLines(gitOutputForTest(t, repoPath, "config", "--get-all", "remote.origin.url"))
	if !reflect.DeepEqual(gotFetch, wantFetch) {
		t.Fatalf("fetch urls = %#v, want %#v", gotFetch, wantFetch)
	}
	wantPush := []string{
		"https://127.0.0.1:1/private/repo-a.git",
		"https://127.0.0.1:1/private/repo-b.git",
	}
	gotPush := splitGitConfigLines(gitOutputForTest(t, repoPath, "config", "--get-all", "remote.origin.pushurl"))
	if !reflect.DeepEqual(gotPush, wantPush) {
		t.Fatalf("push urls = %#v, want %#v", gotPush, wantPush)
	}
	assertNoTokenInTree(t, dataDir, fetchToken)
	assertNoTokenInTree(t, dataDir, pushToken)
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

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForRunningScan(t *testing.T, store *storage.Store, repository string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		scans, err := store.Scans(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, scan := range scans {
			if scan.Repository == repository && scan.Status == "running" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for running scan in %s; scans=%#v", repository, scans)
		}
		time.Sleep(10 * time.Millisecond)
	}
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

// withGitExec substitutes the package's git-command seam for the duration of
// the calling test, restoring the previous value on cleanup. Like
// t.Setenv, this mutates process-wide state, so a test using it must not
// call t.Parallel(): Go only runs parallel tests concurrently after every
// non-parallel test (including this one) has already completed.
func withGitExec(t *testing.T, fn gitExecFunc) {
	t.Helper()
	previous := currentGitExecFn.Load()
	currentGitExecFn.Store(fn)
	t.Cleanup(func() { currentGitExecFn.Store(previous) })
}

// failingGitCommand returns a gitExecFunc that fails only invocations whose
// first argument is subcommand, delegating every other git invocation to the
// real production implementation.
func failingGitCommand(subcommand string, failure error) gitExecFunc {
	return func(ctx context.Context, dir string, env []string, args []string) gitExecResult {
		if len(args) > 0 && args[0] == subcommand {
			return gitExecResult{err: failure}
		}
		return productionGitExec(ctx, dir, env, args)
	}
}

// twoPhaseFixture runs one successful ScanAll against createOriginFixtureSourceA
// and returns everything a failure test needs to induce and assert a second,
// failing attempt against the same populated cache.
func twoPhaseFixture(t *testing.T) (ctx context.Context, scanner Scanner, store *storage.Store, repoPath, sourceSHA string) {
	t.Helper()
	ctx = context.Background()
	source, sha := createOriginFixtureSourceA(t)
	dataDir := t.TempDir()
	var err error
	store, err = storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	repoCacheDir := filepath.Join(dataDir, "repos")
	cfg := config.Config{
		Server:       config.ServerConfig{DataDir: dataDir, RepoCacheDir: repoCacheDir},
		Auth:         config.AuthConfig{Mode: "dev-no-auth"},
		Repositories: []config.RepositoryConfig{{Name: "fixture", URL: "file://" + source, DefaultRef: "main"}},
		Monitoring:   config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner = New(cfg, store, slog.Default())
	if err := scanner.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, scanner, store, filepath.Join(repoCacheDir, "fixture"), sha
}

// assertTerminalFailedScan asserts the newest persisted scan row for
// repoName is a terminal error with the exact commit truthfulness this task
// requires: the failed row's CommitSHA, and the repository summary's
// LastCommit, both equal wantCommit (the prior successful SHA, or empty when
// none ever succeeded).
func assertTerminalFailedScan(t *testing.T, ctx context.Context, store *storage.Store, repoName, wantCommit string) {
	t.Helper()
	scans, err := store.Scans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// started_at has only second resolution, so ties are possible within a
	// fast test; select by the monotonically increasing ID rather than
	// trusting ORDER BY started_at DESC to break ties correctly.
	var newest core.Scan
	found := false
	for _, scan := range scans {
		if scan.Repository != repoName {
			continue
		}
		if !found || scan.ID > newest.ID {
			newest = scan
			found = true
		}
	}
	if !found {
		t.Fatalf("no scan found for repository %s", repoName)
	}
	if newest.Status != "error" {
		t.Fatalf("newest scan status = %q, want error: %#v", newest.Status, newest)
	}
	if newest.CommitSHA != wantCommit {
		t.Fatalf("newest scan commit_sha = %q, want %q", newest.CommitSHA, wantCommit)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range summary.Repositories {
		if repo.Name == repoName && repo.LastCommit != wantCommit {
			t.Fatalf("summary LastCommit = %q, want %q", repo.LastCommit, wantCommit)
		}
	}
}

func assertTerminalOKScan(t *testing.T, ctx context.Context, store *storage.Store, repoName, wantCommit string) {
	t.Helper()
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range summary.Repositories {
		if repo.Name == repoName {
			if repo.Status != "ok" {
				t.Fatalf("status = %q, want ok", repo.Status)
			}
			if repo.LastCommit != wantCommit {
				t.Fatalf("LastCommit = %q, want %q", repo.LastCommit, wantCommit)
			}
			return
		}
	}
	t.Fatalf("repository %s missing from summary", repoName)
}

func assertServiceNames(t *testing.T, ctx context.Context, store *storage.Store, repoName string, want ...string) {
	t.Helper()
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, service := range summary.Services {
		if service.Repository == repoName {
			got = append(got, service.Name)
		}
	}
	sort.Strings(got)
	wantSorted := append([]string{}, want...)
	sort.Strings(wantSorted)
	if !reflect.DeepEqual(got, wantSorted) {
		t.Fatalf("services for %s = %#v, want %#v", repoName, got, wantSorted)
	}
}

func assertAncestor(t *testing.T, repoDir, ancestor, descendant string) {
	t.Helper()
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = repoDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("git merge-base --is-ancestor %s %s failed: %v", ancestor, descendant, err)
	}
}

// createOriginFixtureSourceA builds the minimal single-service fixture the
// origin-reconciliation and divergence tests share: branch main, a single
// docker-compose.yml, and the exact baseline commit subject and service name
// the spec requires.
func createOriginFixtureSourceA(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	writeComposeService(t, dir, "source-a")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "fixture: source A baseline")
	return dir, strings.TrimSpace(gitOutputForTest(t, dir, "rev-parse", "HEAD"))
}

// createOriginFixtureSourceBFastForward clones sourceA and adds one strict
// descendant commit, so the result is a fast-forward of source A on the same
// branch and history.
func createOriginFixtureSourceBFastForward(t *testing.T, sourceA string) (dir, sha string) {
	t.Helper()
	parent := t.TempDir()
	runGit(t, parent, "clone", sourceA, "source-b")
	dir = filepath.Join(parent, "source-b")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	writeComposeService(t, dir, "source-b")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "fixture: source B fast-forward")
	return dir, strings.TrimSpace(gitOutputForTest(t, dir, "rev-parse", "HEAD"))
}

// createOriginFixtureSourceBDiverged builds an unrelated history on branch
// main, so its tip never contains source A's commit as an ancestor.
func createOriginFixtureSourceBDiverged(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "user.email", "test@example.invalid")
	writeComposeService(t, dir, "source-b-diverged")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "fixture: source B diverged")
	return dir, strings.TrimSpace(gitOutputForTest(t, dir, "rev-parse", "HEAD"))
}

func writeComposeService(t *testing.T, dir, service string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "docker-compose.yml"), fmt.Sprintf(`
services:
  %s:
    image: example/%s:v1
`, service, service))
}

func TestScanAllPersistsRootPreflightFailure(t *testing.T) {
	ctx := context.Background()
	source, sourceSHA := createOriginFixtureSourceA(t)
	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repoCacheDir := filepath.Join(dataDir, "repos")
	cfg := config.Config{
		Server:       config.ServerConfig{DataDir: dataDir, RepoCacheDir: repoCacheDir},
		Auth:         config.AuthConfig{Mode: "dev-no-auth"},
		Repositories: []config.RepositoryConfig{{Name: "fixture", URL: "file://" + source, DefaultRef: "main"}},
		Monitoring:   config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	if err := scanner.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	assertServiceNames(t, ctx, store, "fixture", "source-a")

	// Corrupt the cache root itself so filepath.Abs+MkdirAll+EvalSymlinks
	// preflight fails before any candidate resolution or Git command.
	if err := os.RemoveAll(repoCacheDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoCacheDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := scanner.ScanAll(ctx); err == nil {
		t.Fatal("ScanAll succeeded, want root preflight failure")
	}
	assertTerminalFailedScan(t, ctx, store, "fixture", sourceSHA)
	assertServiceNames(t, ctx, store, "fixture", "source-a")
}

func TestExternalCacheSymlinkIsUntouched(t *testing.T) {
	ctx := context.Background()
	external := createFixtureRepo(t)
	resolvedExternal, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	repoCacheDir := filepath.Join(dataDir, "repos")
	if err := os.MkdirAll(repoCacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	repoName := "fixture"
	link := filepath.Join(repoCacheDir, safeName(repoName))
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}

	beforeHead := strings.TrimSpace(gitOutputForTest(t, external, "rev-parse", "HEAD"))
	beforeRemotes := gitOutputForTest(t, external, "remote", "-v")
	beforeStatus := gitOutputForTest(t, external, "status", "--porcelain")
	indexPath := filepath.Join(external, ".git", "index")
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(external, "prod", "compose.yaml")
	beforeMarker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var touched bool
	withGitExec(t, func(ctx context.Context, dir string, env []string, args []string) gitExecResult {
		if dir == external || dir == resolvedExternal {
			mu.Lock()
			touched = true
			mu.Unlock()
		}
		return productionGitExec(ctx, dir, env, args)
	})

	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Config{
		Server:       config.ServerConfig{DataDir: dataDir, RepoCacheDir: repoCacheDir},
		Auth:         config.AuthConfig{Mode: "dev-no-auth"},
		Repositories: []config.RepositoryConfig{{Name: repoName, URL: "file://" + external, DefaultRef: "main"}},
		Monitoring:   config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	if err := scanner.ScanAll(ctx); err == nil {
		t.Fatal("ScanAll succeeded, want containment failure for external symlink")
	}
	assertTerminalFailedScan(t, ctx, store, repoName, "")

	mu.Lock()
	gotTouched := touched
	mu.Unlock()
	if gotTouched {
		t.Fatal("a Git command ran against the external symlinked repository")
	}

	afterHead := strings.TrimSpace(gitOutputForTest(t, external, "rev-parse", "HEAD"))
	if afterHead != beforeHead {
		t.Fatalf("external HEAD changed: %q -> %q", beforeHead, afterHead)
	}
	afterRemotes := gitOutputForTest(t, external, "remote", "-v")
	if afterRemotes != beforeRemotes {
		t.Fatalf("external remotes changed:\nbefore=%q\nafter=%q", beforeRemotes, afterRemotes)
	}
	afterStatus := gitOutputForTest(t, external, "status", "--porcelain")
	if afterStatus != beforeStatus {
		t.Fatalf("external worktree status changed:\nbefore=%q\nafter=%q", beforeStatus, afterStatus)
	}
	afterIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeIndex, afterIndex) {
		t.Fatal(".git/index bytes changed")
	}
	afterMarker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeMarker, afterMarker) {
		t.Fatal("representative worktree file changed")
	}
}

func TestOriginEnumerationEmptyListRepairs(t *testing.T) {
	ctx := context.Background()
	source, sourceSHA := createOriginFixtureSourceA(t)
	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repoCacheDir := filepath.Join(dataDir, "repos")
	repoPath := filepath.Join(repoCacheDir, "fixture")
	runGit(t, dataDir, "clone", "file://"+source, repoPath)
	runGit(t, repoPath, "config", "--unset-all", "remote.origin.url")

	cfg := config.Config{
		Server:       config.ServerConfig{DataDir: dataDir, RepoCacheDir: repoCacheDir},
		Auth:         config.AuthConfig{Mode: "dev-no-auth"},
		Repositories: []config.RepositoryConfig{{Name: "fixture", URL: "file://" + source, DefaultRef: "main"}},
		Monitoring:   config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	if err := scanner.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}

	fetchURLs := splitGitConfigLines(gitOutputForTest(t, repoPath, "config", "--get-all", "remote.origin.url"))
	if len(fetchURLs) != 1 || fetchURLs[0] != "file://"+source {
		t.Fatalf("fetch urls = %#v, want repaired single configured url", fetchURLs)
	}
	assertTerminalOKScan(t, ctx, store, "fixture", sourceSHA)
	assertServiceNames(t, ctx, store, "fixture", "source-a")
}

func TestOriginEnumerationReadFailureStopsBeforeFetch(t *testing.T) {
	ctx, scanner, store, _, sourceSHA := twoPhaseFixture(t)

	urlGetAllCount := 0
	fetchInvoked := false
	withGitExec(t, func(ctx context.Context, dir string, env []string, args []string) gitExecResult {
		if len(args) > 0 && args[0] == "fetch" {
			fetchInvoked = true
		}
		if len(args) >= 3 && args[0] == "config" && args[1] == "--get-all" && args[2] == "remote.origin.url" {
			urlGetAllCount++
			// The first call is T-060's own scrub enumeration; the second is
			// this task's origin enumeration. Only the second fails.
			if urlGetAllCount == 2 {
				return gitExecResult{err: errors.New("simulated origin enumeration read failure")}
			}
		}
		return productionGitExec(ctx, dir, env, args)
	})

	if err := scanner.ScanAll(ctx); err == nil {
		t.Fatal("ScanAll succeeded, want origin enumeration read failure")
	}
	if fetchInvoked {
		t.Fatal("fetch ran despite origin enumeration read failure")
	}
	assertTerminalFailedScan(t, ctx, store, "fixture", sourceSHA)
	assertServiceNames(t, ctx, store, "fixture", "source-a")
}

func TestOriginReconciliationWriteFailureStopsBeforeFetch(t *testing.T) {
	ctx, scanner, store, repoPath, sourceSHA := twoPhaseFixture(t)
	// Force a mismatch so reconciliation attempts a write.
	runGit(t, repoPath, "config", "--add", "remote.origin.url", "https://example.test/other/repo.git")

	fetchInvoked := false
	withGitExec(t, func(ctx context.Context, dir string, env []string, args []string) gitExecResult {
		if len(args) > 0 && args[0] == "fetch" {
			fetchInvoked = true
		}
		if len(args) >= 2 && args[0] == "config" && (args[1] == "--unset-all" || args[1] == "--add") {
			return gitExecResult{err: errors.New("simulated origin reconciliation write failure")}
		}
		return productionGitExec(ctx, dir, env, args)
	})

	if err := scanner.ScanAll(ctx); err == nil {
		t.Fatal("ScanAll succeeded, want origin reconciliation write failure")
	}
	if fetchInvoked {
		t.Fatal("fetch ran despite origin reconciliation write failure")
	}
	assertTerminalFailedScan(t, ctx, store, "fixture", sourceSHA)
	assertServiceNames(t, ctx, store, "fixture", "source-a")
}

func TestScanAllPersistsFetchFailure(t *testing.T) {
	ctx, scanner, store, _, sourceSHA := twoPhaseFixture(t)
	withGitExec(t, failingGitCommand("fetch", errors.New("simulated fetch failure")))
	if err := scanner.ScanAll(ctx); err == nil {
		t.Fatal("ScanAll succeeded, want fetch failure")
	}
	assertTerminalFailedScan(t, ctx, store, "fixture", sourceSHA)
	assertServiceNames(t, ctx, store, "fixture", "source-a")
}

func TestScanAllPersistsCheckoutFailure(t *testing.T) {
	ctx, scanner, store, _, sourceSHA := twoPhaseFixture(t)
	withGitExec(t, failingGitCommand("checkout", errors.New("simulated checkout failure")))
	if err := scanner.ScanAll(ctx); err == nil {
		t.Fatal("ScanAll succeeded, want checkout failure")
	}
	assertTerminalFailedScan(t, ctx, store, "fixture", sourceSHA)
	assertServiceNames(t, ctx, store, "fixture", "source-a")
}

// TestScanAllPersistsFastForwardFailureForHEAD proves the removed
// DefaultRef-HEAD exception: before this task, a `git pull --ff-only`
// failure was silently ignored whenever DefaultRef was "HEAD". It is fatal
// now, for every DefaultRef.
func TestScanAllPersistsFastForwardFailureForHEAD(t *testing.T) {
	ctx := context.Background()
	source, sourceSHA := createOriginFixtureSourceA(t)
	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repoCacheDir := filepath.Join(dataDir, "repos")
	cfg := config.Config{
		Server:       config.ServerConfig{DataDir: dataDir, RepoCacheDir: repoCacheDir},
		Auth:         config.AuthConfig{Mode: "dev-no-auth"},
		Repositories: []config.RepositoryConfig{{Name: "fixture", URL: "file://" + source, DefaultRef: "HEAD"}},
		Monitoring:   config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	if err := scanner.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}

	withGitExec(t, failingGitCommand("pull", errors.New("simulated fast-forward failure")))
	if err := scanner.ScanAll(ctx); err == nil {
		t.Fatal("ScanAll succeeded, want fast-forward failure even with DefaultRef HEAD")
	}
	assertTerminalFailedScan(t, ctx, store, "fixture", sourceSHA)
	assertServiceNames(t, ctx, store, "fixture", "source-a")
}

func TestScanAllPersistsRevParseFailure(t *testing.T) {
	ctx, scanner, store, _, sourceSHA := twoPhaseFixture(t)
	withGitExec(t, failingGitCommand("rev-parse", errors.New("simulated rev-parse failure")))
	if err := scanner.ScanAll(ctx); err == nil {
		t.Fatal("ScanAll succeeded, want rev-parse failure")
	}
	assertTerminalFailedScan(t, ctx, store, "fixture", sourceSHA)
	assertServiceNames(t, ctx, store, "fixture", "source-a")
}

func TestScanAllReconcilesFastForwardOrigin(t *testing.T) {
	ctx := context.Background()
	sourceA, aSHA := createOriginFixtureSourceA(t)
	sourceB, bSHA := createOriginFixtureSourceBFastForward(t, sourceA)
	if aSHA == bSHA {
		t.Fatal("fixture setup: A_SHA and B_SHA must differ")
	}
	assertAncestor(t, sourceB, aSHA, bSHA)

	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repoCacheDir := filepath.Join(dataDir, "repos")
	cfg := config.Config{
		Server:       config.ServerConfig{DataDir: dataDir, RepoCacheDir: repoCacheDir},
		Auth:         config.AuthConfig{Mode: "dev-no-auth"},
		Repositories: []config.RepositoryConfig{{Name: "fixture", URL: "file://" + sourceA, DefaultRef: "main"}},
		Monitoring:   config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	if err := scanner.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	assertServiceNames(t, ctx, store, "fixture", "source-a")

	repoPath := filepath.Join(repoCacheDir, "fixture")
	safePush := "https://example.test/safe/push.git"
	runGit(t, repoPath, "config", "--add", "remote.origin.pushurl", safePush)
	marker := filepath.Join(repoPath, ".git", "gitops-no-reclone-marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg.Repositories[0].URL = "file://" + sourceB
	scanner = New(cfg, store, slog.Default())
	if err := scanner.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cache was recloned: marker missing: %v", err)
	}
	fetchURLs := splitGitConfigLines(gitOutputForTest(t, repoPath, "config", "--get-all", "remote.origin.url"))
	if len(fetchURLs) != 1 || fetchURLs[0] != "file://"+sourceB {
		t.Fatalf("fetch urls = %#v, want exactly [file://%s]", fetchURLs, sourceB)
	}
	pushURLs := splitGitConfigLines(gitOutputForTest(t, repoPath, "config", "--get-all", "remote.origin.pushurl"))
	if len(pushURLs) != 1 || pushURLs[0] != safePush {
		t.Fatalf("push urls = %#v, want preserved [%s]", pushURLs, safePush)
	}
	head := strings.TrimSpace(gitOutputForTest(t, repoPath, "rev-parse", "HEAD"))
	if head != bSHA {
		t.Fatalf("HEAD = %q, want fast-forwarded to %q", head, bSHA)
	}
	assertTerminalOKScan(t, ctx, store, "fixture", bSHA)
	assertServiceNames(t, ctx, store, "fixture", "source-b")
}

func TestScanAllDivergedOriginFailsTruthfully(t *testing.T) {
	ctx := context.Background()
	sourceA, aSHA := createOriginFixtureSourceA(t)
	sourceBDiverged, bSHA := createOriginFixtureSourceBDiverged(t)
	if aSHA == bSHA {
		t.Fatal("fixture setup: A_SHA and diverged B_SHA must differ")
	}

	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repoCacheDir := filepath.Join(dataDir, "repos")
	cfg := config.Config{
		Server:       config.ServerConfig{DataDir: dataDir, RepoCacheDir: repoCacheDir},
		Auth:         config.AuthConfig{Mode: "dev-no-auth"},
		Repositories: []config.RepositoryConfig{{Name: "fixture", URL: "file://" + sourceA, DefaultRef: "main"}},
		Monitoring:   config.MonitoringConfig{DefaultInterval: "30s"},
	}
	scanner := New(cfg, store, slog.Default())
	if err := scanner.ScanAll(ctx); err != nil {
		t.Fatal(err)
	}
	assertServiceNames(t, ctx, store, "fixture", "source-a")

	repoPath := filepath.Join(repoCacheDir, "fixture")
	marker := filepath.Join(repoPath, ".git", "gitops-no-reclone-marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	headBefore := strings.TrimSpace(gitOutputForTest(t, repoPath, "rev-parse", "HEAD"))
	indexBefore, err := os.ReadFile(filepath.Join(repoPath, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}

	cfg.Repositories[0].URL = "file://" + sourceBDiverged
	scanner = New(cfg, store, slog.Default())
	if err := scanner.ScanAll(ctx); err == nil {
		t.Fatal("ScanAll succeeded, want diverged fast-forward failure")
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cache was recloned or reset: marker missing: %v", err)
	}
	headAfter := strings.TrimSpace(gitOutputForTest(t, repoPath, "rev-parse", "HEAD"))
	if headAfter != headBefore {
		t.Fatalf("HEAD changed despite ff-only failure: %q -> %q", headBefore, headAfter)
	}
	indexAfter, err := os.ReadFile(filepath.Join(repoPath, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indexBefore, indexAfter) {
		t.Fatal(".git/index mutated despite ff-only failure")
	}
	fetchURLs := splitGitConfigLines(gitOutputForTest(t, repoPath, "config", "--get-all", "remote.origin.url"))
	if len(fetchURLs) != 1 || fetchURLs[0] != "file://"+sourceBDiverged {
		t.Fatalf("fetch urls = %#v, want reconciled to [file://%s] despite ff-only failure", fetchURLs, sourceBDiverged)
	}
	assertTerminalFailedScan(t, ctx, store, "fixture", aSHA)
	assertServiceNames(t, ctx, store, "fixture", "source-a")
}

func TestRepositorySyncOperationBudgetIncludesOriginReconciliation(t *testing.T) {
	t.Parallel()
	if repoSyncGitCommands != 7 {
		t.Fatalf("repoSyncGitCommands = %d, want 7 (origin enumeration, reconciliation unset-all+add, fetch, checkout, fast-forward pull, and HEAD resolution)", repoSyncGitCommands)
	}
	want := time.Duration(repoSyncGitCommands)*GitCommandTimeout + 30*time.Second
	if repoSyncOperationTimeout != want {
		t.Fatalf("repoSyncOperationTimeout = %v, want %v derived from repoSyncGitCommands", repoSyncOperationTimeout, want)
	}
}
