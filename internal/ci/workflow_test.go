package ci

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseWorkflowTriggersAndSerialization(t *testing.T) {
	t.Parallel()
	root := workflowNode(t, readWorkflow(t))
	dispatch := mappingValue(mappingValue(root, "on", nil, 0), "workflow_dispatch", nil, 0)
	if mappingValue(mappingValue(dispatch, "inputs", nil, 0), "bump", nil, 0) == nil || mappingValue(mappingValue(dispatch, "inputs", nil, 0), "expected_revision", nil, 0) == nil || mappingValue(mappingValue(dispatch, "inputs", nil, 0), "reconciliation_source", nil, 0) == nil {
		t.Fatal("workflow_dispatch inputs are incomplete")
	}
	release := mappingValue(mappingValue(root, "jobs", nil, 0), "release", nil, 0)
	if scalarValue(release, "needs") != "test" || !strings.Contains(scalarValue(release, "if"), "workflow_dispatch") || !strings.Contains(scalarValue(release, "if"), "github.ref == 'refs/heads/main'") {
		t.Fatal("release admission is not main-only and test-gated")
	}
	concurrency := mappingValue(release, "concurrency", nil, 0)
	if scalarValue(concurrency, "group") != "gitops-dashboard-release" || scalarValue(concurrency, "queue") != "" || scalarValue(concurrency, "cancel-in-progress") != "false" {
		t.Fatal("release concurrency must use the supported non-cancelling form")
	}
	permissions := mappingValue(release, "permissions", nil, 0)
	if scalarValue(permissions, "contents") != "write" || scalarValue(permissions, "packages") != "write" || scalarValue(permissions, "actions") != "write" {
		t.Fatal("release permissions are not scoped")
	}
}

func TestEmptyRemoteTagFixtureAllocatesFirstPatch(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "empty.git")
	if out, err := exec.Command("git", "init", "--bare", remote).CombinedOutput(); err != nil {
		t.Fatalf("create empty remote: %v: %s", err, out)
	}
	out, err := exec.Command("git", "ls-remote", "--tags", remote, "refs/tags/*").Output()
	if err != nil || len(out) != 0 {
		t.Fatalf("empty remote tags = %q, %v", out, err)
	}
	got, err := AllocateVersion(nil, "source", BumpPatch)
	if err != nil || got != "v0.0.1" {
		t.Fatalf("first allocation = %q, %v", got, err)
	}
}

func TestCheckedInWorkflowProvidesDispatchReleaseCapability(t *testing.T) {
	t.Parallel()
	ok, err := WorkflowSupportsReleaseDispatch([]byte(readWorkflow(t)))
	if err != nil {
		t.Fatalf("inspect checked-in workflow capability: %v", err)
	}
	if !ok {
		t.Fatal("cmd/release must dispatch only to a workflow that allocates and publishes a release")
	}
}

func TestReleaseWorkflowAllocatesAndPublishesAllMainTags(t *testing.T) {
	t.Parallel()
	release := mappingValue(mappingValue(workflowNode(t, readWorkflow(t)), "jobs", nil, 0), "release", nil, 0)
	encoded, err := yaml.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	releaseJob := string(encoded)
	// Step names and fields are parsed as YAML above; run snippets below are
	// executable behavior, not comments or broad document substrings.
	versionStep := namedStepNode(mappingValue(release, "steps", nil, 0), "Select effective release version")
	channelsStep := namedStepNode(mappingValue(release, "steps", nil, 0), "Publish moving image channels")
	releaseStep := namedStepNode(mappingValue(release, "steps", nil, 0), "Create or repair GitHub Release")
	releaseJob += "\n" + scalarValue(versionStep, "run") + "\n" + scalarValue(channelsStep, "run") + "\n" + scalarValue(releaseStep, "run")
	for _, want := range []string{
		`commit="$(git rev-parse HEAD)"`,
		`echo "short_sha=${commit::12}" >> "$GITHUB_OUTPUT"`,
		`git merge-base --is-ancestor "$commit" origin/main`,
		`go run ./cmd/version-allocator --source "$commit" --bump "$BUMP"`,
		`--force-with-lease="refs/tags/$version:"`,
		`linux/amd64,linux/arm64`,
		`-t "$IMAGE:sha-$SHORT_SHA"`,
		`highest_compatible_release()`,
		`git ls-remote --tags origin 'refs/tags/v*'`,
		`docker buildx imagetools create -t "$IMAGE:$minor" "$IMAGE@$minor_digest"`,
		`docker buildx imagetools create -t "$IMAGE:$major" "$IMAGE@$major_digest"`,
	} {
		if !strings.Contains(releaseJob, want) {
			t.Fatalf("release workflow missing %q", want)
		}
	}
	if !strings.Contains(releaseJob, `printf '%s' "$tags"`) || strings.Contains(releaseJob, `printf '%s\n' "$tags"`) {
		t.Fatal("empty remote tag output must remain empty for bootstrap allocation")
	}
	if !strings.Contains(releaseJob, `tolower($1) == "docker-content-digest:"`) {
		t.Fatal("immutable image digest header matching must be awk-portable")
	}
	if strings.Contains(releaseJob, "IGNORECASE") || strings.Contains(releaseJob, "targetCommitish") || !strings.Contains(releaseJob, "gitops-dashboard-release:start") {
		t.Fatal("release repair must use delimited metadata and peeled-tag authority")
	}
	if !strings.Contains(releaseJob, `if [[ "$latest_allowed" == true ]]; then`) || !strings.Contains(releaseJob, `prior_latest_digest="$(docker buildx imagetools inspect "$IMAGE:latest"`) || !strings.Contains(releaseJob, `[[ "$COMMIT" == "$(git rev-parse origin/main)" ]] && latest_allowed=true`) {
		t.Fatal("latest must be guarded by the current origin/main revision")
	}
	if !strings.Contains(releaseJob, `docker buildx imagetools create -t "$IMAGE:sha-$SHORT_SHA" "$IMAGE@$EXACT_DIGEST"`) {
		t.Fatal("sha tag must be published independently of latest")
	}
	for _, want := range []string{"Verify immutable exact image", "immutable image $VERSION has different version or revision metadata", "reuse=true", "Create or repair GitHub Release"} {
		if !strings.Contains(releaseJob, want) {
			t.Fatalf("release workflow lacks immutable exact-tag behavior %q", want)
		}
	}
	for _, disallowed := range []string{
		`short_sha="${GITHUB_SHA::12}"`,
		`echo "commit=${GITHUB_SHA}" >> "$GITHUB_OUTPUT"`,
		`git merge-base --is-ancestor "$GITHUB_SHA" origin/main`,
	} {
		if strings.Contains(releaseJob, disallowed) {
			t.Fatalf("release workflow must not use raw GITHUB_SHA for release metadata: found %q", disallowed)
		}
	}
}

