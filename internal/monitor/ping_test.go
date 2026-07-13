package monitor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/hostinventory"
	"github.com/example/gitops-dashboard/internal/scanner"
	"github.com/example/gitops-dashboard/internal/storage"
)

func TestPingTargetSyncsInventoryAndPersistsStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := t.TempDir()
	writeFile(t, filepath.Join(source, "inventory", "hosts.yml"), `
all:
  hosts:
    up:
      ansible_host: 10.0.0.1
    down:
      ansible_host: 10.0.0.2
`)
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "fixture")

	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	target := config.PingTarget{Name: "homelab", Repository: "fixture", AnsibleInventory: "inventory/hosts.yml", Timeout: "1s"}
	monitor := New(config.Config{
		Server: config.ServerConfig{
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(dataDir, "repos"),
		},
		Repositories: []config.RepositoryConfig{{
			Name:       "fixture",
			URL:        "file://" + source,
			DefaultRef: "main",
		}},
	}, store, slog.Default())
	err = monitor.checkPingWithPinger(ctx, target, func(_ context.Context, address string, _ time.Duration) error {
		if address == "10.0.0.2" {
			return errors.New("timeout")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]core.Service{}
	for _, service := range summary.Services {
		byName[service.Name] = service
	}
	if byName["up"].Runtime != "host" || byName["up"].Health != core.HealthHealthy {
		t.Fatalf("up service = %#v", byName["up"])
	}
	if byName["down"].Runtime != "host" || byName["down"].Health != core.HealthUnhealthy {
		t.Fatalf("down service = %#v", byName["down"])
	}
	if len(summary.Statuses) != 2 {
		t.Fatalf("statuses = %#v, want two ping results", summary.Statuses)
	}
	if len(summary.Uptime) != 2 {
		t.Fatalf("uptime = %#v, want two ping series", summary.Uptime)
	}
	if byName["up"].Repository != "fixture" || byName["up"].SourcePath != "inventory/hosts.yml" {
		t.Fatalf("up service provenance = %#v", byName["up"])
	}
	if byName["up"].SourceCommit == "" {
		t.Fatal("up service SourceCommit is empty")
	}
}

func TestPingCheckSkipsCanceledResults(t *testing.T) {
	t.Parallel()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	monitor := New(config.Config{}, store, slog.Default())
	done := make(chan error, 1)
	go func() {
		done <- monitor.checkPingWithPinger(ctx, config.PingTarget{Name: "host", Host: "10.0.0.1", Timeout: "1s"}, func(ctx context.Context, _ string, _ time.Duration) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ping check to start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled ping check to finish")
	}

	statuses, err := store.StatusResults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 {
		t.Fatalf("statuses = %#v, want no persisted ping result after cancellation", statuses)
	}
}

func TestPingTargetReusesInventoryUntilRefreshInterval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := t.TempDir()
	inventoryPath := filepath.Join(source, "inventory", "hosts.yml")
	writeFile(t, inventoryPath, `
all:
  hosts:
    one:
      ansible_host: 10.0.0.1
`)
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "one host")

	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	target := config.PingTarget{
		Name:               "homelab",
		Repository:         "fixture",
		AnsibleInventory:   "inventory/hosts.yml",
		Timeout:            "1s",
		MinRefreshInterval: "1h",
	}
	monitor := New(config.Config{
		Server: config.ServerConfig{
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(dataDir, "repos"),
		},
		Repositories: []config.RepositoryConfig{{
			Name:       "fixture",
			URL:        "file://" + source,
			DefaultRef: "main",
		}},
	}, store, slog.Default())
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	monitor.pingCache.now = func() time.Time { return now }
	pinger := func(context.Context, string, time.Duration) error { return nil }

	if err := monitor.checkPingWithPinger(ctx, target, pinger); err != nil {
		t.Fatal(err)
	}
	assertHostServiceCount(t, store, 1)

	writeFile(t, inventoryPath, `
all:
  hosts:
    one:
      ansible_host: 10.0.0.1
    two:
      ansible_host: 10.0.0.2
`)
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "two hosts")

	now = now.Add(30 * time.Minute)
	if err := monitor.checkPingWithPinger(ctx, target, pinger); err != nil {
		t.Fatal(err)
	}
	assertHostServiceCount(t, store, 1)

	now = now.Add(31 * time.Minute)
	if err := monitor.checkPingWithPinger(ctx, target, pinger); err != nil {
		t.Fatal(err)
	}
	assertHostServiceCount(t, store, 2)
}

