// Package ci contains helpers shared by CI and release tooling.
package ci

import (
	"crypto/sha256"
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"

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
	var document yaml.Node
	if err := yaml.Unmarshal(workflow, &document); err != nil {
		return false, fmt.Errorf("parse workflow YAML: %w", err)
	}
	if len(document.Content) == 0 {
		return false, nil
	}
	on := mappingValue(document.Content[0], "on", map[*yaml.Node]bool{}, 0)
	dispatch := mappingValue(on, "workflow_dispatch", map[*yaml.Node]bool{}, 0)
	inputs := mappingValue(dispatch, "inputs", map[*yaml.Node]bool{}, 0)
	return mappingValue(inputs, "bump", map[*yaml.Node]bool{}, 0) != nil &&
		mappingValue(inputs, "dispatch_token", map[*yaml.Node]bool{}, 0) != nil, nil
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

// Allocate returns a new version or the single, provably identical prior
// allocation for source. A source commit may never be released twice with
// incompatible bump requests.
func Allocate(tags []Tag, source string, bump Bump) (Allocation, error) {
	if bump != BumpPatch && bump != BumpMinor && bump != BumpMajor {
		return Allocation{}, fmt.Errorf("unsupported version bump")
	}
	var sourceTags []Tag
	max := version{}
	for _, tag := range tags {
		parsed, ok := parseVersion(tag.Name)
		if !ok {
			logRejectedTag("invalid SemVer", tag.Name)
			continue
		}
		// Source ownership is checked before incrementability. An existing
		// release remains idempotent even if that component cannot be bumped.
		if tag.Commit == source {
			sourceTags = append(sourceTags, tag)
			continue
		}
		if !incrementable(parsed, bump) {
			logRejectedTag("non-incrementable SemVer", tag.Name)
			continue
		}
		if max.less(parsed) {
			max = parsed
		}
	}
	if len(sourceTags) > 0 {
		if len(sourceTags) != 1 || !sourceTagMatchesBump(sourceTags[0].Name, bump) {
			return Allocation{}, fmt.Errorf("source commit already has incompatible or ambiguous release tag")
		}
		return Allocation{Version: sourceTags[0].Name, Reused: true}, nil
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
