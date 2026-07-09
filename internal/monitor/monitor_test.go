package monitor

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	clientgotesting "k8s.io/client-go/testing"
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
		Images:       []string{"example/api:v1"},
	}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name":      "api",
				"namespace": "prod",
			},
			"spec": map[string]any{
				"selector": map[string]any{
					"matchLabels": map[string]any{"app": "api"},
				},
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{
							map[string]any{"name": "api", "image": "example/api:v2"},
						},
					},
				},
			},
			"status": map[string]any{
				"replicas":          int64(2),
				"readyReplicas":     int64(2),
				"availableReplicas": int64(2),
				"containerStatuses": []any{
					map[string]any{
						"name":    "api",
						"image":   "example/api:ignored",
						"imageID": "docker-pullable://example/api@sha256:ignored",
					},
				},
			},
		},
	}, testPod("api-1", "prod", map[string]string{"app": "api"}, "example/api:v1", "docker-pullable://example/api@sha256:release"))
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client); err != nil {
		t.Fatal(err)
	}
	if health, _ := kubeHealth("Deployment", map[string]any{"status": map[string]any{"replicas": int64(2), "readyReplicas": int64(1)}}); health != core.HealthDegraded {
		t.Fatal("expected degraded health for partially ready workload")
	}
	if _, ok := gvrForKind("Deployment"); !ok {
		t.Fatal("deployment GVR missing")
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || len(statuses[0].ObservedImages) != 1 {
		t.Fatalf("statuses = %#v, want observed Kubernetes pod image metadata", statuses)
	}
	if statuses[0].ObservedImages[0].Reference.Tag != "v1" {
		t.Fatalf("observed image = %#v, want pod status image rather than workload spec/status image", statuses[0].ObservedImages[0])
	}
	if statuses[0].ObservedImages[0].RepoDigests[0].Digest != "sha256:release" {
		t.Fatalf("observed image digests = %#v, want pod status imageID digest", statuses[0].ObservedImages)
	}
}

func TestKubernetesWorkloadHealthByKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		kind   string
		object map[string]any
		want   core.HealthState
	}{
		{
			name:   "healthy DaemonSet",
			kind:   "DaemonSet",
			object: testDaemonSet("node-agent", "prod", map[string]string{"app": "node-agent"}, 3, 3, 3, 3, 0).Object,
			want:   core.HealthHealthy,
		},
		{
			name:   "unhealthy DaemonSet",
			kind:   "DaemonSet",
			object: testDaemonSet("node-agent", "prod", map[string]string{"app": "node-agent"}, 3, 0, 0, 0, 0).Object,
			want:   core.HealthUnhealthy,
		},
		{
			name:   "healthy StatefulSet",
			kind:   "StatefulSet",
			object: testStatefulSet("db", "prod", map[string]string{"app": "db"}, 2, 2).Object,
			want:   core.HealthHealthy,
		},
		{
			name:   "degraded StatefulSet",
			kind:   "StatefulSet",
			object: testStatefulSet("db", "prod", map[string]string{"app": "db"}, 3, 1).Object,
			want:   core.HealthDegraded,
		},
		{
			name:   "completed Job",
			kind:   "Job",
			object: testJob("backup", "prod", int64(1), int64(1), 0, 0, "Complete").Object,
			want:   core.HealthHealthy,
		},
		{
			name:   "failed Job",
			kind:   "Job",
			object: testJob("backup", "prod", int64(1), 0, int64(1), 0, "Failed").Object,
			want:   core.HealthUnhealthy,
		},
		{
			name:   "successful CronJob",
			kind:   "CronJob",
			object: testCronJob("backup", "prod", "2026-07-08T10:00:00Z", "2026-07-08T10:01:00Z", 0).Object,
			want:   core.HealthHealthy,
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, message := kubeHealth(tc.kind, tc.object)
			if got != tc.want {
				t.Fatalf("health = %s, want %s; message=%q", got, tc.want, message)
			}
		})
	}
}

