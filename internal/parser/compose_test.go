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
name: custom-stack
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
	if project.Name != "custom-stack" {
		t.Fatalf("project name = %q, want custom-stack", project.Name)
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

func TestParseComposeResolvesAnchoredAliases(t *testing.T) {
	t.Parallel()
	inlinePath := filepath.Join(t.TempDir(), "inline.yaml")
	if err := os.WriteFile(inlinePath, []byte(`
services:
  web:
    image: example/web:v1
    ports:
      - "127.0.0.1:8080:80"
    labels:
      - "traefik.http.routers.web.rule=Host('web.example.test')"
    environment:
      LOG_LEVEL: debug
      SECRET_TOKEN: redacted
`), 0o600); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(t.TempDir(), "alias.yaml")
	if err := os.WriteFile(aliasPath, []byte(`
x-ports: &ports
  - "127.0.0.1:8080:80"
x-labels: &labels
  - "traefik.http.routers.web.rule=Host('web.example.test')"
x-env: &env
  LOG_LEVEL: debug
  SECRET_TOKEN: redacted
services:
  web:
    image: example/web:v1
    ports: *ports
    labels: *labels
    environment: *env
`), 0o600); err != nil {
		t.Fatal(err)
	}

	inlineProject, err := ParseCompose(inlinePath)
	if err != nil {
		t.Fatal(err)
	}
	aliasProject, err := ParseCompose(aliasPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(inlineProject.Services) != 1 || len(aliasProject.Services) != 1 {
		t.Fatalf("services inline=%#v alias=%#v, want one each", inlineProject.Services, aliasProject.Services)
	}
	inlineService := inlineProject.Services[0]
	aliasService := aliasProject.Services[0]
	if !sameStringSlice(aliasService.Ports, inlineService.Ports) {
		t.Fatalf("aliased ports = %v, want %v", aliasService.Ports, inlineService.Ports)
	}
	if !sameStringSlice(aliasService.EnvVars, inlineService.EnvVars) {
		t.Fatalf("aliased env vars = %v, want %v", aliasService.EnvVars, inlineService.EnvVars)
	}
	if !sameStringSlice(aliasService.Exposure, inlineService.Exposure) {
		t.Fatalf("aliased exposure = %v, want %v", aliasService.Exposure, inlineService.Exposure)
	}
	if !sameStringSlice(aliasService.Warnings, inlineService.Warnings) {
		t.Fatalf("aliased warnings = %v, want %v", aliasService.Warnings, inlineService.Warnings)
	}
}

func TestParseComposeAliasCycleDoesNotHang(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(`
x-loop: &loop
  - *loop
services:
  web:
    image: example/web:v1
    ports: *loop
    labels: *loop
    environment: *loop
`), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := ParseCompose(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Services) != 1 {
		t.Fatalf("services = %#v, want one service", project.Services)
	}
	if len(project.Services[0].Exposure) != 0 {
		t.Fatalf("exposure = %v, want no routes from recursive alias", project.Services[0].Exposure)
	}
}

func TestParseComposeStaticAddressesRequireExplicitPortEvidence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(`
services:
  no-port:
    image: example/no-port:v1
    networks:
      app:
        ipv4_address: 10.10.10.127
  declared-target:
    image: example/target:v1
    ports:
      - "9000:8080"
    networks:
      app:
        ipv4_address: 10.10.10.128
  exposed:
    image: example/exposed:v1
    expose:
      - "8080"
      - "8443/tcp"
    networks:
      app:
        ipv4_address: 10.10.10.129
        ipv6_address: 2001:db8::1
  host-network:
    image: example/host:v1
    network_mode: host
    ports:
      - "127.0.0.1:9000:8080"
    expose:
      - "8080"
    networks:
      app:
        ipv4_address: 10.10.10.130
    labels:
      - "traefik.http.routers.host.rule=Host('host.example.test')"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := ParseCompose(path)
	if err != nil {
		t.Fatal(err)
	}
	services := map[string]ComposeService{}
	for _, service := range project.Services {
		services[service.Name] = service
	}
	if got := services["no-port"].Exposure; !sameStringSlice(got, []string{"address/10.10.10.127"}) {
		t.Fatalf("no-port exposure = %v, want non-monitorable static address only", got)
	}
	if got := services["declared-target"].Exposure; !sameStringSlice(got, []string{"http://10.10.10.128:8080"}) {
		t.Fatalf("declared-target exposure = %v, want target-port route", got)
	}
	for _, route := range []string{
		"http://10.10.10.129:8080",
		"https://10.10.10.129:8443",
		"http://[2001:db8::1]:8080",
		"https://[2001:db8::1]:8443",
	} {
		if !contains(services["exposed"].Exposure, route) {
			t.Fatalf("exposed routes = %v, want %q", services["exposed"].Exposure, route)
		}
	}
	if got := services["host-network"].Exposure; !sameStringSlice(got, []string{"https://host.example.test"}) {
		t.Fatalf("host-network exposure = %v, want only explicit router route", got)
	}
}

func TestParseComposePortRangesAndProtocols(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(`
services:
  ranges:
    image: example/ranges:v1
    ports:
      - "127.0.0.1:8080-8082:8080-8082"
    expose:
      - "9000-9001"
      - 9100
      - "9200-9201/udp"
    networks:
      app:
        ipv4_address: 10.10.10.140
  long-scalar:
    image: example/long-scalar:v1
    ports:
      - target: 7000
        published: 7000
        host_ip: 127.0.0.1
        protocol: tcp
      - target: 7300
        published: 7300
        host_ip: 127.0.0.1
        protocol: udp
    networks:
      app:
        ipv4_address: 10.10.10.141
  udp-only:
    image: example/udp:v1
    ports:
      - "127.0.0.1:514:514/udp"
    expose:
      - "515/udp"
      - "516/sctp"
    networks:
      app:
        ipv4_address: 10.10.10.142
`), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := ParseCompose(path)
	if err != nil {
		t.Fatal(err)
	}
	services := map[string]ComposeService{}
	for _, service := range project.Services {
		services[service.Name] = service
	}
	if got := services["ranges"].Exposure; !sameStringSlice(got, []string{
		"http://10.10.10.140:8080", "http://10.10.10.140:8081", "http://10.10.10.140:8082",
		"http://10.10.10.140:9000", "http://10.10.10.140:9001", "http://10.10.10.140:9100",
		"http://127.0.0.1:8080", "http://127.0.0.1:8081", "http://127.0.0.1:8082",
	}) {
		t.Fatalf("range exposure = %v", got)
	}
	if got := services["long-scalar"].Exposure; !sameStringSlice(got, []string{
		"http://10.10.10.141:7000", "http://127.0.0.1:7000",
	}) {
		t.Fatalf("long scalar exposure = %v", got)
	}
	if got := services["udp-only"].Exposure; !sameStringSlice(got, []string{"address/10.10.10.142"}) {
		t.Fatalf("udp exposure = %v, want static address only", got)
	}
}

func TestParseComposePortRangeAllocationPoolsDoNotInventPublishedRoutes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(`
services:
  short-allocation:
    image: example/short-allocation:v1
    ports:
      - "127.0.0.1:8000-8002:80"
    networks:
      app:
        ipv4_address: 10.10.10.160
  long-allocation:
    image: example/long-allocation:v1
    ports:
      - target: 81
        published: "8100-8102"
        host_ip: 127.0.0.1
        protocol: tcp
    networks:
      app:
        ipv4_address: 10.10.10.161
  short-equivalent:
    image: example/short-equivalent:v1
    ports:
      - "127.0.0.1:8200-8201:9200-9201"
  invalid-long-target:
    image: example/invalid-long-target:v1
    ports:
      - target: "9300-9301"
        published: "8300-8301"
        host_ip: 127.0.0.1
        protocol: tcp
`), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := ParseCompose(path)
	if err != nil {
		t.Fatal(err)
	}
	services := map[string]ComposeService{}
	for _, service := range project.Services {
		services[service.Name] = service
	}
	if got := services["short-allocation"].Exposure; !sameStringSlice(got, []string{"http://10.10.10.160:80"}) {
		t.Fatalf("short allocation exposure = %v, want static target route only", got)
	}
	if got := services["long-allocation"].Exposure; !sameStringSlice(got, []string{"http://10.10.10.161:81"}) {
		t.Fatalf("long allocation exposure = %v, want static target route only", got)
	}
	if got := services["short-equivalent"].Exposure; !sameStringSlice(got, []string{"http://127.0.0.1:8200", "http://127.0.0.1:8201"}) {
		t.Fatalf("short equivalent exposure = %v", got)
	}
	if got := services["invalid-long-target"].Exposure; len(got) != 0 {
		t.Fatalf("invalid long target exposure = %v, want no route", got)
	}
	if !containsWarning(services["invalid-long-target"].Warnings, "target range skipped") {
		t.Fatalf("invalid long target warnings = %v, want skipped-target-range warning", services["invalid-long-target"].Warnings)
	}
}

func TestParseComposeIPv6WildcardBindsUseDeclaredAddressOnly(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(`
services:
  bare-wildcard:
    image: example/bare:v1
    ports:
      - target: 80
        published: 8080
        host_ip: "::"
    networks:
      app:
        ipv4_address: 10.10.10.150
  bracketed-wildcard:
    image: example/bracketed:v1
    ports:
      - "[::]:8081:81"
    networks:
      app:
        ipv6_address: 2001:db8::150
`), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := ParseCompose(path)
	if err != nil {
		t.Fatal(err)
	}
	services := map[string]ComposeService{}
	for _, service := range project.Services {
		services[service.Name] = service
	}
	if got := services["bare-wildcard"].Exposure; !sameStringSlice(got, []string{"http://10.10.10.150:80"}) {
		t.Fatalf("bare wildcard exposure = %v", got)
	}
	if got := services["bracketed-wildcard"].Exposure; !sameStringSlice(got, []string{"http://[2001:db8::150]:81"}) {
		t.Fatalf("bracketed wildcard exposure = %v", got)
	}
}

func TestParseComposeRejectsAllIPv6UnspecifiedPublishedBinds(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(`
services:
  short-canonical:
    image: example/short-canonical:v1
    ports:
      - "[::]:8080:80"
  short-expanded:
    image: example/short-expanded:v1
    ports:
      - "[0:0:0:0:0:0:0:0]:8081:81"
  short-zero-compressed:
    image: example/short-zero-compressed:v1
    ports:
      - "[::0]:8082:82"
  short-ipv4-mapped:
    image: example/short-ipv4-mapped:v1
    ports:
      - "[::ffff:0.0.0.0]:8083:83"
  long-canonical:
    image: example/long-canonical:v1
    ports:
      - target: 84
        published: 8084
        host_ip: "::"
  long-expanded:
    image: example/long-expanded:v1
    ports:
      - target: 85
        published: 8085
        host_ip: "0:0:0:0:0:0:0:0"
  long-zero-compressed:
    image: example/long-zero-compressed:v1
    ports:
      - target: 86
        published: 8086
        host_ip: "::0"
  long-ipv4-mapped:
    image: example/long-ipv4-mapped:v1
    ports:
      - target: 87
        published: 8087
        host_ip: "::ffff:0.0.0.0"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := ParseCompose(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, service := range project.Services {
		if len(service.Exposure) != 0 {
			t.Fatalf("%s exposure = %v, want no route for unspecified bind", service.Name, service.Exposure)
		}
	}
}

func TestAccessRouteBracketsPortlessIPv6(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"2001:db8::2", "[2001:db8::2]"} {
		if got := accessRoute(host, ""); got != "http://[2001:db8::2]" {
			t.Fatalf("accessRoute(%q, no port) = %q, want bracketed IPv6 authority", host, got)
		}
	}
}

func TestParseComposeTreatsProjectNameInterpolationAsUnknown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "default interpolation", raw: "${GITOPS_DASHBOARD_TEST_STACK_NAME:-prod}", want: ""},
		{name: "unresolved interpolation", raw: "${GITOPS_DASHBOARD_TEST_STACK_NAME}", want: ""},
		{name: "unbraced interpolation", raw: "$GITOPS_DASHBOARD_TEST_STACK_NAME", want: ""},
		{name: "escaped dollar", raw: "foo$$bar", want: "foo$bar"},
		{name: "literal", raw: "custom-stack", want: "custom-stack"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			project := parseComposeProjectName(t, tc.raw)
			if project.Name != tc.want {
				t.Fatalf("project name = %q, want %q", project.Name, tc.want)
			}
		})
	}
}

func parseComposeProjectName(t *testing.T, name string) ComposeProject {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(`
name: `+name+`
services:
  web:
    image: example/web:v1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	project, err := ParseCompose(path)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
