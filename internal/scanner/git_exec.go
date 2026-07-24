package scanner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"sync/atomic"
)

// gitExecResult is the raw outcome of a single git subprocess invocation,
// before any redaction or error wrapping is applied by a caller such as
// gitOutput or gitConfigGetAll.
type gitExecResult struct {
	stdout string
	stderr string
	err    error
}

// gitExecFunc runs one git subprocess. It is the sole seam through which
// every git invocation in this package runs, letting scanner tests inject
// deterministic, command-specific failures without a real network or a shim
// binary on PATH.
type gitExecFunc func(ctx context.Context, dir string, env []string, args []string) gitExecResult

// currentGitExecFn holds the active gitExecFunc, initialized to
// productionGitExec. Access is through atomic.Value so concurrent scans
// (scheduled loops, coalesced ScanAll callers, and detached syncs) never
// race on it; only scanner tests ever substitute it, via withGitExec, and
// must restore the previous value before returning.
var currentGitExecFn atomic.Value

func init() {
	currentGitExecFn.Store(gitExecFunc(productionGitExec))
}

func productionGitExec(ctx context.Context, dir string, env []string, args []string) gitExecResult {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return gitExecResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

// invokeGit runs args through the currently active gitExecFunc.
func invokeGit(ctx context.Context, dir string, env []string, args ...string) gitExecResult {
	fn := currentGitExecFn.Load().(gitExecFunc)
	return fn(ctx, dir, env, args)
}
