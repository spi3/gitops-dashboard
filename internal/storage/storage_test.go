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
