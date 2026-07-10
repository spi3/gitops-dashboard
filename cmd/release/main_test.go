package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/ci"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGitHubRepo(t *testing.T) {
	for _, tt := range []struct {
		remote, want string
		ok           bool
	}{
		{"https://github.com/acme/repo.git", "acme/repo", true}, {"ssh://git@github.com/acme/repo.git", "acme/repo", true}, {"git@github.com:acme/repo.git", "acme/repo", true}, {"https://token:secret@github.com/acme/repo.git", "", false}, {"ssh://git:secret@github.com/acme/repo.git", "", false}, {"http://github.com/acme/repo", "", false}, {"https://evil.invalid/acme/repo", "", false},
		{"ssh://git@github.com/acme/repo.git?access_token=secret", "", false}, {"https://github.com/acme/repo#secret", "", false}, {"git@github.com:acme/repo?secret", "", false}, {"https://github.com/acme!/repo", "", false}, {"https://github.com/acme/repo name", "", false},
	} {
		got, err := githubRepo(tt.remote)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("githubRepo(%q) = %q, %v", tt.remote, got, err)
		}
	}
}

func TestGitHubRequestOnlyDefersExhaustedRateLimits(t *testing.T) {
	future := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			a := &github{repo: "acme/repo", token: "test", client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				header := make(http.Header)
				header.Set("X-RateLimit-Remaining", "42")
				header.Set("X-RateLimit-Reset", future)
				return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d test", status), Header: header, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})}}
			err := a.request(context.Background(), "GET", "/fixture", nil, nil)
			if !permanentAPIError(err) {
				t.Fatalf("status %d error was not permanent: %v", status, err)
			}
		})
	}
}

func TestGitHubRequestDefersOnlyExhaustedRateLimit(t *testing.T) {
	a := &github{repo: "acme/repo", token: "test", client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("X-RateLimit-Remaining", "0")
		header.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		return &http.Response{StatusCode: http.StatusTooManyRequests, Status: "429 test", Header: header, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	err := a.request(context.Background(), "GET", "/fixture", nil, nil)
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.retryAfter <= 0 || permanentAPIError(err) {
		t.Fatalf("exhausted rate limit = %v, retry=%v", err, apiErr.retryAfter)
	}
}

func TestGitRunCancelsProcessGroupWithRetainedStderr(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 30 >&2 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	g := &git{root: dir, bin: fake, env: []string{"PATH=/usr/bin:/bin"}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := g.run(ctx, "fixture")
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("run did not cancel process group promptly: %v", err)
	}
}

func TestReleaseSignalContextCancelsOnSIGHUP(t *testing.T) {
	ctx, stop := releaseSignalContext()
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("SIGHUP did not cancel release context")
	}
}

func TestInspectedOriginRejectsAmbiguousAuthority(t *testing.T) {
	gitBin, _ := toolPath("git")
	ghBin, _ := toolPath("gh")
	for i, config := range []string{
		"[remote \"origin\"]\nurl = https://github.com/acme/repo.git\nurl = https://github.com/evil/repo.git\n",
		"[remote \"origin\"]\nurl = https://github.com/acme/repo.git\npushurl = ssh://git@github.com/evil/repo.git\n",
	} {
		dir := t.TempDir()
		if out, err := exec.Command(gitBin, "init", dir).CombinedOutput(); err != nil {
			t.Fatalf("init: %v: %s", err, out)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := inspectedOrigin(context.Background(), dir, gitBin, cleanEnv(gitBin, ghBin)); (err == nil) != (i == 1) {
			t.Fatal("ambiguous origin accepted")
		}
		if err := authorityGuard(context.Background(), dir, gitBin, cleanEnv(gitBin, ghBin)); (err == nil) != (i == 0) {
			t.Fatalf("pushurl guard result = %v", err)
		}
	}
}

func TestAuthorityGuardDoesNotLeakUnsafeConfigKey(t *testing.T) {
	gitBin, _ := toolPath("git")
	ghBin, _ := toolPath("gh")
	dir := t.TempDir()
	if out, err := exec.Command(gitBin, "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	secret := "token-like-secret"
	config := "[remote \"" + secret + ":credential\"]\nurl = https://github.com/acme/repo.git\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	err := authorityGuard(context.Background(), dir, gitBin, cleanEnv(gitBin, ghBin))
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "sha256=") {
		t.Fatalf("unsafe key leaked: %v", err)
	}
}

func TestAuthorityGuardRejectsValuelessFetchPruneSettings(t *testing.T) {
	gitBin, _ := toolPath("git")
	ghBin, _ := toolPath("gh")
	for _, key := range []string{"prune", "pruneTags"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			if out, err := exec.Command(gitBin, "init", dir).CombinedOutput(); err != nil {
				t.Fatalf("init: %v: %s", err, out)
			}
			config := "[remote \"origin\"]\nurl = https://github.com/acme/repo.git\n[fetch]\n" + key + "\n"
			if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			err := authorityGuard(context.Background(), dir, gitBin, cleanEnv(gitBin, ghBin))
			if err == nil || !strings.Contains(err.Error(), "fetch."+strings.ToLower(key)) {
				t.Fatalf("valueless fetch.%s was accepted: %v", key, err)
			}
		})
	}
}