func TestKubernetesUnsupportedKindPersistsNotApplicable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	services := []core.Service{{
		ID:           "svc",
		Name:         "plex-release",
		Runtime:      "kubernetes",
		Kind:         "HelmRelease",
		Namespace:    "plex",
		ResourceName: "plex-release",
	}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme())
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client); err != nil {
		t.Fatal(err)
	}
	if _, ok := gvrForKind("HelmRelease"); ok {
		t.Fatal("HelmRelease should be explicit not supported, not queried as a native workload")
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %#v, want one explicit unsupported status", statuses)
	}
	if statuses[0].Health != core.HealthNotApplicable {
		t.Fatalf("health = %s, want not_applicable", statuses[0].Health)
	}
	if !strings.Contains(statuses[0].Message, "not supported") || !strings.Contains(statuses[0].Message, "HelmRelease") {
		t.Fatalf("message = %q, want unsupported HelmRelease explanation", statuses[0].Message)
	}
}

func TestKubernetesObservedImagesReportMixedPodVersions(t *testing.T) {
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
		Images:       []string{"example/api:v1"},
	}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), testDeployment("api", "prod", map[string]string{"app": "api"}, 2), testPod("api-1", "prod", map[string]string{"app": "api"}, "example/api:v1", "docker-pullable://example/api@sha256:v1"), testPod("api-2", "prod", map[string]string{"app": "api"}, "example/api:v2", "docker-pullable://example/api@sha256:v2"))
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client); err != nil {
		t.Fatal(err)
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || len(statuses[0].ObservedImages) != 2 {
		t.Fatalf("statuses = %#v, want both pod image versions", statuses)
	}
	tags := map[string]bool{}
	for _, image := range statuses[0].ObservedImages {
		tags[image.Reference.Tag] = true
	}
	if !tags["v1"] || !tags["v2"] {
		t.Fatalf("observed images = %#v, want v1 and v2", statuses[0].ObservedImages)
	}
}

func TestKubernetesObservedImagesIgnoreTerminatingPods(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:           "svc",
		Name:         "api",
		Runtime:      "kubernetes",
		Kind:         "Deployment",
		Namespace:    "prod",
		ResourceName: "api",
		Images:       []string{"example/api:v1.0.0"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/deployment.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	oldPod := testPod("api-old", "prod", map[string]string{"app": "api"}, "example/api:v0.9.0", "docker-pullable://example/api@sha256:old")
	oldPod.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-07-08T12:00:00Z"
	failedPod := testPod("api-failed", "prod", map[string]string{"app": "api"}, "example/api:v0.8.0", "docker-pullable://example/api@sha256:failed")
	failedPod.Object["status"].(map[string]any)["phase"] = "Failed"
	currentPod := testPod("api-1", "prod", map[string]string{"app": "api"}, "example/api:v1.0.0", "docker-pullable://example/api@sha256:current")
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), testDeployment("api", "prod", map[string]string{"app": "api"}, 1), oldPod, failedPod, currentPod)
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, []core.Service{service}, client); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].ImageVersionState != core.ImageVersionMatching {
		t.Fatalf("image version state = %s, want matching; checks=%#v", summary.Services[0].ImageVersionState, summary.Services[0].ImageVersionChecks)
	}
	if len(summary.Statuses) != 1 || len(summary.Statuses[0].ObservedImages) != 1 {
		t.Fatalf("observed images = %#v, want only live pod image metadata", summary.Statuses)
	}
	if got := summary.Statuses[0].ObservedImages[0].Reference.Tag; got != "v1.0.0" {
		t.Fatalf("observed image tag = %q, want v1.0.0", got)
	}
	if got := summary.Statuses[0].ObservedImages[0].RepoDigests[0].Digest; got != "sha256:current" {
		t.Fatalf("observed digest = %q, want current pod digest", got)
	}
}

