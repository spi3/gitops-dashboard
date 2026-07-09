package monitor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/hostinventory"
	"github.com/example/gitops-dashboard/internal/scanner"
	"github.com/example/gitops-dashboard/internal/storage"
)

const (
	defaultPingTimeout                  = 2 * time.Second
	defaultPingInventoryRefreshInterval = 5 * time.Minute
	pingConcurrency                     = 16
)

func PingConcurrencyLimit() int {
	return pingConcurrency
}

func DefaultPingTimeout() time.Duration {
	return defaultPingTimeout
}

type pingFunc func(context.Context, string, time.Duration) error

func (monitor Monitor) SyncPingTargets(ctx context.Context) error {
	var combined error
	for _, target := range monitor.cfg.Runtime.Ping {
		if _, err := monitor.syncPingTarget(ctx, target, true); err != nil {
			monitor.logger.Error("ping inventory sync failed", "target", target.EffectiveName(), "error", err)
			combined = err
		}
	}
	if combined == nil {
		if err := monitor.store.PruneRuntimeServices(ctx, "host", pingRuntimeSources(monitor.cfg.Runtime.Ping)); err != nil {
			monitor.logger.Error("ping inventory prune failed", "error", err)
			combined = err
		}
	}
	return combined
}

type pingInventoryCache struct {
	mu        sync.Mutex
	entries   map[string]pingInventoryEntry
	refreshes map[string]*pingInventoryRefresh
	now       func() time.Time
}

type pingInventoryEntry struct {
	services []core.Service
	commit   string
	syncedAt time.Time
}

type pingInventoryRefresh struct {
	done     chan struct{}
	services []core.Service
	err      error
}

func newPingInventoryCache() *pingInventoryCache {
	return &pingInventoryCache{
		entries:   map[string]pingInventoryEntry{},
		refreshes: map[string]*pingInventoryRefresh{},
		now:       time.Now,
	}
}

func (monitor Monitor) syncPingTarget(ctx context.Context, target config.PingTarget, force bool) ([]core.Service, error) {
	if monitor.pingCache == nil {
		services, _, err := monitor.refreshPingTarget(ctx, target)
		return services, err
	}
	key := pingInventoryCacheKey(target)
	refreshInterval := pingInventoryRefreshInterval(target)
	entry, ok, refresh, now := monitor.pingCache.snapshot(key)
	if refresh != nil {
		if !force && ok {
			return clonePingServices(entry.services), nil
		}
		return waitPingInventoryRefresh(ctx, refresh)
	}
	if !force && ok && now.Sub(entry.syncedAt) < refreshInterval {
		stale, err := monitor.pingInventoryCacheStale(ctx, target, entry)
		if err != nil {
			return nil, err
		}
		if !stale {
			return clonePingServices(entry.services), nil
		}
	}

	refresh, wait, staleServices, useStale := monitor.pingCache.startRefresh(key, entry, ok, force)
	if useStale {
		return staleServices, nil
	}
	if wait {
		return waitPingInventoryRefresh(ctx, refresh)
	}

	services, commit, err := monitor.refreshPingTarget(ctx, target)
	monitor.pingCache.finishRefresh(key, refresh, services, commit, err)
	if err != nil {
		return nil, err
	}
	return clonePingServices(services), nil
}

func (cache *pingInventoryCache) snapshot(key string) (pingInventoryEntry, bool, *pingInventoryRefresh, time.Time) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = map[string]pingInventoryEntry{}
	}
	if cache.refreshes == nil {
		cache.refreshes = map[string]*pingInventoryRefresh{}
	}
	now := time.Now()
	if cache.now != nil {
		now = cache.now()
	}
	entry, ok := cache.entries[key]
	entry.services = clonePingServices(entry.services)
	return entry, ok, cache.refreshes[key], now
}

func (cache *pingInventoryCache) startRefresh(key string, staleEntry pingInventoryEntry, hasStale bool, force bool) (*pingInventoryRefresh, bool, []core.Service, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = map[string]pingInventoryEntry{}
	}
	if cache.refreshes == nil {
		cache.refreshes = map[string]*pingInventoryRefresh{}
	}
	if refresh := cache.refreshes[key]; refresh != nil {
		if !force && hasStale {
			return nil, false, clonePingServices(staleEntry.services), true
		}
		return refresh, true, nil, false
	}
	refresh := &pingInventoryRefresh{done: make(chan struct{})}
	cache.refreshes[key] = refresh
	return refresh, false, nil, false
}