func TestNewReleaseRejectsAlternateRefsBeforeRepositoryGitRuns(t *testing.T) {
	dir := t.TempDir()
	gitBin, _ := toolPath("git")
	if out, err := exec.Command(gitBin, "init", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	marker := filepath.Join(dir, "ran")
	config := "[remote \"origin\"]\nurl = https://github.com/acme/repo.git\n[core]\nalternateRefsCommand = touch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if _, err := newRelease(); err == nil || !strings.Contains(err.Error(), "core.alternaterefscommand") {
		t.Fatalf("newRelease = %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("poisoned config executed: %v", err)
	}
}

func TestSSHRemoteKeepsProtocolUser(t *testing.T) {
	remote := "ssh://git@github.com/acme/repo.git"
	if got := releaseRemoteURL(remote); got != remote {
		t.Fatalf("protocol user was stripped: %q", got)
	}
}

func TestAnnotatedTagUsesConstructedIdentity(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	gitPath, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	ghPath, err := toolPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	g := &git{root: dir, bin: gitPath, env: cleanEnv(gitPath, ghPath)}
	if _, err := g.input(context.Background(), "fixture", "hash-object", "-w", "--stdin"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.run(context.Background(), "add", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.run(context.Background(), "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.run(context.Background(), "tag", "-a", "v1.0.0", "-m", "Release v1.0.0", "HEAD"); err != nil {
		t.Fatalf("annotated tag without user config: %v", err)
	}
}
func TestCleanEnvDoesNotInheritTransportOverrides(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "evil")
	t.Setenv("GH_CONFIG_DIR", "evil")
	git, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gh, err := toolPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range cleanEnv(git, gh) {
		if strings.HasPrefix(value, "GH_CONFIG_DIR=") || value == "GIT_SSH_COMMAND=evil" || strings.HasPrefix(value, "GIT_CONFIG_KEY_") {
			t.Fatalf("unsafe environment inherited: %q", value)
		}
	}
}

func TestCleanEnvPinsTrustedPathsAndTagIdentity(t *testing.T) {
	git, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gh, err := toolPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(cleanEnv(git, gh), "\n")
	for _, want := range []string{"PATH=/usr/bin", "GIT_COMMITTER_NAME=", "GIT_COMMITTER_EMAIL=", "GIT_SSH_COMMAND=/usr/bin/ssh -F /dev/null", "GIT_CONFIG_COUNT=0", "GIT_CONFIG_PARAMETERS="} {
		if !strings.Contains(env, want) {
			t.Fatalf("clean environment missing %q: %s", want, env)
		}
	}
}

func TestAuthorityGuardRejectsCanonicalTransportAndExecutableConfig(t *testing.T) {
	gitBin, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	ghBin, err := toolPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"url.https://evil.invalid/.insteadOf", "url.https://evil.invalid/.pushInsteadOf",
		"core.sshCommand", "http.https://github.com/.sslVerify", "https.https://github.com/.sslCAInfo",
		"core.fsmonitor", "gpg.program", "tag.gpgSign",
	} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			if out, err := exec.Command(gitBin, "init", dir).CombinedOutput(); err != nil {
				t.Fatalf("init: %v: %s", err, out)
			}
			config := filepath.Join(dir, ".git", "config")
			if out, err := exec.Command(gitBin, "config", "--file", config, key, "fixture").CombinedOutput(); err != nil {
				t.Fatalf("config %s: %v: %s", key, err, out)
			}
			if err := authorityGuard(context.Background(), dir, gitBin, cleanEnv(gitBin, ghBin)); err == nil {
				t.Fatalf("authorityGuard accepted %s", key)
			}
		})
	}
}

