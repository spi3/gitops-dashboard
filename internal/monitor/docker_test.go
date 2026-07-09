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
		Labels: map[string]string{dockerComposeServiceLabel: "web"},
		State:  "running",
		Status: "Up 1 minute",
	}})
	if health != core.HealthHealthy || message == "" {
		t.Fatalf("health=%s message=%q", health, message)
	}
	health, _ = dockerHealth(service, []dockerContainer{{
		Names:  []string{"/stack-web-1"},
		Labels: map[string]string{dockerComposeServiceLabel: "web"},
		State:  "exited",
	}})
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
		Labels: map[string]string{dockerComposeServiceLabel: "web"},
		State:  "running",
		Status: "Up 1 minute (unhealthy)",
	}})
	if health != core.HealthUnhealthy {
		t.Fatalf("health=%s, want unhealthy", health)
	}
	health, _ = dockerHealth(service, []dockerContainer{{
		Names:  []string{"/stack-web-1"},
		Image:  "example/web:v1",
		Labels: map[string]string{dockerComposeServiceLabel: "web"},
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
			Labels: map[string]string{
				dockerComposeProjectLabel: "web",
				dockerComposeServiceLabel: "web",
			},
			State:  "running",
			Status: "Up 1 minute",
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

func TestDockerCheckBindsServicesToAssociatedTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	serenityAPI := dockerContainerAPIServer(t, []dockerContainer{
		{
			ID:    "serenity-lookalike",
			Names: []string{"/stack-app-worker-1"},
			Image: "example/app-worker:v1",
			Labels: map[string]string{
				dockerComposeProjectLabel: "app-worker",
				dockerComposeServiceLabel: "app-worker",
			},
			State:  "running",
			Status: "Up 1 minute",
		},
		{
			ID:    "legacy-lookalike",
			Names: []string{"/legacy-app"},
			Image: "example/legacy:v1",
			Labels: map[string]string{
				dockerComposeProjectLabel: "prod",
				dockerComposeServiceLabel: "legacy-app",
			},
			State:  "running",
			Status: "Up 1 minute",
		},
	})
	defer serenityAPI.Close()
	albertAPI := dockerContainerAPIServer(t, []dockerContainer{
		{
			ID:    "albert-worker",
			Names: []string{"/stack-app-worker-1"},
			Image: "example/app-worker:v1",
			Labels: map[string]string{
				dockerComposeProjectLabel: "app-worker",
				dockerComposeServiceLabel: "app-worker",
			},
			State:  "running",
			Status: "Up 1 minute",
		},
	})
	defer albertAPI.Close()

	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	services := []core.Service{
		{
			ID:           "serenity-app",
			Name:         "app",
			Repository:   "kube",
			SourceCommit: "abc123",
			SourcePath:   "docker_files/serenity/app/docker-compose.yml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
			Images:       []string{"example/app:v1"},
		},
		{
			ID:           "albert-app-worker",
			Name:         "app-worker",
			Repository:   "kube",
			SourceCommit: "abc123",
			SourcePath:   "docker_files/albert/app-worker/docker-compose.yml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
			Images:       []string{"example/app-worker:v1"},
		},
		{
			ID:           "legacy-app",
			Name:         "legacy-app",
			Repository:   "kube",
			SourceCommit: "abc123",
			SourcePath:   "prod/docker-compose.yml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
			Images:       []string{"example/legacy:v1"},
		},
	}
	scanID, err := store.StartScan(ctx, "kube")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "kube", "abc123", services, nil); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Runtime: config.RuntimeConfig{Docker: []config.DockerTarget{
		{Name: "serenity", Host: serenityAPI.URL},
		{Name: "albert", Host: albertAPI.URL},
	}}}
	monitor := New(cfg, store, slog.Default())
	for _, target := range cfg.Runtime.Docker {
		if err := monitor.checkDocker(ctx, target, services); err != nil {
			t.Fatal(err)
		}
	}

	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byServiceTarget := map[string]core.StatusResult{}
	for _, status := range statuses {
		byServiceTarget[status.ServiceID+"/"+status.Target] = status
	}
	if len(byServiceTarget) != 2 {
		t.Fatalf("statuses = %#v, want exactly bound target rows", statuses)
	}
	serenityStatus := byServiceTarget["serenity-app/serenity"]
	if serenityStatus.Health != core.HealthUnknown || serenityStatus.Message != "container not found" {
		t.Fatalf("serenity-app status = %#v, want not found on serenity", serenityStatus)
	}
	albertStatus := byServiceTarget["albert-app-worker/albert"]
	if albertStatus.Health != core.HealthHealthy {
		t.Fatalf("albert-app-worker status = %#v, want healthy on albert", albertStatus)
	}
	for _, key := range []string{
		"serenity-app/albert",
		"albert-app-worker/serenity",
		"legacy-app/serenity",
		"legacy-app/albert",
	} {
		if status, ok := byServiceTarget[key]; ok {
			t.Fatalf("%s should not have been checked: %#v", key, status)
		}
	}
}

func TestDockerCheckPrunesSkippedTargetStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	serenityAPI := dockerContainerAPIServer(t, nil)
	defer serenityAPI.Close()
	albertAPI := dockerContainerAPIServer(t, nil)
	defer albertAPI.Close()

	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:           "albert-app",
		Name:         "app",
		Repository:   "kube",
		SourceCommit: "abc123",
		SourcePath:   "docker_files/albert/app/docker-compose.yml",
		Runtime:      "compose",
		Health:       core.HealthUnknown,
	}
	if err := store.ReplaceConfiguredServices(ctx, "kube", service.SourcePath, []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: service.ID,
		Target:    "serenity",
		Health:    core.HealthHealthy,
		Message:   "stale pre-upgrade result",
		CheckedAt: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Runtime: config.RuntimeConfig{Docker: []config.DockerTarget{
		{Name: "serenity", Host: serenityAPI.URL},
		{Name: "albert", Host: albertAPI.URL},
	}}}
	monitor := New(cfg, store, slog.Default())
	if err := monitor.checkDocker(ctx, cfg.Runtime.Docker[0], []core.Service{service}); err != nil {
		t.Fatal(err)
	}

	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 {
		t.Fatalf("statuses = %#v, want stale skipped target row pruned", statuses)
	}
}

func TestDockerStatusUsesComposeLabelsAsIdentity(t *testing.T) {
	t.Parallel()
	service := core.Service{
		Name:           "web",
		SourcePath:     "docker_files/serenity/path-project/docker-compose.yml",
		Runtime:        "compose",
		ComposeProject: "custom-stack",
	}
	health, _ := dockerHealth(service, []dockerContainer{
		{
			Names: []string{"/web"},
			Labels: map[string]string{
				dockerComposeProjectLabel: "web",
				dockerComposeServiceLabel: "worker",
			},
			State:  "running",
			Status: "Up 1 minute (unhealthy)",
		},
		{
			Names: []string{"/generated-name"},
			Labels: map[string]string{
				dockerComposeProjectLabel: "custom-stack",
				dockerComposeServiceLabel: "web",
			},
			State:  "running",
			Status: "Up 1 minute",
		},
	})
	if health != core.HealthHealthy {
		t.Fatalf("health = %s, want healthy label match without exact name fallback", health)
	}

	health, message := dockerHealth(service, []dockerContainer{{
		Names: []string{"/generated-name"},
		Labels: map[string]string{
			dockerComposeProjectLabel: "other-stack",
			dockerComposeServiceLabel: "web",
		},
		State:  "running",
		Status: "Up 1 minute",
	}})
	if health != core.HealthUnknown || message != "container not found" {
		t.Fatalf("health=%s message=%q, want literal project name mismatch to fail", health, message)
	}
}

func TestDockerStatusUsesLiteralComposeProjectLabel(t *testing.T) {
	t.Parallel()
	service := core.Service{
		Name:           "web",
		SourcePath:     "docker_files/serenity/path-project/docker-compose.yml",
		Runtime:        "compose",
		ComposeProject: "prod",
	}
	health, _ := dockerHealth(service, []dockerContainer{{
		Names: []string{"/generated-name"},
		Labels: map[string]string{
			dockerComposeProjectLabel: "prod",
			dockerComposeServiceLabel: "web",
		},
		State:  "running",
		Status: "Up 1 minute",
	}})
	if health != core.HealthHealthy {
		t.Fatalf("health = %s, want healthy literal project label match", health)
	}
}

func TestDockerStatusUsesEscapedDollarComposeProjectLabelStrictly(t *testing.T) {
	t.Parallel()
	service := core.Service{
		Name:           "web",
		Runtime:        "compose",
		ComposeProject: "foo$bar",
	}
	health, _ := dockerHealth(service, []dockerContainer{{
		Names: []string{"/generated-name"},
		Labels: map[string]string{
			dockerComposeProjectLabel: "foo$bar",
			dockerComposeServiceLabel: "web",
		},
		State:  "running",
		Status: "Up 1 minute",
	}})
	if health != core.HealthHealthy {
		t.Fatalf("health = %s, want healthy escaped-dollar literal project label match", health)
	}

	health, message := dockerHealth(service, []dockerContainer{{
		Names: []string{"/generated-name"},
		Labels: map[string]string{
			dockerComposeProjectLabel: "other",
			dockerComposeServiceLabel: "web",
		},
		State:  "running",
		Status: "Up 1 minute",
	}})
	if health != core.HealthUnknown || message != "container not found" {
		t.Fatalf("health=%s message=%q, want escaped-dollar literal project mismatch to fail", health, message)
	}
}

func TestDockerStatusMatchesServiceLabelWhenProjectUnknown(t *testing.T) {
	t.Parallel()
	service := core.Service{
		Name:       "web",
		SourcePath: "docker_files/serenity/path-project/docker-compose.yml",
		Runtime:    "compose",
	}
	health, _ := dockerHealth(service, []dockerContainer{{
		Names: []string{"/generated-name"},
		Labels: map[string]string{
			dockerComposeProjectLabel: "runtime-project",
			dockerComposeServiceLabel: "web",
		},
		State:  "running",
		Status: "Up 1 minute",
	}})
	if health != core.HealthHealthy {
		t.Fatalf("health = %s, want healthy service-label match with unknown project", health)
	}
}

