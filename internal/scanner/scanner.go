package scanner

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/environment"
	"github.com/example/gitops-dashboard/internal/hostinventory"
	"github.com/example/gitops-dashboard/internal/parser"
	"github.com/example/gitops-dashboard/internal/sanitizer"
	"github.com/example/gitops-dashboard/internal/storage"
)

type Scanner struct {
	cfg    config.Config
	store  *storage.Store
	logger *slog.Logger
}

const (
	GitCommandTimeout        = 2 * time.Minute
	repoSyncGitCommands      = 6
	repoSyncOperationTimeout = time.Duration(repoSyncGitCommands)*GitCommandTimeout + 30*time.Second
)

func New(cfg config.Config, store *storage.Store, logger *slog.Logger) Scanner {
	return Scanner{cfg: cfg, store: store, logger: logger}
}

func (scanner Scanner) RunScheduled(ctx context.Context) {
	if len(scanner.cfg.Repositories) == 0 {
		return
	}
	if _, err := scanner.ensureRepoCacheDir(); err != nil {
		scanner.logger.Error("scheduled scans disabled", "error", err)
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
	if _, err := scanner.ensureRepoCacheDir(); err != nil {
		return err
	}
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
	key, err := scanner.repoOperationKey(repo)
	if err != nil {
		return err
	}
	_, err = repoScanFlights.do(ctx, key, func() (any, error) {
		return nil, scanner.scanOneUnshared(ctx, repo)
	})
	return err
}

func (scanner Scanner) scanOneUnshared(ctx context.Context, repo config.RepositoryConfig) error {
	scanID, err := scanner.store.StartScan(ctx, repo.Name)
	if err != nil {
		return err
	}
	commit := ""
	var services []core.Service
	scanErr := func() error {
		path, err := scanner.syncRepo(ctx, repo)
		if err != nil {
			return err
		}
		commit, err = gitOutput(ctx, path, sanitizer.Redactor{}, nil, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		services, err = scanner.parseRepo(path, repo, strings.TrimSpace(commit))
		return err
	}()
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := scanner.store.FinishScan(finishCtx, scanID, repo.Name, strings.TrimSpace(commit), services, scanErr); err != nil {
		return err
	}
	return scanErr
}

func (scanner Scanner) SyncRepo(ctx context.Context, repo config.RepositoryConfig) (string, error) {
	return scanner.syncRepo(ctx, repo)
}

func CurrentCommit(ctx context.Context, repoPath string) (string, error) {
	commit, err := gitOutput(ctx, repoPath, sanitizer.Redactor{}, nil, "rev-parse", "HEAD")
	return strings.TrimSpace(commit), err
}

func (scanner Scanner) syncRepo(ctx context.Context, repo config.RepositoryConfig) (string, error) {
	key, err := scanner.repoOperationKey(repo)
	if err != nil {
		return "", err
	}
	value, err := repoSyncFlights.doDetached(ctx, key, func() (any, error) {
		// Keep the shared git operation alive when a monitor-scoped caller times out;
		// each caller still bounds its own wait through doDetached.
		syncCtx, cancel := context.WithTimeout(context.Background(), repoSyncOperationTimeout)
		defer cancel()
		return scanner.syncRepoUnshared(syncCtx, repo)
	})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}

func (scanner Scanner) syncRepoUnshared(ctx context.Context, repo config.RepositoryConfig) (string, error) {
	repoCacheDir, err := scanner.ensureRepoCacheDir()
	if err != nil {
		return "", err
	}
	auth, err := scanner.gitAuth(repo)
	if err != nil {
		return "", err
	}
	repoPath := filepath.Join(repoCacheDir, safeName(repo.Name))
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil {
		if err := migrateRemote(ctx, repoPath, auth); err != nil {
			return "", err
		}
		if _, err := gitOutput(ctx, repoPath, auth.redactor, auth.env, "fetch", "--all", "--prune"); err != nil {
			return "", err
		}
		if repo.DefaultRef != "HEAD" {
			if _, err := gitOutput(ctx, repoPath, auth.redactor, auth.env, "checkout", repo.DefaultRef); err != nil {
				return "", err
			}
		}
		if _, err := gitOutput(ctx, repoPath, auth.redactor, auth.env, "pull", "--ff-only"); err != nil && repo.DefaultRef != "HEAD" {
			return "", err
		}
		return repoPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o700); err != nil {
		return "", err
	}
	args := []string{"clone"}
	if repo.DefaultRef != "" && repo.DefaultRef != "HEAD" {
		args = append(args, "--branch", repo.DefaultRef)
	}
	args = append(args, auth.remoteURL, repoPath)
	if _, err := gitOutput(ctx, repoCacheDir, auth.redactor, auth.env, args...); err != nil {
		return "", err
	}
	return repoPath, nil
}

func (scanner Scanner) ensureRepoCacheDir() (string, error) {
	repoCacheDir, err := filepath.Abs(scanner.cfg.Server.RepoCacheDir)
	if err != nil {
		return "", fmt.Errorf("resolve repository cache: %w", err)
	}
	if err := os.MkdirAll(repoCacheDir, 0o700); err != nil {
		return "", fmt.Errorf("create repository cache: %w", err)
	}
	return repoCacheDir, nil
}

type gitAuth struct {
	remoteURL           string
	env                 []string
	redactor            sanitizer.Redactor
	stripRemoteUserinfo bool
}

func (scanner Scanner) gitAuth(repo config.RepositoryConfig) (gitAuth, error) {
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
			remoteURL: repo.URL,
			env:       env,
			redactor:  sanitizer.New(redactionValues...),
		}, nil
	}
	remoteURL := tokenFreeRemoteURL(repo.URL)
	parsed, err := url.Parse(remoteURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return gitAuth{}, fmt.Errorf("repository %s token auth requires an http(s) url", repo.Name)
	}
	basicCredential := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	authHeader := "Authorization: Basic " + basicCredential
	redactionValues = append(redactionValues, token, basicCredential, authHeader)
	if scanner.store != nil {
		scanner.store.AddRedactionValues(redactionValues...)
	}
	env = append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http."+remoteURL+".extraHeader",
		"GIT_CONFIG_VALUE_0="+authHeader,
	)
	return gitAuth{
		remoteURL:           remoteURL,
		env:                 env,
		redactor:            sanitizer.New(redactionValues...),
		stripRemoteUserinfo: true,
	}, nil
}

