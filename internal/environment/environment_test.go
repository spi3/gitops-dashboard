package environment

import "testing"

func TestInferFromFolderName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"clusters/prod/app.yaml":        "production",
		"services/staging/api.yml":      "staging",
		"docker/dev/compose.yaml":       "development",
		"homelab/media/compose.yaml":    "homelab",
		"apps/local/docker-compose.yml": "local",
		"apps/unknown/service.yaml":     "",
	}
	for path, want := range tests {
		if got := Infer(path); got != want {
			t.Fatalf("Infer(%q) = %q, want %q", path, got, want)
		}
	}
}
