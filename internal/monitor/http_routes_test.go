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
	byServiceTarget := map[string]core.StatusResult{}
	for _, status := range statuses {
		byServiceTarget[status.ServiceID+"\x00"+status.Target] = status
	}
	if byServiceTarget["up\x00routes: https://up.example.test"].Health != core.HealthHealthy {
		t.Fatalf("up status = %#v, want healthy", byServiceTarget["up\x00routes: https://up.example.test"])
	}
	if byServiceTarget["down\x00routes: https://down.example.test"].Health != core.HealthUnhealthy {
		t.Fatalf("down status = %#v, want unhealthy", byServiceTarget["down\x00routes: https://down.example.test"])
	}
	if byServiceTarget["missing\x00routes: https://missing.example.test"].Health != core.HealthUnhealthy {
		t.Fatalf("missing status = %#v, want unhealthy", byServiceTarget["missing\x00routes: https://missing.example.test"])
	}
	if byServiceTarget["get-only\x00routes: https://get-only.example.test"].Health != core.HealthHealthy {
		t.Fatalf("get-only status = %#v, want healthy", byServiceTarget["get-only\x00routes: https://get-only.example.test"])
	}
	badFallback := byServiceTarget["fallback\x00routes: https://bad.example.test"]
	goodFallback := byServiceTarget["fallback\x00routes: https://good.example.test"]
	if badFallback.Health != core.HealthError || !strings.Contains(badFallback.Message, "bad.example.test") {
		t.Fatalf("bad fallback status = %#v, want error route check", badFallback)
	}
	if goodFallback.Health != core.HealthHealthy || !strings.Contains(goodFallback.Message, "good.example.test") {
		t.Fatalf("good fallback status = %#v, want healthy route check", goodFallback)
	}
	for _, status := range byServiceTarget {
		if status.ServiceID == "internal" {
			t.Fatalf("internal service produced status: %#v", status)
		}
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