func TestDockerStatusFallsBackToExactContainerName(t *testing.T) {
	t.Parallel()
	service := core.Service{Name: "web", Runtime: "compose"}
	health, _ := dockerHealth(service, []dockerContainer{
		{
			Names:  []string{"/web-worker"},
			State:  "running",
			Status: "Up 1 minute (unhealthy)",
		},
		{
			Names:  []string{"/web"},
			State:  "running",
			Status: "Up 1 minute",
		},
	})
	if health != core.HealthHealthy {
		t.Fatalf("health = %s, want healthy exact-name fallback", health)
	}
}

func TestApplyAgentReportMatchesUnlabeledComposeGeneratedNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	services := []core.Service{
		{
			ID:           "serenity-web",
			Name:         "web",
			Repository:   "kube",
			SourceCommit: "abc123",
			SourcePath:   "docker_files/serenity/stack/docker-compose.yml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
		},
		{
			ID:           "serenity-web-admin",
			Name:         "web-admin",
			Repository:   "kube",
			SourceCommit: "abc123",
			SourcePath:   "docker_files/serenity/stack/docker-compose.yml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
		},
		{
			ID:           "albert-web",
			Name:         "web",
			Repository:   "kube",
			SourceCommit: "abc123",
			SourcePath:   "docker_files/albert/stack/docker-compose.yml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
		},
	}
	scanID, err := store.StartScan(ctx, "kube")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "kube", "abc123", services, nil); err != nil {
		t.Fatal(err)
	}
	monitor := New(config.Config{}, store, slog.Default())
	err = monitor.ApplyAgentReport(ctx, core.AgentMessage{
		Target:    "serenity",
		CheckedAt: time.Date(2026, 7, 8, 13, 0, 0, 0, time.UTC),
		Containers: []core.ContainerStatus{
			{
				Name:   "/stack-web-1",
				State:  "running",
				Status: "Up 1 minute",
			},
			{
				Name:   "/stack-web-admin-1",
				State:  "running",
				Status: "Up 1 minute (unhealthy)",
			},
			{
				Name:   "/stack_api_1",
				State:  "running",
				Status: "Up 1 minute",
			},
		},
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
	if byService["serenity-web"].Health != core.HealthHealthy {
		t.Fatalf("serenity-web status = %#v, want healthy generated-name match", byService["serenity-web"])
	}
	if byService["serenity-web-admin"].Health != core.HealthUnhealthy {
		t.Fatalf("serenity-web-admin status = %#v, want separate unhealthy generated-name match", byService["serenity-web-admin"])
	}
	if _, ok := byService["albert-web"]; ok {
		t.Fatalf("albert-web should not be updated by serenity agent: %#v", byService["albert-web"])
	}
}

func TestDockerStatusReportsNotFoundForLookalikeContainer(t *testing.T) {
	t.Parallel()
	service := core.Service{Name: "app", Runtime: "compose"}
	health, message := dockerHealth(service, []dockerContainer{{
		Names:  []string{"/app-worker"},
		State:  "running",
		Status: "Up 1 minute",
	}})
	if health != core.HealthUnknown || message != "container not found" {
		t.Fatalf("health=%s message=%q, want clear not found", health, message)
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
				Labels: map[string]string{
					dockerComposeProjectLabel: "api",
					dockerComposeServiceLabel: "api",
				},
				State:  "exited",
				Status: "Exited (0) 2 hours ago",
			},
			{
				Name:    "/stack-api-1",
				Image:   "example/api:v1.0.0",
				ImageID: "sha256:current",
				Labels: map[string]string{
					dockerComposeProjectLabel: "api",
					dockerComposeServiceLabel: "api",
				},
				State:  "running",
				Status: "Up 1 minute",
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
					Labels: map[string]string{
						dockerComposeProjectLabel: "prod",
						dockerComposeServiceLabel: "api",
					},
					State:  "running",
					Status: "Up 1 minute",
				},
				{
					ID:      "container-2",
					Names:   []string{"/stack-api-2"},
					Image:   "example/api:v1",
					ImageID: "sha256:api",
					Labels: map[string]string{
						dockerComposeProjectLabel: "prod",
						dockerComposeServiceLabel: "api",
					},
					State:  "running",
					Status: "Up 1 minute",
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
					Labels: map[string]string{
						dockerComposeProjectLabel: "prod",
						dockerComposeServiceLabel: "api",
					},
					State:  "exited",
					Status: "Exited (0) 2 hours ago",
				},
				{
					ID:      "container-current",
					Names:   []string{"/stack-api-2"},
					Image:   "example/api:v1.0.0",
					ImageID: "sha256:current",
					Labels: map[string]string{
						dockerComposeProjectLabel: "prod",
						dockerComposeServiceLabel: "api",
					},
					State:  "running",
					Status: "Up 1 minute",
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

func dockerContainerAPIServer(t *testing.T, containers []dockerContainer) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			_ = json.NewEncoder(w).Encode(containers)
		default:
			http.NotFound(w, r)
		}
	}))
}
