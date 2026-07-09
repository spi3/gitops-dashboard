package monitor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/routetarget"
)

func TestHTTPRoutePolicyBlocksDNSResolvedDeniedIP(t *testing.T) {
	t.Parallel()
	policy := mustRoutePolicy(t, routetarget.EgressPolicyConfig{})
	client := policyHTTPClientWithResolver(&http.Client{Timeout: time.Second}, policy, staticRouteResolver{
		"public.example.test": {{IP: net.ParseIP("169.254.169.254")}},
	})

	health, message := checkHTTPRoute(context.Background(), client, "http://public.example.test")
	if health != core.HealthNotApplicable || !strings.Contains(message, "blocked by policy: default deny cidr 169.254.0.0/16") {
		t.Fatalf("status = %s/%q, want policy block", health, message)
	}
}

func TestHTTPRoutePolicyAllowsCIDROnlyHostnameAfterResolution(t *testing.T) {
	t.Parallel()
	policy := mustRoutePolicy(t, routetarget.EgressPolicyConfig{
		AllowCIDRs: []string{"10.0.0.0/8"},
	})
	target := mustURL(t, "http://app.internal")
	err := checkPolicyURLDestination(context.Background(), target, policy, staticRouteResolver{
		"app.internal": {{IP: net.ParseIP("10.42.0.15")}},
	})
	if err != nil {
		t.Fatalf("checkPolicyURLDestination() = %v, want allowed", err)
	}
}

func TestHTTPRoutePolicyBlocksCIDROnlyHostnameOutsideAllow(t *testing.T) {
	t.Parallel()
	policy := mustRoutePolicy(t, routetarget.EgressPolicyConfig{
		AllowCIDRs: []string{"10.0.0.0/8"},
	})
	target := mustURL(t, "http://app.internal")
	err := checkPolicyURLDestination(context.Background(), target, policy, staticRouteResolver{
		"app.internal": {{IP: net.ParseIP("192.0.2.10")}},
	})
	var policyErr routePolicyError
	if !errors.As(err, &policyErr) || !strings.Contains(err.Error(), "no allow cidr rule matched") {
		t.Fatalf("checkPolicyURLDestination() = %v, want CIDR allow miss", err)
	}
}

func TestHTTPRoutePolicyBlocksRedirectToDeniedDestination(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer server.Close()

	policy := mustRoutePolicy(t, routetarget.EgressPolicyConfig{})
	health, message := checkHTTPRouteWithPolicy(context.Background(), &http.Client{Timeout: time.Second}, server.URL, policy)
	if health != core.HealthNotApplicable || !strings.Contains(message, "blocked by policy: default deny cidr 169.254.0.0/16") {
		t.Fatalf("status = %s/%q, want redirect policy block", health, message)
	}
}

func TestHTTPRoutePolicyAllowsMultiHopRedirectWithinPolicy(t *testing.T) {
	t.Parallel()
	finalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer finalServer.Close()

	middleServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, finalServer.URL, http.StatusFound)
	}))
	defer middleServer.Close()

	startServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, middleServer.URL, http.StatusFound)
	}))
	defer startServer.Close()

	policy := mustRoutePolicy(t, routetarget.EgressPolicyConfig{})
	health, message := checkHTTPRouteWithPolicy(context.Background(), &http.Client{Timeout: time.Second}, startServer.URL, policy)
	if health != core.HealthHealthy {
		t.Fatalf("status = %s/%q, want healthy allowed redirect chain", health, message)
	}
}

func TestRoutePolicyDialerPinsResolvedIP(t *testing.T) {
	t.Parallel()
	errDialStopped := errors.New("dial stopped")
	recorder := &recordingRouteDialer{err: errDialStopped}
	dialer := routePolicyDialer{
		policy: mustRoutePolicy(t, routetarget.EgressPolicyConfig{
			AllowDomains: []string{"app.example.test"},
		}),
		resolver: staticRouteResolver{
			"app.example.test": {{IP: net.ParseIP("203.0.113.10")}},
		},
		dialer: recorder,
	}

	_, err := dialer.DialContext(context.Background(), "tcp", "app.example.test:443")
	if !errors.Is(err, errDialStopped) {
		t.Fatalf("DialContext error = %v, want sentinel dial error", err)
	}
	if got, want := recorder.addresses, []string{"203.0.113.10:443"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("dialed addresses = %#v, want pinned IP %#v", got, want)
	}
}

