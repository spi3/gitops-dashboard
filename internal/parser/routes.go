package parser

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var traefikHostRule = regexp.MustCompile(`Host(SNI)?\(([^)]*)\)`)

// TCPEndpointPrefix marks a Service.Exposure value as TCP inventory: real
// evidence of a non-HTTP listener, kept out of HTTP route generation and
// probing the same way the "address/" and "host/" prefixes already are.
const TCPEndpointPrefix = "tcp/"

// TCPEndpoint is TCP router/backend evidence that must never be turned into
// an HTTP route: an SNI-matched router names a host with no known port, and a
// backend address names a host:port with no scheme.
type TCPEndpoint struct {
	Host string
	Port string
	SNI  bool
}

func (endpoint TCPEndpoint) Exposure() string {
	host := strings.Trim(strings.TrimSpace(endpoint.Host), "[]")
	if host == "" {
		return ""
	}
	if endpoint.Port == "" {
		return TCPEndpointPrefix + host
	}
	return TCPEndpointPrefix + net.JoinHostPort(host, endpoint.Port)
}

func tcpEndpointsExposure(endpoints []TCPEndpoint) []string {
	var result []string
	for _, endpoint := range endpoints {
		if value := endpoint.Exposure(); value != "" {
			result = append(result, value)
		}
	}
	return uniqueSorted(result)
}

func uniqueTCPEndpoints(endpoints []TCPEndpoint) []TCPEndpoint {
	seen := map[TCPEndpoint]bool{}
	var result []TCPEndpoint
	for _, endpoint := range endpoints {
		if endpoint.Host == "" || seen[endpoint] {
			continue
		}
		seen[endpoint] = true
		result = append(result, endpoint)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Host != result[j].Host {
			return result[i].Host < result[j].Host
		}
		return result[i].Port < result[j].Port
	})
	return result
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func accessRoute(host, port string) string {
	host = strings.TrimSpace(strings.Trim(host, `"'`))
	port = strings.TrimSpace(strings.Trim(port, `"'`))
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	if port == "" {
		if strings.HasPrefix(host, "http://") || strings.HasPrefix(host, "https://") || strings.HasPrefix(host, "ssh://") {
			return host
		}
		if net.ParseIP(strings.Trim(host, "[]")) != nil {
			literal := strings.Trim(host, "[]")
			if strings.Contains(literal, ":") {
				return "http://[" + literal + "]"
			}
			return "http://" + literal
		}
		return schemeForHost(host) + "://" + host
	}
	// JoinHostPort correctly brackets IPv6 literals while leaving DNS names
	// unchanged. Trim brackets first because Compose permits bracketed binds.
	host = strings.Trim(host, "[]")
	return schemeForPort(port) + "://" + net.JoinHostPort(host, port)
}

func schemeForHost(host string) string {
	if strings.HasSuffix(host, ".lan") || net.ParseIP(host) != nil {
		return "http"
	}
	return "https"
}

func schemeForPort(port string) string {
	switch strings.TrimSpace(port) {
	case "22":
		return "ssh"
	case "443", "8443", "9443":
		return "https"
	default:
		return "http"
	}
}

// hostRules extracts Host(...) and HostSNI(...) router-rule host matchers.
// Host(...) is HTTP evidence and becomes a route; HostSNI(...) is TCP
// evidence (a public-routing assertion with no known backend port) and
// becomes SNI-only TCP endpoint evidence, never an HTTP route.
func hostRules(value string) ([]string, []TCPEndpoint) {
	var httpHosts []string
	var tcpEndpoints []TCPEndpoint
	for _, match := range traefikHostRule.FindAllStringSubmatch(value, -1) {
		sni := match[1] != ""
		for _, rawHost := range strings.Split(match[2], ",") {
			host := strings.TrimSpace(strings.Trim(rawHost, "`'\""))
			if host == "" || strings.ContainsAny(host, "*^$") {
				continue
			}
			if sni {
				tcpEndpoints = append(tcpEndpoints, TCPEndpoint{Host: host, SNI: true})
				continue
			}
			httpHosts = append(httpHosts, accessRoute(host, ""))
		}
	}
	return uniqueSorted(httpHosts), uniqueTCPEndpoints(tcpEndpoints)
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return fmt.Sprint(typed)
	default:
		return fmt.Sprint(typed)
	}
}
