package routetarget

import "testing"

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
		{name: "userinfo preserved", candidate: "HTTPS://User:P%40ss@APP.EXAMPLE.TEST:443/a%2Fb?x=%20#Frag", want: "https://User:P%40ss@app.example.test/a%2Fb?x=%20#Frag"},
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
