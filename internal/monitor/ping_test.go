package monitor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
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
