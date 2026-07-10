// release creates a manually requested major or minor release tag.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/example/gitops-dashboard/internal/ci"
	"github.com/example/gitops-dashboard/internal/sanitizer"
)

const timeout = 20 * time.Second
const lockRef = "refs/releases/locks/version-allocator"

type release struct {
	root, remote, repo, head, lockOID string
	git                               *git
	api                               *github
	lockHeld                          bool
	releaseJobName                    string
	// Test-only seams leave the production path unchanged (nil selects the
	// concrete guards and signal context below).
	localFallbackOK   func(string) bool
	signalContext     func() (context.Context, context.CancelFunc)
	afterLock         func()
	beforePush        func(string, localTagRef)
	push              func(context.Context, string, localTagRef) error
	sleep             func(context.Context, time.Duration) error
	observationBudget time.Duration
}
type localTagRef struct{ name, oid string }
type configEntry struct {
	key, value string
	hasValue   bool
}
type git struct {
	root, bin string
	env       []string
	redactor  sanitizer.Redactor
}

func toolPath(name string) (string, error) {
	for _, d := range []string{"/usr/bin", "/bin"} {
		p := filepath.Join(d, name)
		if s, e := os.Stat(p); e == nil && !s.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("required trusted executable %q not found in /usr/bin or /bin", name)
}
func cleanEnv(gitPath, ghPath string) []string {
	return []string{"PATH=" + filepath.Dir(gitPath) + ":" + filepath.Dir(ghPath), "HOME=" + os.Getenv("HOME"), "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=0", "GIT_CONFIG_PARAMETERS=", "GIT_SSH_COMMAND=/usr/bin/ssh -F /dev/null", "GIT_COMMITTER_NAME=gitops-dashboard release", "GIT_COMMITTER_EMAIL=release@localhost"}
}
func (g *git) command(ctx context.Context, args ...string) *exec.Cmd {
	// Repository configuration is data, never release-tool authority. These
	// command-line values take precedence even after the config has been vetted.
	a := append([]string{"-C", g.root, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "gpg.program=/bin/false", "-c", "tag.gpgSign=false", "-c", "commit.gpgSign=false"}, args...)
	c := exec.CommandContext(ctx, g.bin, a...)
	c.Env = g.env
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = time.Second
	return c
}
func (g *git) run(ctx context.Context, args ...string) ([]byte, error) {
	op, cancel := fresh(ctx)
	defer cancel()
	c := g.command(op, args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, e := c.Output()
	if e != nil {
		return nil, fmt.Errorf("git %s: %w: %s", g.redactor.Redact(strings.Join(args, " ")), e, g.redactor.Redact(strings.TrimSpace(stderr.String())))
	}
	return out, nil
}
func (g *git) input(ctx context.Context, input string, args ...string) ([]byte, error) {
	op, cancel := fresh(ctx)
	defer cancel()
	c := g.command(op, args...)
	c.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, e := c.Output()
	if e != nil {
		return nil, fmt.Errorf("git %s: %w: %s", g.redactor.Redact(strings.Join(args, " ")), e, g.redactor.Redact(strings.TrimSpace(stderr.String())))
	}
	return out, nil
}
func (g *git) one(ctx context.Context, args ...string) (string, error) {
	b, e := g.run(ctx, args...)
	return strings.TrimSpace(string(b)), e
}
func (g *git) localRef(ctx context.Context, ref string) (string, error) {
	// for-each-ref reports an absent ref as successful empty output, unlike
	// show-ref whose exit status cannot distinguish absence from other failures.
	return g.one(ctx, "for-each-ref", "--format=%(objectname)", ref)
}

// localTagOccupied deliberately does not dereference ref.  A broken symbolic
// ref is absent from for-each-ref, but is still an operator-owned tag name.
func (g *git) localTagOccupied(ctx context.Context, ref string) (bool, error) {
	cmd := g.command(ctx, "symbolic-ref", "-q", ref)
	if out, err := cmd.Output(); err == nil || len(out) != 0 {
		return true, nil
	} else if _, ok := err.(*exec.ExitError); !ok {
		return false, fmt.Errorf("inspect symbolic tag ref %s: %w", ref, err)
	}
	direct, err := g.localRef(ctx, ref)
	if err != nil {
		return false, err
	}
	return direct != "", nil
}

type refState struct{ direct, peeled string }

func (g *git) ref(ctx context.Context, remote, ref string) (refState, error) {
	op, cancel := fresh(ctx)
	defer cancel()
	out, e := g.run(op, "ls-remote", remote, ref, ref+"^{}")
	if e != nil {
		return refState{}, e
	}
	var r refState
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		if f[1] == ref {
			r.direct = f[0]
		}
		if f[1] == ref+"^{}" {
			r.peeled = f[0]
		}
	}
	if r.direct == "" {
		return r, os.ErrNotExist
	}
	return r, nil
}
func (g *git) remoteRef(ctx context.Context, remote, ref string) (string, error) {
	r, e := g.ref(ctx, remote, ref)
	if e != nil {
		return "", e
	}
	if r.peeled != "" {
		return r.peeled, nil
	}
	return r.direct, nil
}

type github struct {
	repo, token string
	client      *http.Client
}

type apiError struct {
	path       string
	statusCode int
	status     string
	retryAfter time.Duration
}

func (e *apiError) Error() string { return fmt.Sprintf("GitHub API %s: %s", e.path, e.status) }

type workflowRun struct {
	ID           int64     `json:"id"`
	RunAttempt   int       `json:"run_attempt"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	CreatedAt    time.Time `json:"created_at"`
	DisplayTitle string    `json:"display_title"`
}

type workflowJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

func (a *github) request(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		r = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, "https://api.github.com/repos/"+a.repo+path, r)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, e := a.client.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		e := &apiError{path: path, statusCode: res.StatusCode, status: res.Status}
		// GitHub uses 403 for some rate-limit responses. Do not turn an
		// ordinary permanent 4xx into a multi-hour retry merely because it
		// includes the standard rate-limit headers.
		if (res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests) && strings.TrimSpace(res.Header.Get("X-RateLimit-Remaining")) == "0" {
			if seconds, parseErr := strconv.Atoi(res.Header.Get("Retry-After")); parseErr == nil && seconds > 0 {
				e.retryAfter = time.Duration(seconds) * time.Second
			} else if reset, parseErr := strconv.ParseInt(res.Header.Get("X-RateLimit-Reset"), 10, 64); parseErr == nil {
				if until := time.Until(time.Unix(reset, 0)); until > 0 {
					e.retryAfter = until
				}
			}
		}
		return e
	}
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	return nil
}
func ghToken(ctx context.Context, bin string, env []string) (string, error) {
	if output, err := ghOutput(ctx, bin, env, "config", "get", "http_unix_socket", "--host", "github.com"); err == nil && strings.TrimSpace(string(output)) != "" {
		return "", errors.New("gh http_unix_socket is not permitted for release API calls")
	}
	b, e := ghOutput(ctx, bin, env, "auth", "token", "--hostname", "github.com")
	if e != nil {
		return "", errors.New("gh auth token lookup failed")
	}
	if t := strings.TrimSpace(string(b)); t != "" {
		return t, nil
	}
	return "", errors.New("empty gh auth token")
}
func ghOutput(ctx context.Context, bin string, env []string, args ...string) ([]byte, error) {
	op, cancel := fresh(ctx)
	defer cancel()
	c := exec.CommandContext(op, bin, args...)
	c.Env = env
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = time.Second
	return c.Output()
}

func main() {
	if strings.Contains(strings.ToLower(os.Getenv("GODEBUG")), "http") {
		fatal("GODEBUG HTTP debugging is not permitted")
	}
	local := flag.Bool("local-fallback", false, "permit guarded local tag publication")
	burn := flag.Bool("burn-version", false, "acknowledge fallback immutable version cost")
	flag.Parse()
	if flag.NArg() != 1 || (flag.Arg(0) != "major" && flag.Arg(0) != "minor") {
		fatal("usage: release [--local-fallback --burn-version] major|minor")
	}
	r, e := newRelease()
	if e != nil {
		fatal("%v", e)
	}
	if e = r.run(ci.Bump(flag.Arg(0)), *local, *burn); e != nil {
		fatal("%v", e)
	}
}
func newRelease() (*release, error) {
	root, e := os.Getwd()
	if e != nil {
		return nil, e
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		return nil, e
	}
	gitBin, e := toolPath("git")
	if e != nil {
		return nil, e
	}
	ghBin, e := toolPath("gh")
	if e != nil {
		return nil, e
	}
	g := &git{root: root, bin: gitBin, env: cleanEnv(gitBin, ghBin)}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	remote, e := inspectedOrigin(ctx, root, gitBin, g.env)
	if e != nil {
		return nil, e
	}
	if e := authorityGuard(ctx, root, gitBin, g.env); e != nil {
		return nil, e
	}
	repo, e := githubRepo(remote)
	if e != nil {
		return nil, e
	}
	g.redactor = sanitizer.New(sanitizer.URLUserinfoValues(remote)...)
	remote = releaseRemoteURL(remote)
	gitDir := filepath.Join(root, ".git")
	if top, e := g.one(ctx, "--git-dir="+gitDir, "--work-tree="+root, "rev-parse", "--show-toplevel"); e != nil || top != root {
		return nil, errors.New("working tree identity does not match the physical repository root")
	}
	if s, e := g.one(ctx, "--git-dir="+gitDir, "--work-tree="+root, "status", "--porcelain", "--untracked-files=all"); e != nil || s != "" {
		return nil, errors.New("working tree must be clean (including untracked files)")
	}
	if b, e := g.one(ctx, "--git-dir="+gitDir, "--work-tree="+root, "branch", "--show-current"); e != nil || b != "main" {
		return nil, errors.New("current branch must be main")
	}
	return &release{root: root, remote: remote, repo: repo, git: g}, nil
}
func releaseRemoteURL(remote string) string {
	// The shared sanitizer intentionally strips userinfo anywhere in text. The
	// only allowed release URL user is SSH's protocol user, git.
	if u, err := url.Parse(remote); err == nil && u.Scheme == "ssh" && u.User != nil && u.User.Username() == "git" {
		return remote
	}
	return sanitizer.StripURLUserinfo(remote)
}

func repoConfigFiles(root string) ([]string, error) {
	gitDir := filepath.Join(root, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil {
		return nil, fmt.Errorf("inspect repository metadata: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("linked worktrees are not supported for release; run from the primary checkout")
	}
	return []string{filepath.Join(gitDir, "config")}, nil
}
func configEntries(ctx context.Context, root, bin string, env []string) ([]configEntry, error) {
	files, err := repoConfigFiles(root)
	if err != nil {
		return nil, err
	}
	var entries []configEntry
	for _, file := range files {
		if _, err := os.Stat(file); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		// --file is evaluated from a neutral directory and cannot run repository
		// hooks, fsmonitor, signing programs, or transport helpers.
		op, cancel := fresh(ctx)
		cmd := exec.CommandContext(op, bin, "-C", os.TempDir(), "config", "--no-includes", "--null", "--file", file, "--list")
		cmd.Env = env
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		cmd.WaitDelay = time.Second
		out, err := cmd.Output()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", file, err)
		}
		for _, item := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
			key, value, hasValue := strings.Cut(item, "\n")
			// git config --null --list represents a valueless setting as key\0.
			// Keep it explicit: silently dropping it would make the allowlist lie.
			if key != "" {
				entries = append(entries, configEntry{key: strings.ToLower(key), value: value, hasValue: hasValue})
			}
		}
	}
	return entries, nil
}
func inspectedOrigin(ctx context.Context, root, bin string, env []string) (string, error) {
	entries, err := configEntries(ctx, root, bin, env)
	if err != nil {
		return "", err
	}
	var origins []string
	for _, e := range entries {
		if e.key == "remote.origin.url" && e.hasValue {
			origins = append(origins, e.value)
		}
	}
	if len(origins) != 1 || strings.TrimSpace(origins[0]) == "" {
		return "", errors.New("origin: exactly one non-empty remote.origin.url is required")
	}
	return origins[0], nil
}

var safeConfigKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var remoteConfigKey = regexp.MustCompile(`^remote\.[A-Za-z0-9_-]+\.(url|fetch)$`)
var branchConfigKey = regexp.MustCompile(`^branch\.[A-Za-z0-9_-]+\.(remote|merge|rebase)$`)

func allowedConfig(key, value string, hasValue bool) bool {
	// There are presently no implicit-true settings in the release allowlist.
	// A valueless entry is still represented above, and is deliberately denied.
	if !hasValue {
		return false
	}
	switch key {
	case "core.repositoryformatversion", "core.filemode", "core.logallrefupdates", "core.ignorecase", "core.precomposeunicode", "core.symlinks", "core.autocrlf", "core.eol":
		return true
	case "core.bare":
		return strings.EqualFold(strings.TrimSpace(value), "false")
	}
	return remoteConfigKey.MatchString(key) || branchConfigKey.MatchString(key)
}
func rejectedConfigKey(key string) string {
	if safeConfigKey.MatchString(key) {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("unprintable-config-key(sha256=%x length=%d)", sum[:8], len(key))
}
func authorityGuard(ctx context.Context, root, bin string, env []string) error {
	entries, err := configEntries(ctx, root, bin, env)
	if err != nil {
		return err
	}
	for _, e := range entries {
		key := e.key
		if strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".pushurl") {
			return fmt.Errorf("repository configuration %s is not permitted for release", rejectedConfigKey(key))
		}
		if !allowedConfig(key, e.value, e.hasValue) {
			return fmt.Errorf("repository configuration %s is not permitted for release", rejectedConfigKey(key))
		}
	}
	return nil
}
func githubRepo(remote string) (string, error) {
	if strings.IndexFunc(remote, unicode.IsControl) >= 0 {
		return "", errors.New("origin must not contain control characters")
	}
	if p, ok := strings.CutPrefix(remote, "git@github.com:"); ok {
		if strings.ContainsAny(p, "?#") {
			return "", errors.New("origin must not contain a query or fragment")
		}
		return githubPath(p)
	}
	if strings.ContainsAny(remote, "?#") {
		return "", errors.New("origin must not contain a query or fragment")
	}
	u, e := url.Parse(remote)
	if e == nil && (u.RawQuery != "" || u.Fragment != "") {
		return "", errors.New("origin must not contain a query or fragment")
	}
	if e == nil && u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return "", errors.New("origin URL userinfo is not permitted")
		}
		if u.Scheme != "ssh" || u.User.Username() != "git" {
			return "", errors.New("origin URL userinfo is not permitted")
		}
	}
	if e == nil && u.Scheme == "https" && u.Host == "github.com" && u.User == nil {
		return githubPath(u.Path)
	}
	if e == nil && u.Scheme == "ssh" && u.Host == "github.com" && u.User != nil && u.User.Username() == "git" {
		return githubPath(u.Path)
	}
	return "", errors.New("origin must be an explicit HTTPS or SSH github.com owner/repository URL")
}

var githubOwner = regexp.MustCompile(`^(?:[A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9-]{0,37}[A-Za-z0-9])$`)
var githubRepository = regexp.MustCompile(`^(?:[A-Za-z0-9]|[A-Za-z0-9][A-Za-z0-9._-]{0,98}[A-Za-z0-9_])$`)

func githubPath(p string) (string, error) {
	p = strings.TrimSuffix(strings.TrimPrefix(p, "/"), ".git")
	parts := strings.Split(p, "/")
	if len(parts) != 2 || !githubOwner.MatchString(parts[0]) || !githubRepository.MatchString(parts[1]) {
		return "", errors.New("origin must name a github.com owner/repository")
	}
	return p, nil
}

func (r *release) run(b ci.Bump, local, burn bool) error {
	contextFactory := releaseSignalContext
	if r.signalContext != nil {
		contextFactory = r.signalContext
	}
	ctx, stop := contextFactory()
	defer stop()
	allowLocal := localFallbackRemoteOK
	if r.localFallbackOK != nil {
		allowLocal = r.localFallbackOK
	}
	if local && !allowLocal(r.remote) {
		return errors.New("--local-fallback requires an SSH GitHub origin; use an ssh remote or the CI release path")
	}
	fetchCtx, cancel := fresh(ctx)
	_, fetchErr := r.git.run(fetchCtx, "fetch", "--no-tags", "--no-prune", "--no-prune-tags", "--no-recurse-submodules", r.remote, "refs/heads/main:refs/remotes/origin/main")
	cancel()
	if e := fetchErr; e != nil {
		return e
	}
	var e error
	r.head, e = r.git.one(ctx, "rev-parse", "HEAD")
	if e != nil {
		return e
	}
	main, e := r.git.one(ctx, "rev-parse", "refs/remotes/origin/main")
	if e != nil || main != r.head {
		return errors.New("HEAD must equal origin/main")
	}
	if r.api == nil {
		ghBin, _ := toolPath("gh")
		token, e := ghToken(ctx, ghBin, r.git.env)
		if e != nil {
			return e
		}
		r.api = &github{r.repo, token, &http.Client{Timeout: timeout}}
	}
	if e = r.green(ctx); e != nil {
		return e
	}
	if !local {
		jobName, supported, e := r.dispatchSupported(ctx)
		if e != nil {
			return e
		}
		if supported {
			r.releaseJobName = jobName
			return r.dispatch(ctx, b)
		}
		return errors.New("remote CI workflow does not provide the required serialized release capability")
	}
	if !burn {
		return errors.New("--local-fallback requires --burn-version to acknowledge immutable-version cost")
	}
	if e = r.active(ctx); e != nil {
		return e
	}
	return r.publish(ctx, b)
}
func releaseSignalContext() (context.Context, context.CancelFunc) {
	// SIGPIPE otherwise terminates the process immediately when a caller drops
	// its output pipe, bypassing the lock and local-tag cleanup defers.
	signal.Ignore(syscall.SIGPIPE)
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}
func localFallbackRemoteOK(remote string) bool {
	return strings.HasPrefix(remote, "ssh://git@github.com/") || strings.HasPrefix(remote, "git@github.com:")
}
func (r *release) green(ctx context.Context) error {
	var all []workflowRun
	seen := map[int64]bool{}
	total := -1
	for page := 1; ; page++ {
		var v struct {
			TotalCount   int           `json:"total_count"`
			WorkflowRuns []workflowRun `json:"workflow_runs"`
		}
		path := fmt.Sprintf("/actions/workflows/ci.yml/runs?head_sha=%s&branch=main&event=push&per_page=100&page=%d", r.head, page)
		if e := r.api.request(ctx, "GET", path, nil, &v); e != nil {
			return fmt.Errorf("CI guard: %w", e)
		}
		if total < 0 {
			total = v.TotalCount
		} else if total != v.TotalCount {
			return errors.New("CI guard: workflow run pagination total changed")
		}
		for _, run := range v.WorkflowRuns {
			if seen[run.ID] {
				return errors.New("CI guard: workflow run pagination contained duplicate run IDs")
			}
			seen[run.ID] = true
			all = append(all, run)
		}
		if total == 0 || len(seen) >= total {
			break
		}
		if len(v.WorkflowRuns) == 0 {
			return errors.New("CI guard: workflow run pagination was incomplete")
		}
	}
	if len(seen) != total {
		return errors.New("CI guard: workflow run pagination was incomplete")
	}
	var newest *workflowRun
	for i := range all {
		x := &all[i]
		if newest == nil || x.CreatedAt.After(newest.CreatedAt) || (x.CreatedAt.Equal(newest.CreatedAt) && x.ID > newest.ID) {
			newest = x
		}
	}
	if newest != nil {
		// The list endpoint has no ordering guarantee. Refetch exactly the run
		// selected from the completed traversal; its current attempt is decisive.
		var current workflowRun
		if e := r.api.request(ctx, "GET", fmt.Sprintf("/actions/runs/%d", newest.ID), nil, &current); e != nil {
			return fmt.Errorf("CI guard: %w", e)
		}
		newest = &current
	}
	if newest != nil && newest.Status == "completed" && newest.Conclusion == "success" {
		return nil
	}
	if newest == nil {
		return errors.New("CI is not green for HEAD: no matching run")
	}
	return fmt.Errorf("CI is not green for HEAD: newest run %d status=%s conclusion=%s", newest.ID, newest.Status, newest.Conclusion)
}
func (r *release) dispatchSupported(ctx context.Context) (string, bool, error) {
	req, e := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/"+r.repo+"/contents/.github/workflows/ci.yml?ref=main", nil)
	if e != nil {
		return "", false, e
	}
	req.Header.Set("Authorization", "Bearer "+r.api.token)
	req.Header.Set("Accept", "application/vnd.github.raw")
	res, e := r.api.client.Do(req)
	if e != nil {
		return "", false, e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", false, fmt.Errorf("cannot fetch remote CI workflow capability: %s", res.Status)
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, 10<<20))
	if e != nil {
		return "", false, e
	}
	return ci.WorkflowReleaseJobName(b)
}
func dispatchToken() (string, error) {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func (r *release) dispatch(ctx context.Context, b ci.Bump) error {
	token, e := dispatchToken()
	if e != nil {
		return e
	}
	// This is an idempotency key, not a credential. Persist it in the operator
	// transcript before POST so an ambiguous response can be recovered later.
	if _, e = fmt.Fprintln(releaseStdout, "release dispatch token: "+token); e != nil {
		return fmt.Errorf("cannot persist release dispatch token: %w", e)
	}
	var accepted struct {
		RunID       int64 `json:"workflow_run_id"`
		WorkflowRun struct {
			ID int64 `json:"id"`
		} `json:"workflow_run"`
	}
	if e = r.api.request(ctx, "POST", "/actions/workflows/ci.yml/dispatches", map[string]any{"ref": "main", "inputs": map[string]string{"bump": string(b), "dispatch_token": token, "expected_revision": r.head}, "return_run_details": true}, &accepted); e != nil {
		// A lost response may follow server-side acceptance; reconcile the exact
		// token over the normal visibility window before reporting failure.
		if permanentAPIError(e) {
			return e
		}
	}
	if accepted.RunID == 0 {
		accepted.RunID = accepted.WorkflowRun.ID
	}
	return r.waitDispatch(ctx, token, accepted.RunID)
}

const dispatchObservationBudget = 2*time.Hour + 15*time.Minute

func (r *release) findDispatch(ctx context.Context, token string) (*workflowRun, error) {
	var v struct {
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	if err := r.api.request(ctx, "GET", "/actions/workflows/ci.yml/runs?event=workflow_dispatch", nil, &v); err != nil {
		return nil, err
	}
	for _, run := range v.WorkflowRuns {
		if strings.Contains(run.DisplayTitle, token) {
			return &run, nil
		}
	}
	return nil, nil
}
func (r *release) waitDispatch(parent context.Context, token string, runID int64) error {
	budget := dispatchObservationBudget
	if r.observationBudget > 0 {
		budget = r.observationBudget
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	delay := 2 * time.Second
	for {
		var err error
		if runID == 0 {
			var found *workflowRun
			found, err = r.findDispatch(ctx, token)
			if found != nil {
				runID = found.ID
			}
		} else {
			var current workflowRun
			err = r.api.request(ctx, "GET", fmt.Sprintf("/actions/runs/%d", runID), nil, &current)
			if err == nil {
				if current.Status != "completed" {
					goto wait
				}
				if current.Conclusion == "success" {
					return r.releaseJobSucceeded(ctx, current.ID)
				}
				return fmt.Errorf("release dispatch run %d concluded %s (%s)", current.ID, current.Conclusion, r.runURL(current.ID))
			}
		}
		if err != nil && permanentAPIError(err) {
			return err
		}
	wait:
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("release dispatch run %s was not completed before timeout", r.runReference(runID, token))
			}
			return fmt.Errorf("release dispatch observation interrupted for %s: %w", r.runReference(runID, token), ctx.Err())
		}
		wait := delay + time.Duration(mathrand.Int64N(int64(time.Second)))
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.retryAfter > wait {
			wait = apiErr.retryAfter
		}
		if r.sleep != nil {
			err = r.sleep(ctx, wait)
		} else {
			err = sleepContext(ctx, wait)
		}
		if err != nil {
			if ctx.Err() != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return fmt.Errorf("release dispatch run %s was not completed before timeout", r.runReference(runID, token))
				}
				return fmt.Errorf("release dispatch observation interrupted for %s: %w", r.runReference(runID, token), ctx.Err())
			}
			return err
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}

func (r *release) runURL(runID int64) string {
	return fmt.Sprintf("https://github.com/%s/actions/runs/%d", r.repo, runID)
}

func (r *release) runReference(runID int64, token string) string {
	if runID != 0 {
		return fmt.Sprintf("%d (%s)", runID, r.runURL(runID))
	}
	return "dispatch token " + token
}

func (r *release) releaseJobSucceeded(ctx context.Context, runID int64) error {
	var jobs struct {
		Jobs []workflowJob `json:"jobs"`
	}
	if err := r.api.request(ctx, "GET", fmt.Sprintf("/actions/runs/%d/jobs", runID), nil, &jobs); err != nil {
		return fmt.Errorf("release dispatch run %d jobs: %w", runID, err)
	}
	for _, job := range jobs.Jobs {
		if job.Name == r.releaseJobName {
			if job.Status == "completed" && job.Conclusion == "success" {
				return nil
			}
			return fmt.Errorf("release dispatch run %d %q job status=%s conclusion=%s (%s)", runID, r.releaseJobName, job.Status, job.Conclusion, r.runURL(runID))
		}
	}
	return fmt.Errorf("release dispatch run %d has no %q job (%s)", runID, r.releaseJobName, r.runURL(runID))
}

func permanentAPIError(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.statusCode >= 400 && apiErr.statusCode < 500 && apiErr.statusCode != http.StatusTooManyRequests && apiErr.retryAfter == 0
}
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (r *release) active(ctx context.Context) error {
	var wf struct {
		State string `json:"state"`
	}
	if e := r.api.request(ctx, "GET", "/actions/workflows/ci.yml", nil, &wf); e != nil {
		return e
	}
	var p struct {
		Enabled bool `json:"enabled"`
	}
	if e := r.api.request(ctx, "GET", "/actions/permissions", nil, &p); e != nil {
		return e
	}
	if wf.State != "active" || !p.Enabled {
		return errors.New("CI workflow or repository Actions is disabled; refusing to burn a version")
	}
	return nil
}
func fresh(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}
func (r *release) createLocalTag(ctx context.Context, tag string) (localTagRef, error) {
	probeCtx, stop := fresh(ctx)
	preexisting, preflightErr := r.git.localTagOccupied(probeCtx, "refs/tags/"+tag)
	stop()
	if preflightErr != nil {
		return localTagRef{}, preflightErr
	}
	if preexisting {
		return localTagRef{}, fmt.Errorf("computed local tag %s already exists; refusing to claim or delete it", tag)
	}
	object := fmt.Sprintf("object %s\ntype commit\ntag %s\ntagger gitops-dashboard release <release@localhost> %d +0000\n\nRelease %s\n", r.head, tag, time.Now().Unix(), tag)
	tagCtx, cancel := fresh(ctx)
	rawOID, tagErr := r.git.input(tagCtx, object, "mktag")
	cancel()
	if tagErr != nil {
		return localTagRef{}, tagErr
	}
	oid := strings.TrimSpace(string(rawOID))
	format, e := r.git.one(ctx, "rev-parse", "--show-object-format")
	if e != nil {
		return localTagRef{}, e
	}
	zero := strings.Repeat("0", 40)
	if format == "sha256" {
		zero = strings.Repeat("0", 64)
	}
	createCtx, stop := fresh(ctx)
	_, e = r.git.run(createCtx, "update-ref", "--no-deref", "refs/tags/"+tag, oid, zero)
	stop()
	if e != nil {
		return localTagRef{}, fmt.Errorf("computed local tag %s already exists or changed during creation: %w", tag, e)
	}
	// update-ref compare-and-create is the ownership proof. Do not re-read the
	// mutable ref: cleanup is bound to precisely the object we installed.
	return localTagRef{name: tag, oid: oid}, nil
}
func (r *release) pushLocalTag(ctx context.Context, tag string, local localTagRef) error {
	commit, err := r.git.one(ctx, "rev-parse", local.oid+"^{}")
	if err != nil || commit != r.head {
		return fmt.Errorf("created local tag %s does not peel to HEAD", tag)
	}
	// The local ref is mutable. Publish the captured object ID so a concurrent
	// local ref update cannot change what the server receives.
	_, err = r.git.run(ctx, "push", "--no-follow-tags", r.remote, local.oid+":refs/tags/"+tag)
	return err
}
func released(tag string) error {
	_, err := fmt.Fprintln(releaseStdout, "released "+tag)
	return err
}

var releaseStdout io.Writer = os.Stdout

func (r *release) publish(ctx context.Context, b ci.Bump) (err error) {
	owner, e := dispatchToken()
	if e != nil {
		return e
	}
	raw, e := r.git.input(ctx, "gitops-dashboard release lock "+r.head+" "+owner, "hash-object", "-w", "--stdin")
	if e != nil {
		return e
	}
	r.lockOID = strings.TrimSpace(string(raw))
	if e = r.acquireLock(ctx); e != nil {
		return e
	}
	if r.afterLock != nil {
		r.afterLock()
	}
	var localTag localTagRef
	defer func() {
		var cleanup error
		if e := r.releaseLock(); e != nil {
			cleanup = fmt.Errorf("stranded release lock %s owner %s: %w", lockRef, r.lockOID, e)
		}
		if localTag.name != "" {
			deleteCtx, cancel := fresh(context.Background())
			_, e := r.git.run(deleteCtx, "update-ref", "--no-deref", "-d", "refs/tags/"+localTag.name, localTag.oid)
			cancel()
			if e != nil {
				cleanup = errors.Join(cleanup, fmt.Errorf("delete local tag %s: %w", localTag.name, e))
			}
		}
		if cleanup != nil {
			err = errors.Join(err, cleanup)
		}
	}()
	tags, e := r.tags(ctx)
	if e != nil {
		return e
	}
	allocation, e := ci.Allocate(tags, r.head, b)
	if e != nil {
		return e
	}
	tag := allocation.Version
	if allocation.Reused {
		commit, e := r.git.remoteRef(ctx, r.remote, "refs/tags/"+tag)
		if e != nil || commit != r.head {
			return fmt.Errorf("reused target tag %s does not resolve to HEAD", tag)
		}
		return released(tag)
	}
	if _, e := r.git.ref(ctx, r.remote, "refs/tags/"+tag); e == nil {
		return fmt.Errorf("computed target tag %s already exists", tag)
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	localTag, e = r.createLocalTag(ctx, tag)
	if e != nil {
		return e
	}
	if r.beforePush != nil {
		r.beforePush(tag, localTag)
	}
	for attempt := 0; attempt < 2; attempt++ {
		pushCtx, cancel := fresh(ctx)
		if r.push != nil {
			e = r.push(pushCtx, tag, localTag)
		} else {
			e = r.pushLocalTag(pushCtx, tag, localTag)
		}
		cancel()
		if e == nil {
			state, probeErr := r.git.ref(ctx, r.remote, "refs/tags/"+tag)
			if probeErr != nil || state.direct != localTag.oid || state.peeled != r.head {
				return fmt.Errorf("created target tag %s does not peel to HEAD", tag)
			}
			return released(tag)
		}
		probe, stop := fresh(ctx)
		state, re := r.git.ref(probe, r.remote, "refs/tags/"+tag)
		stop()
		if re == nil && state.direct == localTag.oid && state.peeled == r.head {
			return released(tag)
		}
		if re == nil {
			return fmt.Errorf("computed target tag %s already exists", tag)
		}
		if !errors.Is(re, os.ErrNotExist) {
			return fmt.Errorf("tag push outcome unknown: %w", re)
		}
	}
	return e
}

// acquireLock only creates the ref. A lost push response is reconciled before
// returning, so a successfully created lock is always cleaned up by publish.
func (r *release) acquireLock(ctx context.Context) error {
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		// A failed create push is ambiguous until a fresh ref read proves it did
		// not land. Keep cleanup authority while reconciling it.
		r.lockHeld = true
		pushCtx, cancel := fresh(ctx)
		// An empty expected value means the destination ref must not exist: this
		// is create-only even if a server permits non-fast-forward updates.
		_, e := r.git.run(pushCtx, "push", "--no-follow-tags", "--force-with-lease="+lockRef+":", r.remote, r.lockOID+":"+lockRef)
		cancel()
		if e == nil {
			r.lockHeld = true
			return nil
		}
		last = e
		probe, stop := fresh(ctx)
		state, reconcile := r.git.ref(probe, r.remote, lockRef)
		stop()
		if reconcile == nil {
			if state.direct == r.lockOID {
				r.lockHeld = true
				return nil
			}
			return errors.New("release allocator lock is held by another owner")
		}
		if errors.Is(reconcile, os.ErrNotExist) {
			r.lockHeld = false
			continue
		}
		last = fmt.Errorf("lock acquisition outcome unknown: %w", reconcile)
	}
	if r.lockHeld {
		cleanup := r.releaseLock()
		return errors.Join(fmt.Errorf("release allocator lock was not acquired after reconciliation: %w", last), cleanup)
	}
	return fmt.Errorf("release allocator lock was not acquired after reconciliation: %w", last)
}

func (r *release) tags(ctx context.Context) ([]ci.Tag, error) {
	op, cancel := fresh(ctx)
	defer cancel()
	out, e := r.git.run(op, "ls-remote", "--tags", r.remote, "refs/tags/*")
	if e != nil {
		return nil, e
	}
	m := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && strings.HasPrefix(f[1], "refs/tags/") {
			m[strings.TrimPrefix(strings.TrimSuffix(f[1], "^{}"), "refs/tags/")] = f[0]
		}
	}
	var tags []ci.Tag
	for n, c := range m {
		tags = append(tags, ci.Tag{Name: n, Commit: c})
	}
	return tags, nil
}
func (r *release) releaseLock() error {
	if !r.lockHeld {
		return nil
	}
	var last error
	for i := 0; i < 3; i++ {
		probeCtx, cancel := fresh(context.Background())
		state, e := r.git.ref(probeCtx, r.remote, lockRef)
		cancel()
		if e != nil {
			if errors.Is(e, os.ErrNotExist) {
				return nil
			}
			last = e
			continue
		}
		if state.direct != r.lockOID {
			return nil
		}
		deleteCtx, cancel := fresh(context.Background())
		_, e = r.git.run(deleteCtx, "push", "--no-follow-tags", "--force-with-lease="+lockRef+":"+r.lockOID, r.remote, ":"+lockRef)
		cancel()
		if e != nil {
			last = e
			continue
		}
		verifyCtx, cancel := fresh(context.Background())
		state, e = r.git.ref(verifyCtx, r.remote, lockRef)
		cancel()
		if errors.Is(e, os.ErrNotExist) {
			r.lockHeld = false
			return nil
		}
		if e != nil {
			last = e
			continue
		}
		if state.direct != r.lockOID {
			r.lockHeld = false
			return nil
		}
		last = errors.New("original lock remains")
	}
	if last == nil {
		last = errors.New("cleanup did not complete")
	}
	return last
}
func fatal(f string, a ...any) { fmt.Fprintf(os.Stderr, "release refused: "+f+"\n", a...); os.Exit(1) }
