package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
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
	if len(statuses) != 6 {
		t.Fatalf("statuses = %#v, want one status per checkable route", statuses)
	}
	byTarget := map[string]core.StatusResult{}
	for _, status := range statuses {
		if status.Target == "routes" {
			t.Fatalf("aggregate route status was persisted: %#v", status)
		}
		byTarget[status.Target] = status
	}
	if byTarget["routes: https://up.example.test"].Health != core.HealthHealthy {
		t.Fatalf("up status = %#v, want healthy route target", byTarget["routes: https://up.example.test"])
	}
	if byTarget["routes: https://down.example.test"].Health != core.HealthUnhealthy {
		t.Fatalf("down health = %s, want unhealthy", byTarget["routes: https://down.example.test"].Health)
	}
	if byTarget["routes: https://missing.example.test"].Health != core.HealthUnhealthy {
		t.Fatalf("missing health = %s, want unhealthy", byTarget["routes: https://missing.example.test"].Health)
	}
	if byTarget["routes: https://get-only.example.test"].Health != core.HealthHealthy ||
		!strings.HasPrefix(byTarget["routes: https://get-only.example.test"].Message, "GET ") {
		t.Fatalf("get-only status = %#v, want healthy GET fallback", byTarget["routes: https://get-only.example.test"])
	}
	if byTarget["routes: https://bad.example.test"].Health != core.HealthError {
		t.Fatalf("bad fallback status = %#v, want error", byTarget["routes: https://bad.example.test"])
	}
	if byTarget["routes: https://good.example.test"].Health != core.HealthHealthy {
		t.Fatalf("good fallback status = %#v, want healthy", byTarget["routes: https://good.example.test"])
	}
	for _, status := range statuses {
		if status.ServiceID == "internal" {
			t.Fatalf("internal service produced status: %#v", status)
		}
	}
}

func TestHTTPRouteCheckSkipsMigratedRouteOverride(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldRoute, newRoute := "http://10.10.10.127", "http://10.10.10.127:8080"
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "routes: " + oldRoute, Health: core.HealthHealthy, CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc", "routes: "+oldRoute, true); err != nil {
		t.Fatal(err)
	}
	// The storage migration is the successful-rescan handoff; monitor then sees
	// the replacement identity and must not put it back on the probe queue.
	if err := store.MigrateRouteTargetReplacements(ctx, []storage.RouteTargetReplacement{{ServiceID: "svc", OldRoute: oldRoute, NewRoute: newRoute}}, nil); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	client := &http.Client{Transport: routeTransport(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return routeResponse(req, http.StatusOK), nil
	})}
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkHTTPRoutesWithClient(ctx, config.HTTPRouteTarget{Name: "routes"}, []core.Service{{ID: "svc", Exposure: []string{newRoute}}}, client); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests.Load())
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range statuses {
		if status.Target == "routes: "+oldRoute {
			t.Fatalf("old target remained: %#v", status)
		}
		if status.Target == "routes: "+newRoute && status.Health != core.HealthNotApplicable {
			t.Fatalf("migrated status = %#v, want not applicable", status)
		}
	}
}

