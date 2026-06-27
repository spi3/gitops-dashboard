package scanner

import (
	"bytes"
	"context"
	"crypto/sha1"
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
	"github.com/example/gitops-dashboard/internal/parser"
	"github.com/example/gitops-dashboard/internal/storage"
)

type Scanner struct {
	cfg    config.Config
	store  *storage.Store
	logger *slog.Logger
}

func New(cfg config.Config, store *storage.Store, logger *slog.Logger) Scanner {
	return Scanner{cfg: cfg, store: store, logger: logger}
}

func (scanner Scanner) RunScheduled(ctx context.Context) {
	if len(scanner.cfg.Repositories) == 0 {
		return
	}
	if err := os.MkdirAll(scanner.cfg.Server.RepoCacheDir, 0o700); err != nil {
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
	if err := os.MkdirAll(scanner.cfg.Server.RepoCacheDir, 0o700); err != nil {
		return fmt.Errorf("create repository cache: %w", err)
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
		commit, err = gitOutput(ctx, path, nil, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		services, err = scanner.parseRepo(path, repo.Name, strings.TrimSpace(commit))
		return err
	}()
	if err := scanner.store.FinishScan(ctx, scanID, repo.Name, strings.TrimSpace(commit), services, scanErr); err != nil {
		return err
	}
	return scanErr
}

func (scanner Scanner) syncRepo(ctx context.Context, repo config.RepositoryConfig) (string, error) {
	repoPath := filepath.Join(scanner.cfg.Server.RepoCacheDir, safeName(repo.Name))
	if _, err := os.Stat(filepath.Join(repoPath, ".git")); err == nil {
		if _, err := gitOutput(ctx, repoPath, gitEnv(repo), "fetch", "--all", "--prune"); err != nil {
			return "", err
		}
		if repo.DefaultRef != "HEAD" {
			if _, err := gitOutput(ctx, repoPath, gitEnv(repo), "checkout", repo.DefaultRef); err != nil {
				return "", err
			}
		}
		if _, err := gitOutput(ctx, repoPath, gitEnv(repo), "pull", "--ff-only"); err != nil && repo.DefaultRef != "HEAD" {
			return "", err
		}
		return repoPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(repoPath), 0o700); err != nil {
		return "", err
	}
	cloneURL, err := scanner.cloneURL(repo)
	if err != nil {
		return "", err
	}
	args := []string{"clone"}
	if repo.DefaultRef != "" && repo.DefaultRef != "HEAD" {
		args = append(args, "--branch", repo.DefaultRef)
	}
	args = append(args, cloneURL, repoPath)
	if _, err := gitOutput(ctx, scanner.cfg.Server.RepoCacheDir, gitEnv(repo), args...); err != nil {
		return "", err
	}
	return repoPath, nil
}

func (scanner Scanner) cloneURL(repo config.RepositoryConfig) (string, error) {
	token, err := repo.Token()
	if err != nil {
		return "", err
	}
	if token == "" {
		return repo.URL, nil
	}
	parsed, err := url.Parse(repo.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("repository %s token auth requires an https url", repo.Name)
	}
	parsed.User = url.UserPassword("x-access-token", token)
	return parsed.String(), nil
}

func (scanner Scanner) parseRepo(repoPath, repoName, commit string) ([]core.Service, error) {
	var services []core.Service
	var parseErrors []string
	err := filepath.WalkDir(repoPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			parseErrors = append(parseErrors, walkErr.Error())
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repoPath, path)
		if err != nil {
			return nil
		}
		switch {
		case parser.IsComposeFile(rel):
			parsed, err := parser.ParseCompose(path)
			if err != nil {
				parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", rel, err))
				return nil
			}
			services = append(services, scanner.composeServices(repoName, commit, rel, parsed)...)
		case parser.IsYAMLFile(rel):
			parsed, err := parser.ParseKubernetes(path)
			if err != nil {
				parseErrors = append(parseErrors, fmt.Sprintf("%s: %v", rel, err))
				return nil
			}
			services = append(services, scanner.kubeServices(repoName, commit, rel, parsed)...)
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
	return services, nil
}

func (scanner Scanner) composeServices(repoName, commit, sourcePath string, project parser.ComposeProject) []core.Service {
	var services []core.Service
	for _, svc := range project.Services {
		warnings := append([]string{}, svc.Warnings...)
		id := serviceID(repoName, "compose", sourcePath, svc.Name)
		services = append(services, core.Service{
			ID:           id,
			Name:         svc.Name,
			Repository:   repoName,
			SourceCommit: commit,
			SourcePath:   sourcePath,
			Runtime:      "compose",
			Kind:         "Service",
			ResourceName: svc.Name,
			Environment:  environment.Infer(sourcePath),
			Health:       core.HealthUnknown,
			Images:       compact([]string{svc.Image}),
			Ports:        svc.Ports,
			Dependencies: svc.DependsOn,
			Storage:      svc.Volumes,
			Exposure:     svc.Networks,
			ConfigRefs:   svc.EnvVars,
			Warnings:     warnings,
		})
	}
	return services
}

func (scanner Scanner) kubeServices(repoName, commit, sourcePath string, resources []parser.KubernetesResource) []core.Service {
	var services []core.Service
	for _, resource := range resources {
		if !resource.IsWorkload() {
			continue
		}
		id := serviceID(repoName, "kubernetes", sourcePath, resource.Namespace+"/"+resource.Name)
		services = append(services, core.Service{
			ID:           id,
			Name:         resource.Name,
			Repository:   repoName,
			SourceCommit: commit,
			SourcePath:   sourcePath,
			Runtime:      "kubernetes",
			Kind:         resource.Kind,
			Namespace:    resource.Namespace,
			ResourceName: resource.Name,
			Environment:  environment.Infer(sourcePath),
			Health:       core.HealthUnknown,
			Images:       resource.Images,
			Ports:        resource.Ports,
			Storage:      resource.Storage,
			Exposure:     resource.Exposure,
			ConfigRefs:   resource.ConfigRefs,
			Warnings:     resource.Warnings,
		})
	}
	return services
}

func gitOutput(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, redact(stderr.String()))
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

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func redact(value string) string {
	if len(value) > 1000 {
		value = value[:1000]
	}
	return strings.ReplaceAll(value, "\n", " ")
}
