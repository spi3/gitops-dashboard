package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
)

func TestStorePersistsSummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repos := []config.RepositoryConfig{{Name: "repo", URL: "https://example.invalid/repo.git", DefaultRef: "main"}}
	if err := store.EnsureRepositories(ctx, repos); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	services := []core.Service{{
		ID:           "svc",
		Name:         "api",
		Repository:   "repo",
		SourceCommit: "abc123",
		SourcePath:   "prod/compose.yaml",
		Runtime:      "compose",
		Environment:  "production",
		Health:       core.HealthUnknown,
		Images:       []string{"example/api:v1"},
		Warnings:     []string{"missing healthcheck"},
	}}
	if err := store.FinishScan(ctx, scanID, "repo", "abc123", services, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc",
		Target:    "local",
		Health:    core.HealthHealthy,
		Message:   "running",
		CheckedAt: time.Date(2026, 6, 27, 16, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Repositories) != 1 || summary.Repositories[0].LastCommit != "abc123" {
		t.Fatalf("repositories = %#v", summary.Repositories)
	}
	if len(summary.Services) != 1 || summary.Services[0].Images[0] != "example/api:v1" {
		t.Fatalf("services = %#v", summary.Services)
	}
	if len(summary.Scans) != 1 || summary.Scans[0].CommitSHA != "abc123" {
		t.Fatalf("scans = %#v", summary.Scans)
	}
	if len(summary.Statuses) != 1 || summary.Statuses[0].Target != "local" {
		t.Fatalf("statuses = %#v", summary.Statuses)
	}
	if summary.Services[0].Health != core.HealthHealthy {
		t.Fatalf("service health = %s, want healthy", summary.Services[0].Health)
	}
	service := summary.Services[0]
	listFields := map[string][]string{
		"images":       service.Images,
		"ports":        service.Ports,
		"dependencies": service.Dependencies,
		"storage":      service.Storage,
		"exposure":     service.Exposure,
		"configRefs":   service.ConfigRefs,
		"warnings":     service.Warnings,
	}
	for name, values := range listFields {
		if values == nil {
			t.Fatalf("%s field is nil, want empty slice", name)
		}
	}
}

