package routetarget

import (
	"net/netip"
	"testing"
)

func TestNormalizePreservesOnlySignificantTrailingSlashes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		candidate string
		want      string
	}{
		{name: "root with slash", candidate: "https://app.example.test/", want: "https://app.example.test"},
		{name: "root without slash", candidate: "https://app.example.test", want: "https://app.example.test"},
		{name: "non-root with trailing slash", candidate: "https://app.example.test/admin/", want: "https://app.example.test/admin/"},
		{name: "non-root without trailing slash", candidate: "https://app.example.test/admin", want: "https://app.example.test/admin"},
		{name: "root with default port and case", candidate: "HTTPS://APP.EXAMPLE.TEST:443/", want: "https://app.example.test"},
		{name: "non-root trailing slash with default port and case", candidate: "HTTPS://APP.EXAMPLE.TEST:443/admin/", want: "https://app.example.test/admin/"},
		{name: "escaped slash preserved", candidate: "https://app.example.test/a%2Fb", want: "https://app.example.test/a%2Fb"},
		{name: "literal slash distinct from escaped slash", candidate: "https://app.example.test/a/b", want: "https://app.example.test/a/b"},
		{name: "escaped space preserved", candidate: "https://app.example.test/a%20b", want: "https://app.example.test/a%20b"},
		{name: "query and fragment preserved", candidate: "HTTPS://APP.EXAMPLE.TEST:443/admin/?q=a%2Fb&space=%20#Frag%20", want: "https://app.example.test/admin/?q=a%2Fb&space=%20#Frag%20"},
		{name: "root slash with query and fragment collapsed", candidate: "HTTPS://APP.EXAMPLE.TEST:443/?q=a%2Fb#Frag%20", want: "https://app.example.test?q=a%2Fb#Frag%20"},
		{name: "userinfo stripped", candidate: "HTTPS://User:P%40ss@APP.EXAMPLE.TEST:443/a%2Fb?x=%20#Frag", want: "https://app.example.test/a%2Fb?x=%20#Frag"},
		{name: "bare root with slash", candidate: "app.example.test/", want: "https://app.example.test"},
		{name: "bare non-root with trailing slash", candidate: "app.example.test/admin/", want: "https://app.example.test/admin/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := Normalize(tc.candidate)
			if !ok || got != tc.want {
				t.Fatalf("Normalize(%q) = %q, %v; want %q, true", tc.candidate, got, ok, tc.want)
			}
		})
	}
}

func TestCanonicalTargetPreservesNonRootTrailingSlash(t *testing.T) {
	t.Parallel()
	got, ok := CanonicalTarget("routes: HTTPS://APP.EXAMPLE.TEST:443/admin/")
	if !ok || got != "routes: https://app.example.test/admin/" {
		t.Fatalf("CanonicalTarget() = %q, %v; want non-root trailing slash preserved", got, ok)
	}
}

func TestCanonicalTargetPreservesEscapedPathIdentity(t *testing.T) {
	t.Parallel()
	escaped, ok := CanonicalTarget("routes: HTTPS://APP.EXAMPLE.TEST:443/a%2Fb")
	if !ok || escaped != "routes: https://app.example.test/a%2Fb" {
		t.Fatalf("escaped CanonicalTarget() = %q, %v; want escaped path preserved", escaped, ok)
	}
	literal, ok := CanonicalTarget("routes: HTTPS://APP.EXAMPLE.TEST:443/a/b")
	if !ok || literal != "routes: https://app.example.test/a/b" {
		t.Fatalf("literal CanonicalTarget() = %q, %v; want literal path preserved", literal, ok)
	}
	if escaped == literal {
		t.Fatalf("escaped and literal paths collapsed to %q", escaped)
	}
}

func TestCanonicalTargetStripsURLUserinfo(t *testing.T) {
	t.Parallel()
	got, ok := CanonicalTarget("routes: HTTPS://User:P%40ss@APP.EXAMPLE.TEST:443/admin")
	if !ok || got != "routes: https://app.example.test/admin" {
		t.Fatalf("CanonicalTarget() = %q, %v; want userinfo stripped", got, ok)
	}
}

