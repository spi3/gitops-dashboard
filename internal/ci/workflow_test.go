package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
	for job, want := range map[string]int{"test": 15, "build": 45, "publish": 45} {
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
