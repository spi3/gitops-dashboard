package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCompose(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(`
services:
  web:
    image: example/web:v1
    ports:
      - "8080:80"
    depends_on:
      - db
    environment:
      - SECRET_TOKEN=redacted
      - LOG_LEVEL=debug
    volumes:
      - web-data:/data
  db:
    image: postgres:16
    healthcheck:
      test: ["CMD", "pg_isready"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := ParseCompose(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Services) != 2 {
		t.Fatalf("services = %d, want 2", len(project.Services))
	}
	if project.Services[1].Name != "web" {
		t.Fatalf("service order/name = %q, want web", project.Services[1].Name)
	}
	if len(project.Services[1].Warnings) != 1 {
		t.Fatalf("web warnings = %v, want missing healthcheck", project.Services[1].Warnings)
	}
	if got := project.Services[1].EnvVars; len(got) != 2 || got[0] != "LOG_LEVEL" || got[1] != "SECRET_TOKEN" {
		t.Fatalf("env vars = %v, want names only", got)
	}
}