func TestReleaseWorkflowGuardsStaleLatest(t *testing.T) {
	release := mappingValue(mappingValue(workflowNode(t, readWorkflow(t)), "jobs", nil, 0), "release", nil, 0)
	channels := scalarValue(namedStepNode(mappingValue(release, "steps", nil, 0), "Publish moving image channels"), "run")
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "commands.log")
	writeExecutable(t, filepath.Join(bin, "git"), `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  ls-remote) printf 'deadbeef\trefs/tags/v1.2.3\n' ;;
  fetch) count_file="${HARNESS_STATE}/fetches"; count=0; [[ -f "$count_file" ]] && count="$(cat "$count_file")"; echo $((count + 1)) > "$count_file" ;;
  rev-parse) count="$(cat "${HARNESS_STATE}/fetches")"; if [[ "$count" == 1 ]]; then printf '%s\n' "$COMMIT"; else printf '%s\n' "$ADVANCED_COMMIT"; fi ;;
  *) echo "unexpected git invocation: $*" >&2; exit 1 ;;
esac
`)
	writeExecutable(t, filepath.Join(bin, "docker"), `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *" inspect "* ]]; then
  if [[ "$*" == *":latest "* ]]; then printf 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'; else printf 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n'; fi
  exit 0
fi
if [[ "$*" == *" create "* ]]; then printf '%s\n' "$*" >> "$HARNESS_LOG"; exit 0; fi
echo "unexpected docker invocation: $*" >&2
exit 1
`)
	state := t.TempDir()
	output := filepath.Join(t.TempDir(), "github-output")
	cmd := exec.Command("bash", "-ceu", channels)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "HARNESS_STATE="+state, "HARNESS_LOG="+log,
		"GITHUB_OUTPUT="+output, "IMAGE=ghcr.io/acme/repo", "VERSION=v1.2.4",
		"COMMIT=1111111111111111111111111111111111111111", "ADVANCED_COMMIT=2222222222222222222222222222222222222222",
		"EXACT_DIGEST=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"MAIN_RELEASE=true", "SHORT_SHA=111111111111",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run production channel script in race harness: %v: %s", err, out)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	got := string(commands)
	if !strings.Contains(got, "-t ghcr.io/acme/repo:sha-111111111111 ghcr.io/acme/repo@sha256:bbbb") {
		t.Fatalf("stale queued run did not publish its commit-specific tag: %s", got)
	}
	if !strings.Contains(got, "-t ghcr.io/acme/repo:latest ghcr.io/acme/repo@sha256:bbbb") || !strings.Contains(got, "-t ghcr.io/acme/repo:latest ghcr.io/acme/repo@sha256:aaaa") {
		t.Fatalf("main-advance race did not publish then restore latest: %s", got)
	}
	result, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), "sha_published=true") || !strings.Contains(string(result), "latest_published=false") {
		t.Fatalf("race outputs do not describe actual published references: %s", result)
	}
}

func TestReleaseWorkflowNeverDeletesAndConvergesLatestWithoutPriorPointer(t *testing.T) {
	release := mappingValue(mappingValue(workflowNode(t, readWorkflow(t)), "jobs", nil, 0), "release", nil, 0)
	channels := scalarValue(namedStepNode(mappingValue(release, "steps", nil, 0), "Publish moving image channels"), "run")
	bin, state, log, output := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "commands.log"), filepath.Join(t.TempDir(), "github-output")
	writeExecutable(t, filepath.Join(bin, "git"), `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  ls-remote)
    printf '%s\trefs/tags/%s\n' "$COMMIT" "$VERSION"
    if [[ "${ADVANCED_RELEASE_AVAILABLE:-false}" == true ]]; then
      printf '%s\trefs/tags/v1.2.4\n' "$ADVANCED_COMMIT"
    fi
    ;;
  fetch) count_file="${HARNESS_STATE}/fetches"; count=0; [[ -f "$count_file" ]] && count="$(cat "$count_file")"; echo $((count + 1)) > "$count_file" ;;
  rev-parse)
    count="$(cat "${HARNESS_STATE}/fetches")"
    if [[ "${MAIN_ADVANCES:-false}" == true && "$count" != 1 ]]; then printf '%s\n' "$ADVANCED_COMMIT"; else printf '%s\n' "$COMMIT"; fi
    ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(t, filepath.Join(bin, "docker"), `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *"DELETE"* || "$*" == *" delete "* ]]; then
  printf 'registry deletion request: %s\n' "$*" >> "$HARNESS_LOG"
  exit 97