func TestRoutePolicyDialerFallsBackAcrossAllowedAddresses(t *testing.T) {
	t.Parallel()
	recorder := &recordingRouteDialer{
		failures: map[string]error{"10.0.0.1:443": errors.New("connection refused")},
		success:  map[string]bool{"10.0.0.2:443": true},
	}
	dialer := routePolicyDialer{
		policy: mustRoutePolicy(t, routetarget.EgressPolicyConfig{
			AllowCIDRs: []string{"10.0.0.0/8"},
		}),
		resolver: staticRouteResolver{
			"app.internal": {
				{IP: net.ParseIP("10.0.0.1")},
				{IP: net.ParseIP("10.0.0.2")},
			},
		},
		dialer: recorder,
	}

	conn, err := dialer.DialContext(context.Background(), "tcp", "app.internal:443")
	if err != nil {
		t.Fatalf("DialContext() = %v, want fallback success", err)
	}
	_ = conn.Close()
	if got, want := recorder.addresses, []string{"10.0.0.1:443", "10.0.0.2:443"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dialed addresses = %#v, want resolver-order fallback %#v", got, want)
	}
}

func TestRoutePolicyDialerPreservesDualStackResolverOrder(t *testing.T) {
	t.Parallel()
	recorder := &recordingRouteDialer{
		failures: map[string]error{"[2001:db8::1]:443": errors.New("network unreachable")},
		success:  map[string]bool{"10.0.0.2:443": true},
	}
	dialer := routePolicyDialer{
		policy: mustRoutePolicy(t, routetarget.EgressPolicyConfig{
			AllowCIDRs: []string{"2001:db8::/32", "10.0.0.0/8"},
		}),
		resolver: staticRouteResolver{
			"dual.internal": {
				{IP: net.ParseIP("2001:db8::1")},
				{IP: net.ParseIP("10.0.0.2")},
			},
		},
		dialer: recorder,
	}

	conn, err := dialer.DialContext(context.Background(), "tcp", "dual.internal:443")
	if err != nil {
		t.Fatalf("DialContext() = %v, want dual-stack fallback success", err)
	}
	_ = conn.Close()
	if got, want := recorder.addresses, []string{"[2001:db8::1]:443", "10.0.0.2:443"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dialed addresses = %#v, want dual-stack resolver order %#v", got, want)
	}
}

func TestRoutePolicyDialerBlocksWhenAnyResolvedIPIsDenied(t *testing.T) {
	t.Parallel()
	recorder := &recordingRouteDialer{}
	dialer := routePolicyDialer{
		policy: mustRoutePolicy(t, routetarget.EgressPolicyConfig{
			AllowDomains: []string{"app.example.test"},
		}),
		resolver: staticRouteResolver{
			"app.example.test": {
				{IP: net.ParseIP("203.0.113.10")},
				{IP: net.ParseIP("169.254.169.254")},
			},
		},
		dialer: recorder,
	}

	_, err := dialer.DialContext(context.Background(), "tcp", "app.example.test:443")
	var policyErr routePolicyError
	if !errors.As(err, &policyErr) || !strings.Contains(err.Error(), "default deny cidr 169.254.0.0/16") {
		t.Fatalf("DialContext error = %v, want policy block", err)
	}
	if len(recorder.addresses) != 0 {
		t.Fatalf("dialed addresses = %#v, want no dial after denied resolution", recorder.addresses)
	}
}

type staticRouteResolver map[string][]net.IPAddr

func (resolver staticRouteResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := resolver[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host}
	}
	return ips, nil
}

type recordingRouteDialer struct {
	addresses []string
	failures  map[string]error
	success   map[string]bool
	err       error
}

func (dialer *recordingRouteDialer) DialContext(_ context.Context, _ string, address string) (net.Conn, error) {
	dialer.addresses = append(dialer.addresses, address)
	if err, ok := dialer.failures[address]; ok {
		return nil, err
	}
	if dialer.success[address] {
		return noopConn{}, nil
	}
	if dialer.err != nil {
		return nil, dialer.err
	}
	return nil, errors.New("unexpected dial address: " + address)
}

func mustRoutePolicy(t *testing.T, cfg routetarget.EgressPolicyConfig) routetarget.EgressPolicy {
	t.Helper()
	policy, err := routetarget.NewEgressPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

type noopConn struct{}

func (noopConn) Read(_ []byte) (int, error)         { return 0, errors.New("not implemented") }
func (noopConn) Write(_ []byte) (int, error)        { return 0, errors.New("not implemented") }
func (noopConn) Close() error                       { return nil }
func (noopConn) LocalAddr() net.Addr                { return noopAddr("local") }
func (noopConn) RemoteAddr() net.Addr               { return noopAddr("remote") }
func (noopConn) SetDeadline(_ time.Time) error      { return nil }
func (noopConn) SetReadDeadline(_ time.Time) error  { return nil }
func (noopConn) SetWriteDeadline(_ time.Time) error { return nil }

type noopAddr string

func (addr noopAddr) Network() string { return string(addr) }
func (addr noopAddr) String() string  { return string(addr) }
