package dockerapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListContainersDecodesDockerHealthRepresentations(t *testing.T) {
	t.Parallel()
	dockerAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			http.NotFound(w, r)
			return
		}
		// This object matches the Health field returned by the local dockerd
		// /containers/json endpoint. Older Engine API versions return a string.
		_, _ = w.Write([]byte(`[
			{"Id":"object-health","State":"running","Status":"Up 2 days (unhealthy)","Health":{"Status":"unhealthy","FailingStreak":8470}},
			{"Id":"string-health","State":"running","Status":"Up 1 minute","Health":"healthy"},
			{"Id":"no-healthcheck","State":"running","Status":"Up 1 minute","Health":null}
		]`))
	}))
	defer dockerAPI.Close()

	containers, err := ListContainers(context.Background(), dockerAPI.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 3 {
		t.Fatalf("containers = %#v, want three containers", containers)
	}
	for index, want := range []string{"unhealthy", "healthy", ""} {
		if got := containers[index].Health; got != want {
			t.Fatalf("container %q health = %q, want %q", containers[index].ID, got, want)
		}
	}
}
