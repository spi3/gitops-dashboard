package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestUnsupportedBumpDoesNotReachCLIStderr(t *testing.T) {
	secret := "https://user:token-like-secret@example.invalid"
	cmd := exec.Command("go", "run", ".", "--bump", secret, "--source", strings.Repeat("a", 40))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("unsupported bump was accepted")
	}
	if strings.Contains(string(out), secret) || strings.Contains(string(out), "token-like-secret") {
		t.Fatalf("CLI leaked unsupported bump: %s", out)
	}
}

func TestParseTagLineRejectsMalformedFields(t *testing.T) {
	good := "v1.2.3\t" + strings.Repeat("a", 40)
	for _, line := range []string{good + "\textra", "v1.2.3\t", "\t" + strings.Repeat("a", 40), " v1.2.3\t" + strings.Repeat("a", 40), "v1.2.3\t" + strings.Repeat("A", 40), "v1.2.3 " + strings.Repeat("a", 40)} {
		if _, err := parseTagLine(line); err == nil {
			t.Fatalf("parseTagLine accepted %q", line)
		}
	}
	if _, err := parseTagLine(good); err != nil {
		t.Fatalf("parseTagLine rejected valid line: %v", err)
	}
}
