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
)

func TestCollectDockerInspectsRepoDigests(t *testing.T) {
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
			_ = json.NewEncoder(w).Encode(dockerImageInspect{
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