fi
if [[ "$*" == *" inspect "* ]]; then
  if [[ "$*" == *":latest "* ]]; then
    [[ -f "$HARNESS_STATE/latest" ]] && cat "$HARNESS_STATE/latest" || exit 1
  elif [[ "$*" == *":v1.2.4 "* && "${ADVANCED_RELEASE_AVAILABLE:-false}" == true ]]; then
    printf '%s\n' "$ADVANCED_DIGEST"
  else
    printf '%s\n' "$EXACT_DIGEST"
  fi
  exit 0
fi
if [[ "$*" == *" create "* ]]; then
  printf 'docker %s\n' "$*" >> "$HARNESS_LOG"
  if [[ "$*" == *"-t $IMAGE:latest "* ]]; then source="${!#}"; printf '%s\n' "${source##*@}" > "$HARNESS_STATE/latest"; fi
  exit 0
fi
exit 1
`)

	run := func(t *testing.T, version, commit, digest string, mainAdvances, advancedReleased bool) string {
		t.Helper()
		if err := os.Remove(filepath.Join(state, "fetches")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Remove(output); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-ceu", channels)
		cmd.Env = append(os.Environ(),
			"PATH="+bin+":"+os.Getenv("PATH"), "HARNESS_STATE="+state, "HARNESS_LOG="+log, "GITHUB_OUTPUT="+output,
			"IMAGE=ghcr.io/acme/repo", "VERSION="+version, "COMMIT="+commit, "ADVANCED_COMMIT=2222222222222222222222222222222222222222",
			"EXACT_DIGEST="+digest, "ADVANCED_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "MAIN_RELEASE=true", "SHORT_SHA="+commit[:12],
			"MAIN_ADVANCES="+strconv.FormatBool(mainAdvances), "ADVANCED_RELEASE_AVAILABLE="+strconv.FormatBool(advancedReleased),
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("run production absent-prior-latest race harness: %v: %s", err, out)
		}
		result, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		return string(result)
	}

	t.Run("advances to an already released newer main digest", func(t *testing.T) {
		result := run(t, "v1.2.3", "1111111111111111111111111111111111111111", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", true, true)
		latest, err := os.ReadFile(filepath.Join(state, "latest"))
		if err != nil || strings.TrimSpace(string(latest)) != "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" {
			t.Fatalf("latest after advancing to released main = %q, %v", latest, err)
		}
		if !strings.Contains(result, "latest_published=false") {
			t.Fatalf("advanced latest must not be claimed by the older release: %s", result)
		}
	})

	t.Run("leaves the successful digest until the newer release converges it", func(t *testing.T) {
		_ = os.Remove(filepath.Join(state, "latest"))
		first := run(t, "v1.2.3", "1111111111111111111111111111111111111111", "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", true, false)
		latest, err := os.ReadFile(filepath.Join(state, "latest"))
		if err != nil || strings.TrimSpace(string(latest)) != "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
			t.Fatalf("latest after unreleased main advance = %q, %v", latest, err)
		}
		if !strings.Contains(first, "latest_published=true") {
			t.Fatalf("successful held latest must be recorded: %s", first)
		}
		second := run(t, "v1.2.4", "2222222222222222222222222222222222222222", "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", false, false)
		latest, err = os.ReadFile(filepath.Join(state, "latest"))
		if err != nil || strings.TrimSpace(string(latest)) != "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" {
			t.Fatalf("latest after subsequent release convergence = %q, %v", latest, err)
		}
		if !strings.Contains(second, "latest_published=true") {
			t.Fatalf("newer successful release must publish latest: %s", second)
		}
	})

	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), "registry deletion request") || strings.Contains(channels, "DELETE") || strings.Contains(channels, "curl ") {
		t.Fatalf("channel workflow issued a registry deletion request: %s", commands)
	}
}

func TestReleaseWorkflowStaleQueuedRunPublishesSHAWithoutLatest(t *testing.T) {
	release := mappingValue(mappingValue(workflowNode(t, readWorkflow(t)), "jobs", nil, 0), "release", nil, 0)
	channels := scalarValue(namedStepNode(mappingValue(release, "steps", nil, 0), "Publish moving image channels"), "run")
	bin, state, log, output := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "commands.log"), filepath.Join(t.TempDir(), "github-output")
	writeExecutable(t, filepath.Join(bin, "git"), `#!/usr/bin/env bash
