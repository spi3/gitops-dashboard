package ci

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// auditStageName is the Dockerfile stage that inspects the ignore-filtered
// build context and produces the /audit-ok marker every local-copy stage
// must depend on before its first local COPY/ADD.
const auditStageName = "context-audit"

// allowedLocalCopySources is the exact, narrow set of local-context COPY
// sources the Dockerfile may reference. It is a policy allowlist, not a
// duplicated source manifest: it does not enumerate every file under
// "internal" or "cmd/gitops-dashboard", only the directories/files a stage
// may name as a COPY source.
var allowedLocalCopySources = map[string]bool{
	"go.mod":                          true,
	"go.sum":                          true,
	"cmd/gitops-dashboard":            true,
	"internal":                        true,
	"package.json":                    true,
	"package-lock.json":               true,
	"tsconfig.json":                   true,
	"vite.config.ts":                  true,
	"eslint.config.js":                true,
	"web":                             true,
	"Dockerfile":                      true,
	"docker-entrypoint.sh":            true,
	"scripts/docker-context-audit.sh": true,
}

// requiredFinalSensitivePatterns are the defined secret-bearing path classes
// that must be excluded from the Docker build context, and must appear as
// the final (non-overridable) .dockerignore rules.
var requiredFinalSensitivePatterns = []string{
	"data/**",
	"**/.env",
	"**/.env.*",
	"**/.kube/**",
	"**/kubeconfig",
	"**/kubeconfig.*",
	"**/id_rsa",
	"**/id_dsa",
	"**/id_ecdsa",
	"**/id_ed25519",
	"**/*.db",
	"**/*.db-shm",
	"**/*.db-wal",
	"**/*.sqlite",
	"**/*.sqlite-shm",
	"**/*.sqlite-wal",
	"**/*.sqlite3",
	"**/*.sqlite3-shm",
	"**/*.sqlite3-wal",
	"**/*.key",
	"**/*.pem",
	"**/*.p12",
	"**/*.pfx",
}

// TestDockerBuildContextPolicy parses the real checked-in Dockerfile and
// .dockerignore (never a duplicated source manifest) to enforce that: no
// stage performs a broad local copy; every local COPY/ADD source is
// explicitly allowlisted; every stage with a local copy has an explicit
// graph dependency on the context-audit stage before its first local copy;
// and .dockerignore is deny-first with the required sensitive-path rules as
// final, non-overridable entries.
//
// With GITOPS_DASHBOARD_DOCKER_CONTEXT_TEST=1 it additionally builds a real
// fixture through Docker: a missing Docker CLI or inaccessible daemon is a
// hard failure (never a skip), and the fixture proves that forbidden
// root/nested files are absent from the build context and that a
// private-key marker in an otherwise allowed source file fails the build
// before local copying.
func TestDockerBuildContextPolicy(t *testing.T) {
	dockerfile := readDockerfile(t)
	dockerignore := readDockerignore(t)

	validateDockerBuildContextPolicy(t, dockerfile, dockerignore)

	if os.Getenv("GITOPS_DASHBOARD_DOCKER_CONTEXT_TEST") != "1" {
		t.Skip("set GITOPS_DASHBOARD_DOCKER_CONTEXT_TEST=1 to run the Docker context-audit fixture")
	}
	runDockerContextAuditFixture(t)
}