func TestHTTPRouteCheckRecordsTimeoutForEveryQueuedRoute(t *testing.T) {
	t.Parallel()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	services := make([]core.Service, 0, httpRouteConcurrency+2)
	for i := 0; i < httpRouteConcurrency+2; i++ {
		services = append(services, core.Service{
			ID:       fmt.Sprintf("svc-%02d", i),
			Exposure: []string{fmt.Sprintf("https://app-%02d.example.test", i)},
		})
	}
	client := &http.Client{Transport: routeTransport(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkHTTPRoutesWithClient(ctx, config.HTTPRouteTarget{Name: "routes"}, services, client); err != nil {
		t.Fatal(err)
	}
	statuses, err := store.StatusResults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != len(services) {
		t.Fatalf("statuses = %#v, want %d timeout rows", statuses, len(services))
	}
	for _, status := range statuses {
		if status.Health != core.HealthError || !strings.Contains(status.Message, "timed out") {
			t.Fatalf("status = %#v, want explicit timeout error", status)
		}
	}
}

func TestHTTPRouteCheckSkipsCanceledResults(t *testing.T) {
	t.Parallel()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const target = "routes: https://app.example.test"
	if err := store.UpsertStatus(context.Background(), core.StatusResult{
		ServiceID: "app",
		Target:    target,
		Health:    core.HealthHealthy,
		Message:   "previous healthy",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	client := &http.Client{Transport: routeTransport(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	monitor := New(config.Config{}, store, slog.Default())
	done := make(chan error, 1)
	go func() {
		done <- monitor.checkHTTPRoutesWithClient(ctx, config.HTTPRouteTarget{Name: "routes"}, []core.Service{
			{ID: "app", Exposure: []string{"https://app.example.test"}},
		}, client)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for route check to start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled route check to finish")
	}

	statuses, err := store.StatusResults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Target != target || statuses[0].Health != core.HealthHealthy || statuses[0].Message != "previous healthy" {
		t.Fatalf("statuses = %#v, want previous healthy route status unchanged", statuses)
	}
}

func TestHTTPRouteCheckPrunesStaleRouteSpecificStatusesButKeepsOverrides(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	staleTarget := "routes: https://old.example.test"
	staleHistoryTarget := "routes: https://gone.example.test"
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "app",
		Target:    staleTarget,
		Health:    core.HealthError,
		Message:   "old failed",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "app", staleTarget, true); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "app",
		Target:    staleHistoryTarget,
		Health:    core.HealthError,
		Message:   "gone failed",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "app",
		Target:    "routes",
		Health:    core.HealthHealthy,
		Message:   "legacy aggregate status",
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
		t.Fatalf("statuses = %#v, want one current route target", statuses)
	}
	if statuses[0].Target != "routes: https://app.example.test" || statuses[0].Health != core.HealthHealthy {
		t.Fatalf("status = %#v, want healthy current route target", statuses[0])
	}
	ignored, err := store.MonitorNotApplicable(ctx, "app", staleTarget)
	if err != nil {
		t.Fatal(err)
	}
	if !ignored {
		t.Fatal("stale route override was pruned, want override to persist")
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 || summary.Uptime[0].Target != "routes: https://app.example.test" {
		t.Fatalf("uptime = %#v, want only current route history", summary.Uptime)
	}
}

func TestHTTPRouteCheckParentOverrideSuppressesRouteProbing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "app",
		Target:    "routes",
		Health:    core.HealthHealthy,
		Message:   "legacy route status",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "app", "routes", true); err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int64
	client := &http.Client{Transport: routeTransport(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return routeResponse(req, http.StatusOK), nil
	})}
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkHTTPRoutesWithClient(ctx, config.HTTPRouteTarget{Name: "routes"}, []core.Service{
		{ID: "app", Exposure: []string{"https://app.example.test", "https://other.example.test"}},
	}, client); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests.Load())
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Target != "routes" || statuses[0].Health != core.HealthNotApplicable {
		t.Fatalf("statuses = %#v, want only parent not_applicable status", statuses)
	}
}

func TestHTTPRouteCheckPerRouteOverrideSkipsOnlyThatRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	localTarget := "routes: http://10.10.10.20"
	publicTarget := "routes: https://app.example.test"
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "app",
		Target:    localTarget,
		Health:    core.HealthError,
		Message:   "dial failed",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "app", localTarget, true); err != nil {
		t.Fatal(err)
	}

	var localRequests atomic.Int64
	var publicRequests atomic.Int64
	client := &http.Client{Transport: routeTransport(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Host {
		case "10.10.10.20":
			localRequests.Add(1)
			return nil, errors.New("local route should not be probed")
		case "app.example.test":
			publicRequests.Add(1)
			return routeResponse(req, http.StatusOK), nil
		default:
			return nil, errors.New("unexpected host: " + req.URL.Host)
		}
	})}
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkHTTPRoutesWithClient(ctx, config.HTTPRouteTarget{Name: "routes"}, []core.Service{
		{ID: "app", Exposure: []string{"http://10.10.10.20", "https://app.example.test"}},
	}, client); err != nil {
		t.Fatal(err)
	}
	if localRequests.Load() != 0 {
		t.Fatalf("local route requests = %d, want 0", localRequests.Load())
	}
	if publicRequests.Load() != 1 {
		t.Fatalf("public route requests = %d, want 1", publicRequests.Load())
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]core.StatusResult{}
	for _, status := range statuses {
		byTarget[status.Target] = status
	}
	if byTarget[localTarget].Health != core.HealthNotApplicable {
		t.Fatalf("local status = %#v, want not_applicable", byTarget[localTarget])
	}
	if byTarget[publicTarget].Health != core.HealthHealthy {
		t.Fatalf("public status = %#v, want healthy", byTarget[publicTarget])
	}
}

