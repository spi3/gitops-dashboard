package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/routetarget"
)

const (
	defaultHTTPRouteTimeout = 5 * time.Second
	httpRouteConcurrency    = 8
)

func (monitor Monitor) checkHTTPRoutes(ctx context.Context, target config.HTTPRouteTarget, services []core.Service) error {
	timeout, err := target.TimeoutDuration()
	if err != nil {
		return err
	}
	if timeout == 0 {
		timeout = defaultHTTPRouteTimeout
	}
	client := &http.Client{Timeout: timeout}
	return monitor.checkHTTPRoutesWithClient(ctx, target, services, client)
}

func (monitor Monitor) checkHTTPRoutesWithClient(ctx context.Context, target config.HTTPRouteTarget, services []core.Service, client *http.Client) error {
	targetName := target.Name
	if targetName == "" {
		targetName = "routes"
	}

	targetPrefix := targetName + ": "
	checks := []httpRouteCheck{}
	overriddenStatuses := []core.StatusResult{}
	for _, service := range services {
		routes := httpRoutes(service.Exposure)
		parentNotApplicable, err := monitor.store.MonitorNotApplicable(ctx, service.ID, targetName)
		if err != nil {
			return err
		}
		if parentNotApplicable {
			if err := monitor.store.PruneStatusTargets(ctx, service.ID, "", targetPrefix, nil); err != nil {
				return err
			}
			overriddenStatuses = append(overriddenStatuses, core.StatusResult{
				ServiceID: service.ID,
				Target:    targetName,
				Health:    core.HealthNotApplicable,
				Message:   "not applicable",
				CheckedAt: time.Now().UTC(),
			})
			continue
		}

		keepTargets := routeStatusTargets(targetPrefix, routes)
		if err := monitor.store.PruneStatusTargets(ctx, service.ID, targetName, targetPrefix, keepTargets); err != nil {
			return err
		}
		for _, route := range routes {
			statusTarget := targetPrefix + route
			routeNotApplicable, err := monitor.store.MonitorNotApplicable(ctx, service.ID, statusTarget)
			if err != nil {
				return err
			}
			if routeNotApplicable {
				overriddenStatuses = append(overriddenStatuses, core.StatusResult{
					ServiceID: service.ID,
					Target:    statusTarget,
					Health:    core.HealthNotApplicable,
					Message:   "not applicable",
					CheckedAt: time.Now().UTC(),
				})
				continue
			}
			checks = append(checks, httpRouteCheck{
				serviceID: service.ID,
				target:    statusTarget,
				route:     route,
			})
		}
	}

	for _, status := range overriddenStatuses {
		if err := monitor.store.UpsertStatus(ctx, status); err != nil {
			return err
		}
	}

	results := make(chan core.StatusResult, len(checks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, httpRouteConcurrency)

	for _, check := range checks {
		wg.Add(1)
		go func(check httpRouteCheck) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			health, message := checkHTTPRoute(ctx, client, check.route)
			results <- core.StatusResult{
				ServiceID: check.serviceID,
				Target:    check.target,
				Health:    health,
				Message:   message,
				CheckedAt: time.Now().UTC(),
			}
		}(check)
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

func checkHTTPRoute(ctx context.Context, client *http.Client, route string) (core.HealthState, string) {
	statusCode, status, method, err := doRouteRequest(ctx, client, http.MethodHead, route)
	if err == nil && statusCode == http.StatusMethodNotAllowed {
		statusCode, status, method, err = doRouteRequest(ctx, client, http.MethodGet, route)
	}
	if err != nil {
		return core.HealthError, fmt.Sprintf("%s failed: %s", route, err)
	}
	message := fmt.Sprintf("%s %s -> %s", method, route, status)
	switch {
	case statusCode >= 200 && statusCode < 400:
		return core.HealthHealthy, message
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return core.HealthHealthy, message
	case statusCode == http.StatusNotFound || statusCode == http.StatusGone:
		return core.HealthUnhealthy, message
	case statusCode >= 400 && statusCode < 500:
		return core.HealthDegraded, message
	default:
		return core.HealthUnhealthy, message
	}
}

func doRouteRequest(ctx context.Context, client *http.Client, method, route string) (int, string, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, route, nil)
	if err != nil {
		return 0, "", method, err
	}
	req.Header.Set("User-Agent", "gitops-dashboard/route-check")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", method, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, resp.Status, method, nil
}

func firstHTTPRoute(exposure []string) (string, bool) {
	routes := httpRoutes(exposure)
	if len(routes) == 0 {
		return "", false
	}
	return routes[0], true
}

type httpRouteCheck struct {
	serviceID string
	target    string
	route     string
}

func routeStatusTargets(prefix string, routes []string) []string {
	targets := make([]string, 0, len(routes))
	for _, route := range routes {
		targets = append(targets, prefix+route)
	}
	return targets
}

func httpRoutes(exposure []string) []string {
	return routetarget.Routes(exposure)
}

func normalizeHTTPRoute(candidate string) (string, bool) {
	return routetarget.Normalize(candidate)
}
