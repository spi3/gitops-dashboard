package scanner

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/environment"
	"github.com/example/gitops-dashboard/internal/hostinventory"
	"github.com/example/gitops-dashboard/internal/parser"
	"github.com/example/gitops-dashboard/internal/routetarget"
	"github.com/example/gitops-dashboard/internal/sanitizer"
	"github.com/example/gitops-dashboard/internal/storage"
)

type Scanner struct {
	cfg    config.Config
	store  *storage.Store
	logger *slog.Logger
}

const (
	GitCommandTimeout = 2 * time.Minute

	// repoSyncGitCommands bounds a single repository synchronization attempt
	// (syncRepoUnshared, for a repository whose cache already exists) to at
	// most 7 network- or filesystem-capable Git subprocess invocations:
	// origin enumeration (`config --get-all`), origin reconciliation
	// (`config --unset-all` plus `config --add`), fetch, checkout,
	// fast-forward pull, and HEAD resolution. Each is individually bounded
	// by GitCommandTimeout; this constant sizes the outer
	// repoSyncOperationTimeout so a legitimate full sequence never races
	// that per-command budget.
	repoSyncGitCommands      = 7
	repoSyncOperationTimeout = time.Duration(repoSyncGitCommands)*GitCommandTimeout + 30*time.Second
)

func New(cfg config.Config, store *storage.Store, logger *slog.Logger) Scanner {
	return Scanner{cfg: cfg, store: store, logger: logger}
}

func (scanner Scanner) RunScheduled(ctx context.Context) {
	if len(scanner.cfg.Repositories) == 0 {
		return
	}
	if err := scanner.store.EnsureRepositories(ctx, scanner.cfg.Repositories); err != nil {
		scanner.logger.Error("scheduled scans disabled", "error", err)
		return
	}
	for _, repo := range scanner.cfg.Repositories {
		interval, err := repo.ScanDuration()
		if err != nil {
			scanner.logger.Error("scheduled scan disabled", "repository", repo.Name, "error", err)
			continue
		}
		if interval == 0 {
			continue
		}
		go scanner.runRepoLoop(ctx, repo, interval)
	}
}

func (scanner Scanner) ScanAll(ctx context.Context) error {
	if err := scanner.store.EnsureRepositories(ctx, scanner.cfg.Repositories); err != nil {
		return err
	}
	var combined error
	for _, repo := range scanner.cfg.Repositories {
		if err := scanner.scanOne(ctx, repo); err != nil {
			scanner.logger.Error("repository scan failed", "repository", repo.Name, "error", err)
			combined = err
		}
	}
	return combined
}

func (scanner Scanner) runRepoLoop(ctx context.Context, repo config.RepositoryConfig, interval time.Duration) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := scanner.scanOne(ctx, repo); err != nil {
				scanner.logger.Error("scheduled repository scan failed", "repository", repo.Name, "error", err)
			}
			timer.Reset(interval)
		}
	}
}

func (scanner Scanner) scanOne(ctx context.Context, repo config.RepositoryConfig) error {
	// repoOperationKey is a non-I/O lexical key: a per-repository scan row
	// (below, inside scanOneUnshared) must exist before cache-root
	// resolution can fail, so key derivation itself must never touch the
	// filesystem.
	_, err := repoScanFlights.do(ctx, scanner.repoOperationKey(repo), func() (any, error) {
		return nil, scanner.scanOneUnshared(ctx, repo)
	})
	return err
}