func countRows(t *testing.T, store *Store, query string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(), query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestUptimeTracksHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc", Target: "local", Health: core.HealthHealthy, Message: "up", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc", Target: "local", Health: core.HealthUnhealthy, Message: "down", CheckedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results"); got != 1 {
		t.Fatalf("status_results rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history"); got != 2 {
		t.Fatalf("status_history rows = %d, want 2", got)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 {
		t.Fatalf("uptime = %#v", summary.Uptime)
	}
	stat := summary.Uptime[0]
	if stat.ServiceID != "svc" || stat.Target != "local" {
		t.Fatalf("stat identity = %#v", stat)
	}
	if stat.CheckCount != 2 {
		t.Fatalf("checkCount = %d, want 2", stat.CheckCount)
	}
	if stat.UptimePercent != 50.0 {
		t.Fatalf("uptimePercent = %v, want 50.0", stat.UptimePercent)
	}
	if len(stat.Samples) != 2 {
		t.Fatalf("samples len = %d, want 2", len(stat.Samples))
	}
	if !stat.Samples[0].CheckedAt.Before(stat.Samples[1].CheckedAt) {
		t.Fatalf("samples not ascending: %v then %v", stat.Samples[0].CheckedAt, stat.Samples[1].CheckedAt)
	}
	if stat.Samples[0].Health != core.HealthHealthy || stat.Samples[1].Health != core.HealthUnhealthy {
		t.Fatalf("sample healths = %s, %s", stat.Samples[0].Health, stat.Samples[1].Health)
	}
}

func TestSummaryHealthPrefersActionableStatusAcrossTargets(t *testing.T) {
	t.Parallel()
	statusTime := time.Date(2026, 6, 27, 16, 0, 0, 0, time.UTC)
	services := []core.Service{{ID: "svc", Health: core.HealthUnknown}}
	statuses := []core.StatusResult{
		{ServiceID: "svc", Target: "docker", Health: core.HealthUnknown, CheckedAt: statusTime},
		{ServiceID: "svc", Target: "routes", Health: core.HealthHealthy, CheckedAt: statusTime},
		{ServiceID: "svc", Target: "docker", Health: core.HealthUnknown, CheckedAt: statusTime.Add(time.Minute)},
	}
	applyLatestStatus(services, statuses)
	if services[0].Health != core.HealthHealthy {
		t.Fatalf("health = %s, want healthy", services[0].Health)
	}
}

func TestUptimeHealthClassification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	healths := []core.HealthState{
		core.HealthHealthy, core.HealthDegraded, core.HealthUnhealthy, core.HealthUnknown, core.HealthError,
	}
	for i, health := range healths {
		if err := store.UpsertStatus(ctx, core.StatusResult{
			ServiceID: "svc", Target: "local", Health: health, Message: "check",
			CheckedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 {
		t.Fatalf("uptime = %#v", summary.Uptime)
	}
	stat := summary.Uptime[0]
	if stat.CheckCount != 5 {
		t.Fatalf("checkCount = %d, want 5", stat.CheckCount)
	}
	// healthy and degraded count as up, so 2 of 5 checks are up.
	if stat.UptimePercent != 40.0 {
		t.Fatalf("uptimePercent = %v, want 40.0", stat.UptimePercent)
	}
}

func TestUptimeSamplesCappedAndOrdered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	for i := 0; i < 45; i++ {
		if err := store.UpsertStatus(ctx, core.StatusResult{
			ServiceID: "svc", Target: "local", Health: core.HealthHealthy, Message: "up",
			CheckedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 {
		t.Fatalf("uptime = %#v", summary.Uptime)
	}
	stat := summary.Uptime[0]
	if stat.CheckCount != 45 {
		t.Fatalf("checkCount = %d, want 45", stat.CheckCount)
	}
	if len(stat.Samples) != 40 {
		t.Fatalf("samples len = %d, want 40", len(stat.Samples))
	}
	for i := 1; i < len(stat.Samples); i++ {
		if stat.Samples[i].CheckedAt.Before(stat.Samples[i-1].CheckedAt) {
			t.Fatalf("samples not ascending at index %d", i)
		}
	}
	// The oldest 5 of 45 are dropped, so the earliest retained sample is the 6th insert.
	if wantFirst := base.Add(5 * time.Minute); !stat.Samples[0].CheckedAt.Equal(wantFirst) {
		t.Fatalf("first sample = %v, want %v", stat.Samples[0].CheckedAt, wantFirst)
	}
	if wantLast := base.Add(44 * time.Minute); !stat.Samples[len(stat.Samples)-1].CheckedAt.Equal(wantLast) {
		t.Fatalf("last sample = %v, want %v", stat.Samples[len(stat.Samples)-1].CheckedAt, wantLast)
	}
}

func TestUptimePruneAndWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	// Insert directly so these rows survive to be observed; UpsertStatus would prune the old one in its own transaction.
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	stale := now.Add(-30 * time.Hour).Format(time.RFC3339)
	for _, checkedAt := range []string{old, stale} {
		if _, err := store.db.ExecContext(ctx, `
INSERT INTO status_history(service_id, target, health, message, checked_at)
VALUES('svc', 'local', 'healthy', 'up', ?)`, checkedAt); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history"); got != 2 {
		t.Fatalf("pre-prune history rows = %d, want 2", got)
	}
	// A fresh check triggers the 7-day prune and adds a sample inside the window.
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc", Target: "local", Health: core.HealthHealthy, Message: "up", CheckedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// The 10-day-old row is pruned; the 30-hour-old row and the fresh row remain.
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history"); got != 2 {
		t.Fatalf("post-prune history rows = %d, want 2", got)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 {
		t.Fatalf("uptime = %#v", summary.Uptime)
	}
	// Only the fresh row is inside the 24h window; the 30-hour-old row is excluded from stats.
	if summary.Uptime[0].CheckCount != 1 {
		t.Fatalf("checkCount = %d, want 1", summary.Uptime[0].CheckCount)
	}
	if len(summary.Uptime[0].Samples) != 1 {
		t.Fatalf("samples len = %d, want 1", len(summary.Uptime[0].Samples))
	}
}

func TestUptimeEmptyIsSlice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Uptime == nil {
		t.Fatal("uptime is nil, want empty slice")
	}
	if len(summary.Uptime) != 0 {
		t.Fatalf("uptime = %#v, want empty", summary.Uptime)
	}
}
