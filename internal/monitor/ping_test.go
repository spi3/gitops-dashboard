package monitor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
)

func TestPingTargetSyncsInventoryAndPersistsStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inventory := filepath.Join(t.TempDir(), "hosts.yml")
	if err := os.WriteFile(inventory, []byte(`
all:
  hosts:
    up:
      ansible_host: 10.0.0.1
    down:
      ansible_host: 10.0.0.2
`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	target := config.PingTarget{Name: "homelab", AnsibleInventory: inventory, Timeout: "1s"}
	monitor := New(config.Config{}, store, slog.Default())
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
}