func (scanner Scanner) scanOneUnshared(ctx context.Context, repo config.RepositoryConfig) error {
	scanID, err := scanner.store.StartScan(ctx, repo.Name)
	if err != nil {
		return err
	}
	var result syncResult
	var services []core.Service
	scanErr := func() error {
		var err error
		result, err = scanner.syncRepo(ctx, repo)
		if err != nil {
			return err
		}
		services, err = scanner.parseRepo(result.path, repo, result.commit)
		return err
	}()
	var replacements []storage.RouteTargetReplacement
	var exclusions []storage.RouteTargetExclusion
	if scanErr == nil {
		replacementCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		previous, err := scanner.store.Services(replacementCtx)
		if err != nil {
			scanErr = err
		} else {
			unresolved, err := scanner.store.RouteTargetExclusions(replacementCtx, repo.Name)
			if err != nil {
				scanErr = err
			} else {
				replacements, exclusions = routeTargetReplacements(previous, services, repo.Name, unresolved)
			}
			if len(exclusions) > 0 {
				scanner.logger.Warn("route identity replacement ambiguous; retaining old target state", "repository", repo.Name, "count", len(exclusions))
			}
		}
		cancel()
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// A failed attempt is never authoritative for the repository's last
	// known commit: it persists whatever commit previously succeeded (or
	// stays empty when none ever did), rather than this attempt's own
	// partial or empty result.
	commit := result.commit
	if scanErr != nil {
		prior, priorErr := scanner.priorSuccessfulCommit(finishCtx, repo.Name)
		if priorErr != nil {
			scanner.logger.Warn("could not resolve prior commit for failed scan", "repository", repo.Name, "error", priorErr)
		} else {
			commit = prior
		}
	}
	if err := scanner.store.FinishScanWithRouteTargetChanges(finishCtx, scanID, repo.Name, commit, services, scanErr, replacements, exclusions, scanner.cfg.Runtime.HTTP); err != nil {
		return err
	}
	return scanErr
}

// priorSuccessfulCommit returns the repository's last known commit as
// recorded by its most recent finished scan (empty if none has ever
// succeeded). It is read before FinishScanWithRouteTargetChanges overwrites
// it, so a failed attempt can pass the same value back in and leave it
// truthfully unchanged.
func (scanner Scanner) priorSuccessfulCommit(ctx context.Context, repoName string) (string, error) {
	repos, err := scanner.store.Repositories(ctx)
	if err != nil {
		return "", err
	}
	for _, repo := range repos {
		if repo.Name == repoName {
			return repo.LastCommit, nil
		}
	}
	return "", nil
}

// routeTargetReplacements derives only provenance-backed, bijective replacements
// from route set differences. The shared canonicalizer makes normalization-only
// changes disappear; all remaining candidates must agree on every URL component
// except the identity-changing port.
func routeTargetReplacements(previous, current []core.Service, repository string, unresolved []storage.RouteTargetExclusion) ([]storage.RouteTargetReplacement, []storage.RouteTargetExclusion) {
	oldByID := make(map[string]core.Service)
	for _, service := range previous {
		if service.Repository == repository {
			oldByID[service.ID] = service
		}
	}
	replacements := []storage.RouteTargetReplacement{}
	exclusions := []storage.RouteTargetExclusion{}
	for _, service := range current {
		oldService, ok := oldByID[service.ID]
		if !ok {
			continue
		}
		oldRoutes := routetarget.Routes(oldService.Exposure)
		newRoutes := routetarget.Routes(service.Exposure)
		oldOnly, newOnly := routeDifference(oldRoutes, newRoutes), routeDifference(newRoutes, oldRoutes)
		newOwners := map[string]int{}
		candidates := make(map[string][]string, len(oldOnly))
		for _, oldRoute := range oldOnly {
			for _, newRoute := range newOnly {
				if replacementInvariantMatch(oldRoute, newRoute) {
					candidates[oldRoute] = append(candidates[oldRoute], newRoute)
					newOwners[newRoute]++
				}
			}
		}
		for _, oldRoute := range oldOnly {
			matches := candidates[oldRoute]
			if len(matches) == 1 && newOwners[matches[0]] == 1 {
				replacements = append(replacements, storage.RouteTargetReplacement{ServiceID: service.ID, OldRoute: oldRoute, NewRoute: matches[0]})
				continue
			}
			if len(matches) > 0 {
				exclusions = append(exclusions, storage.RouteTargetExclusion{ServiceID: service.ID, OldRoute: oldRoute})
			}
		}
	}

	// An exclusion is not a permanent tombstone. Later scans must reconsider
	// the old identity against the complete current route set, rather than only
	// that scan's set difference: A->{B,C} followed by {B} proves A->B.
	currentByID := make(map[string]core.Service)
	for _, service := range current {
		if service.Repository == repository {
			currentByID[service.ID] = service
		}
	}
	priorCandidates := make(map[storage.RouteTargetExclusion][]string, len(unresolved))
	priorOwners := map[string]int{}
	for _, exclusion := range unresolved {
		service, ok := currentByID[exclusion.ServiceID]
		if !ok || routeContains(routetarget.Routes(service.Exposure), exclusion.OldRoute) {
			continue
		}
		for _, route := range routetarget.Routes(service.Exposure) {
			if replacementInvariantMatch(exclusion.OldRoute, route) {
				priorCandidates[exclusion] = append(priorCandidates[exclusion], route)
				priorOwners[exclusion.ServiceID+"\x00"+route]++
			}
		}
	}
	for _, exclusion := range unresolved {
		matches := priorCandidates[exclusion]
		if len(matches) == 1 && priorOwners[exclusion.ServiceID+"\x00"+matches[0]] == 1 {
			replacements = append(replacements, storage.RouteTargetReplacement{ServiceID: exclusion.ServiceID, OldRoute: exclusion.OldRoute, NewRoute: matches[0]})
			continue
		}
		if len(matches) > 1 || (len(matches) == 1 && priorOwners[exclusion.ServiceID+"\x00"+matches[0]] > 1) {
			exclusions = append(exclusions, exclusion)
		}
	}
	return replacements, exclusions
}

func routeContains(routes []string, route string) bool {
	for _, candidate := range routes {
		if candidate == route {
			return true
		}
	}
	return false
}

func routeDifference(left, right []string) []string {
	rightSet := make(map[string]struct{}, len(right))
	for _, route := range right {
		rightSet[route] = struct{}{}
	}
	difference := make([]string, 0, len(left))
	for _, route := range left {
		if _, ok := rightSet[route]; !ok {
			difference = append(difference, route)
		}
	}
	return difference
}

func replacementInvariantMatch(oldRoute, newRoute string) bool {
	oldURL, oldErr := url.Parse(oldRoute)
	newURL, newErr := url.Parse(newRoute)
	if oldErr != nil || newErr != nil {
		return false
	}
	return oldURL.Scheme == newURL.Scheme && oldURL.Hostname() == newURL.Hostname() &&
		oldURL.EscapedPath() == newURL.EscapedPath() && oldURL.RawQuery == newURL.RawQuery && oldURL.Fragment == newURL.Fragment && oldURL.Port() != newURL.Port()
}

// syncResult is what a repository synchronization attempt produces: the
// resolved on-disk repository path and the commit it landed on.
type syncResult struct {
	path   string
	commit string
}

func (scanner Scanner) SyncRepo(ctx context.Context, repo config.RepositoryConfig) (string, error) {
	result, err := scanner.syncRepo(ctx, repo)
	if err != nil {
		return "", err
	}
	return result.path, nil
}

func CurrentCommit(ctx context.Context, repoPath string) (string, error) {
	commit, err := gitOutput(ctx, repoPath, sanitizer.Redactor{}, nil, "rev-parse", "HEAD")
	return strings.TrimSpace(commit), err
}

func (scanner Scanner) syncRepo(ctx context.Context, repo config.RepositoryConfig) (syncResult, error) {
	// This group coalesces the underlying Git work itself (distinct from
	// repoScanFlights, which coalesces the scan row/attempt); see
	// repoOperationKey and scanOne.
	value, err := repoSyncFlights.doDetached(ctx, scanner.repoOperationKey(repo), func() (any, error) {
		// Keep the shared git operation alive when a monitor-scoped caller times out;
		// each caller still bounds its own wait through doDetached.
		syncCtx, cancel := context.WithTimeout(context.Background(), repoSyncOperationTimeout)
		defer cancel()
		return scanner.syncRepoUnshared(syncCtx, repo)
	})
	if err != nil {
		return syncResult{}, err
	}
	return value.(syncResult), nil
}

func (scanner Scanner) syncRepoUnshared(ctx context.Context, repo config.RepositoryConfig) (syncResult, error) {
	resolvedRoot, err := scanner.resolveRepoCacheRoot()
	if err != nil {
		return syncResult{}, err
	}
	repoPath, pathExists, err := resolveContainedRepoPath(resolvedRoot, repo)
	if err != nil {
		return syncResult{}, err
	}
	// cacheExists tracks whether repoPath holds a usable Git working copy,
	// not merely whether something exists there: a leftover, non-git
	// directory (e.g. from a prior interrupted clone) still routes through
	// clone below, exactly as an absent path does.
	cacheExists := false
	if pathExists {
		if _, statErr := os.Stat(filepath.Join(repoPath, ".git")); statErr == nil {
			cacheExists = true
		}
	}

	// Register configured URL-userinfo redaction values immediately, before
	// any scrub mutation can fail, so a scrub error can never itself leak a
	// credential embedded in the configured URL.
	redactionValues := sanitizer.URLUserinfoValues(repo.URL)
	if scanner.store != nil {
		scanner.store.AddRedactionValues(redactionValues...)
	}
	redactor := sanitizer.New(redactionValues...)
	env := gitEnv(repo)

	if cacheExists {
		// Scrub any credential-bearing cached origin URLs before resolving
		// the repository's own credential source, validating its transport,
		// reconciling the origin, or performing any network-capable Git
		// command. A scrub failure fails the attempt with no network access.
		if err := scrubCachedOriginCredentials(ctx, repoPath, redactor, env); err != nil {
			return syncResult{}, err
		}
	}

	auth, err := scanner.gitAuth(repo)
	if err != nil {
		return syncResult{}, err
	}

	if cacheExists {
		if err := reconcileOrigin(ctx, repoPath, auth.redactor, auth.env, auth.remoteURL); err != nil {
			return syncResult{}, err
		}
		if _, err := gitOutput(ctx, repoPath, auth.redactor, auth.env, "fetch", "--all", "--prune"); err != nil {
			return syncResult{}, err
		}
		if repo.DefaultRef != "HEAD" {
			if _, err := gitOutput(ctx, repoPath, auth.redactor, auth.env, "checkout", repo.DefaultRef); err != nil {
				return syncResult{}, err
			}
		}
		if _, err := gitOutput(ctx, repoPath, auth.redactor, auth.env, "pull", "--ff-only"); err != nil {
			return syncResult{}, err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(repoPath), 0o700); err != nil {
			return syncResult{}, err
		}
		args := []string{"clone"}
		if repo.DefaultRef != "" && repo.DefaultRef != "HEAD" {
			args = append(args, "--branch", repo.DefaultRef)
		}
		args = append(args, auth.remoteURL, repoPath)
		if _, err := gitOutput(ctx, resolvedRoot, auth.redactor, auth.env, args...); err != nil {
			return syncResult{}, err
		}
	}

	commit, err := gitOutput(ctx, repoPath, auth.redactor, auth.env, "rev-parse", "HEAD")
	if err != nil {
		return syncResult{}, err
	}
	return syncResult{path: repoPath, commit: strings.TrimSpace(commit)}, nil
}

// resolveRepoCacheRoot resolves the owned repository cache root: it ensures
// the configured directory exists, then resolves it with filepath.Abs and
// filepath.EvalSymlinks so every downstream containment check compares
// against the same real, symlink-free path.
func (scanner Scanner) resolveRepoCacheRoot() (string, error) {
	abs, err := filepath.Abs(scanner.cfg.Server.RepoCacheDir)
	if err != nil {
		return "", fmt.Errorf("resolve repository cache: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("create repository cache: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve repository cache: %w", err)
	}
	return resolved, nil
}

type gitAuth struct {
	remoteURL    string
	env          []string
	redactor     sanitizer.Redactor
	useTokenAuth bool
}

// gitAuth resolves repository credential authentication. It must only be
// called after any existing repository cache has been scrubbed of stale
// credentials: it performs repository credential-source and URL-transport
// validation (embedded HTTP(S) userinfo, token-auth transport, unset
// tokenEnv) and only then reads the repository's token, so that an unset
// tokenEnv, an unreadable tokenFile, embedded userinfo, or an invalid
// transport surfaces as a repository scan error rather than as a startup
// failure that could run before cache cleanup.
func (scanner Scanner) gitAuth(repo config.RepositoryConfig) (gitAuth, error) {
	clean, stripped, err := credentialFreeRemoteURL(repo.URL)
	if err != nil {
		return gitAuth{}, fmt.Errorf("repository %s has an invalid remote url", repo.Name)
	}
	if stripped {
		return gitAuth{}, fmt.Errorf("repository %s remote url must not include embedded credentials", repo.Name)
	}
	hasTokenSource := repo.TokenEnv != "" || repo.TokenFile != ""
	if hasTokenSource {
		parsed, parseErr := url.Parse(repo.URL)
		if parseErr != nil || !strings.EqualFold(parsed.Scheme, "https") {
			return gitAuth{}, fmt.Errorf("repository %s token authentication requires an https remote url", repo.Name)
		}
	}
	if repo.TokenEnv != "" && strings.TrimSpace(os.Getenv(repo.TokenEnv)) == "" {
		return gitAuth{}, fmt.Errorf("repository %s references unset token env %s", repo.Name, repo.TokenEnv)
	}
	token, err := repo.Token()
	if err != nil {
		return gitAuth{}, err
	}
	env := gitEnv(repo)
	redactionValues := sanitizer.URLUserinfoValues(repo.URL)
	if token == "" {
		if scanner.store != nil {
			scanner.store.AddRedactionValues(redactionValues...)
		}
		return gitAuth{
			remoteURL: clean,
			env:       env,
			redactor:  sanitizer.New(redactionValues...),
		}, nil
	}
	basicCredential := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	authHeader := "Authorization: Basic " + basicCredential
	redactionValues = append(redactionValues, token, basicCredential, authHeader)
	if scanner.store != nil {
		scanner.store.AddRedactionValues(redactionValues...)
	}
	env = append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http."+clean+".extraHeader",
		"GIT_CONFIG_VALUE_0="+authHeader,
	)
	return gitAuth{
		remoteURL:    clean,
		env:          env,
		redactor:     sanitizer.New(redactionValues...),
		useTokenAuth: true,
	}, nil
}

func (scanner Scanner) parseRepo(repoPath string, repo config.RepositoryConfig, commit string) ([]core.Service, error) {
	var services []core.Service
	var kubeResources []parser.KubernetesResource
	var traefikRoutes []parser.TraefikRoute
	var traefikTCPRoutes []parser.TraefikTCPRoute
	var parseErrors []string
	pathFilter := newRepoPathFilter(repo)
	err := filepath.WalkDir(repoPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			parseErrors = append(parseErrors, walkErr.Error())
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			rel, err := filepath.Rel(repoPath, path)
			if err == nil && pathFilter.shouldSkipDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoPath, path)
		if err != nil {
			return nil
		}
		if !pathFilter.shouldScan(rel) {
			return nil
		}
		switch {
		case parser.IsComposeFile(rel):
			parsed, err := parser.ParseCompose(path)
			if err != nil {
				parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", rel, err))
				return nil
			}
			services = append(services, scanner.composeServices(repo.Name, commit, rel, parsed)...)
		case parser.IsYAMLFile(rel):
			parsed, err := parser.ParseKubernetes(path)
			if err != nil {
				parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", rel, err))
				return nil
			}
			for i := range parsed {
				parsed[i].SourcePath = rel
			}
			kubeResources = append(kubeResources, parsed...)
			routes, tcpRoutes, err := parser.ParseTraefikRoutes(path)
			if err != nil {
				parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", rel, err))
				return nil
			}
			traefikRoutes = append(traefikRoutes, routes...)
			traefikTCPRoutes = append(traefikTCPRoutes, tcpRoutes...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(parseErrors) > 0 {
		sort.Strings(parseErrors)
		return services, fmt.Errorf("parse errors: %s", strings.Join(parseErrors, "; "))
	}
	services = append(services, scanner.kubeServices(repo.Name, commit, enrichKubernetesExposure(kubeResources))...)
	services = applyTraefikRoutes(services, traefikRoutes, traefikTCPRoutes)
	pingServices, err := scanner.pingServices(repoPath, repo.Name, commit)
	if err != nil {
		return services, err
	}
	services = append(services, pingServices...)
	return services, nil
}

func (scanner Scanner) pingServices(repoPath, repoName, commit string) ([]core.Service, error) {
	var services []core.Service
	for _, target := range scanner.cfg.Runtime.Ping {
		if target.Repository != repoName || target.AnsibleInventory == "" {
			continue
		}
		inventoryPath := filepath.Join(repoPath, filepath.FromSlash(target.AnsibleInventory))
		parsed, err := hostinventory.ServicesForTarget(target, inventoryPath, commit)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", target.AnsibleInventory, err)
		}
		services = append(services, parsed...)
	}
	return services, nil
}

func (scanner Scanner) composeServices(repoName, commit, sourcePath string, project parser.ComposeProject) []core.Service {
	var services []core.Service
	for _, svc := range project.Services {
		warnings := append([]string{}, svc.Warnings...)
		warnings = append(warnings, core.MutableImageWarnings([]string{svc.Image})...)
		id := serviceID(repoName, "compose", sourcePath, svc.Name)
		services = append(services, core.Service{
			ID:             id,
			Name:           svc.Name,
			Repository:     repoName,
			SourceCommit:   commit,
			SourcePath:     sourcePath,
			Runtime:        "compose",
			ComposeProject: project.Name,
			Kind:           "Service",
			ResourceName:   svc.Name,
			Environment:    environment.Infer(sourcePath),
			Health:         core.HealthUnknown,
			Images:         compact([]string{svc.Image}),
			Ports:          svc.Ports,
			Dependencies:   svc.DependsOn,
			Storage:        svc.Volumes,
			Exposure:       uniqueSorted(append(append([]string{}, svc.Networks...), svc.Exposure...)),
			ConfigRefs:     svc.EnvVars,
			Warnings:       warnings,
		})
	}
	return services
}

func (scanner Scanner) kubeServices(repoName, commit string, resources []parser.KubernetesResource) []core.Service {
	var services []core.Service
	for _, resource := range resources {
		if !resource.IsWorkload() {
			continue
		}
		warnings := append([]string{}, resource.Warnings...)
		warnings = append(warnings, core.MutableImageWarnings(resource.Images)...)
		id := serviceID(repoName, "kubernetes", resource.SourcePath, resource.Namespace+"/"+resource.Name)
		services = append(services, core.Service{
			ID:           id,
			Name:         resource.Name,
			Repository:   repoName,
			SourceCommit: commit,
			SourcePath:   resource.SourcePath,
			Runtime:      "kubernetes",
			Kind:         resource.Kind,
			Namespace:    resource.Namespace,
			ResourceName: resource.Name,
			Environment:  environment.Infer(resource.SourcePath),
			Health:       core.HealthUnknown,
			Images:       resource.Images,
			Ports:        resource.Ports,
			Dependencies: resource.Dependencies,
			Storage:      resource.Storage,
			Exposure:     resource.Exposure,
			ConfigRefs:   resource.ConfigRefs,
			Warnings:     warnings,
		})
	}
	return services
}

func enrichKubernetesExposure(resources []parser.KubernetesResource) []parser.KubernetesResource {
	ingressByService := map[string][]string{}
	configByName := map[string][]string{}
	var services []parser.KubernetesResource
	for _, resource := range resources {
		key := namespacedName(resource.Namespace, resource.Name)
		if resource.Kind == "Ingress" {
			for _, backend := range resource.Backends {
				ingressByService[namespacedName(resource.Namespace, backend)] = append(ingressByService[namespacedName(resource.Namespace, backend)], resource.Exposure...)
			}
		}
		if resource.Kind == "ConfigMap" {
			configByName[key] = append(configByName[key], resource.Exposure...)
		}
		if resource.Kind == "Service" {
			services = append(services, resource)
		}
	}
	for i := range resources {
		resource := &resources[i]
		if !resource.IsWorkload() {
			continue
		}
		exposure := append([]string{}, resource.Exposure...)
		for _, service := range services {
			if service.Namespace != resource.Namespace || !selectorMatches(service.Selector, resource.Labels) {
				continue
			}
			exposure = append(exposure, service.Exposure...)
			exposure = append(exposure, ingressByService[namespacedName(service.Namespace, service.Name)]...)
		}
		for _, ref := range resource.ConfigRefs {
			if strings.HasPrefix(ref, "ConfigMap/") {
				exposure = append(exposure, configByName[namespacedName(resource.Namespace, strings.TrimPrefix(ref, "ConfigMap/"))]...)
			}
		}
		resource.Exposure = uniqueSorted(exposure)
	}
	return resources
}

func selectorMatches(selector, labels map[string]string) bool {
	if len(selector) == 0 || len(labels) == 0 {
		return false
	}
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func namespacedName(namespace, name string) string {
	if namespace == "" {
		namespace = "default"
	}
	return namespace + "/" + name
}

func applyTraefikRoutes(services []core.Service, routes []parser.TraefikRoute, tcpRoutes []parser.TraefikTCPRoute) []core.Service {
	routesByName := map[string][]string{}
	for _, route := range routes {
		key := normalizedName(route.Service)
		routesByName[key] = append(routesByName[key], route.Routes...)
	}
	for _, route := range tcpRoutes {
		key := normalizedName(route.Service)
		for _, endpoint := range route.Endpoints {
			if value := endpoint.Exposure(); value != "" {
				routesByName[key] = append(routesByName[key], value)
			}
		}
	}
	for i := range services {
		for _, key := range serviceRouteKeys(services[i]) {
			services[i].Exposure = append(services[i].Exposure, routesByName[key]...)
		}
		services[i].Exposure = uniqueSorted(services[i].Exposure)
	}
	return services
}

func serviceRouteKeys(service core.Service) []string {
	return uniqueSorted([]string{
		normalizedName(service.Name),
		normalizedName(service.ResourceName),
	})
}

func normalizedName(value string) string {
	value = strings.ToLower(value)
	replacer := strings.NewReplacer("_", "-", ".", "-")
	return replacer.Replace(value)
}

func gitOutput(ctx context.Context, dir string, redactor sanitizer.Redactor, env []string, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, GitCommandTimeout)
	defer cancel()
	result := invokeGit(runCtx, dir, env, args...)
	if result.err != nil {
		return "", fmt.Errorf("git %s: %w: %s", redactor.Redact(strings.Join(args, " ")), result.err, redactor.Redact(result.stderr))
	}
	return result.stdout, nil
}

func gitEnv(repo config.RepositoryConfig) []string {
	if repo.SSHKeyPath == "" {
		return nil
	}
	ssh := "ssh -i " + shellQuote(repo.SSHKeyPath) + " -o IdentitiesOnly=yes"
	if repo.KnownHosts != "" {
		ssh += " -o UserKnownHostsFile=" + shellQuote(repo.KnownHosts)
	}
	return []string{"GIT_SSH_COMMAND=" + ssh}
}

func safeName(name string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "-")
	return replacer.Replace(strings.ToLower(name))
}

func serviceID(parts ...string) string {
	h := sha1.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func compact(values []string) []string {
	var result []string
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func uniqueSorted(values []string) []string {
	values = compact(values)
	sort.Strings(values)
	result := values[:0]
	previous := ""
	for _, value := range values {
		if value == previous {
			continue
		}
		result = append(result, value)
		previous = value
	}
	return result
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
