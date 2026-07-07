package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

type Monitor struct {
	cfg    config.Config
	store  *storage.Store
	logger *slog.Logger
}

func New(cfg config.Config, store *storage.Store, logger *slog.Logger) Monitor {
	return Monitor{cfg: cfg, store: store, logger: logger}
}

func (monitor Monitor) Run(ctx context.Context) {
	defaultInterval, err := monitor.cfg.DefaultInterval()
	if err != nil {
		monitor.logger.Error("monitoring scheduler disabled", "error", err)
		return
	}
	for _, target := range monitor.cfg.Runtime.Docker {
		if target.Kind == "agent" {
			continue
		}
		go monitor.runDockerLoop(ctx, target, target.CheckInterval(defaultInterval))
	}
	for _, target := range monitor.cfg.Runtime.Kubernetes {
		go monitor.runKubernetesLoop(ctx, target, target.CheckInterval(defaultInterval))
	}
	for _, target := range monitor.cfg.Runtime.HTTP {
		go monitor.runHTTPRouteLoop(ctx, target, target.CheckInterval(defaultInterval))
	}
}

func (monitor Monitor) CheckAll(ctx context.Context) error {
	services, err := monitor.store.Services(ctx)
	if err != nil {
		return err
	}
	var combined error
	for _, target := range monitor.cfg.Runtime.Docker {
		if target.Kind == "agent" {
			continue
		}
		if err := monitor.checkDocker(ctx, target, services); err != nil {
			monitor.logger.Error("docker monitoring failed", "target", target.Name, "error", err)
			combined = err
		}
	}
	for _, target := range monitor.cfg.Runtime.Kubernetes {
		if err := monitor.checkKubernetes(ctx, target, services); err != nil {
			monitor.logger.Error("kubernetes monitoring failed", "target", target.Name, "error", err)
			combined = err
		}
	}
	for _, target := range monitor.cfg.Runtime.HTTP {
		if err := monitor.checkHTTPRoutes(ctx, target, services); err != nil {
			monitor.logger.Error("http route monitoring failed", "target", target.Name, "error", err)
			combined = err
		}
	}
	return combined
}

func (monitor Monitor) runDockerLoop(ctx context.Context, target config.DockerTarget, interval time.Duration) {
	monitor.runTargetLoop(ctx, target.Name, interval, func(checkCtx context.Context, services []core.Service) error {
		return monitor.checkDocker(checkCtx, target, services)
	})
}

func (monitor Monitor) runKubernetesLoop(ctx context.Context, target config.KubernetesTarget, interval time.Duration) {
	monitor.runTargetLoop(ctx, target.Name, interval, func(checkCtx context.Context, services []core.Service) error {
		return monitor.checkKubernetes(checkCtx, target, services)
	})
}

func (monitor Monitor) runHTTPRouteLoop(ctx context.Context, target config.HTTPRouteTarget, interval time.Duration) {
	monitor.runTargetLoop(ctx, target.Name, interval, func(checkCtx context.Context, services []core.Service) error {
		return monitor.checkHTTPRoutes(checkCtx, target, services)
	})
}

func (monitor Monitor) runTargetLoop(ctx context.Context, targetName string, interval time.Duration, check func(context.Context, []core.Service) error) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			services, err := monitor.store.Services(checkCtx)
			if err == nil {
				err = check(checkCtx, services)
			}
			cancel()
			if err != nil {
				failures++
				monitor.logger.Error("runtime monitoring failed", "target", targetName, "error", err, "failures", failures)
			} else {
				failures = 0
			}
			timer.Reset(nextInterval(interval, failures))
		}
	}
}

func nextInterval(interval time.Duration, failures int) time.Duration {
	if failures < 2 {
		return interval
	}
	multiplier := 1 << min(failures-1, 3)
	next := interval * time.Duration(multiplier)
	if next > 5*time.Minute {
		return 5 * time.Minute
	}
	return next
}