func migrateRemote(ctx context.Context, repoPath string, auth gitAuth) error {
	if !auth.stripRemoteUserinfo {
		return nil
	}
	remoteURL, err := gitOutput(ctx, repoPath, auth.redactor, auth.env, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	remoteURL = strings.TrimSpace(remoteURL)
	tokenFreeURL := tokenFreeRemoteURL(remoteURL)
	if tokenFreeURL == remoteURL {
		return nil
	}
	_, err = gitOutput(ctx, repoPath, auth.redactor, auth.env, "remote", "set-url", "origin", tokenFreeURL)
	return err
}

func tokenFreeRemoteURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User == nil {
		return raw
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return raw
	}
	parsed.User = nil
	return parsed.String()
}

func (scanner Scanner) parseRepo(repoPath string, repo config.RepositoryConfig, commit string) ([]core.Service, error) {
	var services []core.Service
	var kubeResources []parser.KubernetesResource
	var traefikRoutes []parser.TraefikRoute
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
			routes, err := parser.ParseTraefikRoutes(path)
			if err != nil {
				parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", rel, err))
				return nil
			}
			traefikRoutes = append(traefikRoutes, routes...)
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
	services = applyTraefikRoutes(services, traefikRoutes)
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

func applyTraefikRoutes(services []core.Service, routes []parser.TraefikRoute) []core.Service {
	routesByName := map[string][]string{}
	for _, route := range routes {
		key := normalizedName(route.Service)
		routesByName[key] = append(routesByName[key], route.Routes...)
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
	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", redactor.Redact(strings.Join(args, " ")), err, redactor.Redact(stderr.String()))
	}
	return out.String(), nil
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
