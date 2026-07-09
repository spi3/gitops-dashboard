package ci

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPublishWorkflowDoesNotAutoTagLatestOnReleaseTags(t *testing.T) {
	t.Parallel()
	workflow := readWorkflow(t)
	if !strings.Contains(workflow, "flavor: |\n            latest=false") {
		t.Fatal("publish metadata action must disable automatic latest tags")
	}
	if !strings.Contains(workflow, "type=raw,value=latest,enable=${{ github.ref == 'refs/heads/main' }}") {
		t.Fatal("publish workflow must keep the explicit latest tag gated to main")
	}
}

func TestPublishWorkflowUsesCheckedOutCommitForReleaseMetadata(t *testing.T) {
	t.Parallel()
	workflow := readWorkflow(t)
	publishJob := workflowSection(t, workflow, "  publish:")
	for _, want := range []string{
		`commit="$(git rev-parse HEAD)"`,
		`short_sha="${commit::12}"`,
		`echo "commit=${commit}" >> "$GITHUB_OUTPUT"`,
		`git merge-base --is-ancestor "$commit" origin/main`,
		`COMMIT=${{ steps.buildmeta.outputs.commit }}`,
		`org.opencontainers.image.revision=${{ steps.buildmeta.outputs.commit }}`,
	} {
		if !strings.Contains(publishJob, want) {
			t.Fatalf("publish workflow missing checked-out commit metadata wiring %q", want)
		}
	}
	for _, disallowed := range []string{
		`short_sha="${GITHUB_SHA::12}"`,
		`echo "commit=${GITHUB_SHA}" >> "$GITHUB_OUTPUT"`,
		`git merge-base --is-ancestor "$GITHUB_SHA" origin/main`,
	} {
		if strings.Contains(publishJob, disallowed) {
			t.Fatalf("publish workflow must not use raw GITHUB_SHA for release metadata: found %q", disallowed)
		}
	}
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

func workflowSection(t *testing.T, workflow, marker string) string {
	t.Helper()
	start := strings.Index(workflow, marker)
	if start < 0 {
		t.Fatalf("workflow section %q not found", marker)
	}
	return workflow[start:]
}
