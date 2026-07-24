package scanner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/sanitizer"
)

// scpLikeRemote matches Git's traditional [user@]host:path remote syntax
// (e.g. git@github.com:org/repo.git), which never contains "://" and is
// therefore distinct from an absolute URI.
var scpLikeRemote = regexp.MustCompile(`^([A-Za-z0-9._~%!$&'()*+,;=-]+@)?([^/@:\s]+):(.+)$`)

// credentialFreeRemoteURL validates a single Git remote value and, for
// HTTP(S) remotes, strips embedded userinfo credentials. Every other
// supported remote form (SSH, git:, file:, SCP-like, and filesystem paths)
// is returned byte-for-byte unchanged with stripped=false. Errors never echo
// the raw remote or any userinfo it may contain.
//
// This is a package-private contract consumed by the credential-lifecycle
// pipeline in this package and, per the T-060/T-061 handoff, by future
// scanner work that formalizes cache-path containment semantics.
func credentialFreeRemoteURL(raw string) (clean string, stripped bool, err error) {
	if raw == "" || containsControlOrWhitespace(raw) {
		return "", false, errors.New("git remote is empty or contains control characters or whitespace")
	}
	if parsed, parseErr := url.Parse(raw); parseErr == nil && parsed.IsAbs() {
		if isHTTPScheme(parsed.Scheme) {
			if parsed.Host == "" {
				return "", false, errors.New("git remote is not a valid http(s) URL")
			}
			if parsed.User == nil {
				return raw, false, nil
			}
			parsed.User = nil
			return parsed.String(), true, nil
		}
		if validNonHTTPAbsoluteURI(parsed) {
			return raw, false, nil
		}
		return "", false, errors.New("git remote is not a supported URL, SCP-like address, or filesystem path")
	}
	if scpLikeRemote.MatchString(raw) {
		return raw, false, nil
	}
	if cleaned := filepath.Clean(raw); cleaned != "" && cleaned != "." {
		return raw, false, nil
	}
	return "", false, errors.New("git remote is not a supported URL, SCP-like address, or filesystem path")
}

func isHTTPScheme(scheme string) bool {
	return strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")
}

func validNonHTTPAbsoluteURI(parsed *url.URL) bool {
	if strings.EqualFold(parsed.Scheme, "file") {
		return parsed.Path != ""
	}
	return parsed.Host != "" || parsed.Opaque != "" || parsed.Path != ""
}

