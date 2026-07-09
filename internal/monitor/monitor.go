package monitor

import (
	"context"
	"encoding/json"
	"errors"
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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

var ErrAgentTargetUnauthorized = errors.New("agent target is not authorized for token")

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
	for _, target := range monitor.cfg.Runtime.Ping {
		go monitor.runPingLoop(ctx, target, target.CheckInterval(defaultInterval))
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
	for _, target := range monitor.cfg.Runtime.Ping {
		if err := monitor.checkPing(ctx, target); err != nil {
			monitor.logger.Error("ping monitoring failed", "target", target.EffectiveName(), "error", err)
			combined = err
		}
	}
	return combined
}

func (monitor Monitor) ApplyAgentReport(ctx context.Context, message core.AgentMessage, authorizedTargets []string) error {
	target := strings.TrimSpace(message.Target)
	if !agentTargetAllowed(target, authorizedTargets) {
		return fmt.Errorf("%w: %q", ErrAgentTargetUnauthorized, target)
	}
	message.Target = target
	if err := monitor.store.UpsertAgent(ctx, message); err != nil {
		return err
	}
	services, err := monitor.store.Services(ctx)
	if err != nil {
		return err
	}
	checkedAt := message.CheckedAt.UTC()
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	containers := agentDockerContainers(message.Containers)
	for _, service := range services {
		if service.Runtime != "compose" || composeServiceTarget(service) != target {
			continue
		}
		health, statusMessage, observedImages := dockerStatus(ctx, service, target, containers, nil)
		if err := monitor.store.UpsertStatus(ctx, core.StatusResult{
			ServiceID:      service.ID,
			Target:         target,
			Health:         health,
			Message:        statusMessage,
			CheckedAt:      checkedAt,
			ObservedImages: observedImages,
		}); err != nil {
			return err
		}
	}
	return nil
}

func agentTargetAllowed(target string, authorizedTargets []string) bool {
	if target == "" {
		return false
	}
	for _, authorizedTarget := range authorizedTargets {
		if target == strings.TrimSpace(authorizedTarget) {
			return true
		}
	}
	return false
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

func (monitor Monitor) runPingLoop(ctx context.Context, target config.PingTarget, interval time.Duration) {
	monitor.runTargetLoop(ctx, target.EffectiveName(), interval, func(checkCtx context.Context, _ []core.Service) error {
		return monitor.checkPing(checkCtx, target)
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
	imageInspector, err := newDockerImageInspector(target.Host)
	if err != nil {
		imageInspector = nil
	}
	for _, service := range services {
		if service.Runtime != "compose" {
			continue
		}
		health, message, observedImages := dockerStatus(ctx, service, target.Name, containers, imageInspector)
		if err := monitor.store.UpsertStatus(ctx, core.StatusResult{
			ServiceID:      service.ID,
			Target:         target.Name,
			Health:         health,
			Message:        message,
			CheckedAt:      time.Now().UTC(),
			ObservedImages: observedImages,
		}); err != nil {
			return err
		}
	}
	return nil
}

func dockerHealth(service core.Service, containers []dockerContainer) (core.HealthState, string) {
	health, message, _ := dockerStatus(context.Background(), service, "", containers, nil)
	return health, message
}

func dockerStatus(ctx context.Context, service core.Service, target string, containers []dockerContainer, imageInspector *dockerImageInspector) (core.HealthState, string, []core.ObservedImage) {
	matches := matchingDockerContainers(service, containers)
	observedImages := observedDockerImages(ctx, target, matches, imageInspector)
	for _, container := range matches {
		if matchesContainer(service, container) {
			if strings.EqualFold(container.State, "running") {
				switch strings.ToLower(strings.TrimSpace(container.Health)) {
				case "unhealthy":
					return core.HealthUnhealthy, container.Status, observedImages
				case "starting":
					return core.HealthDegraded, container.Status, observedImages
				case "healthy":
					return core.HealthHealthy, container.Status, observedImages
				}
				status := strings.ToLower(container.Status)
				if strings.Contains(status, "(unhealthy)") {
					return core.HealthUnhealthy, container.Status, observedImages
				}
				if strings.Contains(status, "(health: starting)") {
					return core.HealthDegraded, container.Status, observedImages
				}
				return core.HealthHealthy, container.Status, observedImages
			}
			return core.HealthUnhealthy, container.Status, observedImages
		}
	}
	return core.HealthUnknown, "no matching container", nil
}

func matchingDockerContainers(service core.Service, containers []dockerContainer) []dockerContainer {
	var matches []dockerContainer
	for _, container := range containers {
		if matchesContainer(service, container) {
			matches = append(matches, container)
		}
	}
	return matches
}

func observedDockerImages(ctx context.Context, target string, containers []dockerContainer, imageInspector *dockerImageInspector) []core.ObservedImage {
	images := make([]core.ObservedImage, 0, len(containers))
	for _, container := range containers {
		if !liveDockerContainer(container.State, container.Status) {
			continue
		}
		repoDigests := container.RepoDigests
		if imageInspector != nil {
			repoDigests = imageInspector.repoDigests(ctx, container)
		}
		images = append(images, core.NewObservedImage(target, "docker", container.Image, container.ImageID, repoDigests))
	}
	return images
}

func liveDockerContainer(state, status string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running", "restarting", "paused":
		return true
	case "":
		normalizedStatus := strings.ToLower(strings.TrimSpace(status))
		return strings.HasPrefix(normalizedStatus, "up") || strings.HasPrefix(normalizedStatus, "restarting")
	default:
		return false
	}
}

type dockerImageInspector struct {
	client  *http.Client
	baseURL string
	cache   map[string][]string
}

type dockerImageInspect struct {
	RepoDigests []string `json:"RepoDigests"`
}

func newDockerImageInspector(host string) (*dockerImageInspector, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	client, baseURL, err := dockerHTTPClient(host)
	if err != nil {
		return nil, err
	}
	return &dockerImageInspector{
		client:  client,
		baseURL: baseURL,
		cache:   map[string][]string{},
	}, nil
}

func (inspector *dockerImageInspector) repoDigests(ctx context.Context, container dockerContainer) []string {
	if inspector == nil {
		return container.RepoDigests
	}
	key := strings.TrimSpace(container.ImageID)
	if key == "" {
		key = strings.TrimSpace(container.Image)
	}
	if key == "" {
		return container.RepoDigests
	}
	if digests, ok := inspector.cache[key]; ok {
		return mergeDockerRepoDigests(container.RepoDigests, digests)
	}
	digests := inspector.inspectRepoDigests(ctx, key)
	inspector.cache[key] = digests
	return mergeDockerRepoDigests(container.RepoDigests, digests)
}

func (inspector *dockerImageInspector) inspectRepoDigests(ctx context.Context, key string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, inspector.baseURL+"/images/"+url.PathEscape(key)+"/json", nil)
	if err != nil {
		return nil
	}
	resp, err := inspector.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil
	}
	var image dockerImageInspect
	if err := json.NewDecoder(resp.Body).Decode(&image); err != nil {
		return nil
	}
	return image.RepoDigests
}

func mergeDockerRepoDigests(values ...[]string) []string {
	seen := map[string]struct{}{}
	var result []string
	for _, list := range values {
		for _, value := range list {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
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
	ID          string   `json:"Id"`
	Names       []string `json:"Names"`
	Image       string   `json:"Image"`
	ImageID     string   `json:"ImageID"`
	RepoDigests []string `json:"RepoDigests"`
	State       string   `json:"State"`
	Status      string   `json:"Status"`
	Health      string   `json:"Health"`
}

func agentDockerContainers(statuses []core.ContainerStatus) []dockerContainer {
	containers := make([]dockerContainer, 0, len(statuses))
	for _, status := range statuses {
		var names []string
		if status.Name != "" {
			names = []string{status.Name}
		}
		containers = append(containers, dockerContainer{
			ID:          status.ID,
			Names:       names,
			Image:       status.Image,
			ImageID:     status.ImageID,
			RepoDigests: status.RepoDigests,
			State:       status.State,
			Status:      status.Status,
			Health:      status.Health,
		})
	}
	return containers
}

func composeServiceTarget(service core.Service) string {
	path := strings.ReplaceAll(service.SourcePath, "\\", "/")
	parts := strings.Split(path, "/")
	if len(parts) >= 2 && parts[0] == "docker_files" {
		return parts[1]
	}
	return ""
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
			observedImages, err := observedKubernetesImages(ctx, clientset, target.Name, namespace, resource.Object)
			if err != nil {
				status.Message = fmt.Sprintf("%s; image metadata unavailable: %v", status.Message, err)
			} else {
				status.ObservedImages = observedImages
			}
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

func podGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Version: "v1", Resource: "pods"}
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

func observedKubernetesImages(ctx context.Context, clientset dynamic.Interface, target, namespace string, object map[string]any) ([]core.ObservedImage, error) {
	selector, ok, err := workloadLabelSelector(object)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	pods, err := clientset.Resource(podGVR()).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list pods for workload images: %w", err)
	}
	var images []core.ObservedImage
	for _, pod := range pods.Items {
		podImages := observedPodImages(target, pod.Object)
		if !liveKubernetesPod(pod.Object, len(podImages) > 0) {
			continue
		}
		images = append(images, podImages...)
	}
	return uniqueObservedKubernetesImages(images), nil
}

func liveKubernetesPod(object map[string]any, hasObservedImages bool) bool {
	if value, found, _ := unstructured.NestedFieldNoCopy(object, "metadata", "deletionTimestamp"); found {
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return false
			}
		case nil:
		default:
			return false
		}
	}
	phase, _, _ := unstructured.NestedString(object, "status", "phase")
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "running":
		return true
	case "pending":
		return hasObservedImages
	default:
		return false
	}
}