case "$1" in
  ls-remote) printf 'deadbeef\trefs/tags/v1.2.3\n' ;;
  fetch) : ;;
  rev-parse) printf '%s\n' "$ADVANCED_COMMIT" ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(t, filepath.Join(bin, "docker"), `#!/usr/bin/env bash
if [[ "$*" == *" inspect "* ]]; then printf 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'; exit 0; fi
if [[ "$*" == *" create "* ]]; then printf '%s\n' "$*" >> "$HARNESS_LOG"; exit 0; fi
exit 1
`)
	cmd := exec.Command("bash", "-ceu", channels)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HARNESS_STATE="+state, "HARNESS_LOG="+log, "GITHUB_OUTPUT="+output, "IMAGE=ghcr.io/acme/repo", "VERSION=v1.2.4", "COMMIT=1111111111111111111111111111111111111111", "ADVANCED_COMMIT=2222222222222222222222222222222222222222", "EXACT_DIGEST=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "MAIN_RELEASE=true", "SHORT_SHA=111111111111")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run production stale-queue channel script: %v: %s", err, out)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(commands), "-t ghcr.io/acme/repo:sha-111111111111") || strings.Contains(string(commands), "-t ghcr.io/acme/repo:latest") {
		t.Fatalf("stale queued references = %s", commands)
	}
	result, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result), "sha_published=true") || !strings.Contains(string(result), "latest_published=false") {
		t.Fatalf("stale queued outputs = %s", result)
	}
	releaseRun := scalarValue(namedStepNode(mappingValue(release, "steps", nil, 0), "Create or repair GitHub Release"), "run")
	start, end := strings.Index(releaseRun, "release_image_refs() {"), strings.Index(releaseRun, "generated_release_block() {")
	if start < 0 || end < start {
		t.Fatal("release image-ref helper missing")
	}
	refCmd := exec.Command("bash", "-ceu", releaseRun[start:end]+"\nrelease_image_refs")
	refCmd.Env = append(os.Environ(), "IMAGE=ghcr.io/acme/repo", "VERSION=v1.2.4", "DIGEST=sha256:exact", "MINOR_DIGEST=sha256:minor", "MAJOR_DIGEST=sha256:major", "SHORT_SHA=111111111111", "SHA_PUBLISHED=true", "LATEST_PUBLISHED=false")
	refs, err := refCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generate stale queued release references: %v: %s", err, refs)
	}
	if !strings.Contains(string(refs), "sha-111111111111") || strings.Contains(string(refs), ":latest") {
		t.Fatalf("stale queued release references = %s", refs)
	}
}

