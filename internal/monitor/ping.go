package monitor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/hostinventory"
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
	return combined
}

func (monitor Monitor) syncPingTarget(ctx context.Context, target config.PingTarget) ([]core.Service, error) {
	services, err := hostinventory.ServicesForTarget(target)
	if err != nil {
		return nil, err
	}
	if err := monitor.store.ReplaceConfiguredServices(ctx, hostinventory.RepositoryName(target), hostinventory.Source(target), services); err != nil {
		return nil, err
	}
	return services, nil
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
