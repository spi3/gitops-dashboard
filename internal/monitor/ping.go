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
	defaultPingTimeout = 2 * time.Second
	pingConcurrency    = 16
)

type pingFunc func(context.Context, string, time.Duration) error

func (monitor Monitor) SyncPingTargets(ctx context.Context) error {
	var combined error
	for _, target := range monitor.cfg.Runtime.Ping {
		if _, err := monitor.syncPingTarget(ctx, target); err != nil {
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

func (monitor Monitor) syncPingTarget(ctx context.Context, target config.PingTarget) ([]core.Service, error) {
	inventoryPath, commit, err := monitor.resolvePingInventory(ctx, target)
	if err != nil {
		return nil, err
	}
	services, err := hostinventory.ServicesForTarget(target, inventoryPath, commit)
	if err != nil {
		return nil, err
	}
	if err := monitor.store.ReplaceRuntimeServices(ctx, hostinventory.RepositoryName(target), hostinventory.Source(target), "host", services); err != nil {
		return nil, err
	}
	return services, nil
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
	return monitor.checkPingWithPinger(ctx, target, systemPing)
}

func (monitor Monitor) checkPingWithPinger(ctx context.Context, target config.PingTarget, ping pingFunc) error {
	services, err := monitor.syncPingTarget(ctx, target)
	if err != nil {
		return err
	}
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
	sem := make(chan struct{}, pingConcurrency)

	for _, service := range services {
		service := service
		address := strings.TrimSpace(service.ResourceName)
		if address == "" {
			address = service.Name
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			health, message := checkPingHost(ctx, ping, address, timeout)
			results <- core.StatusResult{
				ServiceID: service.ID,
				Target:    targetName,
				Health:    health,
				Message:   message,
				CheckedAt: time.Now().UTC(),
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for status := range results {
		if err := monitor.store.UpsertStatus(ctx, status); err != nil {
			return err
		}
	}
	return nil
}

func checkPingHost(ctx context.Context, ping pingFunc, address string, timeout time.Duration) (core.HealthState, string) {
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ping(pingCtx, address, timeout); err != nil {
		health := core.HealthUnhealthy
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			health = core.HealthError
		}
		return health, fmt.Sprintf("ping %s failed: %s", address, err)
	}
	return core.HealthHealthy, fmt.Sprintf("ping %s succeeded", address)
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