func containsControlOrWhitespace(value string) bool {
	for _, r := range value {
		if r == 0 || r == 0x7f || (r < 0x20) || unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

// resolveContainedRepoPath resolves repo's on-disk cache candidate under
// resolvedRoot and verifies containment before any Git command or
// repository mutation runs, including T-060 origin reads/scrubs, origin
// enumeration/reconciliation, fetch, checkout, pull, clone destination
// creation, and rev-parse HEAD.
//
// A missing, non-symlink candidate is valid and may proceed to clone under
// resolvedRoot. An existing candidate — including one reached only through a
// symlink — is resolved with filepath.EvalSymlinks and must land strictly
// inside resolvedRoot, checked with filepath.Rel rather than a string-prefix
// comparison so a sibling directory sharing a prefix (e.g. "root-other")
// is never mistaken for containment. Dangling or cyclic symlinks fail.
func resolveContainedRepoPath(resolvedRoot string, repo config.RepositoryConfig) (repoPath string, exists bool, err error) {
	candidate := filepath.Join(resolvedRoot, safeName(repo.Name))
	if _, statErr := os.Lstat(candidate); statErr != nil {
		if !os.IsNotExist(statErr) {
			return "", false, fmt.Errorf("resolve repository %s cache path: %w", repo.Name, statErr)
		}
		if !containedRel(resolvedRoot, candidate) {
			return "", false, containmentError(repo.Name)
		}
		return candidate, false, nil
	}
	resolvedCandidate, evalErr := filepath.EvalSymlinks(candidate)
	if evalErr != nil {
		return "", false, fmt.Errorf("resolve repository %s cache path: %w", repo.Name, evalErr)
	}
	if !containedRel(resolvedRoot, resolvedCandidate) {
		return "", false, containmentError(repo.Name)
	}
	return resolvedCandidate, true, nil
}

// containedRel reports whether resolvedCandidate is strictly nested under
// resolvedRoot: neither equal to it, "..", above it, nor an absolute
// component path.
func containedRel(resolvedRoot, resolvedCandidate string) bool {
	rel, err := filepath.Rel(resolvedRoot, resolvedCandidate)
	if err != nil || filepath.IsAbs(rel) || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func containmentError(name string) error {
	return fmt.Errorf("repository %s cache path escapes the repository cache directory", name)
}

// scrubCachedOriginCredentials removes HTTP(S) userinfo from every cached
// remote.origin.url (fetch) and remote.origin.pushurl (push) value,
// preserving each list's exact count and ordering. It must run before any
// repository credential-source validation, configured-origin reconciliation,
// or network-capable Git command for a repository whose cache already
// exists.
func scrubCachedOriginCredentials(ctx context.Context, repoPath string, redactor sanitizer.Redactor, env []string) error {
	if err := scrubConfigURLList(ctx, repoPath, redactor, env, "remote.origin.url"); err != nil {
		return err
	}
	if err := scrubConfigURLList(ctx, repoPath, redactor, env, "remote.origin.pushurl"); err != nil {
		return err
	}
	return nil
}

func scrubConfigURLList(ctx context.Context, repoPath string, redactor sanitizer.Redactor, env []string, key string) error {
	values, err := gitConfigGetAll(ctx, repoPath, redactor, env, key)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}
	cleaned := make([]string, len(values))
	changed := false
	for i, value := range values {
		clean, stripped, err := credentialFreeRemoteURL(value)
		if err != nil {
			return fmt.Errorf("cached %s value is invalid: %w", key, err)
		}
		cleaned[i] = clean
		if stripped {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := gitConfigUnsetAll(ctx, repoPath, redactor, env, key); err != nil {
		return err
	}
	for _, value := range cleaned {
		if err := gitConfigAdd(ctx, repoPath, redactor, env, key, value); err != nil {
			return err
		}
	}
	return nil
}

// gitConfigGetAll runs `git config --get-all <key>` and returns the ordered
// values. Exit 0 parses all output values in order. Exit 1 with zero stdout
// bytes is a valid empty list (the key is unset). Every other nonzero exit,
// or exit 1 with output, is a read failure.
func gitConfigGetAll(ctx context.Context, repoPath string, redactor sanitizer.Redactor, env []string, key string) ([]string, error) {
	runCtx, cancel := context.WithTimeout(ctx, GitCommandTimeout)
	defer cancel()
	result := invokeGit(runCtx, repoPath, env, "config", "--get-all", key)
	if result.err == nil {
		return splitGitConfigLines(result.stdout), nil
	}
	var exitErr *exec.ExitError
	if errors.As(result.err, &exitErr) && exitErr.ExitCode() == 1 && result.stdout == "" {
		return nil, nil
	}
	return nil, fmt.Errorf("git config --get-all %s: %w: %s", key, result.err, redactor.Redact(result.stderr))
}

func gitConfigUnsetAll(ctx context.Context, repoPath string, redactor sanitizer.Redactor, env []string, key string) error {
	return gitConfigCommand(ctx, repoPath, redactor, env, "--unset-all", key)
}

func gitConfigAdd(ctx context.Context, repoPath string, redactor sanitizer.Redactor, env []string, key, value string) error {
	return gitConfigCommand(ctx, repoPath, redactor, env, "--add", key, value)
}

func gitConfigCommand(ctx context.Context, repoPath string, redactor sanitizer.Redactor, env []string, args ...string) error {
	runCtx, cancel := context.WithTimeout(ctx, GitCommandTimeout)
	defer cancel()
	result := invokeGit(runCtx, repoPath, env, append([]string{"config"}, args...)...)
	if result.err != nil {
		return fmt.Errorf("git config %s: %w: %s", redactor.Redact(strings.Join(args, " ")), result.err, redactor.Redact(result.stderr))
	}
	return nil
}

func splitGitConfigLines(output string) []string {
	if output == "" {
		return nil
	}
	lines := strings.Split(output, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// reconcileOrigin keeps the cached origin's fetch URL list equal to exactly
// one entry: configuredCleanURL, the configured remote after
// credentialFreeRemoteURL. It runs after T-060 scrubbing and before any
// network-capable Git command, enumerating with exactly `git config
// --get-all remote.origin.url` (see gitConfigGetAll for exit-code
// semantics). A read failure, or a write failure while repairing a
// mismatch, stops before fetch. This subsumes T-060's narrower
// token-auth-only origin sync: an origin already equal to configuredCleanURL
// — which is always the case once fetch and the token-auth extraHeader
// (keyed by exact URL string) need to agree — is left untouched. Push URLs,
// already scrubbed by T-060, are never touched here.
func reconcileOrigin(ctx context.Context, repoPath string, redactor sanitizer.Redactor, env []string, configuredCleanURL string) error {
	fetchURLs, err := gitConfigGetAll(ctx, repoPath, redactor, env, "remote.origin.url")
	if err != nil {
		return err
	}
	if len(fetchURLs) == 1 {
		clean, _, cleanErr := credentialFreeRemoteURL(fetchURLs[0])
		if cleanErr == nil && clean == configuredCleanURL {
			return nil
		}
	}
	if len(fetchURLs) > 0 {
		if err := gitConfigUnsetAll(ctx, repoPath, redactor, env, "remote.origin.url"); err != nil {
			return err
		}
	}
	return gitConfigAdd(ctx, repoPath, redactor, env, "remote.origin.url", configuredCleanURL)
}
