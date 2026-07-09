package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
			Name:    "/stack-web-1",
			Image:   "example/web:v1",
			ImageID: "sha256:local",
			State:   "running",
			Status:  "Up 1 minute",
		}},
	}, []string{"serenity"})
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
	if len(byService["serenity-web"].ObservedImages) != 1 || byService["serenity-web"].ObservedImages[0].Reference.Tag != "v1" {
		t.Fatalf("serenity-web observed images = %#v, want docker image metadata", byService["serenity-web"].ObservedImages)
	}
	if byService["serenity-missing"].Health != core.HealthUnknown {
		t.Fatalf("serenity-missing status = %#v, want unknown", byService["serenity-missing"])
	}
	if _, ok := byService["albert-web"]; ok {
		t.Fatalf("albert-web should not be updated by serenity agent: %#v", byService["albert-web"])
	}
}

func TestApplyAgentReportRejectsUnauthorizedTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	monitor := New(config.Config{}, store, slog.Default())
	err = monitor.ApplyAgentReport(ctx, core.AgentMessage{
		Target: "albert",
		Containers: []core.ContainerStatus{{
			Name:  "/stack-web-1",
			Image: "example/web:v1",
			State: "running",
		}},
	}, []string{"serenity"})
	if !errors.Is(err, ErrAgentTargetUnauthorized) {
		t.Fatalf("err = %v, want ErrAgentTargetUnauthorized", err)
	}
	agents, err := store.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Fatalf("agents = %#v, want no persisted report", agents)
	}
}

func TestApplyAgentReportObservedImagesIgnoreStoppedMatchingContainers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:           "serenity-api",
		Name:         "api",
		Repository:   "kube",
		SourceCommit: "abc123",
		SourcePath:   "docker_files/serenity/api/docker-compose.yml",
		Runtime:      "compose",
		Health:       core.HealthUnknown,
		Images:       []string{"example/api:v1.0.0"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "kube", service.SourcePath, []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	monitor := New(config.Config{}, store, slog.Default())
	err = monitor.ApplyAgentReport(ctx, core.AgentMessage{
		Target:    "serenity",
		CheckedAt: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
		Containers: []core.ContainerStatus{
			{
				Name:    "/stack-api-old",
				Image:   "example/api:v0.9.0",
				ImageID: "sha256:old",
				State:   "exited",
				Status:  "Exited (0) 2 hours ago",
			},
			{
				Name:    "/stack-api-1",
				Image:   "example/api:v1.0.0",
				ImageID: "sha256:current",
				State:   "running",
				Status:  "Up 1 minute",
			},
		},
	}, []string{"serenity"})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].ImageVersionState != core.ImageVersionMatching {
		t.Fatalf("image version state = %s, want matching; checks=%#v", summary.Services[0].ImageVersionState, summary.Services[0].ImageVersionChecks)
	}
	if len(summary.Statuses) != 1 || len(summary.Statuses[0].ObservedImages) != 1 {
		t.Fatalf("observed images = %#v, want only the live agent container image", summary.Statuses)
	}
	if got := summary.Statuses[0].ObservedImages[0].Reference.Tag; got != "v1.0.0" {
		t.Fatalf("observed image tag = %q, want v1.0.0", got)
	}
}

func TestDockerCheckInspectsRepoDigestsForImageComparison(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inspectCalls := 0
	dockerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			_ = json.NewEncoder(w).Encode([]dockerContainer{
				{
					ID:      "container-1",
					Names:   []string{"/stack-api-1"},
					Image:   "example/api:v1",
					ImageID: "sha256:api",
					State:   "running",
					Status:  "Up 1 minute",
				},
				{
					ID:      "container-2",
					Names:   []string{"/stack-api-2"},
					Image:   "example/api:v1",
					ImageID: "sha256:api",
					State:   "running",
					Status:  "Up 1 minute",
				},
			})
		case strings.HasPrefix(r.URL.Path, "/images/"):
			inspectCalls++
			_ = json.NewEncoder(w).Encode(dockerImageInspect{
				RepoDigests: []string{"example/api@sha256:release"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer dockerAPI.Close()

	store, err := storage.Open(t.TempDir() + "/dashboard.db")
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
		Images:      []string{"example/api@sha256:release"},
		Environment: "production",
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkDocker(ctx, config.DockerTarget{Name: "docker", Host: dockerAPI.URL}, []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want 1 cached lookup for shared image ID", inspectCalls)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].ImageVersionState != core.ImageVersionMatching {
		t.Fatalf("image version state = %s, want matching; checks=%#v", summary.Services[0].ImageVersionState, summary.Services[0].ImageVersionChecks)
	}
	if len(summary.Statuses) != 1 || len(summary.Statuses[0].ObservedImages) == 0 {
		t.Fatalf("statuses = %#v, want observed docker images", summary.Statuses)
	}
	if len(summary.Statuses[0].ObservedImages[0].RepoDigests) != 1 || summary.Statuses[0].ObservedImages[0].RepoDigests[0].Digest != "sha256:release" {
		t.Fatalf("repo digests = %#v, want inspected digest metadata", summary.Statuses[0].ObservedImages[0].RepoDigests)
	}
}

func TestDockerObservedImagesIgnoreStoppedMatchingContainers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inspectedImages := map[string]int{}
	dockerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			_ = json.NewEncoder(w).Encode([]dockerContainer{
				{
					ID:      "container-old",
					Names:   []string{"/stack-api-1"},
					Image:   "example/api:v0.9.0",
					ImageID: "sha256:old",
					State:   "exited",
					Status:  "Exited (0) 2 hours ago",
				},
				{
					ID:      "container-current",
					Names:   []string{"/stack-api-2"},
					Image:   "example/api:v1.0.0",
					ImageID: "sha256:current",
					State:   "running",
					Status:  "Up 1 minute",
				},
			})
		case strings.HasPrefix(r.URL.Path, "/images/"):
			inspectedImages[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/images/"), "/json")]++
			_ = json.NewEncoder(w).Encode(dockerImageInspect{
				RepoDigests: []string{"example/api@sha256:current"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer dockerAPI.Close()

	store, err := storage.Open(t.TempDir() + "/dashboard.db")
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
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkDocker(ctx, config.DockerTarget{Name: "docker", Host: dockerAPI.URL}, []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].ImageVersionState != core.ImageVersionMatching {
		t.Fatalf("image version state = %s, want matching; checks=%#v", summary.Services[0].ImageVersionState, summary.Services[0].ImageVersionChecks)
	}
	if len(summary.Statuses) != 1 || len(summary.Statuses[0].ObservedImages) != 1 {
		t.Fatalf("observed images = %#v, want only the live container image", summary.Statuses)
	}
	if got := summary.Statuses[0].ObservedImages[0].Reference.Tag; got != "v1.0.0" {
		t.Fatalf("observed image tag = %q, want v1.0.0", got)
	}
	if inspectedImages["sha256:old"] != 0 {
		t.Fatalf("stopped image was inspected: %#v", inspectedImages)
	}
	if inspectedImages["sha256:current"] != 1 {
		t.Fatalf("inspected images = %#v, want one current image inspect", inspectedImages)
	}
}

func TestDockerHTTPClientRejectsInvalidHost(t *testing.T) {
	t.Parallel()
	if _, _, err := dockerHTTPClient("not-a-host"); err == nil {
		t.Fatal("expected invalid host error")
	}
}