func TestReleaseWorkflowReconcilesDisplacedCurrentMain(t *testing.T) {
	release := mappingValue(mappingValue(workflowNode(t, readWorkflow(t)), "jobs", nil, 0), "release", nil, 0)
	reconcileStep := namedStepNode(mappingValue(release, "steps", nil, 0), "Reconcile displaced main release")
	reconcile := scalarValue(reconcileStep, "run")
	channels := scalarValue(namedStepNode(mappingValue(release, "steps", nil, 0), "Publish moving image channels"), "run")
	if reconcile == "" {
		t.Fatal("release workflow lacks displaced-main reconciliation")
	}
	if guard := scalarValue(reconcileStep, "if"); guard != "${{ !cancelled() }}" {
		t.Fatalf("displaced-main reconciliation guard = %q, want cancellation-safe !cancelled()", guard)
	}

	// Model the harmful ordering: A holds the release group, C is pending, and
	// B finishes tests last and replaces C. C allocated its tag and exact image,
	// then failed before latest, SHA, and Release publication. B must dispatch
	// C's idempotent same-source repair rather than treating the tag as done.
	const b = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const c = "cccccccccccccccccccccccccccccccccccccccc"
	const d = "dddddddddddddddddddddddddddddddddddddddd"
	bin, state := t.TempDir(), t.TempDir()
	dispatchLog, dockerLog := filepath.Join(state, "dispatch.log"), filepath.Join(state, "docker.log")
	writeExecutable(t, filepath.Join(bin, "git"), `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  fetch) exit 0 ;;
  rev-parse)
    [[ "$2" == HEAD ]] && { printf '%s\n' "$HEAD_COMMIT"; exit 0; }
    [[ "$2" == origin/main ]] && { printf '%s\n' "$MAIN_COMMIT"; exit 0; }
    exit 1 ;;
  merge-base) [[ "$2" == --is-ancestor && "$3" == "$RECONCILIATION_SOURCE" && "$4" == "$MAIN_COMMIT" ]] ;;
  ls-remote)
    # C receives a strict release tag only after the simulated dispatch runs.
    [[ -f "$HARNESS_STATE/released" ]] && printf '%s\trefs/tags/v1.2.4\n' "$C_COMMIT"
    exit 0 ;;
  *) exit 1 ;;
esac
`)
	writeExecutable(t, filepath.Join(bin, "gh"), `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *"--method POST"* ]]; then
  printf '%s\n' "$*" > "$HARNESS_DISPATCH_LOG"
  printf '%s\n' "$C_COMMIT" > "$HARNESS_STATE/queued"
  exit 0
fi
if [[ "$*" == *"matching-refs/tags/"* ]]; then
  printf '%s\n' '[{"ref":"refs/tags/v1.2.4","object":{"type":"tag","sha":"tag-object"}}]'
  exit 0
fi
if [[ "$*" == *"git/tags/tag-object"* ]]; then
  printf '%s\n' "$C_COMMIT"
  exit 0
fi
if [[ "$*" == *"releases/tags/"* ]]; then
  exit 1
fi
# C's original pending run was replaced by B, so the API has no active C run.
printf '%s\n' '{"workflow_runs":[]}'
`)
	writeExecutable(t, filepath.Join(bin, "docker"), `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *" inspect "* ]]; then
  if [[ "$*" == *":v1.2.4 "* ]]; then
    if [[ "${EXACT_IMAGE_AVAILABLE:-true}" == true ]]; then
      printf '%s\n' "$C_DIGEST"
      exit 0
    fi
    exit 1
  fi
  if [[ "$*" == *":sha-${C_COMMIT:0:12} "* || "$*" == *":latest "* ]]; then
    printf '%s\n' 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    exit 0
  fi
fi
exit 1
`)
	cmd := exec.Command("bash", "-ceu", reconcile)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "HARNESS_STATE="+state, "HARNESS_DISPATCH_LOG="+dispatchLog,
		"HEAD_COMMIT="+b, "MAIN_COMMIT="+c, "C_COMMIT="+c, "C_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "IMAGE=ghcr.io/acme/repo", "GITHUB_REPOSITORY=acme/repo", "EVENT_NAME=push", "RECONCILIATION_SOURCE=",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run production displaced-main reconciliation: %v: %s", err, out)
	}
	dispatch, err := os.ReadFile(dispatchLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workflows/ci.yml/dispatches", "inputs[bump]=patch", "inputs[expected_revision]=" + c, "inputs[reconciliation_source]=" + c, "inputs[dispatch_token]=reconcile-" + c[:12]} {
		if !strings.Contains(string(dispatch), want) {
			t.Fatalf("displaced C reconciliation did not dispatch %q: %s", want, dispatch)
		}
	}

	// A tag-only partial release fails before the exact image was published.
	// The production reconciliation script must still dispatch C's same-source
	// patch repair; the workflow-level !cancelled() guard ensures it is reached
	// after the failed channel-publication step without running after cancellation.
	if err := os.Remove(dispatchLog); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-ceu", reconcile)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "HARNESS_STATE="+state, "HARNESS_DISPATCH_LOG="+dispatchLog,
		"HEAD_COMMIT="+b, "MAIN_COMMIT="+c, "C_COMMIT="+c, "C_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "EXACT_IMAGE_AVAILABLE=false", "IMAGE=ghcr.io/acme/repo", "GITHUB_REPOSITORY=acme/repo", "EVENT_NAME=push", "RECONCILIATION_SOURCE=",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run tag-only displaced-main reconciliation: %v: %s", err, out)
	}
	dispatch, err = os.ReadFile(dispatchLog)
	if err != nil || !strings.Contains(string(dispatch), "inputs[reconciliation_source]="+c) {
		t.Fatalf("tag-only displaced C reconciliation did not dispatch same-source repair: %s, %v", dispatch, err)
	}

	// A marked repair may advance the bounded chain to D, but it must never
	// dispatch C again once C is the current head.
	if err := os.Remove(dispatchLog); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-ceu", reconcile)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "HARNESS_STATE="+state, "HARNESS_DISPATCH_LOG="+dispatchLog,
		"HEAD_COMMIT="+b, "MAIN_COMMIT="+c, "C_COMMIT="+c, "C_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "IMAGE=ghcr.io/acme/repo", "GITHUB_REPOSITORY=acme/repo", "EVENT_NAME=workflow_dispatch", "RECONCILIATION_SOURCE="+c,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run same-head reconciliation guard: %v: %s", err, out)
	}
	if _, err := os.Stat(dispatchLog); !os.IsNotExist(err) {
		t.Fatalf("same-head reconciliation dispatched again: %v", err)
	}

	cmd = exec.Command("bash", "-ceu", reconcile)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "HARNESS_STATE="+state, "HARNESS_DISPATCH_LOG="+dispatchLog,
		"HEAD_COMMIT="+c, "MAIN_COMMIT="+d, "C_COMMIT="+c, "C_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "IMAGE=ghcr.io/acme/repo", "GITHUB_REPOSITORY=acme/repo", "EVENT_NAME=workflow_dispatch", "RECONCILIATION_SOURCE="+c,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run newer-head reconciliation chain: %v: %s", err, out)
	}
	dispatch, err = os.ReadFile(dispatchLog)
	if err != nil || !strings.Contains(string(dispatch), "inputs[reconciliation_source]="+d) {
		t.Fatalf("reconciliation did not dispatch strictly newer D: %s, %v", dispatch, err)
	}

	// Simulate the dispatched C run completing its normal allocation, then run
	// the production channel script to prove latest converges to C. The fake
	// remote now exposes C's strict exact release tag, as allocation would.
	if err := os.WriteFile(filepath.Join(state, "released"), []byte(c), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "docker"), `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *"DELETE"* || "$*" == *" delete "* ]]; then
  printf 'registry deletion request: %s\n' "$*" >> "$HARNESS_DOCKER_LOG"
  exit 97
fi
if [[ "$*" == *" inspect "* ]]; then
  printf '%s\n' "$C_DIGEST"
  exit 0
fi
if [[ "$*" == *" create "* ]]; then
  printf '%s\n' "$*" >> "$HARNESS_DOCKER_LOG"
  if [[ "$*" == *"-t $IMAGE:latest "* ]]; then printf '%s\n' "$C_DIGEST" > "$HARNESS_STATE/latest"; fi
  exit 0
fi
exit 1
`)
	cmd = exec.Command("bash", "-ceu", channels)
	cmd.Env = append(os.Environ(),
		"PATH="+bin+":"+os.Getenv("PATH"), "HARNESS_STATE="+state, "HARNESS_DOCKER_LOG="+dockerLog,
		"HEAD_COMMIT="+c, "MAIN_COMMIT="+c, "C_COMMIT="+c, "IMAGE=ghcr.io/acme/repo", "VERSION=v1.2.4", "COMMIT="+c,
		"EXACT_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "C_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"MAIN_RELEASE=true", "SHORT_SHA="+c[:12], "GITHUB_OUTPUT="+filepath.Join(state, "github-output"),
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run simulated reconciled C channels: %v: %s", err, out)
	}
	latest, err := os.ReadFile(filepath.Join(state, "latest"))
	if err != nil || strings.TrimSpace(string(latest)) != "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("reconciled C did not converge latest: %q, %v", latest, err)
	}
	commands, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), "registry deletion request") || strings.Contains(readWorkflow(t), "--method DELETE") {
		t.Fatalf("reconciliation issued a deletion request: %s", commands)
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseWorkflowChannelsConvergeToHighestCompatibleRelease(t *testing.T) {
	t.Parallel()
	release := mappingValue(mappingValue(workflowNode(t, readWorkflow(t)), "jobs", nil, 0), "release", nil, 0)
	channels := scalarValue(namedStepNode(mappingValue(release, "steps", nil, 0), "Publish moving image channels"), "run")
	for _, want := range []string{
		`highest_compatible_release()`,
		`ref ~ /^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/`,
		`sort -u -V`,
		`minor_version="$(highest_compatible_release "^${minor}\\.[0-9]+$")"`,
		`major_version="$(highest_compatible_release "^${major}\\.[0-9]+\\.[0-9]+$")"`,
		`docker buildx imagetools create -t "$IMAGE:$minor" "$IMAGE@$minor_digest"`,
		`docker buildx imagetools create -t "$IMAGE:$major" "$IMAGE@$major_digest"`,
	} {
		if !strings.Contains(channels, want) {
			t.Fatalf("channel convergence behavior missing %q", want)
		}
	}
}

func TestReleaseWorkflowDelayedReleaseNotesResolveMovingChannelDigests(t *testing.T) {
	release := mappingValue(mappingValue(workflowNode(t, readWorkflow(t)), "jobs", nil, 0), "release", nil, 0)
	run := scalarValue(namedStepNode(mappingValue(release, "steps", nil, 0), "Create or repair GitHub Release"), "run")
	start, end := strings.Index(run, "release_image_refs() {"), strings.Index(run, "generated_release_block() {")
	if start < 0 || end < start {
		t.Fatal("release image-ref helper missing")
	}
	cmd := exec.Command("bash", "-ceu", run[start:end]+"\nrelease_image_refs")
	cmd.Env = append(os.Environ(),
		"IMAGE=ghcr.io/acme/repo", "VERSION=v1.2.3", "DIGEST=sha256:exact", "MINOR_DIGEST=sha256:newer-minor", "MAJOR_DIGEST=sha256:newer-major",
		"SHORT_SHA=0123456789ab", "SHA_PUBLISHED=true", "LATEST_PUBLISHED=false",
	)
	refs, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render delayed release references: %v: %s", err, refs)
	}
	got := string(refs)
	for _, want := range []string{
		"- ghcr.io/acme/repo:v1.2.3@sha256:exact",
		"- ghcr.io/acme/repo:v1.2@sha256:newer-minor",
		"- ghcr.io/acme/repo:v1@sha256:newer-major",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("delayed release metadata omits resolved reference %q: %s", want, got)
		}
	}
}

