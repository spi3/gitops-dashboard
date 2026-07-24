package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/dockerapi"
	"github.com/example/gitops-dashboard/internal/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var ErrAgentTargetUnauthorized = errors.New("agent target is not authorized for token")

const (
	dockerComposeProjectLabel       = core.DockerComposeProjectLabel
	dockerComposeServiceLabel       = core.DockerComposeServiceLabel
	defaultCheckRunTimeout          = 20 * time.Second
	checkRunDeadlineMargin          = 2 * time.Second
	statusWriteTimeout              = 30 * time.Second
	statusHistoryMaintenanceTimeout = 20 * time.Second
)

type dockerContainer = dockerapi.Container
type dockerImageInspector = dockerapi.ImageInspector

type Monitor struct {
	cfg    config.Config
	store  *storage.Store
	logger *slog.Logger

	pingCache *pingInventoryCache
	ping      pingFunc

	kubernetesCycleBudget kubernetesCycleBudgetFunc
	kubernetesClient      kubernetesClientFunc
}

func New(cfg config.Config, store *storage.Store, logger *slog.Logger) Monitor {
	if store != nil {
		if interval, err := cfg.DefaultInterval(); err == nil {
			for _, target := range cfg.Runtime.Docker {
				store.SetStatusTTL(target.Name, statusTTL(target.CheckInterval(interval)))
			}
			for _, target := range cfg.Runtime.Kubernetes {
				store.SetStatusTTL(target.Name, statusTTL(target.CheckInterval(interval)))
			}
			for _, target := range cfg.Runtime.HTTP {
				name := target.Name
				if name == "" {
					name = "routes"
				}
				store.SetStatusTTL(name, statusTTL(target.CheckInterval(interval)))
			}
			for _, target := range cfg.Runtime.Ping {
				store.SetStatusTTL(target.EffectiveName(), statusTTL(target.CheckInterval(interval)))
			}
		}
	}
	return Monitor{
		cfg:                   cfg,
		store:                 store,
		logger:                logger,
		pingCache:             newPingInventoryCache(),
		ping:                  systemPing,
		kubernetesCycleBudget: computeKubernetesCycleBudget,
		kubernetesClient:      productionKubernetesClient,
	}
}

func statusTTL(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return 2 * interval
}

func (monitor Monitor) Run(ctx context.Context) {
	defaultInterval, err := monitor.cfg.DefaultInterval()
	if err != nil {
		monitor.logger.Error("monitoring scheduler disabled", "error", err)
		return
	}
	go monitor.runStatusHistoryMaintenance(ctx, defaultInterval)
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

func (monitor Monitor) runStatusHistoryMaintenance(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	monitor.pruneStatusHistory(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			monitor.pruneStatusHistory(ctx)
		}
	}
}

