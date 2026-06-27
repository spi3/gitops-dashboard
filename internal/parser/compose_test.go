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
    networks:
      frontend:
        ipv4_address: 10.10.10.20
    labels:
      - "traefik.http.routers.web.rule=Host('web.example.test')"
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
	if !contains(project.Services[1].Exposure, "http://10.10.10.20:80") {
		t.Fatalf("exposure = %v, want static IP route", project.Services[1].Exposure)
	}
	if !contains(project.Services[1].Exposure, "https://web.example.test") {
		t.Fatalf("exposure = %v, want traefik host route", project.Services[1].Exposure)
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