func TestGeneratedReleaseBlockValidationRequiresExactFields(t *testing.T) {
	release := mappingValue(mappingValue(workflowNode(t, readWorkflow(t)), "jobs", nil, 0), "release", nil, 0)
	run := scalarValue(namedStepNode(mappingValue(release, "steps", nil, 0), "Create or repair GitHub Release"), "run")
	const startMarker = "release_image_refs() {"
	const endMarker = "# generated-release-block validation end"
	start, end := strings.Index(run, startMarker), strings.Index(run, endMarker)
	if start < 0 || end < start {
		t.Fatal("release workflow lacks extractable generated-block helpers")
	}
	helpers := run[start:end]
	runHelpers := func(script string, input string) error {
		cmd := exec.Command("bash", "-ceu", helpers+"\n"+script)
		cmd.Stdin = strings.NewReader(input)
		cmd.Env = append(os.Environ(), "DIGEST=sha256:deadbeef", "COMMIT=0123456789abcdef", "IMAGE=ghcr.io/acme/repo", "VERSION=v1.2.3", "MINOR_DIGEST=sha256:minor", "MAJOR_DIGEST=sha256:major", "MAIN_RELEASE=true", "SHORT_SHA=0123456789ab", "SHA_PUBLISHED=true", "LATEST_PUBLISHED=true", "CREATED=2026-07-10T00:00:00Z")
		return cmd.Run()
	}
	if err := runHelpers("block=\"$(generated_release_block)\"\nvalid_generated_release_block \"$block\"", ""); err != nil {
		t.Fatalf("block generated by production helper rejected: %v", err)
	}
	if err := runHelpers("body=\"operator note\n$(generated_release_block)\"\nappend=0\nif ! release_needs_repair \"$body\"; then :; else append=1; fi\n[[ \"$append\" == 0 ]]", ""); err != nil {
		t.Fatalf("rerun would append to its own generated block: %v", err)
	}
	var output strings.Builder
	cmd := exec.Command("bash", "-ceu", helpers+"\ngenerated_release_block")
	cmd.Env = append(os.Environ(), "DIGEST=sha256:deadbeef", "COMMIT=0123456789abcdef", "IMAGE=ghcr.io/acme/repo", "VERSION=v1.2.3", "MINOR_DIGEST=sha256:minor", "MAJOR_DIGEST=sha256:major", "MAIN_RELEASE=true", "SHORT_SHA=0123456789ab", "SHA_PUBLISHED=true", "LATEST_PUBLISHED=true", "CREATED=2026-07-10T00:00:00Z")
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		t.Fatalf("generate production block: %v", err)
	}
	block := strings.TrimSuffix(output.String(), "\n")
	for _, field := range []string{
		"<!-- gitops-dashboard-release:start -->", "Digest: sha256:deadbeef", "Commit: 0123456789abcdef", "Timestamp: 2026-07-10T00:00:00Z", "Image refs:",
		"- ghcr.io/acme/repo:v1.2.3@sha256:deadbeef", "- ghcr.io/acme/repo:v1.2@sha256:minor", "- ghcr.io/acme/repo:v1@sha256:major", "- ghcr.io/acme/repo:latest@sha256:deadbeef", "- ghcr.io/acme/repo:sha-0123456789ab", "<!-- gitops-dashboard-release:end -->",
	} {
		t.Run("missing "+field, func(t *testing.T) {
			if err := runHelpers("block=\"$(cat)\"\nvalid_generated_release_block \"$block\"", strings.Replace(block, field, "", 1)); err == nil {
				t.Fatalf("validator accepted missing field %q", field)
			}
		})
		t.Run("corrupt "+field, func(t *testing.T) {
			corrupt := field + "-corrupt"
			if strings.HasPrefix(field, "Timestamp: ") {
				corrupt = "Timestamp:"
			}
			if err := runHelpers("block=\"$(cat)\"\nvalid_generated_release_block \"$block\"", strings.Replace(block, field, corrupt, 1)); err == nil {
				t.Fatalf("validator accepted corrupt field %q", field)
			}
		})
	}
}

