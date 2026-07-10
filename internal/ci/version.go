// Package ci contains helpers shared by CI and release tooling.
package ci

import (
	"crypto/sha256"
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var semVerTag = regexp.MustCompile(`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)

type Bump string

const (
	BumpPatch Bump = "patch"
	BumpMinor Bump = "minor"
	BumpMajor Bump = "major"
)

type Tag struct {
	Name   string
	Commit string
}

// Allocation states whether a version is new or an idempotent reuse of a tag
// already attached to the requested source commit.
type Allocation struct {
	Version string
	Reused  bool
}

func WorkflowSupportsReleaseDispatch(workflow []byte) (bool, error) {
	_, ok, err := WorkflowReleaseJobName(workflow)
	return ok, err
}

// WorkflowReleaseJobName returns the GitHub Actions display name of jobs.release.
// The Actions jobs API reports this name, not the YAML job identifier.
func WorkflowReleaseJobName(workflow []byte) (string, bool, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(workflow, &document); err != nil {
		return "", false, fmt.Errorf("parse workflow YAML: %w", err)
	}
	if len(document.Content) == 0 {
		return "", false, nil
	}
	on := mappingValue(document.Content[0], "on", map[*yaml.Node]bool{}, 0)
	dispatch := mappingValue(on, "workflow_dispatch", map[*yaml.Node]bool{}, 0)
	inputs := mappingValue(dispatch, "inputs", map[*yaml.Node]bool{}, 0)
	for _, input := range []string{"bump", "dispatch_token", "expected_revision"} {
		if mappingValue(inputs, input, map[*yaml.Node]bool{}, 0) == nil {
			return "", false, nil
		}
	}
	if !strings.Contains(scalarValue(document.Content[0], "run-name"), "inputs.dispatch_token") {
		return "", false, nil
	}
	// Validate the release job as YAML structure, never as comment/name-string
	// substrings. cmd/release calls this before it delegates version allocation.
	jobs := mappingValue(document.Content[0], "jobs", map[*yaml.Node]bool{}, 0)
	release := mappingValue(jobs, "release", map[*yaml.Node]bool{}, 0)
	condition := scalarValue(release, "if")
	if release == nil || scalarValue(release, "needs") != "test" ||
		!strings.Contains(condition, "github.event_name == 'workflow_dispatch'") ||
		!strings.Contains(condition, "github.ref == 'refs/heads/main'") || strings.Contains(strings.ToLower(condition), "false") {
		return "", false, nil
	}
	jobName := scalarValue(release, "name")
	if strings.TrimSpace(jobName) == "" {
		return "", false, nil
	}
	concurrency := mappingValue(release, "concurrency", map[*yaml.Node]bool{}, 0)
	if scalarValue(concurrency, "group") != "gitops-dashboard-release" || scalarValue(concurrency, "queue") != "" || scalarValue(concurrency, "cancel-in-progress") != "false" {
		return "", false, nil
	}
	permissions := mappingValue(release, "permissions", map[*yaml.Node]bool{}, 0)
	if scalarValue(permissions, "contents") != "write" || scalarValue(permissions, "packages") != "write" {
		return "", false, nil
	}
	steps := mappingValue(release, "steps", map[*yaml.Node]bool{}, 0)
	for _, required := range []string{
		"Select effective release version",
		"Verify immutable exact image",
		"Build and publish immutable exact image",
		"Publish moving image channels",
		"Create or repair GitHub Release",
	} {
		if !namedStep(steps, required) {
			return "", false, nil
		}
	}
	for _, required := range []string{"Select effective release version", "Verify immutable exact image", "Publish moving image channels", "Create or repair GitHub Release"} {
		if strings.TrimSpace(scalarValue(namedStepNode(steps, required), "run")) == "" {
			return "", false, nil
		}
	}
	if !strings.Contains(scalarValue(namedStepNode(steps, "Select effective release version"), "run"), "version-allocator") || !strings.Contains(scalarValue(namedStepNode(steps, "Build and publish immutable exact image"), "if"), "reuse") || scalarValue(namedStepNode(steps, "Build and publish immutable exact image"), "uses") == "" {
		return "", false, nil
	}
	return jobName, true, nil
}

func scalarValue(node *yaml.Node, key string) string {
	v := mappingValue(node, key, map[*yaml.Node]bool{}, 0)
	if v == nil || v.Kind != yaml.ScalarNode {
		return ""
	}
	return v.Value
}
func namedStep(steps *yaml.Node, name string) bool { return namedStepNode(steps, name) != nil }
func namedStepNode(steps *yaml.Node, name string) *yaml.Node {
	steps = dereference(steps, map[*yaml.Node]bool{}, 0)
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return nil
	}
	for _, step := range steps.Content {
		if scalarValue(step, "name") == name {
			return step
		}
	}
	return nil
}

func dereference(n *yaml.Node, seen map[*yaml.Node]bool, depth int) *yaml.Node {
	if n == nil || depth > 64 {
		return nil
	}
	for n.Kind == yaml.AliasNode {
		if n.Alias == nil || seen[n] {
			return nil
		}
		seen[n] = true
		n = n.Alias
		depth++
	}
	return n
}
func mappingValue(node *yaml.Node, key string, seen map[*yaml.Node]bool, depth int) *yaml.Node {
	node = dereference(node, seen, depth)
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return dereference(node.Content[i+1], seen, depth+1)
		}
	}
	return nil
}

// AllocateVersion increments the highest strict SemVer release tag. Invalid
// tags (including impractically large components) are deliberately excluded:
// immutable junk must not block subsequent releases.
func AllocateVersion(tags []Tag, source string, bump Bump) (string, error) {
	a, err := Allocate(tags, source, bump)
	return a.Version, err
}

// Allocate returns a new version or a prior allocation for the same source and
// bump shape. A source may carry one auto-patch tag and later manual minor or
// major tags: different shapes are ordinary immutable version maxima, not
// collisions. This makes a rerun idempotent without preventing an operator
// from escalating a previously auto-patched commit.
func Allocate(tags []Tag, source string, bump Bump) (Allocation, error) {
	if bump != BumpPatch && bump != BumpMinor && bump != BumpMajor {
		return Allocation{}, fmt.Errorf("unsupported version bump")
	}
	max := version{}
	var reuse *version
	for _, tag := range tags {
		parsed, ok := parseVersion(tag.Name)
		if !ok {
			logRejectedTag("invalid SemVer", tag.Name)
			continue
		}
		if tag.Commit == source && sourceTagMatchesBump(tag.Name, bump) &&
			(reuse == nil || reuse.less(parsed)) {
			candidate := parsed
			reuse = &candidate
		}
		if !incrementable(parsed, bump) {
			logRejectedTag("non-incrementable SemVer", tag.Name)
			continue
		}
		if max.less(parsed) {
			max = parsed
		}
	}
	if reuse != nil {
		return Allocation{Version: reuse.String(), Reused: true}, nil
	}
	candidate := nextVersion(max, bump)
	return Allocation{Version: candidate.String()}, nil
}

func logRejectedTag(classification, tag string) {
	// Allocator input may be CI-controlled. Keep diagnostics useful without
	// copying possibly credential-bearing text into permanent logs.
	const maxReportedLength = 1024
	sum := sha256.Sum256([]byte(tag))
	length := len(tag)
	if length > maxReportedLength {
		length = maxReportedLength
	}
	log.Printf("release allocator: excluding %s tag (sha256=%x length=%d truncated=%t)", classification, sum[:8], length, len(tag) > maxReportedLength)
}

// sourceTagMatchesBump recognizes the shape uniquely produced by each bump.
// It deliberately does not consult later unrelated tags: retries are keyed to
// the source commit and requested bump, not to the current global maximum.
func sourceTagMatchesBump(name string, bump Bump) bool {
	v, ok := parseVersion(name)
	if !ok {
		return false
	}
	switch bump {
	case BumpMajor:
		return v.major > 0 && v.minor == 0 && v.patch == 0
	case BumpMinor:
		return v.minor > 0 && v.patch == 0
	case BumpPatch:
		return v.patch > 0
	default:
		return false
	}
}

func nextVersion(max version, bump Bump) version {
	switch bump {
	case BumpMajor:
		max.major++
		max.minor, max.patch = 0, 0
	case BumpMinor:
		max.minor++
		max.patch = 0
	case BumpPatch:
		max.patch++
	}
	return max
}

func incrementable(v version, bump Bump) bool {
	switch bump {
	case BumpMajor:
		return v.major < math.MaxInt64
	case BumpMinor:
		return v.minor < math.MaxInt64
	case BumpPatch:
		return v.patch < math.MaxInt64
	default:
		return false
	}
}

type version struct{ major, minor, patch int64 }

func parseVersion(tag string) (version, bool) {
	p := semVerTag.FindStringSubmatch(tag)
	if p == nil {
		return version{}, false
	}
	a, err := strconv.ParseInt(p[1], 10, 64)
	if err != nil {
		return version{}, false
	}
	b, err := strconv.ParseInt(p[2], 10, 64)
	if err != nil {
		return version{}, false
	}
	c, err := strconv.ParseInt(p[3], 10, 64)
	if err != nil {
		return version{}, false
	}
	return version{a, b, c}, true
}
func (v version) less(o version) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}
func (v version) String() string {
	return fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch)
}
