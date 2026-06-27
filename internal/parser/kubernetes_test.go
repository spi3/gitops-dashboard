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
  selector:
    app: api
  type: LoadBalancer
  loadBalancerIP: 10.122.122.50
  ports:
    - port: 80
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: api
  namespace: prod
spec:
  rules:
    - host: api.example.test
      http:
        paths:
          - path: /
            backend:
              service:
                name: api
                port:
                  number: 80
`), 0o600); err != nil {
		t.Fatal(err)
	}
	resources, err := ParseKubernetes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 3 {
		t.Fatalf("resources = %d, want 3", len(resources))
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
	service := resources[1]
	if service.Selector["app"] != "api" || !contains(service.Exposure, "http://10.122.122.50:80") {
		t.Fatalf("unexpected service networking: %#v", service)
	}
	ingress := resources[2]
	if len(ingress.Backends) != 1 || ingress.Backends[0] != "api" {
		t.Fatalf("ingress backends = %v, want api", ingress.Backends)
	}
	if !contains(ingress.Exposure, "https://api.example.test/") {
		t.Fatalf("ingress exposure = %v, want host route", ingress.Exposure)
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

func TestParseKubernetesSkipsNonObjectDocuments(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "playbook.yml")
	if err := os.WriteFile(path, []byte(`
- name: configure host
  hosts: all
  tasks:
    - name: debug
      debug:
        msg: ok
`), 0o600); err != nil {
		t.Fatal(err)
	}
	resources, err := ParseKubernetes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 0 {
		t.Fatalf("resources = %#v, want none", resources)
	}
}

func TestParseKubernetesCronJobAndHelmRelease(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gitops.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: batch/v1
kind: CronJob
metadata:
  name: renovate
  namespace: renovate
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: renovate
              image: renovate/renovate:43.243.1
              env:
                - name: RENOVATE_TOKEN
                  valueFrom:
                    secretKeyRef:
                      name: renovate-env
                      key: token
          restartPolicy: Never
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: plex-release
  namespace: plex
spec:
  chart:
    spec:
      chart: plex-media-server
      sourceRef:
        kind: HelmRepository
        name: plex-repo
  valuesFrom:
    - kind: ConfigMap
      name: plex-values
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
	cronJob := resources[0]
	if !cronJob.IsWorkload() || cronJob.Kind != "CronJob" || cronJob.Images[0] != "renovate/renovate:43.243.1" {
		t.Fatalf("unexpected cronjob: %#v", cronJob)
	}
	if len(cronJob.Warnings) != 0 {
		t.Fatalf("cronjob warnings = %v, want none", cronJob.Warnings)
	}
	if len(cronJob.ConfigRefs) != 1 || cronJob.ConfigRefs[0] != "secretKeyRef/renovate-env" {
		t.Fatalf("cronjob config refs = %v", cronJob.ConfigRefs)
	}
	helmRelease := resources[1]
	if !helmRelease.IsWorkload() || helmRelease.Kind != "HelmRelease" {
		t.Fatalf("unexpected helm release: %#v", helmRelease)
	}
	if len(helmRelease.Dependencies) != 1 || helmRelease.Dependencies[0] != "chart/plex-media-server" {
		t.Fatalf("helm dependencies = %v", helmRelease.Dependencies)
	}
	if len(helmRelease.ConfigRefs) != 2 {
		t.Fatalf("helm config refs = %v, want source and values refs", helmRelease.ConfigRefs)
	}
}

func TestParseKubernetesConfigMapRoutes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: plex-values
  namespace: plex
data:
  values.yaml: |
    ingress:
      enabled: true
      url: mpd.regulalabs.com
    grafana:
      ingress:
        enabled: true
        hosts:
          - grafana.regulalabs.com
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
	if !contains(resources[0].Exposure, "https://mpd.regulalabs.com") {
		t.Fatalf("exposure = %v, want plex route", resources[0].Exposure)
	}
	if !contains(resources[0].Exposure, "https://grafana.regulalabs.com") {
		t.Fatalf("exposure = %v, want grafana route", resources[0].Exposure)
	}
}