func TestPingServicesForTargetExcludesUnrelatedRuntimeServices(t *testing.T) {
	t.Parallel()
	target := config.PingTarget{Repository: "kube", AnsibleInventory: "ansible/hosts.yml"}
	covered := pingServicesForTarget([]core.Service{
		{ID: "host", Repository: hostinventory.RepositoryName(target), SourcePath: hostinventory.Source(target), Runtime: "host"},
		{ID: "docker", Repository: "kube", SourcePath: "docker-compose.yml", Runtime: "compose"},
		{ID: "other-host", Repository: "kube", SourcePath: "other.yml", Runtime: "host"},
	}, target)
	if len(covered) != 1 || covered[0].ID != "host" {
		t.Fatalf("covered services = %#v", covered)
	}
}

func TestPingTargetReusesEmptyInventoryUntilRefreshInterval(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	writeFile(t, filepath.Join(source, "inventory", "hosts.yml"), `
all:
  hosts: {}
`)
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "empty inventory")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	counterPath := filepath.Join(binDir, "git-count")
	gitShim := filepath.Join(binDir, "git")
	script := "#!/bin/sh\n" +
		"count=0\n" +
		"if [ -f " + shellQuote(counterPath) + " ]; then count=$(cat " + shellQuote(counterPath) + "); fi\n" +
		"count=$((count + 1))\n" +
		"echo \"$count\" > " + shellQuote(counterPath) + "\n" +
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

	target := config.PingTarget{
		Name:               "homelab",
		Repository:         "fixture",
		AnsibleInventory:   "inventory/hosts.yml",
		Timeout:            "1s",
		MinRefreshInterval: "1h",
	}
	monitor := New(config.Config{
		Server: config.ServerConfig{
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(dataDir, "repos"),
		},
		Repositories: []config.RepositoryConfig{{
			Name:       "fixture",
			URL:        "file://" + source,
			DefaultRef: "main",
		}},
	}, store, slog.Default())
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	monitor.pingCache.now = func() time.Time { return now }
	pinger := func(context.Context, string, time.Duration) error {
		t.Fatal("empty inventory attempted a ping")
		return nil
	}

	if err := monitor.checkPingWithPinger(ctx, target, pinger); err != nil {
		t.Fatal(err)
	}
	firstGitCount := readGitCount(t, counterPath)
	if firstGitCount == 0 {
		t.Fatal("first empty inventory check did not invoke git")
	}
	assertHostServiceCount(t, store, 0)

	now = now.Add(30 * time.Minute)
	if err := monitor.checkPingWithPinger(ctx, target, pinger); err != nil {
		t.Fatal(err)
	}
	if got := readGitCount(t, counterPath); got != firstGitCount {
		t.Fatalf("git calls after cached empty inventory = %d, want %d", got, firstGitCount)
	}
	assertHostServiceCount(t, store, 0)
}