func validateDockerBuildContextPolicy(t *testing.T, dockerfile, dockerignore string) {
	t.Helper()
	stages := parseDockerfileStages(dockerfile)
	if len(stages) == 0 {
		t.Fatal("Dockerfile contains no stages")
	}
	foundAudit := false
	for _, stage := range stages {
		if stage.name == auditStageName {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatalf("Dockerfile has no %q stage", auditStageName)
	}

	for _, stage := range stages {
		copies := parseCopyInstructions(t, stage)
		firstLocalIndex := -1
		auditDependencyIndex := -1
		for i, c := range copies {
			if c.local {
				if c.verb == "ADD" {
					t.Fatalf("stage %q uses ADD for a local copy %v; only COPY with an explicit allowlisted source is permitted", stage.name, c.sources)
				}
				for _, src := range c.sources {
					if src == "." || src == "./" {
						t.Fatalf("stage %q has a broad local copy of %q; explicit source paths are required", stage.name, src)
					}
					if !allowedLocalCopySources[src] {
						t.Fatalf("stage %q copies disallowed local source %q; it is not in the explicit allowlist", stage.name, src)
					}
				}
				if firstLocalIndex == -1 {
					firstLocalIndex = i
				}
				continue
			}
			if strings.EqualFold(c.fromStage, auditStageName) && auditDependencyIndex == -1 {
				auditDependencyIndex = i
			}
		}
		if firstLocalIndex == -1 || stage.name == auditStageName {
			continue
		}
		if auditDependencyIndex == -1 || auditDependencyIndex > firstLocalIndex {
			t.Fatalf("stage %q must COPY --from=%s before its first local COPY/ADD %v; Dockerfile textual order alone is not an explicit graph dependency", stage.name, auditStageName, copies[firstLocalIndex].sources)
		}
	}

	validateDockerignorePolicy(t, dockerignore)
}

func validateDockerignorePolicy(t *testing.T, dockerignore string) {
	t.Helper()
	lines := dockerignoreLines(dockerignore)
	if len(lines) == 0 || lines[0] != "*" {
		t.Fatalf(".dockerignore must be deny-first: the first effective rule must be exactly \"*\"; got %#v", lines)
	}
	lastAllowIndex := -1
	patternIndex := map[string]int{}
	for i, line := range lines {
		if strings.HasPrefix(line, "!") {
			lastAllowIndex = i
		}
		patternIndex[line] = i
	}
	for _, pattern := range requiredFinalSensitivePatterns {
		idx, ok := patternIndex[pattern]
		if !ok {
			t.Fatalf(".dockerignore is missing the required sensitive-path rule %q", pattern)
		}
		if idx < lastAllowIndex {
			t.Fatalf(".dockerignore rule %q (line %d) is not final: an allow rule at line %d could re-include it", pattern, idx, lastAllowIndex)
		}
	}
}

func dockerignoreLines(dockerignore string) []string {
	var lines []string
	for _, raw := range strings.Split(dockerignore, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

type dockerfileStage struct {
	name         string
	instructions []string
}

func parseDockerfileStages(dockerfile string) []dockerfileStage {
	var stages []dockerfileStage
	for _, instruction := range logicalDockerfileInstructions(dockerfile) {
		fields := strings.Fields(instruction)
		if len(fields) == 0 {
			continue
		}
		if strings.EqualFold(fields[0], "FROM") {
			stage := dockerfileStage{}
			for i := 1; i+1 < len(fields); i++ {
				if strings.EqualFold(fields[i], "AS") {
					stage.name = strings.ToLower(fields[i+1])
					break
				}
			}
			stages = append(stages, stage)
			continue
		}
		if len(stages) == 0 {
			continue
		}
		last := &stages[len(stages)-1]
		last.instructions = append(last.instructions, instruction)
	}
	return stages
}

type copyInstruction struct {
	verb      string
	fromStage string
	sources   []string
	local     bool
}

func parseCopyInstructions(t *testing.T, stage dockerfileStage) []copyInstruction {
	t.Helper()
	var copies []copyInstruction
	for _, instruction := range stage.instructions {
		fields := strings.Fields(instruction)
		if len(fields) < 2 {
			continue
		}
		verb := strings.ToUpper(fields[0])
		if verb != "COPY" && verb != "ADD" {
			continue
		}
		var fromStage string
		var positional []string
		for _, field := range fields[1:] {
			if value, ok := strings.CutPrefix(field, "--from="); ok {
				fromStage = strings.ToLower(strings.Trim(value, "\"'"))
				continue
			}
			if strings.HasPrefix(field, "--") {
				continue
			}
			positional = append(positional, field)
		}
		if len(positional) < 2 {
			t.Fatalf("stage %q: could not parse %s instruction %q", stage.name, verb, instruction)
		}
		copies = append(copies, copyInstruction{
			verb:      verb,
			fromStage: fromStage,
			sources:   positional[:len(positional)-1],
			local:     fromStage == "",
		})
	}
	return copies
}

func readDockerignore(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", ".dockerignore")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// runDockerContextAuditFixture builds a real, temporary copy of the build
// context, augmented with forbidden-path fixture files and a private-key
// marker in an otherwise allowed source file, then runs `docker build
// --target build` against it. The build must fail: the fixture proves the
// forbidden files never reach the daemon (via the audit stage's own
// defense-in-depth report) and that the marker is rejected before any local
// COPY into the ui, build, or runtime stages.
func runDockerContextAuditFixture(t *testing.T) {
	t.Helper()
	dockerPath, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("docker CLI not found; GITOPS_DASHBOARD_DOCKER_CONTEXT_TEST=1 requires it to be present: %v", err)
	}
	infoCtx, cancelInfo := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelInfo()
	if out, err := exec.CommandContext(infoCtx, dockerPath, "info").CombinedOutput(); err != nil {
		t.Fatalf("docker daemon not accessible; GITOPS_DASHBOARD_DOCKER_CONTEXT_TEST=1 requires it to be reachable: %v\n%s", err, out)
	}

	repoRoot := repositoryRoot(t)
	fixtureDir := t.TempDir()
	copyFixtureSources(t, repoRoot, fixtureDir)

	const sentinel = "t060-forbidden-fixture-sentinel-do-not-leak"
	for _, rel := range []string{
		filepath.Join("data", "leaked.db"),
		".env",
		filepath.Join("nested", ".kube", "config"),
		filepath.Join("deep", "dir", "id_rsa"),
		filepath.Join("deep", "dir", "tls.pem"),
	} {
		writeFixtureFile(t, filepath.Join(fixtureDir, rel), sentinel+"\n")
	}

	// Assemble the marker at runtime instead of embedding the literal PEM
	// header contiguously in this test's source, mirroring why the audit
	// script itself splits the marker: a contiguous literal here would be a
	// false-positive hit for any secret scanner run over this repository.
	markerHead := "-----BEGIN"
	markerTail := "PRIVATE KEY-----"
	marker := markerHead + " " + markerTail
	writeFixtureFile(t, filepath.Join(fixtureDir, "internal", "scanner", "context_audit_fixture_marker.txt"), marker+"\nfixture-only content, not a real key\n")

	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelBuild()
	cmd := exec.CommandContext(buildCtx, dockerPath, "build", "--target", "build", "-t", "gitops-dashboard:t060-context-fixture", fixtureDir)
	output, buildErr := cmd.CombinedOutput()
	combined := string(output)
	if buildErr == nil {
		t.Fatalf("docker build succeeded, want rejection of marker-bearing allowed content before local copying:\n%s", combined)
	}
	if strings.Contains(combined, sentinel) {
		t.Fatalf("build output contains the forbidden-fixture sentinel; it must never reach the daemon:\n%s", combined)
	}
	if !strings.Contains(combined, "docker-context-audit: forbidden path classes absent") {
		t.Fatalf("build output does not confirm forbidden root/nested files were absent from the build context:\n%s", combined)
	}
	if strings.Contains(combined, "present in build context") {
		t.Fatalf(".dockerignore did not exclude a forbidden fixture path from the build context:\n%s", combined)
	}
	if !strings.Contains(combined, "private-key marker found in allowed build context content") {
		t.Fatalf("build output does not show the marker-bearing allowed file was rejected:\n%s", combined)
	}
}

func copyFixtureSources(t *testing.T, repoRoot, dst string) {
	t.Helper()
	for _, entry := range []string{
		"Dockerfile", ".dockerignore", "docker-entrypoint.sh",
		"go.mod", "go.sum",
		"package.json", "package-lock.json", "tsconfig.json", "vite.config.ts", "eslint.config.js",
		"cmd", "internal", "web", "scripts",
	} {
		copyFixturePath(t, filepath.Join(repoRoot, entry), filepath.Join(dst, entry))
	}
}

func copyFixturePath(t *testing.T, src, dst string) {
	t.Helper()
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		copyFixtureContent(t, src, dst, info.Mode())
		return
	}
	if err := filepath.WalkDir(src, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		copyFixtureContent(t, path, target, info.Mode())
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func copyFixtureContent(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		t.Fatal(err)
	}
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
