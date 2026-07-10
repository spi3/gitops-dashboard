package ci

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestAllocateVersion(t *testing.T) {
	for _, tt := range []struct {
		name string
		bump Bump
		tags []Tag
		want string
	}{
		{"bootstrap", BumpPatch, nil, "v0.0.1"}, {"numeric", BumpPatch, []Tag{{"v1.10.0", "x"}, {"v1.9.9", "x"}}, "v1.10.1"},
		{"existing unrelated source honors major", BumpMajor, []Tag{{"v1.2.3", "other"}}, "v2.0.0"}, {"huge component excluded", BumpPatch, []Tag{{"v9223372036854775808.0.0", "x"}}, "v0.0.1"},
		{"malformed ignored", BumpPatch, []Tag{{"v01.2.3", "x"}, {"v1.2.3-rc", "x"}, {"v1.2.3", "x"}}, "v1.2.4"},
		{"major overflow excluded", BumpMajor, []Tag{{"v9223372036854775807.0.0", "x"}, {"v2.0.0", "x"}}, "v3.0.0"},
		{"minor overflow excluded", BumpMinor, []Tag{{"v1.9223372036854775807.0", "x"}, {"v1.2.0", "x"}}, "v1.3.0"},
		{"patch overflow excluded", BumpPatch, []Tag{{"v1.2.9223372036854775807", "x"}, {"v1.2.3", "x"}}, "v1.2.4"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AllocateVersion(tt.tags, "source", tt.bump)
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v want %q", got, err, tt.want)
			}
		})
	}
}

func TestAllocateVersionSourceIdempotency(t *testing.T) {
	tags := []Tag{{"v1.2.0", "old"}, {"v1.3.0", "source"}}
	a, err := Allocate(tags, "source", BumpMinor)
	if err != nil || !a.Reused || a.Version != "v1.3.0" {
		t.Fatalf("identical retry = %#v, %v", a, err)
	}
	if _, err := Allocate(tags, "source", BumpMajor); err == nil {
		t.Fatal("different bump for an already released source was accepted")
	}
	if _, err := Allocate(append(tags, Tag{"v1.3.1", "source"}), "source", BumpMinor); err == nil {
		t.Fatal("ambiguous source release was accepted")
	}
}
func TestAllocateVersionSourceRetrySurvivesLaterTag(t *testing.T) {
	tags := []Tag{{"v1.2.0", "old"}, {"v1.3.0", "source"}, {"v1.3.1", "later"}}
	a, err := Allocate(tags, "source", BumpMinor)
	if err != nil || !a.Reused || a.Version != "v1.3.0" {
		t.Fatalf("retry after later tag = %#v, %v", a, err)
	}
}
func TestAllocateVersionSourceOwnsOverflowAdjacentTag(t *testing.T) {
	for _, tt := range []struct {
		bump Bump
		tag  string
	}{
		{BumpMajor, "v9223372036854775807.0.0"},
		{BumpMinor, "v1.9223372036854775807.0"},
		{BumpPatch, "v1.2.9223372036854775807"},
	} {
		t.Run(string(tt.bump), func(t *testing.T) {
			a, err := Allocate([]Tag{{tt.tag, "source"}}, "source", tt.bump)
			if err != nil || !a.Reused || a.Version != tt.tag {
				t.Fatalf("overflow-adjacent source retry = %#v, %v", a, err)
			}
		})
	}
}

func TestAllocateVersionRejectsZeroMajorAsMajorReuse(t *testing.T) {
	if _, err := Allocate([]Tag{{"v0.0.0", "source"}}, "source", BumpMajor); err == nil {
		t.Fatal("v0.0.0 was accepted as a major release retry")
	}
}

func TestAllocateVersionDoesNotLogRejectedTagContents(t *testing.T) {
	var output bytes.Buffer
	old := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(old) })
	for _, tag := range []string{
		"https://user:token-like-secret@example.invalid/repo",
		"token-like-secret",
	} {
		if _, err := AllocateVersion([]Tag{{tag, "other"}}, "source", BumpPatch); err != nil {
			t.Fatal(err)
		}
	}
	got := output.String()
	for _, secret := range []string{"token-like-secret", "user@example.invalid", "https://"} {
		if strings.Contains(got, secret) {
			t.Fatalf("rejected input leaked to logs: %q", got)
		}
	}
	if !strings.Contains(got, "sha256=") || !strings.Contains(got, "length=") {
		t.Fatalf("rejection diagnostic omitted safe classification: %q", got)
	}
}

func TestUnsupportedBumpDoesNotEchoInput(t *testing.T) {
	for _, bump := range []Bump{"token-like-secret", "https://user:token-like-secret@example.invalid"} {
		_, err := Allocate(nil, "source", bump)
		if err == nil || strings.Contains(err.Error(), string(bump)) || strings.Contains(err.Error(), "token-like-secret") {
			t.Fatalf("unsupported bump leaked input: %v", err)
		}
	}
}
func TestWorkflowSupportsAliases(t *testing.T) {
	ok, err := WorkflowSupportsReleaseDispatch([]byte("inputs: &release_inputs\n  bump: {type: choice}\n  dispatch_token: {type: string}\non:\n  workflow_dispatch:\n    inputs: *release_inputs\n"))
	if err != nil || !ok {
		t.Fatalf("got %v, %v", ok, err)
	}
}
