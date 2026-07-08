package routetarget

import (
	"net"
	"net/url"
	"sort"
	"strings"
)

const (
	Parent = "routes"
	Prefix = Parent + ": "
)

func Target(route string) string {
	return Prefix + route
}

func Routes(exposure []string) []string {
	routes := []string{}
	seen := map[string]bool{}
	for _, candidate := range exposure {
		route, ok := Normalize(candidate)
		if !ok || seen[route] {
			continue
		}
		seen[route] = true
		routes = append(routes, route)
	}
	sort.SliceStable(routes, func(i, j int) bool {
		leftScore := routeScore(routes[i])
		rightScore := routeScore(routes[j])
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		if len(routes[i]) != len(routes[j]) {
			return len(routes[i]) < len(routes[j])
		}
		return routes[i] < routes[j]
	})
	return routes
}

func IsChildTarget(target string) bool {
	return strings.HasPrefix(strings.TrimSpace(target), Prefix)
}

func RouteFromTarget(target string) (string, bool) {
	value := strings.TrimSpace(target)
	if !strings.HasPrefix(value, Prefix) {
		return "", false
	}
	return Normalize(strings.TrimSpace(strings.TrimPrefix(value, Prefix)))
}

func CanonicalTarget(target string) (string, bool) {
	value := strings.TrimSpace(target)
	if value == Parent {
		return Parent, true
	}
	route, ok := RouteFromTarget(value)
	if !ok {
		return value, false
	}
	return Target(route), true
}

func Normalize(candidate string) (string, bool) {
	value := strings.TrimSpace(candidate)
	if value == "" || strings.HasPrefix(value, "service/") {
		return "", false
	}
	raw := value
	if !strings.Contains(value, "://") {
		host := hostOnly(value)
		if !isCheckableRouteHost(host) {
			return "", false
		}
		scheme := "https"
		if isLANOrIP(host) {
			scheme = "http"
		}
		raw = scheme + "://" + value
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", false
	}
	if !isCheckableRouteHost(parsed.Hostname()) {
		return "", false
	}
	return canonicalURL(raw, parsed), true
}

func canonicalURL(raw string, parsed *url.URL) string {
	schemeEnd := strings.Index(raw, "://")
	if schemeEnd < 0 {
		return raw
	}
	scheme := strings.ToLower(raw[:schemeEnd])
	rest := raw[schemeEnd+len("://"):]
	authorityEnd := strings.IndexAny(rest, "/?#")
	if authorityEnd < 0 {
		authorityEnd = len(rest)
	}
	authority := rest[:authorityEnd]
	suffix := rest[authorityEnd:]
	return scheme + "://" + canonicalAuthority(authority, parsed, scheme) + canonicalSuffix(suffix)
}

func canonicalAuthority(authority string, parsed *url.URL, scheme string) string {
	userinfo := ""
	hostport := authority
	if userinfoEnd := strings.LastIndex(hostport, "@"); userinfoEnd >= 0 {
		userinfo = hostport[:userinfoEnd+1]
		hostport = hostport[userinfoEnd+1:]
	}
	host := strings.ToLower(strings.Trim(parsed.Hostname(), "[]"))
	afterHost := authorityAfterHost(hostport, parsed.Hostname())
	port := parsed.Port()
	if isDefaultPort(scheme, port) && afterHost == ":"+port {
		afterHost = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return userinfo + host + afterHost
}

func authorityAfterHost(hostport string, parsedHost string) string {
	if strings.HasPrefix(hostport, "[") {
		if hostEnd := strings.Index(hostport, "]"); hostEnd >= 0 {
			return hostport[hostEnd+1:]
		}
		return ""
	}
	if len(hostport) >= len(parsedHost) {
		return hostport[len(parsedHost):]
	}
	return ""
}

func isDefaultPort(scheme, port string) bool {
	return (scheme == "http" && port == "80") || (scheme == "https" && port == "443")
}

func canonicalSuffix(suffix string) string {
	switch {
	case suffix == "/":
		return ""
	case strings.HasPrefix(suffix, "/?"), strings.HasPrefix(suffix, "/#"):
		return suffix[1:]
	default:
		return suffix
	}
}

func routeScore(route string) int {
	parsed, err := url.Parse(route)
	if err != nil {
		return 0
	}
	host := strings.ToLower(strings.Trim(parsed.Hostname(), "[]"))
	score := 0
	if net.ParseIP(host) == nil {
		score += 100
		if strings.HasSuffix(host, ".lan") {
			score += 30
		}
	}
	if parsed.Port() != "" {
		score += 20
	}
	if parsed.Path != "" && parsed.Path != "/" {
		score += 10
	}
	if parsed.Scheme == "https" {
		score += 5
	}
	return score
}

func isCheckableRouteHost(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if strings.HasSuffix(host, ".svc") || strings.Contains(host, ".svc.") || strings.HasSuffix(host, ".cluster.local") {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	return strings.Contains(host, ".")
}

func isLANOrIP(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	return strings.HasSuffix(host, ".lan") || net.ParseIP(host) != nil
}

func hostOnly(value string) string {
	withoutPath := strings.SplitN(value, "/", 2)[0]
	if strings.HasPrefix(withoutPath, "[") {
		end := strings.Index(withoutPath, "]")
		if end > 0 {
			return withoutPath[1:end]
		}
		return withoutPath
	}
	if parsedHost, _, err := net.SplitHostPort(withoutPath); err == nil {
		return parsedHost
	}
	return strings.SplitN(withoutPath, ":", 2)[0]
}
