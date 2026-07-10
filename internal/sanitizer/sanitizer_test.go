package sanitizer

import (
	"strings"
	"testing"
)

func TestRedactRemovesTokenInURLAndCommandText(t *testing.T) {
	token := "secret/token"
	input := "git clone https://x-access-token:secret%2Ftoken@example.com/org/repo.git failed; Authorization: Bearer secret/token"
	got := Redact(input, token)
	if strings.Contains(got, token) || strings.Contains(got, "secret%2Ftoken") {
		t.Fatalf("redacted value still contains token: %q", got)
	}
	if strings.Contains(got, "x-access-token:") {
		t.Fatalf("redacted value still contains URL userinfo: %q", got)
	}
}

func TestRedactRemovesUserinfoForms(t *testing.T) {
	cases := []string{
		"https://user@example.com/org/repo.git",
		"https://user:password@example.com/org/repo.git",
		"git fetch https://x-access-token:token@example.com/org/repo.git",
	}
	for _, input := range cases {
		got := Redact(input)
		if strings.Contains(got, "@example.com") {
			t.Fatalf("redacted value still contains userinfo for %q: %q", input, got)
		}
		if !strings.Contains(got, "https://example.com/org/repo.git") {
			t.Fatalf("redacted value removed URL host/path for %q: %q", input, got)
		}
	}
}

func TestURLUserinfoValuesIgnoresBareUsername(t *testing.T) {
	values := URLUserinfoValues("ssh://git@github.com/org/repo.git")
	if len(values) != 0 {
		t.Fatalf("URLUserinfoValues returned %#v, want no global tokens for bare username", values)
	}
	got := New(values...).Redact("git clone ssh://git@github.com/org/repo.git from github.com")
	if strings.Contains(got, "[REDACTED] clone") || strings.Contains(got, "[REDACTED]hub.com") {
		t.Fatalf("redacted common bare username globally: %q", got)
	}
	if !strings.Contains(got, "git clone") || !strings.Contains(got, "github.com") {
		t.Fatalf("redaction altered non-secret text: %q", got)
	}
	if !strings.Contains(got, "ssh://github.com") {
		t.Fatalf("redaction did not remove URL userinfo structurally: %q", got)
	}
}

func TestStripURLUserinfoStripsEmbeddedAndUsernameOnlyHTTPCredentials(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"routes: https://user:password@host.example/path", "routes: https://host.example/path"},
		{"https://token@host.example/path", "https://host.example/path"},
	} {
		if got := StripURLUserinfo(tt.in); got != tt.want {
			t.Errorf("StripURLUserinfo(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestURLUserinfoValuesRegistersPasswordSecrets(t *testing.T) {
	values := URLUserinfoValues("https://user:pass@example.com/org/repo.git")
	if len(values) != 2 {
		t.Fatalf("URLUserinfoValues returned %#v, want password and userinfo pair", values)
	}
	got := New(values...).Redact("git clone https://user:pass@example.com/org/repo.git failed with pass")
	if strings.Contains(got, "pass") || strings.Contains(got, "user:pass") {
		t.Fatalf("password was not redacted: %q", got)
	}
	if !strings.Contains(got, "git clone") || !strings.Contains(got, "https://example.com/org/repo.git") {
		t.Fatalf("redaction removed non-secret command or URL parts: %q", got)
	}
}
