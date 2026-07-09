package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/dockerapi"
)

func TestCollectDockerInspectsRepoDigests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inspectCalls := 0
	dockerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			_ = json.NewEncoder(w).Encode([]dockerapi.Container{
				{
					ID:      "container-1",
					Names:   []string{"/stack-api-1"},
					Image:   "example/api:v1",
					ImageID: "sha256:api",
					Labels: map[string]string{
						core.DockerComposeProjectLabel:                  "stack",
						core.DockerComposeServiceLabel:                  "api",
						"traefik.http.middlewares.auth.basicauth.users": "admin:$2y$05$secret",
					},
					State:  "running",
					Status: "Up 1 minute",
				},
				{
					ID:      "container-2",
					Names:   []string{"/stack-api-2"},
					Image:   "example/api:v1",
					ImageID: "sha256:api",
					State:   "running",
					Status:  "Up 1 minute",
				},
				{
					ID:      "container-old",
					Names:   []string{"/stack-api-old"},
					Image:   "example/api:v0.9.0",
					ImageID: "sha256:old",
					State:   "exited",
					Status:  "Exited (0) 2 hours ago",
				},
			})
		case strings.HasPrefix(r.URL.Path, "/images/"):
			inspectCalls++
			_ = json.NewEncoder(w).Encode(dockerapi.ImageInspect{
				RepoDigests: []string{"example/api@sha256:release"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer dockerAPI.Close()

	message, err := collectDocker(ctx, config.AgentConfig{
		Target: "serenity",
		Docker: config.DockerTarget{Host: dockerAPI.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspectCalls != 1 {
		t.Fatalf("inspect calls = %d, want one cached lookup for shared image ID", inspectCalls)
	}
	if len(message.Containers) != 3 {
		t.Fatalf("containers = %#v, want all reported containers", message.Containers)
	}
	for _, container := range message.Containers {
		if container.State == "exited" {
			if len(container.RepoDigests) != 0 {
				t.Fatalf("container = %#v, want no inspected repo digest for stopped container", container)
			}
			continue
		}
		if len(container.RepoDigests) != 1 || container.RepoDigests[0] != "example/api@sha256:release" {
			t.Fatalf("container = %#v, want inspected repo digest for live container", container)
		}
	}
	labels := message.Containers[0].Labels
	if len(labels) != 2 ||
		labels[core.DockerComposeProjectLabel] != "stack" ||
		labels[core.DockerComposeServiceLabel] != "api" {
		t.Fatalf("labels = %#v, want only compose identity labels", labels)
	}
	if _, ok := labels["traefik.http.middlewares.auth.basicauth.users"]; ok {
		t.Fatalf("labels leaked sensitive Docker label: %#v", labels)
	}
}

func TestCollectDockerInfersContainerHealthAndRestartCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dockerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			_ = json.NewEncoder(w).Encode([]dockerapi.Container{
				{
					ID:           "container-healthy",
					Names:        []string{"/stack-web-1"},
					Image:        "example/web:v1",
					ImageID:      "sha256:web",
					Labels:       map[string]string{},
					State:        "running",
					Status:       "Up 10 minutes",
					RestartCount: 1,
				},
				{
					ID:           "container-unhealthy",
					Names:        []string{"/stack-api-1"},
					Image:        "example/api:v1",
					ImageID:      "sha256:api",
					State:        "running",
					Status:       "Up 8 minutes (unhealthy)",
					RestartCount: 4,
				},
				{
					ID:           "container-starting",
					Names:        []string{"/stack-worker-1"},
					Image:        "example/worker:v1",
					ImageID:      "sha256:worker",
					State:        "running",
					Status:       "Up 1 minute (health: starting)",
					RestartCount: 3,
				},
				{
					ID:           "container-restarting",
					Names:        []string{"/stack-restarting-1"},
					Image:        "example/restarting:v1",
					ImageID:      "sha256:restarting",
					State:        "restarting",
					Status:       "Restarting (1) 2 seconds ago",
					RestartCount: 7,
				},
				{
					ID:           "container-paused",
					Names:        []string{"/stack-paused-1"},
					Image:        "example/paused:v1",
					ImageID:      "sha256:paused",
					State:        "paused",
					Status:       "Up 4 minutes",
					RestartCount: 12,
				},
				{
					ID:           "container-none",
					Names:        []string{"/stack-none-1"},
					Image:        "example/none:v1",
					ImageID:      "sha256:none",
					State:        "running",
					Status:       "Up 3 minutes (health: none)",
					RestartCount: 0,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer dockerAPI.Close()

	message, err := collectDocker(ctx, config.AgentConfig{
		Target: "serenity",
		Docker: config.DockerTarget{Host: dockerAPI.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.Containers) != 6 {
		t.Fatalf("containers = %#v, want all reported containers", message.Containers)
	}
	tests := []struct {
		id          string
		health      string
		restartHint int
	}{
		{id: "container-healthy", health: "healthy", restartHint: 1},
		{id: "container-unhealthy", health: "unhealthy", restartHint: 4},
		{id: "container-starting", health: "starting", restartHint: 3},
		{id: "container-restarting", health: "starting", restartHint: 7},
		{id: "container-paused", health: "starting", restartHint: 12},
		{id: "container-none", health: "none", restartHint: 0},
	}
	for _, want := range tests {
		for _, container := range message.Containers {
			if container.ID != want.id {
				continue
			}
			if container.Health != want.health {
				t.Fatalf("container %s health = %q, want %q", want.id, container.Health, want.health)
			}
			if container.RestartCount != want.restartHint {
				t.Fatalf("container %s restartCount = %d, want %d", want.id, container.RestartCount, want.restartHint)
			}
		}
	}
}