func (monitor Monitor) pruneStatusHistory(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(ctx, statusHistoryMaintenanceTimeout)
	defer cancel()
	if err := monitor.store.PruneStatusHistory(checkCtx); err != nil {
		monitor.logger.Error("status history prune failed", "error", err)
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
			if !errors.Is(err, context.Canceled) {
				monitor.recordTargetFailure(ctx, target.Name, dockerServicesForTarget(services, target, directDockerTargets(monitor.cfg.Runtime.Docker)))
			}
			monitor.logger.Error("docker monitoring failed", "target", target.Name, "error", err)
			combined = err
		}
	}
	defaultInterval, _ := monitor.cfg.DefaultInterval()
	kubernetesServices := runtimeServices(services, "kubernetes")
	for _, target := range monitor.cfg.Runtime.Kubernetes {
		budget := monitor.kubernetesCycleBudget(kubernetesServices, target.CheckInterval(defaultInterval))
		err := monitor.runCheckWithTimeout(ctx, budget, func(checkCtx context.Context) error {
			return monitor.checkKubernetes(checkCtx, target, services)
		})
		if err != nil {
			monitor.handleKubernetesCheckFailure(ctx, target.Name, err, services)
			monitor.logger.Error("kubernetes monitoring failed", "target", target.Name, "error", err)
			combined = err
		}
	}
	for _, target := range monitor.cfg.Runtime.HTTP {
		err := monitor.runCheckWithTimeout(ctx, monitor.httpRouteRunTimeout(target, services), func(checkCtx context.Context) error {
			return monitor.checkHTTPRoutes(checkCtx, target, services)
		})
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				targetName := target.Name
				if targetName == "" {
					targetName = "routes"
				}
				monitor.recordTargetFailure(ctx, targetName, routeServices(services))
			}
			monitor.logger.Error("http route monitoring failed", "target", target.Name, "error", err)
			combined = err
		}
	}
	for _, target := range monitor.cfg.Runtime.Ping {
		if err := monitor.checkPing(ctx, target); err != nil {
			if !errors.Is(err, context.Canceled) {
				monitor.recordTargetFailure(ctx, target.EffectiveName(), pingServicesForTarget(services, target))
			}
			monitor.logger.Error("ping monitoring failed", "target", target.EffectiveName(), "error", err)
			combined = err
		}
	}
	if err := monitor.store.PruneStatusHistory(ctx); err != nil {
		if combined == nil {
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
	message = core.FilterAgentMessageDockerLabels(message)
	receivedAt := time.Now().UTC()
	message.CheckedAt = clampAgentCheckedAt(message.CheckedAt, receivedAt)
	if ttl := monitor.agentStatusTTL(target); ttl > 0 {
		message.StaleAfter = receivedAt.Add(ttl)
	}
	services, err := monitor.store.Services(ctx)
	if err != nil {
		return err
	}
	checkedAt := message.CheckedAt.UTC()
	containers := agentDockerContainers(message.Containers)
	statuses := make([]core.StatusResult, 0)
	for _, service := range services {
		if service.Runtime != "compose" || composeServiceTarget(service) != target {
			continue
		}
		health, statusMessage, observedImages := dockerStatus(ctx, service, target, containers, nil)
		statuses = append(statuses, core.StatusResult{
			ServiceID:      service.ID,
			Target:         target,
			Health:         health,
			Message:        statusMessage,
			CheckedAt:      checkedAt,
			ExpiresAt:      message.StaleAfter,
			ObservedImages: observedImages,
		})
	}
	return monitor.store.UpsertAgentReport(ctx, message, statuses, receivedAt)
}

const maxAgentClockSkew = 30 * time.Second

func clampAgentCheckedAt(reported, received time.Time) time.Time {
	if reported.IsZero() || reported.Before(received.Add(-maxAgentClockSkew)) || reported.After(received.Add(maxAgentClockSkew)) {
		return received
	}
	return reported.UTC()
}

func (monitor Monitor) agentStatusTTL(target string) time.Duration {
	defaultInterval, err := monitor.cfg.DefaultInterval()
	if err != nil {
		return 0
	}
	for _, candidate := range monitor.cfg.Runtime.Docker {
		if candidate.Kind == "agent" && strings.TrimSpace(candidate.Name) == target {
			return statusTTL(candidate.CheckInterval(defaultInterval))
		}
	}
	return 0
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
	monitor.runTargetLoop(ctx, target.Name, interval, nil, func(checkCtx context.Context, services []core.Service) error {
		return monitor.checkDocker(checkCtx, target, services)
	}, monitor.genericFailureHandler(target.Name, func(services []core.Service) []core.Service {
		return dockerServicesForTarget(services, target, directDockerTargets(monitor.cfg.Runtime.Docker))
	}))
}

func (monitor Monitor) runKubernetesLoop(ctx context.Context, target config.KubernetesTarget, interval time.Duration) {
	monitor.runTargetLoop(ctx, target.Name, interval, func(services []core.Service) time.Duration {
		return monitor.kubernetesCycleBudget(runtimeServices(services, "kubernetes"), interval)
	}, func(checkCtx context.Context, services []core.Service) error {
		return monitor.checkKubernetes(checkCtx, target, services)
	}, func(loopCtx context.Context, err error, services []core.Service) {
		monitor.handleKubernetesCheckFailure(loopCtx, target.Name, err, services)
	})
}

func (monitor Monitor) runHTTPRouteLoop(ctx context.Context, target config.HTTPRouteTarget, interval time.Duration) {
	targetName := target.Name
	if targetName == "" {
		targetName = "routes"
	}
	monitor.runTargetLoop(ctx, targetName, interval, func(services []core.Service) time.Duration {
		return monitor.httpRouteRunTimeout(target, services)
	}, func(checkCtx context.Context, services []core.Service) error {
		return monitor.checkHTTPRoutes(checkCtx, target, services)
	}, monitor.genericFailureHandler(targetName, routeServices))
}

func (monitor Monitor) runPingLoop(ctx context.Context, target config.PingTarget, interval time.Duration) {
	monitor.runTargetLoop(ctx, target.EffectiveName(), interval, nil, func(checkCtx context.Context, _ []core.Service) error {
		return monitor.checkPing(checkCtx, target)
	}, monitor.genericFailureHandler(target.EffectiveName(), func(services []core.Service) []core.Service {
		return pingServicesForTarget(services, target)
	}))
}

// genericFailureHandler is the shared runTargetLoop failure callback used by
// every runtime except Kubernetes: any error other than context.Canceled
// writes the generic target failure rows for the runtime's covered services.
// Kubernetes uses handleKubernetesCheckFailure instead, which additionally
// distinguishes an explicit phase/request deadline from the parent context's
// own cancellation or deadline (see kubernetes_bounds.go).
func (monitor Monitor) genericFailureHandler(targetName string, covered func([]core.Service) []core.Service) func(context.Context, error, []core.Service) {
	return func(loopCtx context.Context, err error, services []core.Service) {
		if errors.Is(err, context.Canceled) {
			return
		}
		servicesForFailure := services
		if covered != nil {
			servicesForFailure = covered(services)
		}
		monitor.recordTargetFailure(loopCtx, targetName, servicesForFailure)
	}
}

func (monitor Monitor) runTargetLoop(ctx context.Context, targetName string, interval time.Duration, checkTimeout func([]core.Service) time.Duration, check func(context.Context, []core.Service) error, onFailure func(context.Context, error, []core.Service)) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			services, err := monitor.store.Services(ctx)
			if err == nil {
				if checkTimeout != nil {
					timeout := checkTimeout(services)
					err = monitor.runCheckWithTimeout(ctx, timeout, func(checkCtx context.Context) error {
						return check(checkCtx, services)
					})
				} else {
					err = check(ctx, services)
				}
			}
			if pruneErr := monitor.store.PruneStatusHistory(ctx); pruneErr != nil {
				monitor.logger.Error("status history prune failed", "target", targetName, "error", pruneErr)
			}
			if err != nil {
				if onFailure != nil {
					onFailure(ctx, err, services)
				}
				failures++
				monitor.logger.Error("runtime monitoring failed", "target", targetName, "error", err, "failures", failures)
			} else {
				failures = 0
			}
			timer.Reset(nextInterval(interval, failures))
		}
	}
}

