package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseKubernetesMultiDocumentManifest(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: api
          image: example/api:v1
          ports:
            - containerPort: 8080
          envFrom:
            - configMapRef:
                name: api-config
---
apiVersion: v1
kind: Service
metadata:
  name: api
  namespace: prod
spec:
  ports:
    - port: 80
`), 0o600); err != nil {
		t.Fatal(err)
	}
	resources, err := ParseKubernetes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 2 {
		t.Fatalf("resources = %d, want 2", len(resources))
	}
	deployment := resources[0]
	if deployment.Kind != "Deployment" || deployment.Name != "api" || deployment.Namespace != "prod" {
		t.Fatalf("unexpected deployment: %#v", deployment)
	}
	if len(deployment.Images) != 1 || deployment.Images[0] != "example/api:v1" {
		t.Fatalf("images = %v", deployment.Images)
	}
	if len(deployment.Warnings) != 2 {
		t.Fatalf("warnings = %v, want readiness/liveness warnings", deployment.Warnings)
	}
	if len(deployment.ConfigRefs) != 1 || deployment.ConfigRefs[0] != "configMapRef/api-config" {
		t.Fatalf("config refs = %v", deployment.ConfigRefs)
	}
}

func TestParseKubernetesSecretDoesNotExposeValues(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "secret.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: v1
kind: Secret
metadata:
  name: db-password
data:
  password: c2VjcmV0
`), 0o600); err != nil {
		t.Fatal(err)
	}
	resources, err := ParseKubernetes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 {
		t.Fatalf("resources = %d, want 1", len(resources))
	}
	if resources[0].Kind != "Secret" || resources[0].Name != "db-password" {
		t.Fatalf("unexpected resource: %#v", resources[0])
	}
	if len(resources[0].ConfigRefs) != 0 || len(resources[0].Images) != 0 {
		t.Fatalf("secret exposed fields: %#v", resources[0])
	}
}