func TestKubernetesObservedImagesUnknownWhenNoPodsMatch(t *testing.T) {
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
		Images:       []string{"example/api:v1"},
	}}
	client := fake.NewSimpleDynamicClient(runtime.NewScheme(), testDeployment("api", "prod", map[string]string{"app": "api"}, 2), testPod("unrelated-1", "prod", map[string]string{"app": "other"}, "example/api:v1", "docker-pullable://example/api@sha256:v1"))
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client); err != nil {
		t.Fatal(err)
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || len(statuses[0].ObservedImages) != 0 {
		t.Fatalf("statuses = %#v, want no observed images when no pods match", statuses)
	}
}

func TestKubernetesPodImageLookupFailurePreservesWorkloadHealth(t *testing.T) {
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
		Images:       []string{"example/api:v1"},
	}}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "apps", Version: "v1", Resource: "deployments"}: "DeploymentList",
		podGVR(): "PodList",
	}, testDeployment("api", "prod", map[string]string{"app": "api"}, 2))
	client.PrependReactor("list", "pods", func(action clientgotesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("rbac denied"))
	})
	monitor := New(config.Config{}, store, slog.Default())
	if err := monitor.checkKubernetesWithClient(ctx, config.KubernetesTarget{Name: "cluster"}, services, client); err != nil {
		t.Fatal(err)
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses = %#v, want one status", statuses)
	}
	if statuses[0].Health != core.HealthHealthy {
		t.Fatalf("health = %s, want healthy despite pod image lookup failure; status=%#v", statuses[0].Health, statuses[0])
	}
	if len(statuses[0].ObservedImages) != 0 {
		t.Fatalf("observed images = %#v, want unknown image metadata on pod list failure", statuses[0].ObservedImages)
	}
	if !strings.Contains(statuses[0].Message, "image metadata unavailable") {
		t.Fatalf("message = %q, want image metadata note", statuses[0].Message)
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

func testDeployment(name, namespace string, selector map[string]string, replicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"selector": map[string]any{
				"matchLabels": stringMapAny(selector),
			},
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": name, "image": "example/" + name + ":desired"},
					},
				},
			},
		},
		"status": map[string]any{
			"replicas":          replicas,
			"readyReplicas":     replicas,
			"availableReplicas": replicas,
		},
	}}
}

func testStatefulSet(name, namespace string, selector map[string]string, replicas, readyReplicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "StatefulSet",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{
				"matchLabels": stringMapAny(selector),
			},
		},
		"status": map[string]any{
			"replicas":      replicas,
			"readyReplicas": readyReplicas,
		},
	}}
}

func testDaemonSet(name, namespace string, selector map[string]string, desired, ready, available, updated, misscheduled int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"selector": map[string]any{
				"matchLabels": stringMapAny(selector),
			},
		},
		"status": map[string]any{
			"desiredNumberScheduled": desired,
			"numberReady":            ready,
			"numberAvailable":        available,
			"updatedNumberScheduled": updated,
			"numberMisscheduled":     misscheduled,
		},
	}}
}

func testJob(name, namespace string, completions, succeeded, failed, active int64, conditionType string) *unstructured.Unstructured {
	conditions := []any{}
	if conditionType != "" {
		conditions = append(conditions, map[string]any{
			"type":   conditionType,
			"status": "True",
		})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"completions": completions,
		},
		"status": map[string]any{
			"succeeded":  succeeded,
			"failed":     failed,
			"active":     active,
			"conditions": conditions,
		},
	}}
}

func testCronJob(name, namespace, lastSchedule, lastSuccessful string, active int) *unstructured.Unstructured {
	activeJobs := []any{}
	for i := 0; i < active; i++ {
		activeJobs = append(activeJobs, map[string]any{"name": name})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "CronJob",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{},
		"status": map[string]any{
			"active":             activeJobs,
			"lastScheduleTime":   lastSchedule,
			"lastSuccessfulTime": lastSuccessful,
		},
	}}
}

func testPod(name, namespace string, labels map[string]string, image, imageID string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    stringMapAny(labels),
		},
		"status": map[string]any{
			"phase": "Running",
			"containerStatuses": []any{
				map[string]any{
					"name":    "app",
					"image":   image,
					"imageID": imageID,
				},
			},
		},
	}}
}

func stringMapAny(values map[string]string) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