func (monitor Monitor) recordTargetFailure(ctx context.Context, target string, services []core.Service) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}
	now := time.Now().UTC()
	for _, service := range services {
		if err := monitor.upsertMonitorStatus(ctx, core.StatusResult{ServiceID: service.ID, Target: target, Health: core.HealthError, Message: "monitor target check failed", CheckedAt: now}); err != nil {
			monitor.logger.Error("persist monitor target failure", "target", target, "service", service.ID, "error", err)
		}
	}
}

func runtimeServices(services []core.Service, runtime string) []core.Service {
	result := make([]core.Service, 0, len(services))
	for _, service := range services {
		if service.Runtime == runtime {
			result = append(result, service)
		}
	}
	return result
}

func dockerServicesForTarget(services []core.Service, target config.DockerTarget, direct []config.DockerTarget) []core.Service {
	result := make([]core.Service, 0, len(services))
	for _, service := range services {
		if service.Runtime == "compose" && dockerServiceAppliesToTarget(service, target, direct) {
			result = append(result, service)
		}
	}
	return result
}

func routeServices(services []core.Service) []core.Service {
	result := make([]core.Service, 0, len(services))
	for _, service := range services {
		if len(httpRoutes(service.Exposure)) > 0 {
			result = append(result, service)
		}
	}
	return result
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

func (monitor Monitor) runCheckWithTimeout(ctx context.Context, timeout time.Duration, check func(context.Context) error) error {
	if timeout <= 0 {
		timeout = defaultCheckRunTimeout
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return check(checkCtx)
}

func (monitor Monitor) upsertMonitorStatus(ctx context.Context, status core.StatusResult) error {
	writeCtx, cancel, ok := statusWriteContext(ctx)
	if !ok {
		return nil
	}
	defer cancel()
	return monitor.store.UpsertStatus(writeCtx, status)
}

func statusWriteContext(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, nil, false
	}
	writeBase := ctx
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		writeBase = context.WithoutCancel(ctx)
	}
	writeCtx, cancel := context.WithTimeout(writeBase, statusWriteTimeout)
	return writeCtx, cancel, true
}