func workloadLabelSelector(object map[string]any) (string, bool, error) {
	labelSelector := metav1.LabelSelector{}
	matchLabels, found, err := unstructured.NestedStringMap(object, "spec", "selector", "matchLabels")
	if err != nil {
		return "", false, fmt.Errorf("read workload matchLabels: %w", err)
	}
	if found {
		labelSelector.MatchLabels = matchLabels
	}
	expressions, found, err := unstructured.NestedSlice(object, "spec", "selector", "matchExpressions")
	if err != nil {
		return "", false, fmt.Errorf("read workload matchExpressions: %w", err)
	}
	if found {
		for _, item := range expressions {
			expression := kubeMap(item)
			key := kubeString(expression["key"])
			operator := kubeString(expression["operator"])
			if key == "" || operator == "" {
				continue
			}
			labelSelector.MatchExpressions = append(labelSelector.MatchExpressions, metav1.LabelSelectorRequirement{
				Key:      key,
				Operator: metav1.LabelSelectorOperator(operator),
				Values:   kubeStringSlice(expression["values"]),
			})
		}
	}
	if len(labelSelector.MatchLabels) == 0 && len(labelSelector.MatchExpressions) == 0 {
		return "", false, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		return "", false, fmt.Errorf("build workload pod selector: %w", err)
	}
	return selector.String(), true, nil
}