func TestAuthorityGuardDoesNotReadConfigWorktree(t *testing.T) {
	gitBin, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	ghBin, err := toolPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if out, err := exec.Command(gitBin, "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	worktree := filepath.Join(dir, ".git", "config.worktree")
	if err := os.WriteFile(worktree, []byte("[core]\nfsmonitor = /tmp/never-run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := authorityGuard(context.Background(), dir, gitBin, cleanEnv(gitBin, ghBin)); err != nil {
		t.Fatalf("authorityGuard read config.worktree: %v", err)
	}
}

func TestNewReleaseRejectsPoisonedFsmonitorBeforeRepositoryGitRuns(t *testing.T) {
	dir := t.TempDir()
	gitBin, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(gitBin, "init", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	marker := filepath.Join(dir, "executed")
	hook := filepath.Join(dir, "fsmonitor")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, ".git", "config")
	if out, err := exec.Command(gitBin, "config", "--file", config, "core.fsmonitor", hook).CombinedOutput(); err != nil {
		t.Fatalf("config: %v: %s", err, out)
	}
	t.Chdir(dir)
	if _, err := newRelease(); err == nil {
		t.Fatal("newRelease accepted poisoned fsmonitor")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fsmonitor was executed before inspection: %v", err)
	}
}

func TestNewReleaseRejectsIncludedConfigBeforeRepositoryGitRuns(t *testing.T) {
	dir := t.TempDir()
	gitBin, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(gitBin, "init", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	marker := filepath.Join(dir, "executed")
	fsmonitor := filepath.Join(dir, "fsmonitor")
	if err := os.WriteFile(fsmonitor, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	included := filepath.Join(dir, "included-config")
	if err := os.WriteFile(included, []byte("[core]\nfsmonitor = "+fsmonitor+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, ".git", "config")
	if out, err := exec.Command(gitBin, "config", "--file", config, "remote.origin.url", "https://github.com/acme/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("origin config: %v: %s", err, out)
	}
	if out, err := exec.Command(gitBin, "config", "--file", config, "include.path", included).CombinedOutput(); err != nil {
		t.Fatalf("include config: %v: %s", err, out)
	}
	t.Chdir(dir)
	_, err = newRelease()
	if err == nil || !strings.Contains(err.Error(), "include.path") {
		t.Fatalf("newRelease error = %v, want include rejection", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("included fsmonitor ran before inspection: %v", err)
	}
}

func TestRepoConfigFilesRejectsLinkedWorktree(t *testing.T) {
	gitBin, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	primary := t.TempDir()
	if out, err := exec.Command(gitBin, "init", "-b", "main", primary).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(primary, "fixture"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(gitBin, "-C", primary, "add", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("add: %v: %s", err, out)
	}
	if out, err := exec.Command(gitBin, "-C", primary, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	if out, err := exec.Command(gitBin, "-C", primary, "worktree", "add", linked).CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v: %s", err, out)
	}
	_, err = repoConfigFiles(linked)
	if err == nil || !strings.Contains(err.Error(), "primary checkout") {
		t.Fatalf("repoConfigFiles error = %v, want linked-worktree rejection", err)
	}
}

func TestGreenRequiresNewestMatchingRunToSucceed(t *testing.T) {
	for _, body := range []string{
		`{"total_count":2,"workflow_runs":[{"id":1,"status":"completed","conclusion":"success"},{"id":2,"status":"completed","conclusion":"failure"}]}`,
		`{"total_count":2,"workflow_runs":[{"id":1,"status":"completed","conclusion":"success"},{"id":2,"status":"in_progress","conclusion":null}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			r := &release{head: "head", api: &github{repo: "acme/repo", token: "token", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasPrefix(req.URL.Path, "/repos/acme/repo/actions/runs/") {
					body = `{"id":2,"status":"completed","conclusion":"failure"}`
				}
				return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}}}
			err := r.green(context.Background())
			if err == nil || !strings.Contains(err.Error(), "newest run 2") {
				t.Fatalf("green error = %v, want newest failed or pending run", err)
			}
		})
	}
}

func TestGreenPaginatesBeforeSelectingNewestRun(t *testing.T) {
	requests := 0
	r := &release{head: "head", releaseJobName: "Release Container", api: &github{repo: "acme/repo", token: "token", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		body := `{"total_count":2,"workflow_runs":[{"id":1,"status":"completed","conclusion":"success","created_at":"2026-01-01T00:00:00Z"}]}`
		if strings.HasPrefix(req.URL.Path, "/repos/acme/repo/actions/runs/") {
			body = `{"id":2,"status":"completed","conclusion":"failure","created_at":"2026-01-02T00:00:00Z"}`
		} else if req.URL.Query().Get("page") == "2" {
			body = `{"total_count":2,"workflow_runs":[{"id":2,"status":"completed","conclusion":"failure","created_at":"2026-01-02T00:00:00Z"}]}`
		}
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}}
	err := r.green(context.Background())
	if err == nil || !strings.Contains(err.Error(), "newest run 2") || requests != 3 {
		t.Fatalf("green = %v after %d requests", err, requests)
	}
}

func TestGreenRejectsDuplicatePaginationRows(t *testing.T) {
	r := &release{head: "head", releaseJobName: "Release Container", api: &github{repo: "acme/repo", token: "token", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"total_count":2,"workflow_runs":[{"id":1,"status":"completed","conclusion":"success","created_at":"2026-01-01T00:00:00Z"}]}`
		if req.URL.Query().Get("page") == "2" {
			body = `{"total_count":2,"workflow_runs":[{"id":1,"status":"completed","conclusion":"success","created_at":"2026-01-01T00:00:00Z"}]}`
		}
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}}
	if err := r.green(context.Background()); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("green = %v, want duplicate rejection", err)
	}
}

func TestGreenUsesRefetchedRerunState(t *testing.T) {
	requests := 0
	r := &release{head: "head", api: &github{repo: "acme/repo", token: "token", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		body := `{"total_count":1,"workflow_runs":[{"id":1,"run_attempt":1,"status":"completed","conclusion":"success","created_at":"2026-01-01T00:00:00Z"}]}`
		if strings.HasPrefix(req.URL.Path, "/repos/acme/repo/actions/runs/") {
			body = `{"id":1,"run_attempt":2,"status":"in_progress","conclusion":null,"created_at":"2026-01-01T00:00:00Z"}`
		}
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}}
	err := r.green(context.Background())
	if err == nil || !strings.Contains(err.Error(), "in_progress") || requests != 2 {
		t.Fatalf("green = %v after %d requests, want refetched rerun to abort", err, requests)
	}
}

func TestDispatchUsesReturnedRunAndRequiresReleaseJob(t *testing.T) {
	var post map[string]any
	r := &release{head: "head", releaseJobName: "Release Container", api: &github{repo: "acme/repo", token: "token", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := "{}"
		switch {
		case req.Method == http.MethodPost:
			if err := json.NewDecoder(req.Body).Decode(&post); err != nil {
				t.Fatal(err)
			}
			body = `{"workflow_run_id":42}`
		case strings.HasSuffix(req.URL.Path, "/actions/runs/42"):
			body = `{"id":42,"status":"completed","conclusion":"success"}`
		case strings.HasSuffix(req.URL.Path, "/actions/runs/42/jobs"):
			body = `{"jobs":[{"name":"Release Container","status":"completed","conclusion":"success"}]}`
		default:
			t.Fatalf("unexpected request %s %s", req.Method, req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}}
	old := releaseStdout
	releaseStdout = io.Discard
	t.Cleanup(func() { releaseStdout = old })
	if err := r.dispatch(context.Background(), ci.BumpMinor); err != nil {
		t.Fatal(err)
	}
	if post["return_run_details"] != true {
		t.Fatalf("dispatch did not request returned run details: %#v", post)
	}
}

func TestDispatchFailsBeforePostWhenTokenCannotBeRecorded(t *testing.T) {
	requests := 0
	r := &release{head: "head", api: &github{repo: "acme/repo", token: "token", client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { requests++; return nil, errors.New("must not post") })}}}
	old := releaseStdout
	releaseStdout = errWriter{}
	t.Cleanup(func() { releaseStdout = old })
	if err := r.dispatch(context.Background(), ci.BumpMinor); err == nil || requests != 0 {
		t.Fatalf("dispatch = %v, requests=%d", err, requests)
	}
}

func TestRemoteDispatchEndToEndUsesCheckedInWorkflow(t *testing.T) {
	gitBin, _ := toolPath("git")
	ghBin, _ := toolPath("gh")
	root, remote := t.TempDir(), filepath.Join(t.TempDir(), "remote.git")
	for _, args := range [][]string{{"init", "--bare", remote}, {"init", "-b", "main", root}, {"-C", root, "config", "user.name", "test"}, {"-C", root, "config", "user.email", "test@example.invalid"}} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "fixture"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", root, "add", "fixture"}, {"-C", root, "commit", "-m", "fixture"}, {"-C", root, "remote", "add", "origin", remote}, {"-C", root, "push", "origin", "main"}} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	g := &git{root: root, bin: gitBin, env: cleanEnv(gitBin, ghBin)}
	r := &release{root: root, remote: remote, repo: "acme/repo", git: g}
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	jobName, supported, err := ci.WorkflowReleaseJobName(workflow)
	if err != nil || !supported {
		t.Fatalf("workflow release job name = %q, %t, %v", jobName, supported, err)
	}
	r.api = &github{repo: r.repo, token: "test", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch {
		case strings.Contains(req.URL.Path, "/contents/.github/workflows/ci.yml"):
			body = string(workflow)
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/dispatches"):
			body = `{"workflow_run_id":99}`
		case strings.HasSuffix(req.URL.Path, "/actions/runs/99/jobs"):
			body = fmt.Sprintf(`{"jobs":[{"name":%q,"status":"completed","conclusion":"success"}]}`, jobName)
		case strings.HasSuffix(req.URL.Path, "/actions/runs/99"):
			body = `{"id":99,"status":"completed","conclusion":"success"}`
		case strings.Contains(req.URL.Path, "/actions/workflows/ci.yml/runs"):
			body = `{"total_count":1,"workflow_runs":[{"id":1,"status":"completed","conclusion":"success","created_at":"2026-07-10T00:00:00Z"}]}`
		case strings.HasSuffix(req.URL.Path, "/actions/runs/1"):
			body = `{"id":1,"status":"completed","conclusion":"success"}`
		default:
			t.Fatalf("unexpected API request %s %s", req.Method, req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	old := releaseStdout
	releaseStdout = io.Discard
	t.Cleanup(func() { releaseStdout = old })
	if err := r.run(ci.BumpMinor, false, false); err != nil {
		t.Fatal(err)
	}
}

type fakeDispatchAPI struct {
	token     string
	posts     int
	listCalls int
	runCalls  int
}

func TestRemoteDispatchEndToEndRemainingMatrix(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		run  func(t *testing.T, r *release, api *fakeDispatchAPI)
	}{
		{
			name: "delayed visibility correlates token",
			run: func(t *testing.T, r *release, api *fakeDispatchAPI) {
				r.sleep = func(context.Context, time.Duration) error { return nil }
				err := r.run(ci.BumpMinor, false, false)
				if err != nil {
					t.Fatal(err)
				}
				if api.listCalls != 4 {
					t.Fatalf("dispatch list calls = %d, want 4", api.listCalls)
				}
			},
		},
		{
			name: "ambiguous acceptance reconciles without redispatch",
			run: func(t *testing.T, r *release, api *fakeDispatchAPI) {
				r.sleep = func(context.Context, time.Duration) error { return nil }
				err := r.run(ci.BumpMinor, false, false)
				if err != nil {
					t.Fatal(err)
				}
				if api.posts != 1 {
					t.Fatalf("dispatch posts = %d, want 1", api.posts)
				}
			},
		},
		{
			name: "successful real display-name job",
			run: func(t *testing.T, r *release, api *fakeDispatchAPI) {
				if err := r.run(ci.BumpMinor, false, false); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "failed real display-name job includes run URL",
			run: func(t *testing.T, r *release, api *fakeDispatchAPI) {
				err := r.run(ci.BumpMinor, false, false)
				if err == nil || !strings.Contains(err.Error(), `"Release Container" job status=completed conclusion=failure`) || !strings.Contains(err.Error(), r.runURL(99)) {
					t.Fatalf("failure = %v", err)
				}
			},
		},
		{
			name: "skipped real display-name job includes run URL",
			run: func(t *testing.T, r *release, api *fakeDispatchAPI) {
				err := r.run(ci.BumpMinor, false, false)
				if err == nil || !strings.Contains(err.Error(), `"Release Container" job status=completed conclusion=skipped`) || !strings.Contains(err.Error(), r.runURL(99)) {
					t.Fatalf("skipped = %v", err)
				}
			},
		},
		{
			name: "timeout includes run reference",
			run: func(t *testing.T, r *release, api *fakeDispatchAPI) {
				r.observationBudget = time.Millisecond
				r.sleep = func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }
				err := r.run(ci.BumpMinor, false, false)
				if err == nil || !strings.Contains(err.Error(), "before timeout") || !strings.Contains(err.Error(), r.runURL(99)) {
					t.Fatalf("timeout = %v", err)
				}
			},
		},
		{
			name: "signal cancellation includes run reference and leaves no local lock",
			run: func(t *testing.T, r *release, api *fakeDispatchAPI) {
				ctx, cancel := context.WithCancel(context.Background())
				stopped := false
				r.signalContext = func() (context.Context, context.CancelFunc) { return ctx, func() { stopped = true } }
				r.sleep = func(ctx context.Context, _ time.Duration) error { cancel(); <-ctx.Done(); return ctx.Err() }
				err := r.run(ci.BumpMinor, false, false)
				if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), r.runURL(99)) {
					t.Fatalf("cancellation = %v", err)
				}
				if !stopped {
					t.Fatal("run did not defer signal-context cleanup")
				}
				if _, lockErr := r.git.ref(context.Background(), r.remote, lockRef); !errors.Is(lockErr, os.ErrNotExist) || r.lockHeld {
					t.Fatalf("remote dispatch stranded local lock: lockHeld=%t err=%v", r.lockHeld, lockErr)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			api := &fakeDispatchAPI{}
			r := newRemoteDispatchRelease(t, workflow, api, tt.name)
			tt.run(t, r, api)
		})
	}
}

func newRemoteDispatchRelease(t *testing.T, workflow []byte, state *fakeDispatchAPI, scenario string) *release {
	t.Helper()
	jobName, supported, err := ci.WorkflowReleaseJobName(workflow)
	if err != nil || !supported {
		t.Fatalf("workflow release job name = %q, %t, %v", jobName, supported, err)
	}
	gitBin, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	ghBin, err := toolPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	root, remote := t.TempDir(), filepath.Join(t.TempDir(), "remote.git")
	for _, args := range [][]string{{"init", "--bare", remote}, {"init", "-b", "main", root}, {"-C", root, "config", "user.name", "test"}, {"-C", root, "config", "user.email", "test@example.invalid"}} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "fixture"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", root, "add", "fixture"}, {"-C", root, "commit", "-m", "fixture"}, {"-C", root, "remote", "add", "origin", remote}, {"-C", root, "push", "origin", "main"}} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	r := &release{root: root, remote: remote, repo: "acme/repo", git: &git{root: root, bin: gitBin, env: cleanEnv(gitBin, ghBin)}}
	r.api = &github{repo: r.repo, token: "test", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, status := "{}", http.StatusOK
		switch {
		case strings.Contains(req.URL.Path, "/contents/.github/workflows/ci.yml"):
			body = string(workflow)
		case strings.Contains(req.URL.RawQuery, "head_sha="):
			body = `{"total_count":1,"workflow_runs":[{"id":1,"status":"completed","conclusion":"success","created_at":"2026-07-10T00:00:00Z"}]}`
		case strings.HasSuffix(req.URL.Path, "/actions/runs/1"):
			body = `{"id":1,"status":"completed","conclusion":"success"}`
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/dispatches"):
			state.posts++
			var post struct {
				Inputs map[string]string `json:"inputs"`
			}
			if err := json.NewDecoder(req.Body).Decode(&post); err != nil {
				t.Fatal(err)
			}
			state.token = post.Inputs["dispatch_token"]
			if state.token == "" {
				t.Fatal("dispatch omitted idempotency token")
			}
			switch scenario {
			case "delayed visibility correlates token":
				body = `{}`
			case "ambiguous acceptance reconciles without redispatch":
				return nil, errors.New("simulated lost dispatch response after acceptance")
			default:
				body = `{"workflow_run_id":99}`
			}
		case strings.Contains(req.URL.Path, "/actions/workflows/ci.yml/runs"):
			state.listCalls++
			if scenario == "delayed visibility correlates token" && state.listCalls < 4 {
				body = `{"total_count":0,"workflow_runs":[]}`
			} else {
				body = fmt.Sprintf(`{"total_count":1,"workflow_runs":[{"id":99,"status":"queued","display_title":"Release %s"}]}`, state.token)
			}
		case strings.HasSuffix(req.URL.Path, "/actions/runs/99/jobs"):
			conclusion := "success"
			if scenario == "failed real display-name job includes run URL" {
				conclusion = "failure"
			} else if scenario == "skipped real display-name job includes run URL" {
				conclusion = "skipped"
			}
			body = fmt.Sprintf(`{"jobs":[{"name":%q,"status":"completed","conclusion":%q}]}`, jobName, conclusion)
		case strings.HasSuffix(req.URL.Path, "/actions/runs/99"):
			state.runCalls++
			switch scenario {
			case "timeout includes run reference", "signal cancellation includes run reference and leaves no local lock":
				body = `{"id":99,"status":"in_progress"}`
			default:
				body = `{"id":99,"status":"completed","conclusion":"success"}`
			}
		default:
			t.Fatalf("unexpected API request %s %s?%s", req.Method, req.URL.Path, req.URL.RawQuery)
		}
		return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d test", status), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	old := releaseStdout
	releaseStdout = io.Discard
	t.Cleanup(func() { releaseStdout = old })
	return r
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestGitHubRepoRejectsEmptyQueryAndFragmentDelimiters(t *testing.T) {
	for _, remote := range []string{"https://github.com/acme/repo?", "https://github.com/acme/repo#"} {
		if _, err := githubRepo(remote); err == nil {
			t.Fatalf("githubRepo accepted %q", remote)
		}
	}
}

func TestLocalFallbackRequiresSSHOrigin(t *testing.T) {
	if localFallbackRemoteOK("https://github.com/acme/repo.git") {
		t.Fatal("HTTPS local fallback accepted")
	}
	if !localFallbackRemoteOK("ssh://git@github.com/acme/repo.git") {
		t.Fatal("SSH local fallback rejected")
	}
}

func TestGhTokenCancellationKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := ghToken(ctx, gh, []string{"PATH=/usr/bin:/bin"}); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("gh token lookup did not time out safely: %v", err)
	}
}

func TestCreateLocalTagDoesNotClaimTagAfterFailedCreation(t *testing.T) {
	dir := t.TempDir()
	state, fake := filepath.Join(dir, "tag-state"), filepath.Join(dir, "git")
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do [ "$arg" = mktag ] && tag=1; [ "$arg" = for-each-ref ] && show=1; done
if [ -n "$tag" ]; then : > %q; sleep 30; fi
if [ -n "$show" ] && [ -e %q ]; then echo created-oid; exit 0; fi
exit 0
`, state, state)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := &release{head: "head", git: &git{root: dir, bin: fake, env: []string{"PATH=/usr/bin:/bin"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	local, err := r.createLocalTag(ctx, "v1.2.3")
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("tag creation did not time out safely: %v", err)
	}
	if local.name != "" || local.oid != "" {
		t.Fatalf("failed creation claimed cleanup authority: %#v", local)
	}
}

func TestCreateLocalTagRefusesAndPreservesPreexistingTag(t *testing.T) {
	dir := t.TempDir()
	gitBin, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	ghBin, err := toolPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(gitBin, "init", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(gitBin, "-C", dir, "add", "fixture").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command(gitBin, "-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if out, err := exec.Command(gitBin, "-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "tag", "-a", "v1.2.3", "-m", "legitimate local tag").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	before, err := exec.Command(gitBin, "-C", dir, "cat-file", "-p", "refs/tags/v1.2.3").Output()
	if err != nil {
		t.Fatal(err)
	}
	r := &release{head: "HEAD", git: &git{root: dir, bin: gitBin, env: cleanEnv(gitBin, ghBin)}}
	if local, err := r.createLocalTag(context.Background(), "v1.2.3"); err == nil || local.name != "" || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("createLocalTag = %#v, %v", local, err)
	}
	after, err := exec.Command(gitBin, "-C", dir, "cat-file", "-p", "refs/tags/v1.2.3").Output()
	if err != nil || string(after) != string(before) {
		t.Fatalf("pre-existing tag changed: %v\nbefore=%q\nafter=%q", err, before, after)
	}
}

func TestCreateLocalTagRefusesBrokenSymbolicTag(t *testing.T) {
	dir := t.TempDir()
	gitBin, _ := toolPath("git")
	ghBin, _ := toolPath("gh")
	if out, err := exec.Command(gitBin, "init", "-b", "main", dir).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", dir, "add", "fixture"}, {"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture"}, {"-C", dir, "symbolic-ref", "refs/tags/v1.2.3", "refs/tags/operator-missing"}} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	r := &release{head: "HEAD", git: &git{root: dir, bin: gitBin, env: cleanEnv(gitBin, ghBin)}}
	if _, err := r.createLocalTag(context.Background(), "v1.2.3"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("broken symbolic tag was claimed: %v", err)
	}
	out, err := exec.Command(gitBin, "-C", dir, "symbolic-ref", "refs/tags/v1.2.3").Output()
	if err != nil || strings.TrimSpace(string(out)) != "refs/tags/operator-missing" {
		t.Fatalf("symbolic tag changed: %q, %v", out, err)
	}
}

func TestLocalTagCleanupDoesNotDereferenceSubstitutedSymbolicTag(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "git")
	if err := os.WriteFile(fake, []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\n", args)), 0o700); err != nil {
		t.Fatal(err)
	}
	// This is the cleanup command used after an interrupt. It must retain the
	// tag name itself even if an operator has substituted a symbolic ref.
	g := &git{root: dir, bin: fake, env: []string{"PATH=/usr/bin:/bin"}}
	if _, err := g.run(context.Background(), "update-ref", "--no-deref", "-d", "refs/tags/v1.2.3", "owned"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "update-ref --no-deref -d refs/tags/v1.2.3 owned") {
		t.Fatalf("cleanup dereferenced or omitted raw CAS: %s", b)
	}
}

func TestCreateLocalTagCompareAndCreateDoesNotClaimReplacement(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "git")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$*" in
  *symbolic-ref*) exit 1 ;;
  *for-each-ref*) exit 0 ;;
  *mktag*) cat >/dev/null; echo tag-object ;;
  *show-object-format*) echo sha1 ;;
  *update-ref*) exit 1 ;;
esac
`, argsFile)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := &release{head: "head", git: &git{root: dir, bin: fake, env: []string{"PATH=/usr/bin:/bin"}}}
	local, err := r.createLocalTag(context.Background(), "v1.2.3")
	if err == nil || local.name != "" || local.oid != "" {
		t.Fatalf("replacement was claimed: %#v, %v", local, err)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "update-ref --no-deref refs/tags/v1.2.3 tag-object "+strings.Repeat("0", 40)) {
		t.Fatalf("tag creation was not compare-and-create: %s", b)
	}
}

func TestReleaseFetchFlagsPreservePreexistingLocalTagDespitePruneConfig(t *testing.T) {
	gitBin, _ := toolPath("git")
	ghBin, _ := toolPath("gh")
	root, remote := t.TempDir(), filepath.Join(t.TempDir(), "remote.git")
	for _, args := range [][]string{{"init", "--bare", remote}, {"init", "-b", "main", root}, {"-C", root, "config", "user.name", "test"}, {"-C", root, "config", "user.email", "test@example.invalid"}} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "fixture"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", root, "add", "fixture"}, {"-C", root, "commit", "-m", "fixture"}, {"-C", root, "remote", "add", "origin", remote}, {"-C", root, "push", "origin", "main"}, {"-C", root, "tag", "keep-local"}, {"-C", root, "config", "fetch.prune", "true"}, {"-C", root, "config", "fetch.pruneTags", "true"}} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	before, err := exec.Command(gitBin, "-C", root, "rev-parse", "refs/tags/keep-local").Output()
	if err != nil {
		t.Fatal(err)
	}
	g := &git{root: root, bin: gitBin, env: cleanEnv(gitBin, ghBin)}
	if _, err := g.run(context.Background(), "fetch", "--no-tags", "--no-prune", "--no-prune-tags", "--no-recurse-submodules", remote, "refs/heads/main:refs/remotes/origin/main"); err != nil {
		t.Fatal(err)
	}
	after, err := exec.Command(gitBin, "-C", root, "rev-parse", "refs/tags/keep-local").Output()
	if err != nil || string(after) != string(before) {
		t.Fatalf("fetch altered pre-existing local tag: %v before=%q after=%q", err, before, after)
	}
}

func TestPushLocalTagUsesCapturedObjectID(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "git")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\ncase \"$*\" in *rev-parse*) echo head;; esac\n", argsFile)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := &release{head: "head", remote: "origin", git: &git{root: dir, bin: fake, env: []string{"PATH=/usr/bin:/bin"}}}
	if err := r.pushLocalTag(context.Background(), "v1.2.3", localTagRef{name: "v1.2.3", oid: "captured-oid"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "captured-oid:refs/tags/v1.2.3") || strings.Contains(got, "refs/tags/v1.2.3 refs/tags/v1.2.3") {
		t.Fatalf("push did not bind publication to captured oid: %q", got)
	}
}

func TestRemoteRefAcceptsAnnotatedAndLightweightTags(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "git")
	script := `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    *annotated*) printf 'tag-oid\trefs/tags/v1.2.3annotated\nhead\trefs/tags/v1.2.3annotated^{}\n' ;;
    *lightweight*) printf 'head\trefs/tags/v1.2.3lightweight\n' ;;
  esac
done
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	g := &git{root: dir, bin: fake, env: []string{"PATH=/usr/bin:/bin"}}
	for _, tag := range []string{"annotated", "lightweight"} {
		// The fake's suffix is all remoteRef needs to distinguish direct and peeled output.
		ref := "refs/tags/" + tag
		if tag == "annotated" {
			ref = "refs/tags/v1.2.3annotated"
		} else {
			ref = "refs/tags/v1.2.3lightweight"
		}
		got, err := g.remoteRef(context.Background(), "origin", ref)
		if err != nil || got != "head" {
			t.Fatalf("remoteRef(%s) = %q, %v", tag, got, err)
		}
	}
}

func TestReleaseLockTreatsSuccessorAsReleased(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	fakeGit := filepath.Join(dir, "git")
	script := fmt.Sprintf(`#!/bin/sh
for arg in "$@"; do [ "$arg" = ls-remote ] && remote=1; done
if [ -n "$remote" ]; then
  if [ -e %q ]; then printf 'successor\t%s\n'; else : > %q; printf 'original\t%s\n'; fi
fi
`, state, lockRef, state, lockRef)
	if err := os.WriteFile(fakeGit, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	r := &release{remote: "origin", lockOID: "original", lockHeld: true, git: &git{root: dir, bin: fakeGit, env: []string{"PATH=/usr/bin:/bin"}}}
	if err := r.releaseLock(); err != nil {
		t.Fatalf("releaseLock: %v", err)
	}
	if r.lockHeld {
		t.Fatal("successor lock was mistaken for the original owner lock")
	}
}

func TestReleasePublishEndToEndWithBareRemote(t *testing.T) {
	gitBin, err := toolPath("git")
	if err != nil {
		t.Fatal(err)
	}
	ghBin, err := toolPath("gh")
	if err != nil {
		t.Fatal(err)
	}
	root, remote := t.TempDir(), filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command(gitBin, "init", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("bare remote: %v: %s", err, out)
	}
	for _, args := range [][]string{{"init", "-b", "main", root}, {"-C", root, "config", "user.name", "test"}, {"-C", root, "config", "user.email", "test@example.invalid"}} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "fixture"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"-C", root, "add", "fixture"}, {"-C", root, "commit", "-m", "fixture"}, {"-C", root, "remote", "add", "origin", remote}, {"-C", root, "push", "origin", "main"}} {
		if out, err := exec.Command(gitBin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	g := &git{root: root, bin: gitBin, env: cleanEnv(gitBin, ghBin)}
	head, err := g.one(context.Background(), "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	r := &release{root: root, remote: remote, repo: "acme/repo", head: head, git: g}
	r.api = &github{repo: r.repo, token: "test", client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch {
		case strings.HasPrefix(req.URL.Path, "/repos/acme/repo/actions/workflows/ci.yml/runs"):
			body = `{"total_count":1,"workflow_runs":[{"id":7,"run_attempt":1,"status":"completed","conclusion":"success","created_at":"2026-07-10T00:00:00Z"}]}`
		case req.URL.Path == "/repos/acme/repo/actions/runs/7":
			body = `{"id":7,"run_attempt":1,"status":"completed","conclusion":"success","created_at":"2026-07-10T00:00:00Z"}`
		case req.URL.Path == "/repos/acme/repo/actions/workflows/ci.yml":
			body = `{"state":"active"}`
		case req.URL.Path == "/repos/acme/repo/actions/permissions":
			body = `{"enabled":true}`
		default:
			t.Fatalf("unexpected GitHub API request %s", req.URL.Path)
		}
		return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	// Drive the exact production orchestration entrypoint. The local-remote
	// exception is test-only; normal runs still require an SSH GitHub origin.
	r.localFallbackOK = func(string) bool { return true }
	oldOutput := releaseStdout
	var output bytes.Buffer
	releaseStdout = &output
	t.Cleanup(func() { releaseStdout = oldOutput })
	if err := r.run(ci.BumpMinor, true, true); err != nil {
		t.Fatal(err)
	}
	if output.String() != "released v0.1.0\n" {
		t.Fatalf("success output = %q", output.String())
	}
	published, err := g.ref(context.Background(), remote, "refs/tags/v0.1.0")
	if err != nil || published.peeled != head {
		t.Fatalf("published tag = %+v, %v; want peeled %s", published, err, head)
	}
	if objectType, err := g.one(context.Background(), "cat-file", "-t", published.direct); err != nil || objectType != "tag" {
		t.Fatalf("published direct object type = %q, %v; want tag", objectType, err)
	}
	if _, err := g.ref(context.Background(), remote, lockRef); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release lock was not removed: %v", err)
	}
	advance := func() {
		if err := os.WriteFile(filepath.Join(root, "fixture"), []byte(fmt.Sprintf("fixture-%d", time.Now().UnixNano())), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "fixture"}, {"commit", "-m", "next fixture"}, {"push", remote, "main"}} {
			if _, err := g.run(context.Background(), args...); err != nil {
				t.Fatalf("advance %v: %v", args, err)
			}
		}
	}
	advance()
	// A cancellation after acquisition exercises the production defers; an
	// unrelated operator tag must survive it.
	if _, err := g.run(context.Background(), "tag", "keep-local"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r2 := &release{root: root, remote: remote, repo: "acme/repo", git: g, api: r.api, localFallbackOK: func(string) bool { return true }, signalContext: func() (context.Context, context.CancelFunc) { return ctx, func() {} }, afterLock: cancel}
	if err := r2.run(ci.BumpMinor, true, true); err == nil {
		t.Fatal("cancellation while locked succeeded")
	}
	if _, err := g.ref(context.Background(), remote, lockRef); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled release stranded lock: %v", err)
	}
	if _, err := g.one(context.Background(), "rev-parse", "refs/tags/keep-local"); err != nil {
		t.Fatalf("cancelled release removed pre-existing local tag: %v", err)
	}

	// A remote collision introduced after allocation is not adopted, locally or
	// remotely, and cleanup leaves the operator tag alone.
	r3 := &release{root: root, remote: remote, repo: "acme/repo", git: g, api: r.api, localFallbackOK: func(string) bool { return true }}
	var collisionTag, collisionOID, collisionRemoteOID string
	r3.beforePush = func(tag string, local localTagRef) {
		collisionTag, collisionOID = tag, local.oid
		if _, err := g.run(context.Background(), "push", remote, r.head+":refs/tags/"+tag); err != nil {
			t.Fatal(err)
		}
		state, err := g.ref(context.Background(), remote, "refs/tags/"+tag)
		if err != nil {
			t.Fatal(err)
		}
		collisionRemoteOID = state.direct
	}
	if err := r3.run(ci.BumpMinor, true, true); err == nil {
		t.Fatal("remote collision succeeded")
	}
	if collision, err := g.ref(context.Background(), remote, "refs/tags/"+collisionTag); err != nil || collision.direct != collisionRemoteOID {
		t.Fatalf("collision ref = %+v, %v; want unchanged direct %s", collision, err, collisionRemoteOID)
	}
	if local, err := g.localRef(context.Background(), "refs/tags/"+collisionTag); err != nil || local != "" {
		t.Fatalf("collision-owned local tag = %q, %v; want removed (created %s)", local, err, collisionOID)
	}
	if _, err := g.ref(context.Background(), remote, lockRef); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collision stranded lock: %v", err)
	}
	if _, err := g.one(context.Background(), "rev-parse", "refs/tags/keep-local"); err != nil {
		t.Fatalf("collision removed local tag: %v", err)
	}

	// Simulate a lost push response: the seam publishes the captured object to
	// the bare remote, then returns the non-zero response that production must
	// reconcile by reading that exact direct ref.
	advance()
	r4 := &release{root: root, remote: remote, repo: "acme/repo", git: g, api: r.api, localFallbackOK: func(string) bool { return true }}
	var ambiguousTag, ambiguousOID string
	r4.push = func(_ context.Context, tag string, local localTagRef) error {
		ambiguousTag, ambiguousOID = tag, local.oid
		if _, err := g.run(context.Background(), "push", remote, local.oid+":refs/tags/"+tag); err != nil {
			t.Fatal(err)
		}
		return errors.New("simulated lost push response")
	}
	if err := r4.run(ci.BumpMinor, true, true); err != nil {
		t.Fatalf("ambiguous push reconciliation: %v", err)
	}
	if reconciled, err := g.ref(context.Background(), remote, "refs/tags/"+ambiguousTag); err != nil || reconciled.direct != ambiguousOID || reconciled.peeled != r4.head {
		t.Fatalf("ambiguous push ref = %+v, %v; want direct %s peeled %s", reconciled, err, ambiguousOID, r4.head)
	}
	if _, err := g.ref(context.Background(), remote, lockRef); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambiguous push stranded lock: %v", err)
	}
	if _, err := g.one(context.Background(), "rev-parse", "refs/tags/keep-local"); err != nil {
		t.Fatalf("ambiguous push removed local tag: %v", err)
	}

	// A closed output writer must return an error, but cannot bypass cleanup.
	brokenOutput, err := os.CreateTemp(t.TempDir(), "closed-output")
	if err != nil {
		t.Fatal(err)
	}
	if err := brokenOutput.Close(); err != nil {
		t.Fatal(err)
	}
	releaseStdout = brokenOutput
	t.Cleanup(func() { releaseStdout = oldOutput })
	advance()
	r5 := &release{root: root, remote: remote, repo: "acme/repo", git: g, api: r.api, localFallbackOK: func(string) bool { return true }}
	var closedOutputTag, closedOutputOID string
	r5.beforePush = func(tag string, local localTagRef) { closedOutputTag, closedOutputOID = tag, local.oid }
	if err := r5.run(ci.BumpMinor, true, true); err == nil {
		t.Fatal("closed output succeeded")
	}
	if local, err := g.localRef(context.Background(), "refs/tags/"+closedOutputTag); err != nil || local != "" {
		t.Fatalf("closed-output-owned local tag = %q, %v; want removed (created %s)", local, err, closedOutputOID)
	}
	if _, err := g.ref(context.Background(), remote, lockRef); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed output stranded lock: %v", err)
	}
	if _, err := g.one(context.Background(), "rev-parse", "refs/tags/keep-local"); err != nil {
		t.Fatalf("closed output removed local tag: %v", err)
	}
}