func TestStripUserinfo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "http credentials", raw: "https://user:pass@app.example.test/path?q=1", want: "https://app.example.test/path?q=1"},
		{name: "ssh username", raw: "ssh://git@github.com/org/repo.git", want: "ssh://github.com/org/repo.git"},
		{name: "path at sign preserved", raw: "https://app.example.test/path@v1", want: "https://app.example.test/path@v1"},
		{name: "bare host unchanged", raw: "app.example.test", want: "app.example.test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := StripUserinfo(tc.raw); got != tc.want {
				t.Fatalf("StripUserinfo(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEgressPolicyMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		cfg         EgressPolicyConfig
		route       string
		wantAllowed bool
		wantRule    string
	}{
		{
			name:        "private IPv4 allowed by default",
			route:       "http://10.10.10.20",
			wantAllowed: true,
		},
		{
			name:        "IPv4 link-local denied by default",
			route:       "http://169.254.169.254/latest/meta-data",
			wantAllowed: false,
			wantRule:    "default deny cidr 169.254.0.0/16",
		},
		{
			name:        "IPv6 link-local denied by default",
			route:       "http://[fe80::1]/",
			wantAllowed: false,
			wantRule:    "default deny cidr fe80::/10",
		},
		{
			name: "link-local CIDR opt-in overrides default deny",
			cfg: EgressPolicyConfig{
				AllowCIDRs: []string{"169.254.0.0/16"},
			},
			route:       "http://169.254.169.254/latest/meta-data",
			wantAllowed: true,
			wantRule:    "allow cidr 169.254.0.0/16",
		},
		{
			name: "configured CIDR deny blocks private route",
			cfg: EgressPolicyConfig{
				DenyCIDRs: []string{"10.0.0.0/8"},
			},
			route:       "http://10.10.10.20",
			wantAllowed: false,
			wantRule:    "deny cidr 10.0.0.0/8",
		},
		{
			name: "domain deny matches subdomain",
			cfg: EgressPolicyConfig{
				DenyDomains: []string{"metadata.example.test"},
			},
			route:       "https://api.metadata.example.test",
			wantAllowed: false,
			wantRule:    "deny domain metadata.example.test",
		},
		{
			name: "allow list permits matching domain",
			cfg: EgressPolicyConfig{
				AllowDomains: []string{"example.test"},
			},
			route:       "https://app.example.test",
			wantAllowed: true,
			wantRule:    "allow domain example.test",
		},
		{
			name: "allow list blocks non-matching domain",
			cfg: EgressPolicyConfig{
				AllowDomains: []string{"example.test"},
			},
			route:       "https://other.invalid",
			wantAllowed: false,
			wantRule:    "no allow rule matched",
		},
		{
			name: "deny wins over allow",
			cfg: EgressPolicyConfig{
				AllowDomains: []string{"example.test"},
				DenyDomains:  []string{"blocked.example.test"},
			},
			route:       "https://blocked.example.test",
			wantAllowed: false,
			wantRule:    "deny domain blocked.example.test",
		},
		{
			name: "userinfo stripped before policy match",
			cfg: EgressPolicyConfig{
				DenyDomains: []string{"app.example.test"},
			},
			route:       "https://user:pass@app.example.test",
			wantAllowed: false,
			wantRule:    "deny domain app.example.test",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy, err := NewEgressPolicy(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			got := policy.Check(tc.route)
			if got.Allowed != tc.wantAllowed || got.Rule != tc.wantRule {
				t.Fatalf("Check(%q) = allowed:%v rule:%q; want allowed:%v rule:%q", tc.route, got.Allowed, got.Rule, tc.wantAllowed, tc.wantRule)
			}
		})
	}
}

func TestEgressPolicyRejectsInvalidRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  EgressPolicyConfig
	}{
		{name: "domain URL", cfg: EgressPolicyConfig{AllowDomains: []string{"https://example.test"}}},
		{name: "domain IP", cfg: EgressPolicyConfig{DenyDomains: []string{"10.10.10.20"}}},
		{name: "bad CIDR", cfg: EgressPolicyConfig{AllowCIDRs: []string{"not-a-cidr"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewEgressPolicy(tc.cfg); err == nil {
				t.Fatal("NewEgressPolicy succeeded with invalid rule")
			}
		})
	}
}

func TestEgressPolicyDefersHostnameCIDRAllowUntilResolution(t *testing.T) {
	t.Parallel()
	policy, err := NewEgressPolicy(EgressPolicyConfig{AllowCIDRs: []string{"10.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	hostDecision := policy.Check("https://app.internal")
	if !hostDecision.Deferred || hostDecision.Allowed {
		t.Fatalf("hostname decision = %#v, want deferred CIDR decision", hostDecision)
	}
	if decision := policy.CheckResolvedAddr(netip.MustParseAddr("10.12.0.5")); !decision.Allowed {
		t.Fatalf("allowed CIDR resolved decision = %#v, want allowed", decision)
	}
	decision := policy.CheckResolvedAddr(netip.MustParseAddr("192.0.2.10"))
	if decision.Allowed || decision.Rule != "no allow cidr rule matched" {
		t.Fatalf("outside CIDR resolved decision = %#v, want CIDR allow miss", decision)
	}
}
