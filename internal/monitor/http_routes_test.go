package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
)

func TestHTTPRouteCheckPersistsRouteStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	services := []core.Service{
		{ID: "up", Exposure: []string{"https://up.example.test"}},
		{ID: "down", Exposure: []string{"https://down.example.test"}},
		{ID: "get-only", Exposure: []string{"https://get-only.example.test"}},
		{ID: "internal", Exposure: []string{"service/api", "http://api"}},
	}
	client := &http.Client{Transport: routeTransport(func(req *http.Request) (*http.Response, error) {
		status := http.StatusNoContent
		switch req.URL.Host {
		case "down.example.test":
			status = http.StatusServiceUnavailable
		case "get-only.example.test":
			if req.Method == http.MethodHead {
				status = http.StatusMethodNotAllowed
			} else {
				status = http.StatusOK
			}
		}
		return routeResponse(req, status), nil
	})}
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkHTTPRoutesWithClient(ctx, config.HTTPRouteTarget{Name: "routes"}, services, client); err != nil {
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
	if byService["up"].Health != core.HealthHealthy {
		t.Fatalf("up health = %s, want healthy", byService["up"].Health)
	}
	if byService["down"].Health != core.HealthUnhealthy {
		t.Fatalf("down health = %s, want unhealthy", byService["down"].Health)
	}
	if byService["get-only"].Health != core.HealthHealthy {
		t.Fatalf("get-only health = %s, want healthy", byService["get-only"].Health)
	}
	if _, ok := byService["internal"]; ok {
		t.Fatalf("internal service produced status: %#v", byService["internal"])
	}
}

func TestNormalizeHTTPRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		candidate string
		want      string
		ok        bool
	}{
		{name: "https route", candidate: "https://app.example.test/path", want: "https://app.example.test/path", ok: true},
		{name: "lan host", candidate: "app.lan:8080", want: "http://app.lan:8080", ok: true},
		{name: "public host", candidate: "app.example.test", want: "https://app.example.test", ok: true},
		{name: "ip host", candidate: "10.10.10.20:8080", want: "http://10.10.10.20:8080", ok: true},
		{name: "service ref", candidate: "service/app", ok: false},
		{name: "cluster host", candidate: "http://app.default.svc.cluster.local", ok: false},
		{name: "compose network name", candidate: "http://app", ok: false},
		{name: "ssh", candidate: "ssh://example.test:22", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := normalizeHTTPRoute(tc.candidate)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("normalizeHTTPRoute(%q) = %q, %v; want %q, %v", tc.candidate, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestHTTPRouteCheckRecordsRequestErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := &http.Client{
		Timeout: time.Second,
		Transport: routeTransport(func(_ *http.Request) (*http.Response, error) {
			return nil, errors.New("dial failed")
		}),
	}
	health, message := checkHTTPRoute(ctx, client, "https://down.example.test")
	if health != core.HealthError {
		t.Fatalf("health = %s, want error; message=%q", health, message)
	}
}

type routeTransport func(*http.Request) (*http.Response, error)

func (transport routeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func routeResponse(req *http.Request, statusCode int) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
}
