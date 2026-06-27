package monitor

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func TestKubernetesStatusMappingWithFakeClient(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	services := []core.Service{{
		ID:           "svc",
		Name:         "api",
		Runtime:      "kubernetes",
		Kind:         "Deployment",
		Namespace:    "prod",
		ResourceName: "api",
	}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name":      "api",
				"namespace": "prod",
			},
			"status": map[string]any{
				"replicas":          int64(2),
				"readyReplicas":     int64(2),
				"availableReplicas": int64(2),
			},
		},
	})
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client); err != nil {
		t.Fatal(err)
	}
	if kubeHealth(map[string]any{"status": map[string]any{"replicas": int64(2), "readyReplicas": int64(1)}}) != core.HealthDegraded {
		t.Fatal("expected degraded health for partially ready workload")
	}
	if _, ok := gvrForKind("Deployment"); !ok {
		t.Fatal("deployment GVR missing")
	}
}

func TestKubernetesMissingResourceMapsToError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	monitor := New(config.Config{}, store, slog.Default())
	err = monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, []core.Service{{
		ID:           "svc",
		Runtime:      "kubernetes",
		Kind:         "Deployment",
		Namespace:    metav1.NamespaceDefault,
		ResourceName: "missing",
	}}, client)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNextIntervalBacksOffAfterRepeatedFailures(t *testing.T) {
	t.Parallel()
	base := 30 * time.Second
	if got := nextInterval(base, 1); got != base {
		t.Fatalf("first failure interval = %s, want %s", got, base)
	}
	if got := nextInterval(base, 3); got != 2*time.Minute {
		t.Fatalf("third failure interval = %s, want 2m", got)
	}
	if got := nextInterval(time.Minute, 20); got != 5*time.Minute {
		t.Fatalf("capped interval = %s, want 5m", got)
	}
}
