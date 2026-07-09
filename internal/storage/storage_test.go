package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
		Images:       []string{"example/api:v1.0.0"},
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
		ObservedImages: []core.ObservedImage{
			core.NewObservedImage("local", "docker", "example/api:v1.0.0", "sha256:local", nil),
		},
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
	if len(summary.Services) != 1 || summary.Services[0].Images[0] != "example/api:v1.0.0" {
		t.Fatalf("services = %#v", summary.Services)
	}
	if summary.Services[0].ImageVersionState != core.ImageVersionMatching {
		t.Fatalf("image version state = %s, want matching; checks=%#v", summary.Services[0].ImageVersionState, summary.Services[0].ImageVersionChecks)
	}
	if len(summary.Statuses[0].ObservedImages) != 1 || summary.Statuses[0].ObservedImages[0].Reference.Tag != "v1.0.0" {
		t.Fatalf("observed images = %#v, want persisted image metadata", summary.Statuses[0].ObservedImages)
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

func TestStoreRedactsKnownTokensWhenPersistingScanAndStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "storage-secret-token-t008"
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.AddRedactionValues(token)
	repos := []config.RepositoryConfig{{Name: "repo", URL: "https://deploy:" + token + "@example.com/repo.git", DefaultRef: "main"}}
	if err := store.EnsureRepositories(ctx, repos); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	scanErr := errors.New("git clone https://x-access-token:" + token + "@example.com/org/repo.git failed with " + token)
	if err := store.FinishScan(ctx, scanID, "repo", "", nil, scanErr); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc",
		Target:    "local",
		Health:    core.HealthError,
		Message:   "git fetch https://user:" + token + "@example.com/org/repo.git failed with " + token,
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT COALESCE(url, '') FROM repositories`,
		`SELECT COALESCE(error, '') FROM scans`,
		`SELECT COALESCE(error, '') FROM repositories`,
		`SELECT COALESCE(message, '') FROM status_results`,
		`SELECT COALESCE(message, '') FROM status_history`,
	} {
		rows, err := store.db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			assertRedacted(t, value, token)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range summary.Repositories {
		assertRedacted(t, repo.URL, token)
		assertRedacted(t, repo.Error, token)
	}
	for _, scan := range summary.Scans {
		assertRedacted(t, scan.Error, token)
	}
	for _, status := range summary.Statuses {
		assertRedacted(t, status.Message, token)
	}
}

func TestStoreAddRedactionValuesDedupesRawValues(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token := "storage/secret-token"
	store.AddRedactionValues(token)
	firstSize := len(store.redactionTokens)
	if firstSize == 0 {
		t.Fatal("redaction token set is empty")
	}
	store.AddRedactionValues(token)
	if len(store.redactionTokens) != firstSize {
		t.Fatalf("redaction token set size = %d, want %d after duplicate add", len(store.redactionTokens), firstSize)
	}
	for _, value := range store.redactionTokens {
		if strings.Contains(value, "%252F") {
			t.Fatalf("redaction token was re-escaped: %#v", store.redactionTokens)
		}
	}
}

func TestRedactPersistedSensitiveValuesCompactsDatabaseFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "physical-secret-token-t008"
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store.AddRedactionValues(token)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO repositories(name, url, default_ref, status, error)
VALUES(?, ?, 'main', 'error', ?)
`, "repo", "https://deploy:"+token+"@example.com/repo.git", "clone failed: "+token); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO scans(repository, status, started_at, error)
VALUES(?, 'error', ?, ?)
`, "repo", time.Now().UTC().Format(time.RFC3339), "scan failed: "+token); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if !sqliteFilesContainToken(t, dbPath, token) {
		t.Fatal("test setup did not write token bytes into sqlite files")
	}
	if err := store.RedactPersistedSensitiveValues(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if sqliteFilesContainToken(t, dbPath, token) {
		t.Fatal("sqlite files still contain token bytes after persisted redaction cleanup")
	}
}

func TestReplaceConfiguredServicesPreservesStableStatusHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "host-1",
		Name:        "serenity",
		Repository:  "ping/hosts",
		SourcePath:  "hosts.yml",
		Runtime:     "host",
		Kind:        "Host",
		Health:      core.HealthUnknown,
		Environment: "infrastructure",
	}
	if err := store.ReplaceConfiguredServices(ctx, "ping/hosts", "hosts.yml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "host-1",
		Target:    "ping/hosts",
		Health:    core.HealthHealthy,
		Message:   "pong",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceConfiguredServices(ctx, "ping/hosts", "hosts.yml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='host-1'"); got != 1 {
		t.Fatalf("stable host history rows = %d, want 1", got)
	}
	if err := store.ReplaceConfiguredServices(ctx, "ping/hosts", "hosts.yml", nil); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='host-1'"); got != 0 {
		t.Fatalf("removed host history rows = %d, want 0", got)
	}
}

func TestReplaceRuntimeServicesPreservesOtherRepositoryServices(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "kube",
		URL:        "https://example.invalid/kube.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "kube")
	if err != nil {
		t.Fatal(err)
	}
	composeService := core.Service{
		ID:           "compose-1",
		Name:         "web",
		Repository:   "kube",
		SourceCommit: "abc123",
		SourcePath:   "docker_files/web/docker-compose.yml",
		Runtime:      "compose",
		Kind:         "Service",
		Health:       core.HealthUnknown,
	}
	if err := store.FinishScan(ctx, scanID, "kube", "abc123", []core.Service{composeService}, nil); err != nil {
		t.Fatal(err)
	}
	hostService := core.Service{
		ID:           "host-1",
		Name:         "serenity",
		Repository:   "kube",
		SourceCommit: "abc123",
		SourcePath:   "infrastructure/inventory/hosts.yml",
		Runtime:      "host",
		Kind:         "Host",
		Health:       core.HealthUnknown,
	}
	if err := store.ReplaceRuntimeServices(ctx, "kube", "infrastructure/inventory/hosts.yml", "host", []core.Service{hostService}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "host-1",
		Target:    "homelab",
		Health:    core.HealthHealthy,
		Message:   "pong",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRuntimeServices(ctx, "kube", "infrastructure/inventory/hosts.yml", "host", nil); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Services) != 1 || summary.Services[0].ID != "compose-1" {
		t.Fatalf("services = %#v, want only compose service preserved", summary.Services)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='host-1'"); got != 0 {
		t.Fatalf("removed host history rows = %d, want 0", got)
	}
}

func TestPruneRuntimeServicesRemovesStaleSources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "kube",
		URL:        "https://example.invalid/kube.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "kube")
	if err != nil {
		t.Fatal(err)
	}
	keptHost := core.Service{
		ID:           "host-kept",
		Name:         "serenity",
		Repository:   "kube",
		SourceCommit: "abc123",
		SourcePath:   "infrastructure/inventory/hosts.yml",
		Runtime:      "host",
		Kind:         "Host",
		Health:       core.HealthUnknown,
	}
	composeService := core.Service{
		ID:           "compose-1",
		Name:         "gitops-dashboard",
		Repository:   "kube",
		SourceCommit: "abc123",
		SourcePath:   "docker_files/serenity/gitops-dashboard/docker-compose.yml",
		Runtime:      "compose",
		Kind:         "Service",
		Health:       core.HealthUnknown,
	}
	if err := store.FinishScan(ctx, scanID, "kube", "abc123", []core.Service{keptHost, composeService}, nil); err != nil {
		t.Fatal(err)
	}
	staleHost := core.Service{
		ID:           "host-stale",
		Name:         "serenity",
		Repository:   "ping/ansible-hosts",
		SourceCommit: "",
		SourcePath:   "/ansible/hosts.yml",
		Runtime:      "host",
		Kind:         "Host",
		Health:       core.HealthUnknown,
	}
	if err := store.ReplaceRuntimeServices(ctx, "ping/ansible-hosts", "/ansible/hosts.yml", "host", []core.Service{staleHost}); err != nil {
		t.Fatal(err)
	}
	for _, serviceID := range []string{"host-kept", "host-stale"} {
		if err := store.UpsertStatus(ctx, core.StatusResult{
			ServiceID: serviceID,
			Target:    "ansible-hosts",
			Health:    core.HealthHealthy,
			Message:   "pong",
			CheckedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	err = store.PruneRuntimeServices(ctx, "host", []RuntimeServiceSource{{
		Repository: "kube",
		SourcePath: "infrastructure/inventory/hosts.yml",
	}})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	servicesByID := map[string]core.Service{}
	for _, service := range summary.Services {
		servicesByID[service.ID] = service
	}
	if _, ok := servicesByID["host-kept"]; !ok {
		t.Fatalf("kept host missing from services: %#v", summary.Services)
	}
	if _, ok := servicesByID["compose-1"]; !ok {
		t.Fatalf("compose service missing from services: %#v", summary.Services)
	}
	if _, ok := servicesByID["host-stale"]; ok {
		t.Fatalf("stale host still present: %#v", summary.Services)
	}
	for _, repo := range summary.Repositories {
		if repo.Name == "ping/ansible-hosts" {
			t.Fatalf("stale configured repository still present: %#v", summary.Repositories)
		}
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='host-kept'"); got != 1 {
		t.Fatalf("kept host history rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='host-stale'"); got != 0 {
		t.Fatalf("stale host history rows = %d, want 0", got)
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

func TestSummaryHealthAggregatesAcrossTargets(t *testing.T) {
	t.Parallel()
	statusTime := time.Date(2026, 6, 27, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		statuses []core.StatusResult
		want     core.HealthState
	}{
		{
			name: "mixed target results degrade",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "docker", Health: core.HealthUnknown, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "routes", Health: core.HealthHealthy, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "docker", Health: core.HealthUnknown, CheckedAt: statusTime.Add(time.Minute)},
			},
			want: core.HealthDegraded,
		},
		{
			name: "all target results healthy",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "docker", Health: core.HealthHealthy, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "routes", Health: core.HealthHealthy, CheckedAt: statusTime},
			},
			want: core.HealthHealthy,
		},
		{
			name: "single failed target stays failed",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "routes", Health: core.HealthError, CheckedAt: statusTime},
			},
			want: core.HealthError,
		},
		{
			name: "all non-passing targets use worst status",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "docker", Health: core.HealthUnhealthy, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "routes", Health: core.HealthError, CheckedAt: statusTime},
			},
			want: core.HealthError,
		},
		{
			name: "not applicable targets are ignored",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "routes: http://10.10.10.20", Health: core.HealthNotApplicable, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "routes: https://app.example.test", Health: core.HealthHealthy, CheckedAt: statusTime},
			},
			want: core.HealthHealthy,
		},
		{
			name: "only not applicable targets leave health unknown",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "routes: http://10.10.10.20", Health: core.HealthNotApplicable, CheckedAt: statusTime},
			},
			want: core.HealthUnknown,
		},
		{
			name: "all routes not applicable use other monitor targets",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "routes: http://10.10.10.20", Health: core.HealthNotApplicable, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "routes: https://app.example.test", Health: core.HealthNotApplicable, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "docker", Health: core.HealthUnhealthy, CheckedAt: statusTime},
			},
			want: core.HealthUnhealthy,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			services := []core.Service{{ID: "svc", Health: core.HealthUnknown}}
			applyLatestStatus(services, tc.statuses)
			if services[0].Health != tc.want {
				t.Fatalf("health = %s, want %s", services[0].Health, tc.want)
			}
		})
	}
}

func TestSetMonitorNotApplicableRemovesTargetFromHealthAndUptime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "svc-app",
		Name:        "app",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	goodTarget := "routes: https://app.example.test"
	badTarget := "routes: http://10.10.10.20"
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-app", Target: goodTarget, Health: core.HealthHealthy, Message: "ok", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-app", Target: badTarget, Health: core.HealthError, Message: "dial failed", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].Health != core.HealthDegraded {
		t.Fatalf("pre-override service health = %s, want degraded", summary.Services[0].Health)
	}
	if len(summary.Uptime) != 2 {
		t.Fatalf("pre-override uptime = %#v, want two targets", summary.Uptime)
	}

	if err := store.SetMonitorNotApplicable(ctx, "svc-app", badTarget, true); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-app", Target: badTarget, Health: core.HealthError, Message: "dial failed again", CheckedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	summary, err = store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].Health != core.HealthHealthy {
		t.Fatalf("post-override service health = %s, want healthy", summary.Services[0].Health)
	}
	statusByTarget := map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		statusByTarget[status.Target] = status
	}
	if statusByTarget[badTarget].Health != core.HealthNotApplicable {
		t.Fatalf("ignored status = %#v, want not_applicable", statusByTarget[badTarget])
	}
	if len(summary.Uptime) != 1 || summary.Uptime[0].Target != goodTarget {
		t.Fatalf("post-override uptime = %#v, want only good target", summary.Uptime)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='svc-app' AND target='routes: http://10.10.10.20'"); got != 0 {
		t.Fatalf("ignored status history rows = %d, want 0", got)
	}

	if err := store.SetMonitorNotApplicable(ctx, "svc-app", badTarget, false); err != nil {
		t.Fatal(err)
	}
	summary, err = store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statusByTarget = map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		statusByTarget[status.Target] = status
	}
	if statusByTarget[badTarget].Health != core.HealthUnknown {
		t.Fatalf("re-enabled status = %#v, want unknown until next check", statusByTarget[badTarget])
	}
}

func TestSetMonitorNotApplicableClearsImageVersionContribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "svc-api",
		Name:        "api",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Health:      core.HealthUnknown,
		Images:      []string{"example/api:v1.0.0"},
		Environment: "production",
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	status := core.StatusResult{
		ServiceID: "svc-api",
		Target:    "docker",
		Health:    core.HealthHealthy,
		Message:   "running",
		CheckedAt: time.Now().UTC(),
		ObservedImages: []core.ObservedImage{
			core.NewObservedImage("docker", "docker", "example/api:v1.0.0", "sha256:api", nil),
		},
	}
	if err := store.UpsertStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc-api", "docker", true); err != nil {
		t.Fatal(err)
	}
	status.Message = "running after override"
	status.CheckedAt = status.CheckedAt.Add(time.Minute)
	if err := store.UpsertStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].ImageVersionState != core.ImageVersionUnknown {
		t.Fatalf("image version state = %s, want unknown; checks=%#v", summary.Services[0].ImageVersionState, summary.Services[0].ImageVersionChecks)
	}
	if len(summary.Services[0].ImageVersionChecks) != 1 || summary.Services[0].ImageVersionChecks[0].Observed != nil {
		t.Fatalf("image checks = %#v, want no observed image contribution", summary.Services[0].ImageVersionChecks)
	}
	statusByTarget := map[string]core.StatusResult{}
	for _, item := range summary.Statuses {
		statusByTarget[item.Target] = item
	}
	if statusByTarget["docker"].Health != core.HealthNotApplicable {
		t.Fatalf("docker status = %#v, want not_applicable", statusByTarget["docker"])
	}
	if len(statusByTarget["docker"].ObservedImages) != 0 {
		t.Fatalf("observed images = %#v, want cleared on override", statusByTarget["docker"].ObservedImages)
	}
}

func TestParentRouteOverrideSuppressesLateChildStatusWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "svc-app",
		Name:        "app",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure:    []string{"https://app.example.test"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}

	if err := store.SetMonitorNotApplicable(ctx, "svc-app", "routes", true); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-app",
		Target:    "routes: https://app.example.test",
		Health:    core.HealthUnhealthy,
		Message:   "late check failed",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].Health != core.HealthUnknown {
		t.Fatalf("service health = %s, want unknown", summary.Services[0].Health)
	}
	if len(summary.Uptime) != 0 {
		t.Fatalf("uptime = %#v, want no child route contributions", summary.Uptime)
	}
	statusByTarget := map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		statusByTarget[status.Target] = status
	}
	if statusByTarget["routes"].Health != core.HealthNotApplicable {
		t.Fatalf("parent status = %#v, want not_applicable", statusByTarget["routes"])
	}
	if _, ok := statusByTarget["routes: https://app.example.test"]; ok {
		t.Fatalf("late child route status was stored despite parent override: %#v", statusByTarget["routes: https://app.example.test"])
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE service_id='svc-app' AND target='routes: https://app.example.test'"); got != 0 {
		t.Fatalf("child status rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='svc-app' AND target='routes: https://app.example.test'"); got != 0 {
		t.Fatalf("child history rows = %d, want 0", got)
	}
}

func TestSetMonitorNotApplicableAcceptsConfiguredSyntheticRouteTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "svc-app",
		Name:        "app",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure: []string{
			"https://app.example.test",
			"ssh://app.example.test:22",
		},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}

	target := "routes: https://app.example.test"
	if err := store.SetMonitorNotApplicable(ctx, "svc-app", target, true); err != nil {
		t.Fatalf("configured route override failed: %v", err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statusByTarget := map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		statusByTarget[status.Target] = status
	}
	if statusByTarget[target].Health != core.HealthNotApplicable {
		t.Fatalf("synthetic route status = %#v, want not_applicable", statusByTarget[target])
	}

	if err := store.SetMonitorNotApplicable(ctx, "svc-app", "routes: ssh://app.example.test:22", true); !errors.Is(err, ErrStatusNotFound) {
		t.Fatalf("ssh route override error = %v, want ErrStatusNotFound", err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc-app", "routes: https://missing.example.test", true); !errors.Is(err, ErrStatusNotFound) {
		t.Fatalf("missing route override error = %v, want ErrStatusNotFound", err)
	}
}

func TestSetMonitorNotApplicableCanonicalizesSyntheticRouteTargets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		target string
	}{
		{name: "trailing slash", target: "routes: https://app.example.test/"},
		{name: "default port", target: "routes: https://app.example.test:443"},
		{name: "case variant", target: "routes: HTTPS://APP.EXAMPLE.TEST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			service := core.Service{
				ID:          "svc-app",
				Name:        "app",
				Repository:  "repo",
				SourcePath:  "prod/compose.yaml",
				Runtime:     "compose",
				Kind:        "Service",
				Environment: "production",
				Health:      core.HealthUnknown,
				Exposure:    []string{"https://app.example.test"},
			}
			if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
				t.Fatal(err)
			}

			const canonicalTarget = "routes: https://app.example.test"
			if err := store.SetMonitorNotApplicable(ctx, "svc-app", tc.target, true); err != nil {
				t.Fatal(err)
			}
			if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE service_id='svc-app' AND target='routes: https://app.example.test'"); got != 1 {
				t.Fatalf("canonical override rows = %d, want 1", got)
			}
			if tc.target != canonicalTarget {
				var rawRows int
				if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitor_overrides WHERE service_id=? AND target=?`, "svc-app", tc.target).Scan(&rawRows); err != nil {
					t.Fatal(err)
				}
				if rawRows != 0 {
					t.Fatalf("raw override rows = %d, want 0", rawRows)
				}
			}
			ignored, err := store.MonitorNotApplicable(ctx, "svc-app", canonicalTarget)
			if err != nil {
				t.Fatal(err)
			}
			if !ignored {
				t.Fatalf("canonical target was not ignored")
			}
			ignored, err = store.MonitorNotApplicable(ctx, "svc-app", tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if !ignored {
				t.Fatalf("alias target was not ignored")
			}

			if err := store.SetMonitorNotApplicable(ctx, "svc-app", tc.target, false); err != nil {
				t.Fatal(err)
			}
			ignored, err = store.MonitorNotApplicable(ctx, "svc-app", canonicalTarget)
			if err != nil {
				t.Fatal(err)
			}
			if ignored {
				t.Fatalf("canonical target remained ignored after enabling alias")
			}
		})
	}
}

func TestMigrateCanonicalizesStoredRouteTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	service := core.Service{
		ID:          "svc-app",
		Name:        "app",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure:    []string{"https://app.example.test"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO monitor_overrides(service_id, target, not_applicable, updated_at) VALUES
  ('svc-app', 'routes: https://app.example.test', 0, '2026-01-02T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/', 1, '2026-01-01T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test:443', 0, '2026-01-03T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/admin/', 1, '2026-01-04T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/a%2Fb', 1, '2026-01-05T00:00:00Z');
INSERT INTO status_results(service_id, target, health, message, checked_at) VALUES
  ('svc-app', 'routes: https://app.example.test', 'healthy', 'canonical ok', '2026-01-01T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/', 'degraded', 'slash degraded', '2026-01-02T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test:443', 'error', 'default port failed', '2026-01-03T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/admin/', 'healthy', 'admin slash ok', '2026-01-04T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/a%2Fb', 'healthy', 'escaped slash ok', '2026-01-05T00:00:00Z');
INSERT INTO status_history(service_id, target, health, message, checked_at) VALUES
  ('svc-app', 'routes: https://app.example.test/', 'degraded', 'slash degraded', '2026-01-02T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test:443', 'error', 'default port failed', '2026-01-03T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/admin/', 'healthy', 'admin slash ok', '2026-01-04T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/a%2Fb', 'healthy', 'escaped slash ok', '2026-01-05T00:00:00Z');
`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target IN ('routes: https://app.example.test/', 'routes: https://app.example.test:443')"); got != 0 {
		t.Fatalf("alias override rows = %d, want 0", got)
	}
	var notApplicable int
	var updatedAt string
	if err := store.db.QueryRowContext(ctx, `
SELECT not_applicable, updated_at
FROM monitor_overrides
WHERE service_id='svc-app' AND target='routes: https://app.example.test'
`).Scan(&notApplicable, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if notApplicable != 1 || updatedAt != "2026-01-03T00:00:00Z" {
		t.Fatalf("merged override = not_applicable:%d updated_at:%s, want 1 and latest timestamp", notApplicable, updatedAt)
	}
	ignored, err := store.MonitorNotApplicable(ctx, "svc-app", "routes: https://app.example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	if !ignored {
		t.Fatalf("default-port alias was not preserved as an active override")
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target='routes: https://app.example.test/admin/'"); got != 1 {
		t.Fatalf("non-root trailing-slash override rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target='routes: https://app.example.test/admin'"); got != 0 {
		t.Fatalf("non-root slashless override rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target='routes: https://app.example.test/a%2Fb'"); got != 1 {
		t.Fatalf("escaped route override rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target='routes: https://app.example.test/a/b'"); got != 0 {
		t.Fatalf("decoded route override rows = %d, want 0", got)
	}

	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target IN ('routes: https://app.example.test/', 'routes: https://app.example.test:443')"); got != 0 {
		t.Fatalf("alias status rows = %d, want 0", got)
	}
	var health, message, checkedAt string
	if err := store.db.QueryRowContext(ctx, `
SELECT health, message, checked_at
FROM status_results
WHERE service_id='svc-app' AND target='routes: https://app.example.test'
`).Scan(&health, &message, &checkedAt); err != nil {
		t.Fatal(err)
	}
	if health != "not_applicable" || message != "not applicable" || checkedAt != "2026-01-03T00:00:00Z" {
		t.Fatalf("merged status = %s/%s/%s, want active override to force not_applicable", health, message, checkedAt)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target='routes: https://app.example.test/admin/'"); got != 1 {
		t.Fatalf("non-root trailing-slash status rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target='routes: https://app.example.test/admin'"); got != 0 {
		t.Fatalf("non-root slashless status rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target='routes: https://app.example.test/a%2Fb'"); got != 1 {
		t.Fatalf("escaped route status rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target='routes: https://app.example.test/a/b'"); got != 0 {
		t.Fatalf("decoded route status rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target IN ('routes: https://app.example.test/', 'routes: https://app.example.test:443')"); got != 0 {
		t.Fatalf("alias history rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test'"); got != 2 {
		t.Fatalf("canonical history rows = %d, want 2", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test' AND health='not_applicable'"); got != 2 {
		t.Fatalf("canonical not_applicable history rows = %d, want 2", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test/admin/'"); got != 1 {
		t.Fatalf("non-root trailing-slash history rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test/admin'"); got != 0 {
		t.Fatalf("non-root slashless history rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test/a%2Fb'"); got != 1 {
		t.Fatalf("escaped route history rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test/a/b'"); got != 0 {
		t.Fatalf("decoded route history rows = %d, want 0", got)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Services) != 1 || len(summary.Services[0].MonitorRoutes) != 1 || summary.Services[0].MonitorRoutes[0] != "https://app.example.test" {
		t.Fatalf("monitor routes = %#v, want canonical route in summary", summary.Services)
	}
	if summary.Services[0].Health != core.HealthUnknown {
		t.Fatalf("service health = %s, want unknown because all route rows are ignored", summary.Services[0].Health)
	}
	if len(summary.Uptime) != 0 {
		t.Fatalf("uptime = %#v, want ignored route excluded", summary.Uptime)
	}
}

func TestSetMonitorNotApplicableHandlesRoutesParentAndLiteralTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "svc-routes",
		Name:        "routes",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure:    []string{"ssh://routes.example.test:22"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc-routes", "routes", true); !errors.Is(err, ErrStatusNotFound) {
		t.Fatalf("synthetic ordinary routes error = %v, want ErrStatusNotFound", err)
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-routes", Target: "routes", Health: core.HealthHealthy, Message: "ok", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	childTarget := "routes: https://app.example.test"
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-routes", Target: childTarget, Health: core.HealthHealthy, Message: "child ok", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc-routes", "routes", true); err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statusByTarget := map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		statusByTarget[status.Target] = status
	}
	if statusByTarget["routes"].Health != core.HealthNotApplicable {
		t.Fatalf("ordinary routes status = %#v, want not_applicable", statusByTarget["routes"])
	}
	if statusByTarget[childTarget].Health != core.HealthHealthy {
		t.Fatalf("child route status = %#v, want preserved healthy", statusByTarget[childTarget])
	}

	parentService := service
	parentService.ID = "svc-routes-http"
	parentService.Exposure = []string{"https://routes.example.test"}
	if err := store.ReplaceConfiguredServices(ctx, "repo-http", "prod/compose.yaml", []core.Service{parentService}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-routes-http", Target: "routes", Health: core.HealthHealthy, Message: "ok", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	httpChildTarget := "routes: https://routes.example.test"
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-routes-http", Target: httpChildTarget, Health: core.HealthHealthy, Message: "child ok", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc-routes-http", "routes", true); err != nil {
		t.Fatal(err)
	}
	summary, err = store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statusByTarget = map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		if status.ServiceID == "svc-routes-http" {
			statusByTarget[status.Target] = status
		}
	}
	if statusByTarget["routes"].Health != core.HealthNotApplicable {
		t.Fatalf("parent routes status = %#v, want not_applicable", statusByTarget["routes"])
	}
	if _, ok := statusByTarget[httpChildTarget]; ok {
		t.Fatalf("HTTP child route status = %#v, want removed by parent override", statusByTarget[httpChildTarget])
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='svc-routes-http' AND target='routes: https://routes.example.test'"); got != 0 {
		t.Fatalf("HTTP child route history rows = %d, want 0", got)
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

func TestUpsertAgentThenAgentsRoundtrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertAgent(ctx, core.AgentMessage{
		Target: "serenity",
		Containers: []core.ContainerStatus{{
			ID:    "abc123",
			Name:  "/stack-web-1",
			Image: "example/web:v1",
			Labels: map[string]string{
				core.DockerComposeProjectLabel:                  "stack",
				core.DockerComposeServiceLabel:                  "web",
				"traefik.http.middlewares.auth.basicauth.users": "admin:$2y$05$secret",
			},
			State: "running",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, core.AgentMessage{Target: "albert"}); err != nil {
		t.Fatal(err)
	}
	agents, err := store.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %#v, want 2", agents)
	}
	if agents[0].Target != "albert" || agents[1].Target != "serenity" {
		t.Fatalf("agents not sorted by target: %#v", agents)
	}
	if agents[0].Containers == nil {
		t.Fatal("albert containers is nil, want empty slice")
	}
	if len(agents[0].Containers) != 0 {
		t.Fatalf("albert containers = %#v, want empty", agents[0].Containers)
	}
	if agents[0].LastSeenAt == "" {
		t.Fatal("albert last_seen_at is empty, want set")
	}
	serenity := agents[1]
	if len(serenity.Containers) != 1 || serenity.Containers[0].Name != "/stack-web-1" {
		t.Fatalf("serenity containers = %#v", serenity.Containers)
	}
	labels := serenity.Containers[0].Labels
	if len(labels) != 2 ||
		labels[core.DockerComposeProjectLabel] != "stack" ||
		labels[core.DockerComposeServiceLabel] != "web" {
		t.Fatalf("serenity labels = %#v, want only compose identity labels", labels)
	}
	if _, ok := labels["traefik.http.middlewares.auth.basicauth.users"]; ok {
		t.Fatalf("serenity labels leaked sensitive Docker label: %#v", labels)
	}
	if serenity.LastSeenAt == "" {
		t.Fatal("serenity last_seen_at is empty, want set")
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

func assertRedacted(t *testing.T, value, token string) {
	t.Helper()
	if strings.Contains(value, token) {
		t.Fatalf("value contains token %q: %q", token, value)
	}
	if strings.Contains(value, "@example.com") {
		t.Fatalf("value contains URL userinfo: %q", value)
	}
}

func sqliteFilesContainToken(t *testing.T, dbPath, token string) bool {
	t.Helper()
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), token) {
			return true
		}
	}
	return false
}
