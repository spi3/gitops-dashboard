package scanner

import (
	"bytes"
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

// containedRepoPath resolves the on-disk cache path for repo and verifies it
// cannot escape repoCacheDir. The check is a straightforward
// filepath.Rel-based containment test: the resolved path must be, or be
// nested under, repoCacheDir. safeName already excludes path separators from
// repository names, so escape is not expected in practice; this check exists
// so the guarantee is explicit and enforced rather than incidental.
func containedRepoPath(repoCacheDir string, repo config.RepositoryConfig) (string, error) {
	repoPath := filepath.Join(repoCacheDir, safeName(repo.Name))
	rel, err := filepath.Rel(repoCacheDir, repoPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("repository %s cache path escapes the repository cache directory", repo.Name)
	}
	return repoPath, nil
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
	cmd := exec.CommandContext(runCtx, "git", "config", "--get-all", key)
	cmd.Dir = repoPath
	cmd.Env = append(os.Environ(), env...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return splitGitConfigLines(out.String()), nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && out.Len() == 0 {
		return nil, nil
	}
	return nil, fmt.Errorf("git config --get-all %s: %w: %s", key, err, redactor.Redact(stderr.String()))
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
	cmd := exec.CommandContext(runCtx, "git", append([]string{"config"}, args...)...)
	cmd.Dir = repoPath
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git config %s: %w: %s", redactor.Redact(strings.Join(args, " ")), err, redactor.Redact(stderr.String()))
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

// reconcileConfiguredOrigin keeps the cached origin's fetch URL in sync with
// the token-auth URL the scanner is about to use, so the transient
// environment-scoped extraHeader (keyed by exact URL string) actually
// applies. It only acts for token-based auth: credential scrubbing itself is
// already handled by scrubCachedOriginCredentials before this runs.
func reconcileConfiguredOrigin(ctx context.Context, repoPath string, auth gitAuth) error {
	if !auth.useTokenAuth {
		return nil
	}
	current, err := gitOutput(ctx, repoPath, auth.redactor, auth.env, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	current = strings.TrimSpace(current)
	if current == auth.remoteURL {
		return nil
	}
	_, err = gitOutput(ctx, repoPath, auth.redactor, auth.env, "remote", "set-url", "origin", auth.remoteURL)
	return err
}
