package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTraefikRoutesSeparatesHTTPAndTCPEvidence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dynamic.yaml")
	if err := os.WriteFile(path, []byte(`
http:
  routers:
    web:
      rule: Host(`+"`web.example.test`"+`)
      service: web
  services:
    web:
      loadBalancer:
        servers:
          - url: "http://10.10.10.10:8080"
tcp:
  routers:
    db:
      rule: HostSNI(`+"`db.example.test`"+`)
      service: db
  services:
    db:
      loadBalancer:
        servers:
          - address: "db:5432"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	routes, tcpRoutes, err := ParseTraefikRoutes(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(routes) != 1 || routes[0].Service != "web" {
		t.Fatalf("routes = %#v, want only web HTTP evidence", routes)
	}
	if !contains(routes[0].Routes, "https://web.example.test") {
		t.Fatalf("web routes = %v, want Host() router rule", routes[0].Routes)
	}
	if !contains(routes[0].Routes, "http://10.10.10.10:8080") {
		t.Fatalf("web routes = %v, want backend server URL", routes[0].Routes)
	}

	if len(tcpRoutes) != 1 || tcpRoutes[0].Service != "db" {
		t.Fatalf("tcpRoutes = %#v, want only db TCP evidence", tcpRoutes)
	}
	endpoints := tcpRoutes[0].Endpoints
	if len(endpoints) != 2 {
		t.Fatalf("db endpoints = %#v, want SNI host and backend address", endpoints)
	}
	if !containsTCPEndpoint(endpoints, TCPEndpoint{Host: "db.example.test", SNI: true}) {
		t.Fatalf("db endpoints = %#v, want SNI-only router evidence with no invented port", endpoints)
	}
	if !containsTCPEndpoint(endpoints, TCPEndpoint{Host: "db", Port: "5432"}) {
		t.Fatalf("db endpoints = %#v, want backend address evidence", endpoints)
	}
}

func TestParseTraefikRoutesTCPRouterProducesNoHTTPRoute(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dynamic.yaml")
	if err := os.WriteFile(path, []byte(`
tcp:
  routers:
    db:
      rule: HostSNI(`+"`db.example.test`"+`)
      service: db
`), 0o600); err != nil {
		t.Fatal(err)
	}

	routes, tcpRoutes, err := ParseTraefikRoutes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("routes = %#v, want no HTTP route from a TCP router", routes)
	}
	if len(tcpRoutes) != 1 || len(tcpRoutes[0].Endpoints) != 1 {
		t.Fatalf("tcpRoutes = %#v, want one SNI endpoint", tcpRoutes)
	}
}

func TestParseTraefikRoutesPlainBackendAddressProducesNoHTTPRoute(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dynamic.yaml")
	if err := os.WriteFile(path, []byte(`
tcp:
  services:
    db:
      loadBalancer:
        servers:
          - address: "10.10.10.5:5432"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	routes, tcpRoutes, err := ParseTraefikRoutes(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 0 {
		t.Fatalf("routes = %#v, want no invented HTTP route for a TCP backend address", routes)
	}
	if len(tcpRoutes) != 1 || !containsTCPEndpoint(tcpRoutes[0].Endpoints, TCPEndpoint{Host: "10.10.10.5", Port: "5432"}) {
		t.Fatalf("tcpRoutes = %#v, want the backend address as TCP evidence", tcpRoutes)
	}
}

func TestTCPEndpointExposure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		endpoint TCPEndpoint
		want     string
	}{
		{name: "SNI host only", endpoint: TCPEndpoint{Host: "db.example.test", SNI: true}, want: "tcp/db.example.test"},
		{name: "host and port", endpoint: TCPEndpoint{Host: "db", Port: "5432"}, want: "tcp/db:5432"},
		{name: "IPv6 host and port", endpoint: TCPEndpoint{Host: "2001:db8::5", Port: "5432"}, want: "tcp/[2001:db8::5]:5432"},
		{name: "empty host", endpoint: TCPEndpoint{Port: "5432"}, want: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.endpoint.Exposure(); got != tc.want {
				t.Fatalf("Exposure() = %q, want %q", got, tc.want)
			}
		})
	}
}

func containsTCPEndpoint(endpoints []TCPEndpoint, target TCPEndpoint) bool {
	for _, endpoint := range endpoints {
		if endpoint == target {
			return true
		}
	}
	return false
}