func (cache *pingInventoryCache) finishRefresh(key string, refresh *pingInventoryRefresh, services []core.Service, commit string, err error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := time.Now()
	if cache.now != nil {
		now = cache.now()
	}
	if err == nil {
		services = clonePingServices(services)
		cache.entries[key] = pingInventoryEntry{
			services: clonePingServices(services),
			commit:   commit,
			syncedAt: now,
		}
	}
	refresh.services = clonePingServices(services)
	refresh.err = err
	delete(cache.refreshes, key)
	close(refresh.done)
}

func waitPingInventoryRefresh(ctx context.Context, refresh *pingInventoryRefresh) ([]core.Service, error) {
	select {
	case <-refresh.done:
		if refresh.err != nil {
			return nil, refresh.err
		}
		return clonePingServices(refresh.services), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (monitor Monitor) pingInventoryCacheStale(ctx context.Context, target config.PingTarget, entry pingInventoryEntry) (bool, error) {
	if target.Repository == "" || target.AnsibleInventory == "" {
		return false, nil
	}
	commit, ok, err := monitor.store.RuntimeServiceSourceCommit(ctx, hostinventory.RepositoryName(target), hostinventory.Source(target), "host")
	if err != nil {
		return false, err
	}
	if !ok {
		// Empty inventories do not leave a service row to hold the source commit.
		return entry.commit == "" || len(entry.services) > 0, nil
	}
	return commit != entry.commit, nil
}

func (monitor Monitor) refreshPingTarget(ctx context.Context, target config.PingTarget) ([]core.Service, string, error) {
	inventoryPath, commit, err := monitor.resolvePingInventory(ctx, target)
	if err != nil {
		return nil, "", err
	}
	services, err := hostinventory.ServicesForTarget(target, inventoryPath, commit)
	if err != nil {
		return nil, "", err
	}
	if err := monitor.store.ReplaceRuntimeServices(ctx, hostinventory.RepositoryName(target), hostinventory.Source(target), "host", services); err != nil {
		return nil, "", err
	}
	return services, commit, nil
}

func pingInventoryCacheKey(target config.PingTarget) string {
	return strings.Join([]string{
		target.EffectiveName(),
		target.Host,
		target.Repository,
		target.AnsibleInventory,
		target.Environment,
	}, "\x00")
}

func pingInventoryRefreshInterval(target config.PingTarget) time.Duration {
	interval, err := target.MinRefreshDuration()
	if err != nil || interval == 0 {
		return defaultPingInventoryRefreshInterval
	}
	return interval
}

func clonePingServices(services []core.Service) []core.Service {
	cloned := make([]core.Service, len(services))
	for i, service := range services {
		cloned[i] = service
		cloned[i].Images = clonePingSlice(service.Images)
		cloned[i].DesiredImages = clonePingSlice(service.DesiredImages)
		cloned[i].Ports = clonePingSlice(service.Ports)
		cloned[i].Dependencies = clonePingSlice(service.Dependencies)
		cloned[i].Storage = clonePingSlice(service.Storage)
		cloned[i].Exposure = clonePingSlice(service.Exposure)
		cloned[i].MonitorRoutes = clonePingSlice(service.MonitorRoutes)
		cloned[i].ConfigRefs = clonePingSlice(service.ConfigRefs)
		cloned[i].Warnings = clonePingSlice(service.Warnings)
	}
	return cloned
}

func clonePingSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	cloned := make([]T, len(values))
	copy(cloned, values)
	return cloned
}

func pingRuntimeSources(targets []config.PingTarget) []storage.RuntimeServiceSource {
	sources := make([]storage.RuntimeServiceSource, 0, len(targets))
	for _, target := range targets {
		sources = append(sources, storage.RuntimeServiceSource{
			Repository: hostinventory.RepositoryName(target),
			SourcePath: hostinventory.Source(target),
		})
	}
	return sources
}

func (monitor Monitor) resolvePingInventory(ctx context.Context, target config.PingTarget) (string, string, error) {
	if target.AnsibleInventory == "" {
		return "", "", nil
	}
	if target.Repository == "" {
		return target.AnsibleInventory, "", nil
	}
	repo, ok := monitor.repository(target.Repository)
	if !ok {
		return "", "", fmt.Errorf("runtime.ping repository %q is not defined", target.Repository)
	}
	repoScanner := scanner.New(monitor.cfg, monitor.store, monitor.logger)
	repoPath, err := repoScanner.SyncRepo(ctx, repo)
	if err != nil {
		return "", "", err
	}
	commit, err := scanner.CurrentCommit(ctx, repoPath)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(repoPath, filepath.FromSlash(target.AnsibleInventory)), commit, nil
}

func (monitor Monitor) repository(name string) (config.RepositoryConfig, bool) {
	for _, repo := range monitor.cfg.Repositories {
		if repo.Name == name {
			return repo, true
		}
	}
	return config.RepositoryConfig{}, false
}

func (monitor Monitor) checkPing(ctx context.Context, target config.PingTarget) error {
	ping := monitor.ping
	if ping == nil {
		ping = systemPing
	}
	return monitor.checkPingWithPinger(ctx, target, ping)
}

func (monitor Monitor) checkPingWithPinger(ctx context.Context, target config.PingTarget, ping pingFunc) error {
	services, err := monitor.syncPingTarget(ctx, target, false)
	if err != nil {
		return err
	}
	return monitor.runCheckWithTimeout(ctx, monitor.pingRunTimeout(target, services), func(checkCtx context.Context) error {
		return monitor.checkPingServicesWithPinger(checkCtx, target, services, ping)
	})
}

func (monitor Monitor) checkPingServicesWithPinger(ctx context.Context, target config.PingTarget, services []core.Service, ping pingFunc) error {
	timeout, err := target.TimeoutDuration()
	if err != nil {
		return err
	}
	if timeout == 0 {
		timeout = defaultPingTimeout
	}

	targetName := target.EffectiveName()
	results := make(chan core.StatusResult, len(services))
	var wg sync.WaitGroup
	type pingCheck struct {
		service core.Service
		address string
	}
	jobs := make(chan pingCheck, len(services))

	for _, service := range services {
		address := strings.TrimSpace(service.ResourceName)
		if address == "" {
			address = service.Name
		}
		jobs <- pingCheck{service: service, address: address}
	}
	close(jobs)

	workers := min(pingConcurrency, len(services))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for check := range jobs {
				if err := ctx.Err(); err != nil {
					if errors.Is(err, context.DeadlineExceeded) {
						results <- timeoutPingResult(check.service.ID, targetName, check.address, time.Now().UTC())
					}
					continue
				}
				health, message := checkPingHost(ctx, ping, check.address, timeout)
				results <- core.StatusResult{
					ServiceID: check.service.ID,
					Target:    targetName,
					Health:    health,
					Message:   message,
					CheckedAt: time.Now().UTC(),
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for status := range results {
		if err := monitor.upsertMonitorStatus(ctx, status); err != nil {
			return err
		}
	}
	return nil
}

func (monitor Monitor) pingRunTimeout(target config.PingTarget, services []core.Service) time.Duration {
	timeout, err := target.TimeoutDuration()
	if err != nil || timeout == 0 {
		timeout = defaultPingTimeout
	}
	checks := pingCheckCount(target, services)
	if checks == 0 && (target.Host != "" || target.AnsibleInventory != "") {
		checks = 1
	}
	return time.Duration(timeoutWaves(checks, pingConcurrency))*timeout + checkRunDeadlineMargin
}

func pingCheckCount(target config.PingTarget, services []core.Service) int {
	repository := hostinventory.RepositoryName(target)
	source := hostinventory.Source(target)
	checks := 0
	for _, service := range services {
		if service.Repository == repository && service.Runtime == "host" && service.SourcePath == source {
			checks++
		}
	}
	if checks == 0 && target.Host != "" {
		return 1
	}
	return checks
}

func checkPingHost(ctx context.Context, ping pingFunc, address string, timeout time.Duration) (core.HealthState, string) {
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ping(pingCtx, address, timeout); err != nil {
		if errors.Is(pingCtx.Err(), context.DeadlineExceeded) {
			return core.HealthError, fmt.Sprintf("ping %s timed out", address)
		}
		health := core.HealthUnhealthy
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			health = core.HealthError
		}
		return health, fmt.Sprintf("ping %s failed: %s", address, err)
	}
	return core.HealthHealthy, fmt.Sprintf("ping %s succeeded", address)
}

func timeoutPingResult(serviceID, targetName, address string, checkedAt time.Time) core.StatusResult {
	message := fmt.Sprintf("ping %s timed out before check completed", address)
	return core.StatusResult{
		ServiceID: serviceID,
		Target:    targetName,
		Health:    core.HealthError,
		Message:   message,
		CheckedAt: checkedAt,
	}
}

func systemPing(ctx context.Context, address string, timeout time.Duration) error {
	seconds := int((timeout + time.Second - time.Nanosecond) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	cmd := exec.CommandContext(ctx, "ping", "-n", "-c", "1", "-W", strconv.Itoa(seconds), address)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := oneLineOutput(output)
		if message != "" {
			return fmt.Errorf("%w: %s", err, message)
		}
		return err
	}
	return nil
}

func oneLineOutput(output []byte) string {
	message := strings.TrimSpace(string(output))
	message = strings.ReplaceAll(message, "\n", " ")
	if len(message) > 300 {
		message = message[:300]
	}
	return message
}
