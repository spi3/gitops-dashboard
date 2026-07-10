package parser

import (
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var traefikHostRule = regexp.MustCompile(`Host(?:SNI)?\(([^)]*)\)`)

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

func hostRules(value string) []string {
	var result []string
	for _, match := range traefikHostRule.FindAllStringSubmatch(value, -1) {
		for _, rawHost := range strings.Split(match[1], ",") {
			host := strings.TrimSpace(strings.Trim(rawHost, "`'\""))
			if host == "" || strings.ContainsAny(host, "*^$") {
				continue
			}
			result = append(result, accessRoute(host, ""))
		}
	}
	return uniqueSorted(result)
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