func observedPodImages(target string, object map[string]any) []core.ObservedImage {
	var images []core.ObservedImage
	status := kubeMap(object["status"])
	for _, field := range []string{"initContainerStatuses", "containerStatuses"} {
		for _, item := range kubeList(status[field]) {
			container := kubeMap(item)
			image := kubeString(container["image"])
			imageID := kubeString(container["imageID"])
			if image == "" && imageID == "" {
				continue
			}
			images = append(images, core.NewObservedImage(target, "kubernetes", image, imageID, nil))
		}
	}
	return images
}

func uniqueObservedKubernetesImages(images []core.ObservedImage) []core.ObservedImage {
	seen := map[string]struct{}{}
	result := make([]core.ObservedImage, 0, len(images))
	for _, image := range images {
		key := image.Reference.Original + "\x00" + image.ImageID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, image)
	}
	return result
}

func kubeMap(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}

func kubeList(value any) []any {
	if typed, ok := value.([]any); ok {
		return typed
	}
	return nil
}

func kubeString(value any) string {
	if typed, ok := value.(string); ok {
		return strings.TrimSpace(typed)
	}
	return ""
}

func kubeStringSlice(value any) []string {
	values := kubeList(value)
	result := make([]string, 0, len(values))
	for _, item := range values {
		if value := kubeString(item); value != "" {
			result = append(result, value)
		}
	}
	return result
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
