package routetarget

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
)

type EgressPolicyConfig struct {
	AllowDomains []string
	AllowCIDRs   []string
	DenyDomains  []string
	DenyCIDRs    []string
}

type EgressPolicy struct {
	allowRules          []egressRule
	denyRules           []egressRule
	defaultDenyRules    []egressRule
	hasAllowRules       bool
	hasAllowDomainRules bool
	hasAllowCIDRRules   bool
}

type EgressDecision struct {
	Allowed  bool
	Rule     string
	Deferred bool
}

type egressRule struct {
	kind   string
	value  string
	domain string
	prefix netip.Prefix
}

var defaultDenyCIDRs = []string{
	"169.254.0.0/16",
	"fe80::/10",
}

func NewEgressPolicy(cfg EgressPolicyConfig) (EgressPolicy, error) {
	allowRules, err := compileEgressRules("allow", cfg.AllowDomains, cfg.AllowCIDRs)
	if err != nil {
		return EgressPolicy{}, err
	}
	denyRules, err := compileEgressRules("deny", cfg.DenyDomains, cfg.DenyCIDRs)
	if err != nil {
		return EgressPolicy{}, err
	}
	defaultDenyRules, err := compileEgressRules("default deny", nil, defaultDenyCIDRs)
	if err != nil {
		return EgressPolicy{}, err
	}
	return EgressPolicy{
		allowRules:          allowRules,
		denyRules:           denyRules,
		defaultDenyRules:    defaultDenyRules,
		hasAllowRules:       len(allowRules) > 0,
		hasAllowDomainRules: hasRuleKind(allowRules, "domain"),
		hasAllowCIDRRules:   hasRuleKind(allowRules, "cidr"),
	}, nil
}

func (policy EgressPolicy) Check(route string) EgressDecision {
	host := routeHost(route)
	if host == "" {
		return EgressDecision{Allowed: false, Rule: "invalid route"}
	}
	return policy.CheckHost(host)
}

func (policy EgressPolicy) CheckHost(host string) EgressDecision {
	host = normalizeHost(host)
	if host == "" {
		return EgressDecision{Allowed: false, Rule: "invalid route"}
	}
	addr, hasAddr := parseHostAddr(host)
	if hasAddr {
		decision := policy.CheckResolvedAddr(addr)
		if !decision.Allowed {
			return decision
		}
		if policy.hasAllowDomainRules && !policy.hasAllowCIDRRules {
			return EgressDecision{Allowed: false, Rule: "no allow rule matched"}
		}
		return decision
	}
	for _, rule := range policy.denyRules {
		if rule.kind == "domain" && rule.matches(host, addr, hasAddr) {
			return EgressDecision{Allowed: false, Rule: "deny " + rule.String()}
		}
	}
	for _, rule := range policy.allowRules {
		if rule.kind == "domain" && rule.matches(host, addr, hasAddr) {
			return EgressDecision{Allowed: true, Rule: "allow " + rule.String()}
		}
	}
	if policy.hasAllowCIDRRules {
		return EgressDecision{Deferred: true, Rule: "cidr resolution required"}
	}
	if policy.hasAllowRules {
		return EgressDecision{Allowed: false, Rule: "no allow rule matched"}
	}
	return EgressDecision{Allowed: true}
}

func (policy EgressPolicy) CheckResolvedAddr(addr netip.Addr) EgressDecision {
	return policy.CheckResolvedAddrForHost(addr, false)
}

func (policy EgressPolicy) CheckResolvedAddrForHost(addr netip.Addr, hostAllowed bool) EgressDecision {
	host := addr.String()
	for _, rule := range policy.denyRules {
		if rule.kind == "cidr" && rule.matches(host, addr, true) {
			return EgressDecision{Allowed: false, Rule: "deny " + rule.String()}
		}
	}
	for _, rule := range policy.allowRules {
		if rule.kind == "cidr" && rule.matches(host, addr, true) {
			return EgressDecision{Allowed: true, Rule: "allow " + rule.String()}
		}
	}
	for _, rule := range policy.defaultDenyRules {
		if rule.matches(host, addr, true) {
			return EgressDecision{Allowed: false, Rule: "default deny " + rule.String()}
		}
	}
	if policy.hasAllowCIDRRules && !hostAllowed {
		return EgressDecision{Allowed: false, Rule: "no allow cidr rule matched"}
	}
	return EgressDecision{Allowed: true}
}

func hasRuleKind(rules []egressRule, kind string) bool {
	for _, rule := range rules {
		if rule.kind == kind {
			return true
		}
	}
	return false
}

func (rule egressRule) String() string {
	return rule.kind + " " + rule.value
}

func (rule egressRule) matches(host string, addr netip.Addr, hasAddr bool) bool {
	switch rule.kind {
	case "domain":
		return matchesDomainRule(host, rule.domain)
	case "cidr":
		return hasAddr && rule.prefix.Contains(addr)
	default:
		return false
	}
}

func compileEgressRules(group string, domains, cidrs []string) ([]egressRule, error) {
	rules := make([]egressRule, 0, len(domains)+len(cidrs))
	for _, domain := range domains {
		normalized, err := normalizeDomainRule(domain)
		if err != nil {
			return nil, fmt.Errorf("%s domain %q: %w", group, domain, err)
		}
		rules = append(rules, egressRule{kind: "domain", value: normalized, domain: normalized})
	}
	for _, cidr := range cidrs {
		prefix, err := parseCIDRRule(cidr)
		if err != nil {
			return nil, fmt.Errorf("%s cidr %q: %w", group, cidr, err)
		}
		rules = append(rules, egressRule{kind: "cidr", value: prefix.String(), prefix: prefix})
	}
	return rules, nil
}

func normalizeDomainRule(value string) (string, error) {
	domain := strings.ToLower(strings.TrimSpace(value))
	domain = strings.TrimSuffix(domain, ".")
	domain = strings.TrimPrefix(domain, "*.")
	domain = strings.TrimPrefix(domain, ".")
	if domain == "" {
		return "", fmt.Errorf("must not be empty")
	}
	if strings.ContainsAny(domain, "/:@") {
		return "", fmt.Errorf("must be a domain name, not a URL")
	}
	if netipAddr, err := netip.ParseAddr(domain); err == nil && netipAddr.IsValid() {
		return "", fmt.Errorf("must be a domain name, not an IP address")
	}
	return domain, nil
}

func parseCIDRRule(value string) (netip.Prefix, error) {
	cidr := strings.TrimSpace(value)
	prefix, err := netip.ParsePrefix(cidr)
	if err == nil {
		return prefix.Masked(), nil
	}
	addr, addrErr := netip.ParseAddr(cidr)
	if addrErr != nil {
		return netip.Prefix{}, fmt.Errorf("must be a CIDR or IP address")
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

func routeHost(route string) string {
	parsed, err := url.Parse(StripUserinfo(route))
	if err != nil {
		return ""
	}
	return normalizeHost(parsed.Hostname())
}

func parseHostAddr(host string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
}

func normalizeHost(host string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
}

func matchesDomainRule(host, domain string) bool {
	if _, ok := parseHostAddr(host); ok {
		return false
	}
	host = strings.TrimSuffix(normalizeHost(host), ".")
	return host == domain || strings.HasSuffix(host, "."+domain)
}
