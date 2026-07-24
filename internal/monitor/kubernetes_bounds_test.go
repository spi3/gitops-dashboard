package monitor

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
	clientgotesting "k8s.io/client-go/testing"
)

func TestKubernetesRESTConfigTimeout(t *testing.T) {
	t.Parallel()
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	contents := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://example.invalid:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user: {}
`
	if err := os.WriteFile(kubeconfig, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	restCfg, err := kubernetesRESTConfig(kubeconfig, "")
	if err != nil {
		t.Fatal(err)
	}
	if restCfg.Timeout != 10*time.Second {
		t.Fatalf("Timeout = %s, want 10s", restCfg.Timeout)
	}
	if restCfg.Host != "https://example.invalid:6443" {
		t.Fatalf("Host = %q, want the configured cluster server", restCfg.Host)
	}
}

func TestKubernetesCycleBudget(t *testing.T) {
	t.Parallel()
	applicable := func(n int) []core.Service {
		services := make([]core.Service, n)
		for i := range services {
			services[i] = core.Service{Runtime: "kubernetes", Kind: "Deployment"}
		}
		return services
	}
	unsupported := func(n int) []core.Service {
		services := make([]core.Service, n)
		for i := range services {
			services[i] = core.Service{Runtime: "kubernetes", Kind: "HelmRelease"}
		}
		return services
	}
	tests := []struct {
		name     string
		services []core.Service
		interval time.Duration
		want     time.Duration
	}{
		{"zero applicable services", nil, 30 * time.Second, 2 * time.Second},
		{"unsupported-only", unsupported(3), 30 * time.Second, 2 * time.Second},
		{"one applicable, in range", applicable(1), 30 * time.Second, 22 * time.Second},
		{"multiple applicable, uncapped", applicable(2), 2 * time.Minute, 42 * time.Second},
		{"below five seconds clamps to floor", applicable(1), 2 * time.Second, 5 * time.Second},
		{"above two minutes clamps to ceiling", applicable(7), 10 * time.Minute, 2 * time.Minute},
		{"cap-limited within range", applicable(10), 45 * time.Second, 45 * time.Second},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := computeKubernetesCycleBudget(tc.services, tc.interval); got != tc.want {
				t.Fatalf("computeKubernetesCycleBudget(%d services, %s) = %s, want %s", len(tc.services), tc.interval, got, tc.want)
			}
		})
	}
}

func TestKubernetesRequestCounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("two requests for usable pod-selection metadata", func(t *testing.T) {
		t.Parallel()
		store, err := storage.Open(t.TempDir() + "/dashboard.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		services := []core.Service{{ID: "svc", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"}}
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(),
			testDeployment("api", "prod", map[string]string{"app": "api"}, 1),
			testPod("api-1", "prod", map[string]string{"app": "api"}, "example/api:v1", "docker-pullable://example/api@sha256:v1"),
		)
		monitor := New(config.Config{}, store, slog.Default())
		if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client); err != nil {
			t.Fatal(err)
		}
		if got := len(client.Actions()); got != 2 {
			t.Fatalf("actions = %d, want 2 (Get + List)", got)
		}
	})

	t.Run("one request when pod selection is unusable", func(t *testing.T) {
		t.Parallel()
		store, err := storage.Open(t.TempDir() + "/dashboard.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		services := []core.Service{{ID: "svc", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"}}
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(), testDeployment("api", "prod", nil, 1))
		monitor := New(config.Config{}, store, slog.Default())
		if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client); err != nil {
			t.Fatal(err)
		}
		if got := len(client.Actions()); got != 1 {
			t.Fatalf("actions = %d, want 1 (Get only)", got)
		}
	})

	t.Run("zero for unsupported kinds", func(t *testing.T) {
		t.Parallel()
		store, err := storage.Open(t.TempDir() + "/dashboard.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		services := []core.Service{{ID: "svc", Runtime: "kubernetes", Kind: "HelmRelease", Namespace: "prod", ResourceName: "legacy"}}
		client := fake.NewSimpleDynamicClient(runtime.NewScheme())
		monitor := New(config.Config{}, store, slog.Default())
		if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client); err != nil {
			t.Fatal(err)
		}
		if got := len(client.Actions()); got != 0 {
			t.Fatalf("actions = %d, want 0 for an unsupported kind", got)
		}
	})

	t.Run("never more than two per applicable service across multiple services", func(t *testing.T) {
		t.Parallel()
		store, err := storage.Open(t.TempDir() + "/dashboard.db")
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		services := []core.Service{
			{ID: "api", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"},
			{ID: "db", Runtime: "kubernetes", Kind: "StatefulSet", Namespace: "prod", ResourceName: "db"},
		}
		client := fake.NewSimpleDynamicClient(runtime.NewScheme(),
			testDeployment("api", "prod", map[string]string{"app": "api"}, 1),
			testPod("api-1", "prod", map[string]string{"app": "api"}, "example/api:v1", "docker-pullable://example/api@sha256:v1"),
			testStatefulSet("db", "prod", map[string]string{"app": "db"}, 1, 1),
			testPod("db-1", "prod", map[string]string{"app": "db"}, "example/db:v1", "docker-pullable://example/db@sha256:v1"),
		)
		monitor := New(config.Config{}, store, slog.Default())
		if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client); err != nil {
			t.Fatal(err)
		}
		if got := len(client.Actions()); got != 4 {
			t.Fatalf("actions = %d, want exactly 2 per applicable service (4 total)", got)
		}
	})
}

func TestKubernetesGetDeadlineAbortsTargetCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	services := []core.Service{{ID: "svc", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), testDeployment("api", "prod", map[string]string{"app": "api"}, 1))
	client.PrependReactor("get", "deployments", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	monitor := New(config.Config{}, store, slog.Default())
	start := time.Now()
	err = monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed = %s, want an immediate abort rather than waiting for the production request timeout", elapsed)
	}

	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 {
		t.Fatalf("statuses = %#v, want no ordinary per-service status written when a Get deadline aborts the check", statuses)
	}
}

func TestKubernetesPodListDeadlineAbortsTargetCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	services := []core.Service{{ID: "svc", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "apps", Version: "v1", Resource: "deployments"}: "DeploymentList",
		podGVR(): "PodList",
	}, testDeployment("api", "prod", map[string]string{"app": "api"}, 1))
	client.PrependReactor("list", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	monitor := New(config.Config{}, store, slog.Default())
	start := time.Now()
	err = monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed = %s, want an immediate abort rather than waiting for the production request timeout", elapsed)
	}

	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 {
		t.Fatalf("statuses = %#v, want no ordinary per-service status (e.g. a degraded metadata-unavailable message) when a pod List deadline aborts the check", statuses)
	}
}

func TestKubernetesRESTRequestTimeoutAbortsTargetCheck(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer server.Close()
	defer close(block)

	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// A short test-only request timeout paired with a much longer phase
	// budget: only the request timeout can plausibly abort this in time.
	restCfg := &rest.Config{Host: server.URL, Timeout: 50 * time.Millisecond}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}

	services := []core.Service{{ID: "svc", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"}}
	monitor := New(config.Config{}, store, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resultCh := make(chan error, 1)
	start := time.Now()
	go func() {
		resultCh <- monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client)
	}()

	select {
	case err := <-resultCh:
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed > time.Second {
			t.Fatalf("elapsed = %s, want the 50ms request timeout to abort, not the 5s phase budget", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the request timeout to abort the check")
	}
}

func TestKubernetesPhaseDeadlinePersistsCoveredFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	services := []core.Service{
		{ID: "api", Name: "api", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"},
		{ID: "db", Name: "db", Runtime: "kubernetes", Kind: "StatefulSet", Namespace: "prod", ResourceName: "db"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "kubernetes", services); err != nil {
		t.Fatal(err)
	}
	// Seed a prior healthy result for "api" from an earlier attempt, to
	// prove the failure write overwrites earlier applicable-service results
	// from the same failed attempt.
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "api", Target: "cluster", Health: core.HealthHealthy, Message: "prior success", CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), testDeployment("api", "prod", map[string]string{"app": "api"}, 1))
	client.PrependReactor("get", "deployments", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	cfg := config.Config{Runtime: config.RuntimeConfig{Kubernetes: []config.KubernetesTarget{{Name: "cluster"}}}}
	monitor := New(cfg, store, slog.Default())
	monitor.kubernetesClient = func(config.KubernetesTarget) (dynamic.Interface, error) { return client, nil }

	if err := monitor.CheckAll(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CheckAll err = %v, want context.DeadlineExceeded", err)
	}

	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byService := map[string]core.StatusResult{}
	for _, status := range statuses {
		byService[status.ServiceID] = status
	}
	for _, id := range []string{"api", "db"} {
		status, ok := byService[id]
		if !ok {
			t.Fatalf("service %s has no status, want an overwritten failure row", id)
		}
		if status.Health != core.HealthError || status.Message != "monitor target check failed" {
			t.Fatalf("service %s status = %#v, want the generic Kubernetes failure row", id, status)
		}
	}
}

func TestKubernetesDeadlineLeavesLaterUnsupportedServicesUntouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	services := []core.Service{
		{ID: "api", Name: "api", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"},
		{ID: "legacy", Name: "legacy", Runtime: "kubernetes", Kind: "HelmRelease", Namespace: "prod", ResourceName: "legacy"},
		{ID: "unseeded", Name: "unseeded", Runtime: "kubernetes", Kind: "HelmRelease", Namespace: "prod", ResourceName: "unseeded"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "kubernetes", services); err != nil {
		t.Fatal(err)
	}
	// "legacy" already has a prior not-applicable row; "unseeded" has none.
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "legacy", Target: "cluster", Health: core.HealthNotApplicable, Message: "prior not applicable", CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), testDeployment("api", "prod", map[string]string{"app": "api"}, 1))
	client.PrependReactor("get", "deployments", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, context.DeadlineExceeded
	})

	cfg := config.Config{Runtime: config.RuntimeConfig{Kubernetes: []config.KubernetesTarget{{Name: "cluster"}}}}
	monitor := New(cfg, store, slog.Default())
	monitor.kubernetesClient = func(config.KubernetesTarget) (dynamic.Interface, error) { return client, nil }

	if err := monitor.CheckAll(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CheckAll err = %v, want context.DeadlineExceeded", err)
	}

	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byService := map[string]core.StatusResult{}
	for _, status := range statuses {
		byService[status.ServiceID] = status
	}

	if status, ok := byService["api"]; !ok || status.Health != core.HealthError || status.Message != "monitor target check failed" {
		t.Fatalf("api status = %#v, want the generic Kubernetes failure row", status)
	}
	if status, ok := byService["legacy"]; !ok || status.Health != core.HealthNotApplicable || status.Message != "prior not applicable" {
		t.Fatalf("legacy status = %#v, want the untouched prior not-applicable row", status)
	}
	if status, ok := byService["unseeded"]; ok {
		t.Fatalf("unseeded status = %#v, want no status row for an unsupported service the aborted attempt never reached", status)
	}
}

func TestKubernetesParentCancellationSkipsFailurePersistence(t *testing.T) {
	t.Parallel()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	services := []core.Service{{ID: "api", Name: "api", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"}}
	if err := store.ReplaceConfiguredServices(context.Background(), "repo", "kubernetes", services); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	getStarted := make(chan struct{})
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), testDeployment("api", "prod", map[string]string{"app": "api"}, 1))
	client.PrependReactor("get", "deployments", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		close(getStarted)
		<-ctx.Done()
		return true, nil, ctx.Err()
	})

	cfg := config.Config{Runtime: config.RuntimeConfig{Kubernetes: []config.KubernetesTarget{{Name: "cluster"}}}}
	monitor := New(cfg, store, slog.Default())
	monitor.kubernetesClient = func(config.KubernetesTarget) (dynamic.Interface, error) { return client, nil }

	resultCh := make(chan error, 1)
	go func() { resultCh <- monitor.CheckAll(ctx) }()

	select {
	case <-getStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the stalled request to start")
	}
	cancel()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CheckAll err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CheckAll to return promptly after cancellation")
	}

	statuses, err := store.StatusResults(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 {
		t.Fatalf("statuses = %#v, want no generic failure row or history written after parent cancellation", statuses)
	}
}

func TestKubernetesScheduledCycleRecoversAfterTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	services := []core.Service{{ID: "api", Name: "api", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"}}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "kubernetes", services); err != nil {
		t.Fatal(err)
	}

	// A nil selector keeps pod-selection metadata unusable, so each attempt
	// issues only the one workload Get this test's reactor controls.
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), testDeployment("api", "prod", nil, 1))
	attempted := make(chan struct{}, 10)
	var attempt int32
	client.PrependReactor("get", "deployments", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		n := atomic.AddInt32(&attempt, 1)
		attempted <- struct{}{}
		if n == 1 {
			return true, nil, context.DeadlineExceeded
		}
		return false, nil, nil // fall through to the default reactor (succeeds)
	})

	monitor := New(config.Config{}, store, slog.Default())
	monitor.kubernetesClient = func(config.KubernetesTarget) (dynamic.Interface, error) { return client, nil }

	go monitor.runKubernetesLoop(ctx, config.KubernetesTarget{Name: "cluster"}, 30*time.Millisecond)

	select {
	case <-attempted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first attempt")
	}

	waitForStatus(t, store, func(status core.StatusResult) bool {
		return status.Health == core.HealthError && status.Message == "monitor target check failed"
	}, "the first failed attempt's Kubernetes failure row")

	select {
	case <-attempted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the second, recovering attempt")
	}

	waitForStatus(t, store, func(status core.StatusResult) bool {
		return status.Health == core.HealthHealthy
	}, "the second attempt to recover to healthy")
}

// waitForStatus polls the store's single expected status row until match
// succeeds or a two-second wall-clock guard elapses.
func waitForStatus(t *testing.T, store *storage.Store, match func(core.StatusResult) bool, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		statuses, err := store.StatusResults(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(statuses) == 1 && match(statuses[0]) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("statuses = %#v, want %s", statuses, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestCheckAllUsesKubernetesCycleBudget(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer server.Close()
	defer close(block)

	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	services := []core.Service{{ID: "api", Name: "api", Runtime: "kubernetes", Kind: "Deployment", Namespace: "prod", ResourceName: "api"}}
	if err := store.ReplaceConfiguredServices(context.Background(), "repo", "kubernetes", services); err != nil {
		t.Fatal(err)
	}

	// A long per-request timeout that would never fire on its own within
	// the test guard, so only the seam-provided short phase budget can
	// account for a prompt abort.
	restCfg := &rest.Config{Host: server.URL, Timeout: 5 * time.Second}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{Runtime: config.RuntimeConfig{Kubernetes: []config.KubernetesTarget{{Name: "cluster"}}}}
	monitor := New(cfg, store, slog.Default())
	monitor.kubernetesClient = func(config.KubernetesTarget) (dynamic.Interface, error) { return client, nil }
	monitor.kubernetesCycleBudget = func([]core.Service, time.Duration) time.Duration { return 50 * time.Millisecond }

	start := time.Now()
	resultCh := make(chan error, 1)
	go func() { resultCh <- monitor.CheckAll(context.Background()) }()

	select {
	case err := <-resultCh:
		elapsed := time.Since(start)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("CheckAll err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed > time.Second {
			t.Fatalf("elapsed = %s, want the seam phase budget (50ms) to abort, not the 5s request timeout", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the phase budget to abort CheckAll")
	}
}
