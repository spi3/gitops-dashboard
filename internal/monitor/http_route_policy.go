package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/routetarget"
)

const httpRouteRedirectLimit = 10

type routePolicyError struct {
	rule string
}

func (err routePolicyError) Error() string {
	return blockedByPolicyMessage(err.rule)
}

type routeResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type defaultRouteResolver struct{}

func (defaultRouteResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

type routeDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type routeDialFunc func(context.Context, string, string) (net.Conn, error)

func (fn routeDialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return fn(ctx, network, address)
}

type routePolicyDialer struct {
	policy   routetarget.EgressPolicy
	resolver routeResolver
	dialer   routeDialer
}

func (dialer routePolicyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	checkedAddrs, err := dialer.checkedDialAddresses(ctx, network, host, port)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, checkedAddr := range checkedAddrs {
		conn, err := dialer.dialer.DialContext(ctx, network, checkedAddr)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (dialer routePolicyDialer) checkedDialAddresses(ctx context.Context, network, host, port string) ([]string, error) {
	hostDecision := dialer.policy.CheckHost(host)
	if err := routePolicyDecisionError(hostDecision); err != nil {
		return nil, err
	}
	addr, ok := parsePolicyAddr(host)
	if ok {
		decision := dialer.policy.CheckResolvedAddrForHost(addr, hostDecision.Allowed)
		if err := routePolicyDecisionError(decision); err != nil {
			return nil, err
		}
		return []string{net.JoinHostPort(addr.String(), port)}, nil
	}
	ips, err := dialer.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	checkedAddrs := []string{}
	for _, ip := range ips {
		addr, ok := parsePolicyAddr(ip.String())
		if !ok {
			return nil, fmt.Errorf("resolve %s: invalid address %q", host, ip.String())
		}
		decision := dialer.policy.CheckResolvedAddrForHost(addr, hostDecision.Allowed)
		if err := routePolicyDecisionError(decision); err != nil {
			return nil, err
		}
		if routeIPMatchesNetwork(network, addr) {
			checkedAddrs = append(checkedAddrs, net.JoinHostPort(addr.String(), port))
		}
	}
	if len(checkedAddrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses for %s", host, network)
	}
	return checkedAddrs, nil
}

func policyHTTPClient(base *http.Client, policy routetarget.EgressPolicy) *http.Client {
	return policyHTTPClientWithResolver(base, policy, defaultRouteResolver{})
}

func policyHTTPClientWithResolver(base *http.Client, policy routetarget.EgressPolicy, resolver routeResolver) *http.Client {
	if base == nil {
		base = &http.Client{}
	}
	wrapped := *base
	wrapped.Transport = policyRoundTripper(base.Transport, policy, resolver)
	wrapped.CheckRedirect = policyCheckRedirect(base.CheckRedirect, policy, resolver)
	return &wrapped
}

func policyRoundTripper(base http.RoundTripper, policy routetarget.EgressPolicy, resolver routeResolver) http.RoundTripper {
	transport, ok := base.(*http.Transport)
	if base != nil && !ok {
		return base
	}
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport)
	}
	clone := transport.Clone()
	baseDial := clone.DialContext
	if baseDial == nil {
		netDialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		baseDial = netDialer.DialContext
	}
	clone.Proxy = nil
	clone.DialTLS = nil
	clone.DialTLSContext = nil
	clone.DialContext = routePolicyDialer{
		policy:   policy,
		resolver: resolver,
		dialer:   routeDialFunc(baseDial),
	}.DialContext
	return clone
}

func policyCheckRedirect(existing func(*http.Request, []*http.Request) error, policy routetarget.EgressPolicy, resolver routeResolver) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		// Keep Go's normal redirect-following behavior, but enforce route policy
		// on every hop and retain the same default 10-hop cap.
		if len(via) >= httpRouteRedirectLimit {
			return errors.New("stopped after 10 redirects")
		}
		if err := checkPolicyURLDestination(req.Context(), req.URL, policy, resolver); err != nil {
			return err
		}
		if existing != nil {
			return existing(req, via)
		}
		return nil
	}
}

func checkPolicyURLDestination(ctx context.Context, target *url.URL, policy routetarget.EgressPolicy, resolver routeResolver) error {
	host := target.Hostname()
	if host == "" {
		return routePolicyError{rule: "invalid route"}
	}
	hostDecision := policy.CheckHost(host)
	if err := routePolicyDecisionError(hostDecision); err != nil {
		return err
	}
	addr, ok := parsePolicyAddr(host)
	if ok {
		decision := policy.CheckResolvedAddrForHost(addr, hostDecision.Allowed)
		if err := routePolicyDecisionError(decision); err != nil {
			return err
		}
		return nil
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, ip := range ips {
		addr, ok := parsePolicyAddr(ip.String())
		if !ok {
			return fmt.Errorf("resolve %s: invalid address %q", host, ip.String())
		}
		decision := policy.CheckResolvedAddrForHost(addr, hostDecision.Allowed)
		if err := routePolicyDecisionError(decision); err != nil {
			return err
		}
	}
	return nil
}

func parsePolicyAddr(value string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.Trim(value, "[]"))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func routeIPMatchesNetwork(network string, addr netip.Addr) bool {
	switch network {
	case "tcp4", "udp4", "ip4":
		return addr.Is4()
	case "tcp6", "udp6", "ip6":
		return addr.Is6()
	default:
		return true
	}
}

func routePolicyStatus(err error) (string, bool) {
	var policyErr routePolicyError
	if errors.As(err, &policyErr) {
		return blockedByPolicyMessage(policyErr.rule), true
	}
	return "", false
}

func routePolicyDecisionError(decision routetarget.EgressDecision) error {
	if decision.Allowed || decision.Deferred {
		return nil
	}
	return routePolicyError{rule: decision.Rule}
}