func workflowNode(t *testing.T, text string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("parse workflow YAML: %v", err)
	}
	return doc.Content[0]
}

func TestUIStageBuildsOnNativeBuildPlatform(t *testing.T) {
	t.Parallel()
	if err := validateUIStageBuildPlatform(readDockerfile(t)); err != nil {
		t.Fatal(err)
	}
}

func TestUIStageBuildPlatformRejectsOtherPinnedStages(t *testing.T) {
	t.Parallel()
	for name, dockerfile := range map[string]string{
		"braced platform":               readDockerfile(t) + "\nFROM --platform=${BUILDPLATFORM} golang:1.24-alpine AS build\n",
		"braced platform with modifier": readDockerfile(t) + "\nFROM --platform=${BUILDPLATFORM:-$TARGETPLATFORM} golang:1.24-alpine AS build\n",
		"continued platform":            readDockerfile(t) + "\nFROM \\\n    --platform='$BUILDPLATFORM' \\\n    alpine:3.22 AS runtime\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateUIStageBuildPlatform(dockerfile); err == nil {
				t.Fatal("expected an additional BUILDPLATFORM-pinned stage to fail validation")
			}
		})
	}
}

func TestUIStageBuildPlatformRejectsSimilarVariableName(t *testing.T) {
	t.Parallel()
	dockerfile := strings.Replace(readDockerfile(t), "$BUILDPLATFORM", "$BUILDPLATFORM_OVERRIDE", 1)
	if err := validateUIStageBuildPlatform(dockerfile); err == nil {
		t.Fatal("expected ui stage with $BUILDPLATFORM_OVERRIDE to fail validation")
	}
}

func TestUIStageBuildPlatformRejectsMixedPlatformExpression(t *testing.T) {
	t.Parallel()
	dockerfile := strings.Replace(readDockerfile(t), "--platform=$BUILDPLATFORM", "--platform=${TARGETPLATFORM:-$BUILDPLATFORM}", 1)
	if err := validateUIStageBuildPlatform(dockerfile); err == nil {
		t.Fatal("expected ui stage with mixed platform expression to fail validation")
	}
}

func TestUIStageBuildPlatformAcceptsNormalizedPlatformExpression(t *testing.T) {
	t.Parallel()
	dockerfile := "FROM --platform=\"${BUILDPLATFORM}\" node:22-alpine AS ui # build frontend\nFROM alpine:3.22\n"
	if err := validateUIStageBuildPlatform(dockerfile); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowJobsHaveTimeouts(t *testing.T) {
	t.Parallel()
	if err := validateJobTimeouts(readWorkflow(t)); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowJobTimeoutsRejectCommentsAndStepTimeouts(t *testing.T) {
	t.Parallel()
	for name, workflow := range map[string]string{
		"comment only": `jobs:
  test:
    # timeout-minutes: 15
    steps: []
  build:
    timeout-minutes: 45
  publish:
    timeout-minutes: 45
`,
		"step level": `jobs:
  test:
    steps:
      - name: Test
        timeout-minutes: 15
  build:
    timeout-minutes: 45
  publish:
    timeout-minutes: 45
`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateJobTimeouts(workflow); err == nil {
				t.Fatal("expected non-job-level timeout-minutes to fail validation")
			}
		})
	}
}

