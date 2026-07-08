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
		{ID: "missing", Exposure: []string{"https://missing.example.test"}},
		{ID: "get-only", Exposure: []string{"https://get-only.example.test"}},
		{ID: "fallback", Exposure: []string{"https://bad.example.test", "https://good.example.test"}},
		{ID: "internal", Exposure: []string{"service/api", "http://api"}},
	}
	client := &http.Client{Transport: routeTransport(func(req *http.Request) (*http.Response, error) {
		status := http.StatusNoContent
		switch req.URL.Host {
		case "bad.example.test":
			return nil, errors.New("dial failed")
		case "good.example.test":
			status = http.StatusOK
		case "down.example.test":
			status = http.StatusServiceUnavailable
		case "missing.example.test":
			status = http.StatusNotFound
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
		if strings.HasPrefix(status.Target, "routes: ") {
			t.Fatalf("route-specific status was not pruned: %#v", status)
		}
		byService[status.ServiceID] = status
	}
	if byService["up"].Health != core.HealthHealthy || byService["up"].Target != "routes" {
		t.Fatalf("up status = %#v, want healthy routes target", byService["up"])
	}
	if byService["down"].Health != core.HealthUnhealthy {
		t.Fatalf("down health = %s, want unhealthy", byService["down"].Health)
	}
	if byService["missing"].Health != core.HealthUnhealthy {
		t.Fatalf("missing health = %s, want unhealthy", byService["missing"].Health)
	}
	if byService["get-only"].Health != core.HealthHealthy {
		t.Fatalf("get-only health = %s, want healthy", byService["get-only"].Health)
	}
	if byService["fallback"].Health != core.HealthDegraded ||
		!strings.Contains(byService["fallback"].Message, "1/2 route checks passing") ||
		!strings.Contains(byService["fallback"].Message, "bad.example.test") {
		t.Fatalf("fallback status = %#v, want degraded mixed route checks", byService["fallback"])
	}
	if _, ok := byService["internal"]; ok {
		t.Fatalf("internal service produced status: %#v", byService["internal"])
	}
}

func TestHTTPRouteCheckPrunesStaleRouteSpecificStatuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	staleTarget := "routes: https://old.example.test"
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "app",
		Target:    staleTarget,
		Health:    core.HealthError,
		Message:   "old failed",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: routeTransport(func(req *http.Request) (*http.Response, error) {
		return routeResponse(req, http.StatusOK), nil
	})}
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkHTTPRoutesWithClient(ctx, config.HTTPRouteTarget{Name: "routes"}, []core.Service{
		{ID: "app", Exposure: []string{"https://app.example.test"}},
	}, client); err != nil {
		t.Fatal(err)
	}

	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %#v, want one combined route target", statuses)
	}
	if statuses[0].Target != "routes" || statuses[0].Health != core.HealthHealthy {
		t.Fatalf("status = %#v, want healthy routes target", statuses[0])
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

func TestFirstHTTPRoutePrefersMostSpecificRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		exposure []string
		want     string
	}{
		{
			name: "compose service vlan ip has better lan url",
			exposure: []string{
				"gitops_dashboard",
				"http://10.10.10.135",
				"http://gitops-dashboard.lan:8080",
				"https://gitops-dashboard.regulalabs.com",
				"servlan",
			},
			want: "http://gitops-dashboard.lan:8080",
		},
		{
			name: "kubernetes service ip has ingress hostname",
			exposure: []string{
				"http://10.122.122.200:80",
				"http://jellyfin.lan/",
				"https://jellyfin.regulalabs.com",
				"service/jellyfin",
			},
			want: "http://jellyfin.lan/",
		},
		{
			name: "fallback to only bare ip route",
			exposure: []string{
				"servlan",
				"http://10.10.10.55",
			},
			want: "http://10.10.10.55",
		},
		{
			name: "prefer shorter hostname when route scores tie",
			exposure: []string{
				"https://whoami-auth.edge.regulalabs.com",
				"https://whoami.edge.regulalabs.com",
				"https://whoami.regulalabs.com",
			},
			want: "https://whoami.regulalabs.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := firstHTTPRoute(tc.exposure)
			if !ok || got != tc.want {
				t.Fatalf("firstHTTPRoute(%v) = %q, %v; want %q, true", tc.exposure, got, ok, tc.want)
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
