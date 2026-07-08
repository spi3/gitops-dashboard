package monitor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
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
	results := make(chan core.StatusResult, len(services))
	var wg sync.WaitGroup
	sem := make(chan struct{}, httpRouteConcurrency)

	for _, service := range services {
		routes := httpRoutes(service.Exposure)
		keepTargets := []string(nil)
		if len(routes) > 0 {
			keepTargets = []string{targetName}
		}
		if err := monitor.store.PruneStatusTargets(ctx, service.ID, targetName, targetPrefix, keepTargets); err != nil {
			return err
		}
		if len(routes) == 0 {
			continue
		}
		wg.Add(1)
		go func(service core.Service, routes []string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			health, message := checkHTTPRouteCandidates(ctx, client, routes)
			results <- core.StatusResult{
				ServiceID: service.ID,
				Target:    targetName,
				Health:    health,
				Message:   message,
				CheckedAt: time.Now().UTC(),
			}
		}(service, routes)
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

func checkHTTPRouteCandidates(ctx context.Context, client *http.Client, routes []string) (core.HealthState, string) {
	if len(routes) == 0 {
		return core.HealthUnknown, ""
	}
	if len(routes) == 1 {
		return checkHTTPRoute(ctx, client, routes[0])
	}
	results := make([]routeCheckResult, 0, len(routes))
	healthyCount := 0
	allHealthy := true
	for _, route := range routes {
		health, message := checkHTTPRoute(ctx, client, route)
		results = append(results, routeCheckResult{health: health, message: message})
		if health == core.HealthHealthy {
			healthyCount++
			continue
		}
		allHealthy = false
	}
	if allHealthy {
		return core.HealthHealthy, routeChecksMessage(healthyCount, results, results[0])
	}
	worst := worstRouteResult(results)
	if healthyCount > 0 {
		return core.HealthDegraded, routeChecksMessage(healthyCount, results, worst)
	}
	return worst.health, routeChecksMessage(healthyCount, results, worst)
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

func httpRoutes(exposure []string) []string {
	var routes []string
	seen := map[string]bool{}
	for _, candidate := range exposure {
		route, ok := normalizeHTTPRoute(candidate)
		if !ok {
			continue
		}
		if seen[route] {
			continue
		}
		seen[route] = true
		routes = append(routes, route)
	}
	sort.SliceStable(routes, func(i, j int) bool {
		leftScore := routeScore(routes[i])
		rightScore := routeScore(routes[j])
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return preferRoute(routes[i], routes[j])
	})
	return routes
}

func normalizeHTTPRoute(candidate string) (string, bool) {
	value := strings.TrimSpace(candidate)
	if value == "" || strings.HasPrefix(value, "service/") {
		return "", false
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return "", false
		}
		if !isCheckableRouteHost(parsed.Hostname()) {
			return "", false
		}
		return parsed.String(), true
	}

	host := strings.TrimSuffix(value, "/")
	if !isCheckableRouteHost(hostOnly(host)) {
		return "", false
	}
	scheme := "https"
	if isLANOrIP(hostOnly(host)) {
		scheme = "http"
	}
	return scheme + "://" + host, true
}

func isCheckableRouteHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if strings.HasSuffix(host, ".svc") || strings.Contains(host, ".svc.") || strings.HasSuffix(host, ".cluster.local") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return strings.Contains(host, ".")
}

func isLANOrIP(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	return strings.HasSuffix(host, ".lan") || net.ParseIP(host) != nil
}

func hostOnly(host string) string {
	withoutPath := strings.SplitN(host, "/", 2)[0]
	if parsedHost, _, err := net.SplitHostPort(withoutPath); err == nil {
		return parsedHost
	}
	return withoutPath
}

func routeScore(route string) int {
	parsed, err := url.Parse(route)
	if err != nil {
		return 0
	}
	host := strings.ToLower(strings.Trim(parsed.Hostname(), "[]"))
	score := 0
	if net.ParseIP(host) == nil {
		score += 100
		if strings.HasSuffix(host, ".lan") {
			score += 30
		}
	}
	if parsed.Port() != "" {
		score += 20
	}
	if parsed.Path != "" && parsed.Path != "/" {
		score += 10
	}
	if parsed.Scheme == "https" {
		score += 5
	}
	return score
}

func preferRoute(candidate, current string) bool {
	if len(candidate) != len(current) {
		return len(candidate) < len(current)
	}
	return candidate < current
}

func routeResultPriority(health core.HealthState) int {
	switch health {
	case core.HealthError:
		return 0
	case core.HealthUnhealthy:
		return 1
	case core.HealthDegraded:
		return 2
	case core.HealthHealthy:
		return 3
	default:
		return 4
	}
}

type routeCheckResult struct {
	health  core.HealthState
	message string
}

func worstRouteResult(results []routeCheckResult) routeCheckResult {
	worst := results[0]
	for _, result := range results[1:] {
		if routeResultPriority(result.health) < routeResultPriority(worst.health) {
			worst = result
		}
	}
	return worst
}

func routeChecksMessage(healthyCount int, results []routeCheckResult, detail routeCheckResult) string {
	return fmt.Sprintf("%d/%d route checks passing; %s", healthyCount, len(results), detail.message)
}