type dockerStage struct {
	name               string
	platformExpression string
	buildPlatform      bool
}

func validateUIStageBuildPlatform(dockerfile string) error {
	stages := dockerStages(dockerfile)
	uiStages := 0
	for _, stage := range stages {
		if stage.name == "ui" {
			uiStages++
			if !isBuildPlatformExpression(stage.platformExpression) {
				return fmt.Errorf("ui stage must build on $BUILDPLATFORM")
			}
			continue
		}
		if stage.buildPlatform {
			return fmt.Errorf("only ui stage may build on $BUILDPLATFORM; found %q", stage.name)
		}
	}
	if uiStages != 1 {
		return fmt.Errorf("expected exactly one ui stage, found %d", uiStages)
	}
	return nil
}

func dockerStages(dockerfile string) []dockerStage {
	var stages []dockerStage
	for _, instruction := range logicalDockerfileInstructions(dockerfile) {
		fields := strings.Fields(instruction)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		stage := dockerStage{}
		for _, field := range fields[1:] {
			if platform, ok := platformExpression(field); ok {
				stage.platformExpression = platform
			}
			if platformReferencesBuildPlatform(field) {
				stage.buildPlatform = true
			}
		}
		for i := 1; i+1 < len(fields); i++ {
			if strings.EqualFold(fields[i], "AS") {
				stage.name = strings.ToLower(fields[i+1])
				break
			}
		}
		stages = append(stages, stage)
	}
	return stages
}

func logicalDockerfileInstructions(dockerfile string) []string {
	var instructions []string
	var current strings.Builder
	for _, line := range strings.Split(dockerfile, "\n") {
		line = strings.TrimSpace(stripDockerfileComment(line))
		if line == "" {
			continue
		}
		continued := strings.HasSuffix(line, "\\")
		if continued {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
		}
		if current.Len() > 0 && line != "" {
			current.WriteByte(' ')
		}
		current.WriteString(line)
		if continued {
			continue
		}
		instructions = append(instructions, current.String())
		current.Reset()
	}
	if current.Len() > 0 {
		instructions = append(instructions, current.String())
	}
	return instructions
}

func stripDockerfileComment(line string) string {
	var quote rune
	for i, char := range line {
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char == '#' {
			return line[:i]
		}
	}
	return line
}

func platformReferencesBuildPlatform(field string) bool {
	platform, ok := platformExpression(field)
	if !ok {
		return false
	}
	return referencesVariable(platform, "BUILDPLATFORM")
}

func platformExpression(field string) (string, bool) {
	platform, ok := strings.CutPrefix(field, "--platform=")
	if !ok {
		return "", false
	}
	return strings.Trim(platform, "\"'"), true
}

func isBuildPlatformExpression(platform string) bool {
	return platform == "$BUILDPLATFORM" || platform == "${BUILDPLATFORM}"
}

func referencesVariable(value, want string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] != '$' || i+1 == len(value) {
			continue
		}
		start := i + 1
		if value[start] == '{' {
			start++
		}
		if start == len(value) || !isVariableStart(value[start]) {
			continue
		}
		end := start + 1
		for end < len(value) && isVariablePart(value[end]) {
			end++
		}
		if value[start:end] == want {
			return true
		}
		i = end - 1
	}
	return false
}

func isVariableStart(char byte) bool {
	return char == '_' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z'
}

func isVariablePart(char byte) bool {
	return isVariableStart(char) || char >= '0' && char <= '9'
}

func validateJobTimeouts(workflow string) error {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(workflow), &document); err != nil {
		return fmt.Errorf("parse workflow YAML: %w", err)
	}
	if len(document.Content) != 1 {
		return fmt.Errorf("workflow YAML must contain one document")
	}
	jobs := mappingValue(document.Content[0], "jobs", map[*yaml.Node]bool{}, 0)
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return fmt.Errorf("workflow must define a jobs mapping")
	}
	for job, want := range map[string]int{"test": 15, "build": 45, "release": 45} {
		jobNode := mappingValue(jobs, job, map[*yaml.Node]bool{}, 0)
		if jobNode == nil || jobNode.Kind != yaml.MappingNode {
			return fmt.Errorf("workflow must define %s job", job)
		}
		timeout := mappingValue(jobNode, "timeout-minutes", map[*yaml.Node]bool{}, 0)
		if timeout == nil || timeout.Kind != yaml.ScalarNode || timeout.Tag != "!!int" {
			return fmt.Errorf("%s job must define numeric timeout-minutes", job)
		}
		got, err := strconv.Atoi(timeout.Value)
		if err != nil || got != want {
			return fmt.Errorf("%s job timeout-minutes = %q, want %d", job, timeout.Value, want)
		}
	}
	return nil
}

func readWorkflow(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	workflowPath := filepath.Join(filepath.Dir(filename), "..", "..", ".github", "workflows", "ci.yml")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readDockerfile(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dockerfilePath := filepath.Join(filepath.Dir(filename), "..", "..", "Dockerfile")
	data, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func workflowSection(t *testing.T, workflow, marker string) string {
	t.Helper()
	start := strings.Index(workflow, marker)
	if start < 0 {
		t.Fatalf("workflow section %q not found", marker)
	}
	return workflow[start:]
}
