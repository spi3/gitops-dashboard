package monitor

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
)

func TestDockerHealthMapsContainerState(t *testing.T) {
	t.Parallel()
	service := core.Service{Name: "web", Images: []string{"example/web:v1"}}
	health, message := dockerHealth(service, []dockerContainer{{
		Names:  []string{"/stack-web-1"},
		Image:  "example/web:v1",
		State:  "running",
		Status: "Up 1 minute",
	}})
	if health != core.HealthHealthy || message == "" {
		t.Fatalf("health=%s message=%q", health, message)
	}
	health, _ = dockerHealth(service, []dockerContainer{{Names: []string{"/stack-web-1"}, State: "exited"}})
	if health != core.HealthUnhealthy {
		t.Fatalf("health=%s, want unhealthy", health)
	}
	health, _ = dockerHealth(service, nil)
	if health != core.HealthUnknown {
		t.Fatalf("health=%s, want unknown", health)
	}
}

func TestDockerHealthUsesHealthcheckState(t *testing.T) {
	t.Parallel()
	service := core.Service{Name: "web", Images: []string{"example/web:v1"}}
	health, _ := dockerHealth(service, []dockerContainer{{
		Names:  []string{"/stack-web-1"},
		Image:  "example/web:v1",
		State:  "running",
		Status: "Up 1 minute (unhealthy)",
	}})
	if health != core.HealthUnhealthy {
		t.Fatalf("health=%s, want unhealthy", health)
	}
	health, _ = dockerHealth(service, []dockerContainer{{
		Names:  []string{"/stack-web-1"},
		Image:  "example/web:v1",
		State:  "running",
		Status: "Up 1 minute (health: starting)",
	}})
	if health != core.HealthDegraded {
		t.Fatalf("health=%s, want degraded", health)
	}
}

func TestApplyAgentReportPersistsMatchingComposeStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scanID, err := store.StartScan(ctx, "kube")
	if err != nil {
		t.Fatal(err)
	}
	services := []core.Service{
		{
			ID:           "serenity-web",
			Name:         "web",
			Repository:   "kube",
			SourceCommit: "abc123",
			SourcePath:   "docker_files/serenity/web/docker-compose.yml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
			Images:       []string{"example/web:v1"},
		},
		{
			ID:           "serenity-missing",
			Name:         "missing",
			Repository:   "kube",
			SourceCommit: "abc123",
			SourcePath:   "docker_files/serenity/missing/docker-compose.yml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
		},
		{
			ID:           "albert-web",
			Name:         "web",
			Repository:   "kube",
			SourceCommit: "abc123",
			SourcePath:   "docker_files/albert/web/docker-compose.yml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
			Images:       []string{"example/web:v1"},
		},
	}
	if err := store.FinishScan(ctx, scanID, "kube", "abc123", services, nil); err != nil {
		t.Fatal(err)
	}
	monitor := New(config.Config{}, store, slog.Default())
	checkedAt := time.Date(2026, 7, 7, 20, 0, 0, 0, time.UTC)
	err = monitor.ApplyAgentReport(ctx, core.AgentMessage{
		Target:    "serenity",
		CheckedAt: checkedAt,
		Containers: []core.ContainerStatus{{
			Name:   "/stack-web-1",
			Image:  "example/web:v1",
			State:  "running",
			Status: "Up 1 minute",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byService := map[string]core.StatusResult{}
	for _, status := range statuses {
		byService[status.ServiceID] = status
	}
	if byService["serenity-web"].Target != "serenity" || byService["serenity-web"].Health != core.HealthHealthy {
		t.Fatalf("serenity-web status = %#v, want healthy serenity target", byService["serenity-web"])
	}
	if byService["serenity-missing"].Health != core.HealthUnknown {
		t.Fatalf("serenity-missing status = %#v, want unknown", byService["serenity-missing"])
	}
	if _, ok := byService["albert-web"]; ok {
		t.Fatalf("albert-web should not be updated by serenity agent: %#v", byService["albert-web"])
	}
}

func TestDockerHTTPClientRejectsInvalidHost(t *testing.T) {
	t.Parallel()
	if _, _, err := dockerHTTPClient("not-a-host"); err == nil {
		t.Fatal("expected invalid host error")
	}
}