func TestPingTargetRefreshesCachedInventoryAfterRepositoryScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := t.TempDir()
	inventoryPath := filepath.Join(source, "inventory", "hosts.yml")
	writeFile(t, inventoryPath, `
all:
  hosts:
    serenity:
      ansible_host: 10.0.0.1
`)
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "initial host")

	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	target := config.PingTarget{
		Name:               "homelab",
		Repository:         "fixture",
		AnsibleInventory:   "inventory/hosts.yml",
		Timeout:            "1s",
		MinRefreshInterval: "1h",
	}
	cfg := config.Config{
		Server: config.ServerConfig{
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(dataDir, "repos"),
		},
		Repositories: []config.RepositoryConfig{{
			Name:       "fixture",
			URL:        "file://" + source,
			DefaultRef: "main",
		}},
		Runtime: config.RuntimeConfig{
			Ping: []config.PingTarget{target},
		},
	}
	monitor := New(cfg, store, slog.Default())
	var pinged []string
	pinger := func(_ context.Context, address string, _ time.Duration) error {
		pinged = append(pinged, address)
		return nil
	}
	if err := monitor.checkPingWithPinger(ctx, target, pinger); err != nil {
		t.Fatal(err)
	}
	if len(pinged) != 1 || pinged[0] != "10.0.0.1" {
		t.Fatalf("initial pinged addresses = %#v, want old inventory address", pinged)
	}

	writeFile(t, inventoryPath, `
all:
  hosts:
    serenity:
      ansible_host: 10.0.0.2
`)
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "change host address")
	if err := scanner.New(cfg, store, slog.Default()).ScanAll(ctx); err != nil {
		t.Fatal(err)
	}

	pinged = nil
	if err := monitor.checkPingWithPinger(ctx, target, pinger); err != nil {
		t.Fatal(err)
	}
	if len(pinged) != 1 || pinged[0] != "10.0.0.2" {
		t.Fatalf("post-scan pinged addresses = %#v, want refreshed inventory address", pinged)
	}
}