func TestHTTPRouteCheckRecordsPolicyBlocksAndHonorsPolicyChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	services := []core.Service{{
		ID:       "app",
		Exposure: []string{"http://10.10.10.20", "http://169.254.169.254/latest/meta-data"},
	}}
	var requests atomic.Int64
	client := &http.Client{Transport: routeTransport(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return routeResponse(req, http.StatusOK), nil
	})}
	monitor := New(config.Config{}, store, slog.Default())

	if err := monitor.checkHTTPRoutesWithClient(ctx, config.HTTPRouteTarget{
		Name: "routes",
		Egress: config.EgressPolicyConfig{
			Deny: config.EgressPolicyRules{CIDRs: []string{"10.0.0.0/8"}},
		},
	}, services, client); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatalf("HTTP requests = %d, want 0 while routes are blocked", requests.Load())
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byTarget := map[string]core.StatusResult{}
	for _, status := range statuses {
		byTarget[status.Target] = status
	}
	privateStatus := byTarget["routes: http://10.10.10.20"]
	if privateStatus.Health != core.HealthNotApplicable || !strings.Contains(privateStatus.Message, "blocked by policy: deny cidr 10.0.0.0/8") {
		t.Fatalf("private status = %#v, want configured CIDR policy block", privateStatus)
	}
	linkLocalStatus := byTarget["routes: http://169.254.169.254/latest/meta-data"]
	if linkLocalStatus.Health != core.HealthNotApplicable || !strings.Contains(linkLocalStatus.Message, "blocked by policy: default deny cidr 169.254.0.0/16") {
		t.Fatalf("link-local status = %#v, want default policy block", linkLocalStatus)
	}

	if err := monitor.checkHTTPRoutesWithClient(ctx, config.HTTPRouteTarget{
		Name: "routes",
		Egress: config.EgressPolicyConfig{
			Allow: config.EgressPolicyRules{CIDRs: []string{"10.0.0.0/8", "169.254.0.0/16"}},
		},
	}, services, client); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("HTTP requests after policy opt-in = %d, want 2", requests.Load())
	}
	statuses, err = store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byTarget = map[string]core.StatusResult{}
	for _, status := range statuses {
		byTarget[status.Target] = status
	}
	if byTarget["routes: http://10.10.10.20"].Health != core.HealthHealthy {
		t.Fatalf("private status after policy change = %#v, want healthy", byTarget["routes: http://10.10.10.20"])
	}
	if byTarget["routes: http://169.254.169.254/latest/meta-data"].Health != core.HealthHealthy {
		t.Fatalf("link-local status after opt-in = %#v, want healthy", byTarget["routes: http://169.254.169.254/latest/meta-data"])
	}
}

func TestHTTPRoutePolicyBlockClearsStaleUptime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	service := core.Service{
		ID:          "app",
		Name:        "app",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure:    []string{"http://10.10.10.20"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	const target = "routes: http://10.10.10.20"
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "app",
		Target:    target,
		Health:    core.HealthHealthy,
		Message:   "route ok",
		CheckedAt: time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	uptime, err := store.UptimeStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(uptime) != 1 || uptime[0].Target != target {
		t.Fatalf("pre-block uptime = %#v, want healthy route history", uptime)
	}

	var requests atomic.Int64
	client := &http.Client{Transport: routeTransport(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		return routeResponse(req, http.StatusOK), nil
	})}
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkHTTPRoutesWithClient(ctx, config.HTTPRouteTarget{
		Name: "routes",
		Egress: config.EgressPolicyConfig{
			Deny: config.EgressPolicyRules{CIDRs: []string{"10.0.0.0/8"}},
		},
	}, []core.Service{service}, client); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatalf("HTTP requests = %d, want policy block before probe", requests.Load())
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stat := range summary.Uptime {
		if stat.ServiceID == "app" && stat.Target == target {
			t.Fatalf("blocked route remained in summary uptime: %#v", summary.Uptime)
		}
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Target != target || statuses[0].Health != core.HealthNotApplicable || !strings.Contains(statuses[0].Message, "blocked by policy") {
		t.Fatalf("blocked status = %#v, want explicit policy not_applicable", statuses)
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
		{name: "trailing slash", candidate: "https://app.example.test/", want: "https://app.example.test", ok: true},
		{name: "non-root trailing slash", candidate: "https://app.example.test/admin/", want: "https://app.example.test/admin/", ok: true},
		{name: "default https port", candidate: "https://app.example.test:443", want: "https://app.example.test", ok: true},
		{name: "userinfo stripped", candidate: "https://user:pass@app.example.test:443/admin", want: "https://app.example.test/admin", ok: true},
		{name: "non-root default https port", candidate: "https://app.example.test:443/admin/", want: "https://app.example.test/admin/", ok: true},
		{name: "escaped slash", candidate: "https://app.example.test/a%2Fb", want: "https://app.example.test/a%2Fb", ok: true},
		{name: "escaped space", candidate: "https://app.example.test/a%20b", want: "https://app.example.test/a%20b", ok: true},
		{name: "case variant", candidate: "HTTPS://APP.EXAMPLE.TEST", want: "https://app.example.test", ok: true},
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
			want: "http://jellyfin.lan",
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