func (monitor Monitor) checkDocker(ctx context.Context, target config.DockerTarget, services []core.Service) error {
	containers, err := listDockerContainers(ctx, target.Host)
	if err != nil {
		return err
	}
	for _, service := range services {
		if service.Runtime != "compose" {
			continue
		}
		health, message := dockerHealth(service, containers)
		if err := monitor.store.UpsertStatus(ctx, core.StatusResult{
			ServiceID: service.ID,
			Target:    target.Name,
			Health:    health,
			Message:   message,
			CheckedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
	}
	return nil
}

func dockerHealth(service core.Service, containers []dockerContainer) (core.HealthState, string) {
	for _, container := range containers {
		if matchesContainer(service, container) {
			if strings.EqualFold(container.State, "running") {
				return core.HealthHealthy, container.Status
			}
			return core.HealthUnhealthy, container.Status
		}
	}
	return core.HealthUnknown, "no matching container"
}

func matchesContainer(service core.Service, container dockerContainer) bool {
	for _, name := range container.Names {
		if strings.Contains(strings.TrimPrefix(name, "/"), service.Name) {
			return true
		}
	}
	for _, image := range service.Images {
		if image != "" && image == container.Image {
			return true
		}
	}
	return false
}

type dockerContainer struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

func listDockerContainers(ctx context.Context, host string) ([]dockerContainer, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	client, baseURL, err := dockerHTTPClient(host)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/containers/json?all=1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("docker api status %s", resp.Status)
	}
	var containers []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, err
	}
	return containers, nil
}

func dockerHTTPClient(host string) (*http.Client, string, error) {
	parsed, err := url.Parse(host)
	if err != nil {
		return nil, "", err
	}
	if parsed.Scheme == "unix" {
		socketPath := parsed.Path
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		}
		return &http.Client{Transport: transport, Timeout: 10 * time.Second}, "http://docker", nil
	}
	if parsed.Scheme == "tcp" {
		parsed.Scheme = "http"
	}
	if parsed.Scheme == "" {
		return nil, "", fmt.Errorf("docker host must be unix, tcp, http, or https")
	}
	return &http.Client{Timeout: 10 * time.Second}, strings.TrimRight(parsed.String(), "/"), nil
}

func (monitor Monitor) checkKubernetes(ctx context.Context, target config.KubernetesTarget, services []core.Service) error {
	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: target.Kubeconfig}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: target.Context}
	clientCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	restCfg, err := clientCfg.ClientConfig()
	if err != nil {
		return err
	}
	clientset, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	return monitor.checkKubernetesWithClient(ctx, target, services, clientset)
}

func (monitor Monitor) checkKubernetesWithClient(ctx context.Context, target config.KubernetesTarget, services []core.Service, clientset dynamic.Interface) error {
	for _, service := range services {
		if service.Runtime != "kubernetes" {
			continue
		}
		gvr, ok := gvrForKind(service.Kind)
		if !ok {
			continue
		}
		namespace := service.Namespace
		if namespace == "" {
			namespace = "default"
		}
		resource, err := clientset.Resource(gvr).Namespace(namespace).Get(ctx, service.ResourceName, metav1.GetOptions{})
		status := core.StatusResult{
			ServiceID: service.ID,
			Target:    target.Name,
			CheckedAt: time.Now().UTC(),
		}
		if err != nil {
			status.Health = core.HealthError
			status.Message = err.Error()
		} else {
			status.Health = kubeHealth(resource.Object)
			status.Message = fmt.Sprintf("%s/%s found", service.Kind, service.ResourceName)
		}
		if err := monitor.store.UpsertStatus(ctx, status); err != nil {
			return err
		}
	}
	return nil
}

func gvrForKind(kind string) (schema.GroupVersionResource, bool) {
	switch kind {
	case "Deployment":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, true
	case "StatefulSet":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, true
	case "DaemonSet":
		return schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, true
	default:
		return schema.GroupVersionResource{}, false
	}
}

func kubeHealth(object map[string]any) core.HealthState {
	status, _ := object["status"].(map[string]any)
	ready := number(status["readyReplicas"])
	available := number(status["availableReplicas"])
	desired := number(status["replicas"])
	if desired == 0 {
		return core.HealthUnknown
	}
	if ready >= desired || available >= desired {
		return core.HealthHealthy
	}
	if ready > 0 || available > 0 {
		return core.HealthDegraded
	}
	return core.HealthUnhealthy
}

func number(value any) float64 {
	switch typed := value.(type) {
	case int64:
		return float64(typed)
	case int:
		return float64(typed)
	case float64:
		return typed
	default:
		return 0
	}
}