func TestPingCheckSizesDeadlineAfterInventoryRefreshGrowth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inventoryPath := filepath.Join(t.TempDir(), "hosts.yml")
	writePingInventoryHosts(t, inventoryPath, 2)

	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	target := config.PingTarget{
		Name:               "homelab",
		AnsibleInventory:   inventoryPath,
		Timeout:            "3s",
		MinRefreshInterval: "1h",
	}
	monitor := New(config.Config{
		Runtime: config.RuntimeConfig{
			Ping: []config.PingTarget{target},
		},
	}, store, slog.Default())
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	monitor.pingCache.now = func() time.Time { return now }
	monitor.ping = func(context.Context, string, time.Duration) error {
		return nil
	}

	if err := monitor.CheckAll(ctx); err != nil {
		t.Fatal(err)
	}
	assertHostServiceCount(t, store, 2)

	writePingInventoryHosts(t, inventoryPath, 20)
	now = now.Add(2 * time.Hour)
	attempted := make(chan string, 20)
	monitor.ping = func(ctx context.Context, address string, _ time.Duration) error {
		attempted <- address
		select {
		case <-time.After(2700 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if err := monitor.CheckAll(ctx); err != nil {
		t.Fatal(err)
	}
	close(attempted)
	attempts := map[string]bool{}
	for address := range attempted {
		attempts[address] = true
	}
	if len(attempts) != 20 {
		t.Fatalf("attempted hosts = %#v, want all 20 refreshed hosts", attempts)
	}

	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 20 {
		t.Fatalf("statuses = %#v, want 20 refreshed host statuses", statuses)
	}
	for _, status := range statuses {
		if status.Health != core.HealthHealthy || strings.Contains(status.Message, "timed out") {
			t.Fatalf("status = %#v, want successful attempted ping", status)
		}
	}
}

func TestSlowPingInventoryRefreshDoesNotBlockOtherTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fifo := filepath.Join(t.TempDir(), "hosts.yml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	monitor := New(config.Config{}, store, slog.Default())
	slowTarget := config.PingTarget{Name: "slow", AnsibleInventory: fifo, Timeout: "1s"}
	fastTarget := config.PingTarget{Name: "fast", Host: "fast.example.test", Timeout: "1s"}
	slowDone := make(chan error, 1)
	go func() {
		slowDone <- monitor.checkPingWithPinger(ctx, slowTarget, func(context.Context, string, time.Duration) error {
			return nil
		})
	}()
	waitForPingRefresh(t, monitor.pingCache, pingInventoryCacheKey(slowTarget))

	fastDone := make(chan error, 1)
	go func() {
		fastDone <- monitor.checkPingWithPinger(ctx, fastTarget, func(context.Context, string, time.Duration) error {
			return nil
		})
	}()
	fastTimedOut := false
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatalf("fast ping failed: %v", err)
		}
	case <-time.After(150 * time.Millisecond):
		fastTimedOut = true
	}

	releaseFIFO(t, fifo, `
all:
  hosts:
    slow:
      ansible_host: 10.0.0.1
`)
	select {
	case err := <-slowDone:
		if err != nil {
			t.Fatalf("slow ping failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if fastTimedOut {
		t.Fatal("fast ping target blocked behind slow inventory refresh")
	}
}

func TestSyncPingTargetsPrunesStaleInventorySource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := t.TempDir()
	writeFile(t, filepath.Join(source, "inventory", "hosts.yml"), `
all:
  hosts:
    serenity:
      ansible_host: 10.0.0.15
`)
	runGit(t, source, "init", "-b", "main")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "fixture")

	dataDir := t.TempDir()
	store, err := storage.Open(filepath.Join(dataDir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	staleHost := core.Service{
		ID:         "host-stale",
		Name:       "serenity",
		Repository: "ping/ansible-hosts",
		SourcePath: "/ansible/hosts.yml",
		Runtime:    "host",
		Kind:       "Host",
		Health:     core.HealthUnknown,
	}
	if err := store.ReplaceRuntimeServices(ctx, "ping/ansible-hosts", "/ansible/hosts.yml", "host", []core.Service{staleHost}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "host-stale",
		Target:    "ansible-hosts",
		Health:    core.HealthHealthy,
		Message:   "pong",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	monitor := New(config.Config{
		Server: config.ServerConfig{
			DataDir:      dataDir,
			RepoCacheDir: filepath.Join(dataDir, "repos"),
		},
		Repositories: []config.RepositoryConfig{{
			Name:       "fixture",
			URL:        "file://" + source,
			DefaultRef: "main",
		}},
		Runtime: config.RuntimeConfig{
			Ping: []config.PingTarget{{
				Name:             "ansible-hosts",
				Repository:       "fixture",
				AnsibleInventory: "inventory/hosts.yml",
			}},
		},
	}, store, slog.Default())
	if err := monitor.SyncPingTargets(ctx); err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hostServices []core.Service
	for _, service := range summary.Services {
		if service.Runtime == "host" {
			hostServices = append(hostServices, service)
		}
	}
	if len(hostServices) != 1 || hostServices[0].Repository != "fixture" || hostServices[0].SourcePath != "inventory/hosts.yml" {
		t.Fatalf("host services = %#v, want only current repo-backed host", hostServices)
	}
	for _, repo := range summary.Repositories {
		if repo.Name == "ping/ansible-hosts" {
			t.Fatalf("stale configured repository still present: %#v", summary.Repositories)
		}
	}
	if len(summary.Statuses) != 0 {
		t.Fatalf("statuses = %#v, want stale status pruned", summary.Statuses)
	}
}

func assertHostServiceCount(t *testing.T, store *storage.Store, want int) {
	t.Helper()
	summary, err := store.Summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, service := range summary.Services {
		if service.Runtime == "host" {
			got++
		}
	}
	if got != want {
		t.Fatalf("host service count = %d, want %d; services=%#v", got, want, summary.Services)
	}
}

func waitForPingRefresh(t *testing.T, cache *pingInventoryCache, key string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		cache.mu.Lock()
		_, ok := cache.refreshes[key]
		cache.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for ping refresh to start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func releaseFIFO(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.TrimSpace(content) + "\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writePingInventoryHosts(t *testing.T, path string, count int) {
	t.Helper()
	var builder strings.Builder
	builder.WriteString("all:\n  hosts:\n")
	for i := 0; i < count; i++ {
		builder.WriteString("    host-")
		builder.WriteString(strconv.Itoa(i))
		builder.WriteString(":\n      ansible_host: 10.0.0.")
		builder.WriteString(strconv.Itoa(i + 1))
		builder.WriteString("\n")
	}
	writeFile(t, path, builder.String())
}

func readGitCount(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
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
