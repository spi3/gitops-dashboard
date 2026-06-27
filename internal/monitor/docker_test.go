package monitor

import (
	"testing"

	"github.com/example/gitops-dashboard/internal/core"
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

func TestDockerHTTPClientRejectsInvalidHost(t *testing.T) {
	t.Parallel()
	if _, _, err := dockerHTTPClient("not-a-host"); err == nil {
		t.Fatal("expected invalid host error")
	}
}