func timeoutWaves(checks, concurrency int) int {
	if checks < 1 {
		return 1
	}
	if concurrency < 1 {
		return checks
	}
	return (checks + concurrency - 1) / concurrency
}

func (monitor Monitor) checkDocker(ctx context.Context, target config.DockerTarget, services []core.Service) error {
	containers, err := dockerapi.ListContainers(ctx, target.Host)
	if err != nil {
		return err
	}
	containers = filterDockerContainerLabels(containers)
	imageInspector, err := dockerapi.NewImageInspector(target.Host)
	if err != nil {
		imageInspector = nil
	}
	directTargets := directDockerTargets(monitor.cfg.Runtime.Docker)
	if len(directTargets) == 0 {
		directTargets = []config.DockerTarget{target}
	}
	for _, service := range services {
		if service.Runtime != "compose" {
			continue
		}
		if !dockerServiceAppliesToTarget(service, target, directTargets) {
			if err := monitor.store.PruneStatusTargets(ctx, service.ID, target.Name, "", nil); err != nil {
				return err
			}
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

func directDockerTargets(targets []config.DockerTarget) []config.DockerTarget {
	direct := make([]config.DockerTarget, 0, len(targets))
	for _, target := range targets {
		if target.Kind == "agent" {
			continue
		}
		direct = append(direct, target)
	}
	return direct
}

func dockerServiceAppliesToTarget(service core.Service, target config.DockerTarget, directTargets []config.DockerTarget) bool {
	boundTarget := composeServiceTarget(service)
	if boundTarget != "" {
		return boundTarget == strings.TrimSpace(target.Name)
	}

	// Backward-compatible default: legacy scans without a docker_files/<target>
	// source are checked only when there is one direct Docker target to choose.
	if len(directTargets) != 1 {
		return false
	}
	return sameDockerTarget(directTargets[0], target)
}

func sameDockerTarget(left, right config.DockerTarget) bool {
	return strings.TrimSpace(left.Name) == strings.TrimSpace(right.Name) &&
		strings.TrimSpace(left.Kind) == strings.TrimSpace(right.Kind) &&
		strings.TrimSpace(left.Host) == strings.TrimSpace(right.Host)
}

func dockerHealth(service core.Service, containers []dockerContainer) (core.HealthState, string) {
	health, message, _ := dockerStatus(context.Background(), service, "", containers, nil)
	return health, message
}

func dockerStatus(ctx context.Context, service core.Service, target string, containers []dockerContainer, imageInspector *dockerImageInspector) (core.HealthState, string, []core.ObservedImage) {
	matches := matchingDockerContainers(service, containers)
	observedImages := observedDockerImages(ctx, target, matches, imageInspector)
	if len(matches) == 0 {
		return core.HealthUnknown, "container not found", nil
	}
	worst := core.HealthHealthy
	messages := make([]string, 0, len(matches))
	for _, container := range matches {
		lifecycle := strings.ToLower(strings.TrimSpace(container.State))
		if lifecycle != "running" {
			if lifecycle == "restarting" || lifecycle == "paused" {
				worst = worseDockerHealth(worst, core.HealthDegraded)
			} else {
				worst = worseDockerHealth(worst, core.HealthUnhealthy)
			}
			messages = append(messages, strings.TrimSpace(container.Status))
			continue
		}
		health := strings.ToLower(strings.TrimSpace(container.Health))
		if health == "" {
			health = containerHealthFromStatus(strings.ToLower(strings.TrimSpace(container.State)), strings.ToLower(strings.TrimSpace(container.Status)))
		}
		switch health {
		case "healthy", "none":
			// already the best state
		case "starting":
			worst = worseDockerHealth(worst, core.HealthDegraded)
		case "unhealthy":
			worst = worseDockerHealth(worst, core.HealthUnhealthy)
		default:
			if strings.EqualFold(container.State, "restarting") {
				worst = worseDockerHealth(worst, core.HealthDegraded)
			} else {
				worst = worseDockerHealth(worst, core.HealthUnhealthy)
			}
		}
		messages = append(messages, strings.TrimSpace(container.Status))
	}
	if len(matches) == 1 {
		return worst, messages[0], observedImages
	}
	return worst, fmt.Sprintf("%d replicas: %s", len(matches), strings.Join(messages, "; ")), observedImages
}

func worseDockerHealth(left, right core.HealthState) core.HealthState {
	if healthPriority(left) <= healthPriority(right) {
		return left
	}
	return right
}

func healthPriority(health core.HealthState) int {
	switch health {
	case core.HealthUnhealthy:
		return 0
	case core.HealthDegraded:
		return 1
	case core.HealthHealthy:
		return 2
	default:
		return -1
	}
}

func containerHealthFromStatus(state, status string) string {
	switch state {
	case "running":
		if health := parseStatusHealth(status); health != "" {
			return health
		}
		return "healthy"
	case "restarting", "paused":
		return "starting"
	case "exited":
		return "unhealthy"
	}
	return parseStatusHealth(status)
}

func parseStatusHealth(status string) string {
	switch {
	case strings.Contains(status, "(health: unhealthy)") || strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)") || strings.Contains(status, "(starting)"):
		return "starting"
	case strings.Contains(status, "(health: none)") || strings.Contains(status, "(health: no healthcheck)"):
		return "none"
	default:
		return ""
	}
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

func filterDockerContainerLabels(containers []dockerContainer) []dockerContainer {
	for i := range containers {
		containers[i].Labels = core.FilterDockerComposeLabels(containers[i].Labels)
	}
	return containers
}

func observedDockerImages(ctx context.Context, target string, containers []dockerContainer, imageInspector *dockerImageInspector) []core.ObservedImage {
	images := make([]core.ObservedImage, 0, len(containers))
	for _, container := range containers {
		if !dockerapi.LiveContainer(container.State, container.Status) {
			continue
		}
		repoDigests := container.RepoDigests
		if imageInspector != nil {
			repoDigests = imageInspector.RepoDigests(ctx, container)
		}
		images = append(images, core.NewObservedImage(target, "docker", container.Image, container.ImageID, repoDigests))
	}
	return images
}

func matchesContainer(service core.Service, container dockerContainer) bool {
	if _, ok := container.Labels[dockerComposeServiceLabel]; ok {
		return matchesComposeLabels(service, container.Labels)
	}
	expectedNames := map[string]struct{}{}
	if name := strings.TrimSpace(service.Name); name != "" {
		expectedNames[name] = struct{}{}
	}
	if name := strings.TrimSpace(service.ResourceName); name != "" {
		expectedNames[name] = struct{}{}
	}
	for _, name := range container.Names {
		containerName := dockerContainerName(name)
		for expectedName := range expectedNames {
			if containerName == expectedName || matchesGeneratedComposeName(containerName, expectedName) {
				return true
			}
		}
	}
	return false
}

func matchesComposeLabels(service core.Service, labels map[string]string) bool {
	serviceLabel := strings.TrimSpace(labels[dockerComposeServiceLabel])
	expectedService := strings.TrimSpace(service.ResourceName)
	if expectedService == "" {
		expectedService = strings.TrimSpace(service.Name)
	}
	if serviceLabel == "" || serviceLabel != expectedService {
		return false
	}
	projectLabel := strings.TrimSpace(labels[dockerComposeProjectLabel])
	expectedProject := composeProjectName(service)
	if projectLabel == "" || expectedProject == "" {
		return true
	}
	return projectLabel == expectedProject
}

func matchesGeneratedComposeName(containerName, serviceName string) bool {
	containerName = strings.TrimSpace(containerName)
	serviceName = strings.TrimSpace(serviceName)
	if containerName == "" || serviceName == "" {
		return false
	}
	for _, separator := range []string{"-", "_"} {
		suffixStart := strings.LastIndex(containerName, separator)
		if suffixStart <= 0 || suffixStart == len(containerName)-1 {
			continue
		}
		if !isPositiveInteger(containerName[suffixStart+len(separator):]) {
			continue
		}
		serviceEnd := suffixStart
		serviceStart := serviceEnd - len(serviceName)
		if serviceStart <= 0 {
			continue
		}
		if containerName[serviceStart:serviceEnd] != serviceName {
			continue
		}
		if containerName[serviceStart-len(separator):serviceStart] != separator {
			continue
		}
		return true
	}
	return false
}

func isPositiveInteger(value string) bool {
	if value == "" {
		return false
	}
	nonZero := false
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
		if char != '0' {
			nonZero = true
		}
	}
	return nonZero
}

func dockerContainerName(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "/")
}

func composeProjectName(service core.Service) string {
	return strings.TrimSpace(service.ComposeProject)
}

func agentDockerContainers(statuses []core.ContainerStatus) []dockerContainer {
	containers := make([]dockerContainer, 0, len(statuses))
	for _, status := range statuses {
		var names []string
		if status.Name != "" {
			names = []string{status.Name}
		}
		containers = append(containers, dockerContainer{
			ID:           status.ID,
			Names:        names,
			Image:        status.Image,
			ImageID:      status.ImageID,
			RepoDigests:  status.RepoDigests,
			Labels:       status.Labels,
			State:        status.State,
			Status:       status.Status,
			Health:       status.Health,
			RestartCount: status.RestartCount,
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

func (monitor Monitor) checkKubernetes(ctx context.Context, target config.KubernetesTarget, services []core.Service) error {
	clientset, err := monitor.kubernetesClient(target)
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
		status := core.StatusResult{
			ServiceID: service.ID,
			Target:    target.Name,
			CheckedAt: time.Now().UTC(),
		}
		gvr, ok := gvrForKind(service.Kind)
		if !ok {
			status.Health = core.HealthNotApplicable
			status.Message = unsupportedKubernetesKindMessage(service.Kind)
			if err := monitor.store.UpsertStatus(ctx, status); err != nil {
				return err
			}
			continue
		}
		namespace := service.Namespace
		if namespace == "" {
			namespace = "default"
		}
		resource, err := clientset.Resource(gvr).Namespace(namespace).Get(ctx, service.ResourceName, metav1.GetOptions{})
		if err != nil {
			// A phase deadline, a per-request deadline, or the parent
			// context ending (including mid-request) aborts the whole
			// target check immediately rather than degrading into an
			// ordinary per-service error or a write against a dead
			// context. The caller distinguishes these cases and persists
			// defined failure state through a separate bounded write phase
			// (see handleKubernetesCheckFailure).
			if isContextTerminal(err) {
				return err
			}
			status.Health = core.HealthError
			status.Message = err.Error()
		} else {
			status.Health, status.Message = kubeHealth(service.Kind, resource.Object)
			status.Message = fmt.Sprintf("%s/%s found: %s", service.Kind, service.ResourceName, status.Message)
			observedImages, imagesErr := observedKubernetesImages(ctx, clientset, target.Name, namespace, resource.Object)
			if imagesErr != nil {
				if isContextTerminal(imagesErr) {
					return imagesErr
				}
				status.Message = fmt.Sprintf("%s; image metadata unavailable: %v", status.Message, imagesErr)
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
	case "Job":
		return schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}, true
	case "CronJob":
		return schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "cronjobs"}, true
	default:
		return schema.GroupVersionResource{}, false
	}
}

func podGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Version: "v1", Resource: "pods"}
}

func unsupportedKubernetesKindMessage(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "Kubernetes live monitoring not supported for resources without a kind"
	}
	return fmt.Sprintf("Kubernetes live monitoring not supported for %s resources", kind)
}

func kubeHealth(kind string, object map[string]any) (core.HealthState, string) {
	switch kind {
	case "Deployment":
		return deploymentHealth(object)
	case "StatefulSet":
		return statefulSetHealth(object)
	case "DaemonSet":
		return daemonSetHealth(object)
	case "Job":
		return jobHealth(object)
	case "CronJob":
		return cronJobHealth(object)
	default:
		return core.HealthNotApplicable, unsupportedKubernetesKindMessage(kind)
	}
}

func deploymentHealth(object map[string]any) (core.HealthState, string) {
	status, _ := object["status"].(map[string]any)
	desired, ok := desiredReplicas(object, status)
	if !ok {
		return core.HealthUnknown, "Deployment desired replica count unavailable"
	}
	current := number(status["replicas"])
	ready := number(status["readyReplicas"])
	available := number(status["availableReplicas"])
	message := fmt.Sprintf("Deployment ready %.0f/%.0f replicas, available %.0f", ready, desired, available)
	if desired == 0 {
		if current == 0 {
			return core.HealthHealthy, message
		}
		return core.HealthDegraded, message
	}
	if ready >= desired || available >= desired {
		return core.HealthHealthy, message
	}
	if ready > 0 || available > 0 {
		return core.HealthDegraded, message
	}
	return core.HealthUnhealthy, message
}

func statefulSetHealth(object map[string]any) (core.HealthState, string) {
	status := kubeMap(object["status"])
	desired, ok := desiredReplicas(object, status)
	if !ok {
		return core.HealthUnknown, "StatefulSet desired replica count unavailable"
	}
	current := number(status["replicas"])
	ready := number(status["readyReplicas"])
	message := fmt.Sprintf("StatefulSet ready %.0f/%.0f replicas, current %.0f", ready, desired, current)
	if desired == 0 {
		if current == 0 {
			return core.HealthHealthy, message
		}
		return core.HealthDegraded, message
	}
	if ready >= desired {
		return core.HealthHealthy, message
	}
	if ready > 0 {
		return core.HealthDegraded, message
	}
	return core.HealthUnhealthy, message
}

func daemonSetHealth(object map[string]any) (core.HealthState, string) {
	status := kubeMap(object["status"])
	desired, hasDesired := numberField(status, "desiredNumberScheduled")
	if !hasDesired {
		return core.HealthUnknown, "DaemonSet desired scheduled count unavailable"
	}
	ready := number(status["numberReady"])
	available, hasAvailable := numberField(status, "numberAvailable")
	updated, hasUpdated := numberField(status, "updatedNumberScheduled")
	misscheduled := number(status["numberMisscheduled"])
	message := fmt.Sprintf("DaemonSet ready %.0f/%.0f scheduled", ready, desired)
	if hasAvailable {
		message += fmt.Sprintf(", available %.0f", available)
	}
	if hasUpdated {
		message += fmt.Sprintf(", updated %.0f", updated)
	}
	if misscheduled > 0 {
		message += fmt.Sprintf(", misscheduled %.0f", misscheduled)
	}
	if desired == 0 {
		if misscheduled > 0 {
			return core.HealthUnhealthy, message
		}
		return core.HealthHealthy, message
	}
	if ready >= desired {
		if misscheduled > 0 {
			return core.HealthDegraded, message
		}
		if hasAvailable && available < desired {
			return core.HealthDegraded, message
		}
		if hasUpdated && updated < desired {
			return core.HealthDegraded, message
		}
		return core.HealthHealthy, message
	}
	if ready > 0 || (hasAvailable && available > 0) || (hasUpdated && updated > 0) {
		return core.HealthDegraded, message
	}
	return core.HealthUnhealthy, message
}

func jobHealth(object map[string]any) (core.HealthState, string) {
	status := kubeMap(object["status"])
	spec := kubeMap(object["spec"])
	completions := number(spec["completions"])
	if completions == 0 {
		completions = 1
	}
	succeeded := number(status["succeeded"])
	failed := number(status["failed"])
	active := number(status["active"])
	message := fmt.Sprintf("Job succeeded %.0f/%.0f, failed %.0f, active %.0f", succeeded, completions, failed, active)
	if conditionTrue(status, "Complete") || succeeded >= completions {
		return core.HealthHealthy, message
	}
	if conditionTrue(status, "Failed") {
		return core.HealthUnhealthy, message
	}
	if active > 0 || succeeded > 0 {
		return core.HealthDegraded, message
	}
	if failed > 0 {
		return core.HealthDegraded, message
	}
	return core.HealthUnknown, message
}

func cronJobHealth(object map[string]any) (core.HealthState, string) {
	status := kubeMap(object["status"])
	spec := kubeMap(object["spec"])
	if suspended, ok := kubeBool(spec["suspend"]); ok && suspended {
		return core.HealthDegraded, "CronJob is suspended"
	}
	active := len(kubeList(status["active"]))
	lastSchedule, hasLastSchedule := kubeTime(status["lastScheduleTime"])
	lastSuccess, hasLastSuccess := kubeTime(status["lastSuccessfulTime"])
	if active > 0 {
		if hasLastSuccess {
			return core.HealthDegraded, fmt.Sprintf("CronJob has %d active job(s), last successful at %s", active, formatKubeTime(lastSuccess))
		}
		return core.HealthDegraded, fmt.Sprintf("CronJob has %d active job(s), no successful run recorded", active)
	}
	if hasLastSuccess && (!hasLastSchedule || !lastSuccess.Before(lastSchedule)) {
		return core.HealthHealthy, fmt.Sprintf("CronJob last successful at %s", formatKubeTime(lastSuccess))
	}
	if hasLastSchedule {
		if hasLastSuccess {
			return core.HealthUnhealthy, fmt.Sprintf("CronJob last scheduled at %s, last successful at %s", formatKubeTime(lastSchedule), formatKubeTime(lastSuccess))
		}
		return core.HealthUnhealthy, fmt.Sprintf("CronJob last scheduled at %s with no successful run recorded", formatKubeTime(lastSchedule))
	}
	return core.HealthUnknown, "CronJob has not scheduled a job yet"
}

func desiredReplicas(object map[string]any, status map[string]any) (float64, bool) {
	if spec := kubeMap(object["spec"]); len(spec) > 0 {
		if desired, ok := numberField(spec, "replicas"); ok {
			return desired, true
		}
	}
	return numberField(status, "replicas")
}

func conditionTrue(status map[string]any, conditionType string) bool {
	for _, item := range kubeList(status["conditions"]) {
		condition := kubeMap(item)
		if kubeString(condition["type"]) != conditionType {
			continue
		}
		value, ok := kubeBool(condition["status"])
		if ok && value {
			return true
		}
	}
	return false
}

func formatKubeTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}

func kubeTime(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC(), true
	case string:
		if strings.TrimSpace(typed) == "" {
			return time.Time{}, false
		}
		parsed, err := time.Parse(time.RFC3339, typed)
		if err != nil {
			return time.Time{}, false
		}
		return parsed.UTC(), true
	default:
		return time.Time{}, false
	}
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

func kubeBool(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true":
			return true, true
		case "false":
			return false, true
		default:
			return false, false
		}
	default:
		return false, false
	}
}

func numberField(values map[string]any, field string) (float64, bool) {
	if values == nil {
		return 0, false
	}
	value, ok := values[field]
	if !ok {
		return 0, false
	}
	return kubeNumber(value)
}

func number(value any) float64 {
	result, _ := kubeNumber(value)
	return result
}

func kubeNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case json.Number:
		result, err := typed.Float64()
		return result, err == nil
	default:
		return 0, false
	}
}
