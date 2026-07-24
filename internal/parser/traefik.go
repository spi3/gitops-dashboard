package parser

import (
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type TraefikRoute struct {
	Service string
	Routes  []string
}

// TraefikTCPRoute is a Traefik file-provider service's TCP endpoint
// evidence: HostSNI(...) router rules and tcp.services backend addresses.
// Neither carries an HTTP scheme, so neither is HTTP route evidence.
type TraefikTCPRoute struct {
	Service   string
	Endpoints []TCPEndpoint
}

func ParseTraefikRoutes(path string) ([]TraefikRoute, []TraefikTCPRoute, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, nil, err
	}
	doc := mapValue(raw)
	if len(doc) == 0 {
		return nil, nil, nil
	}
	httpConfig := mapValue(doc["http"])
	tcpConfig := mapValue(doc["tcp"])
	routesByService := map[string][]string{}
	tcpByService := map[string][]TCPEndpoint{}
	collectTraefikRouters(routesByService, tcpByService, mapValue(httpConfig["routers"]))
	collectTraefikRouters(routesByService, tcpByService, mapValue(tcpConfig["routers"]))
	for serviceName, service := range mapValue(httpConfig["services"]) {
		if urls := traefikServerURLs(service); len(urls) > 0 {
			routesByService[serviceName] = append(routesByService[serviceName], urls...)
		}
	}
	for serviceName, service := range mapValue(tcpConfig["services"]) {
		if addresses := traefikServerAddresses(service); len(addresses) > 0 {
			tcpByService[serviceName] = append(tcpByService[serviceName], addresses...)
		}
	}
	var routes []TraefikRoute
	for service, values := range routesByService {
		routes = append(routes, TraefikRoute{Service: service, Routes: uniqueSorted(values)})
	}
	var tcpRoutes []TraefikTCPRoute
	for service, endpoints := range tcpByService {
		tcpRoutes = append(tcpRoutes, TraefikTCPRoute{Service: service, Endpoints: uniqueTCPEndpoints(endpoints)})
	}
	return routes, tcpRoutes, nil
}

// collectTraefikRouters extracts host evidence from both http.routers and
// tcp.routers: whether a rule's Host(...) matcher is HTTP evidence or its
// HostSNI(...) matcher is TCP evidence is determined by which matcher
// appears in the rule text itself, not by which config section it came
// from, since that is what Traefik's own rule syntax guarantees.
func collectTraefikRouters(routesByService map[string][]string, tcpByService map[string][]TCPEndpoint, routers map[string]any) {
	for name, rawRouter := range routers {
		router := mapValue(rawRouter)
		service := stringValue(router["service"])
		if service == "" {
			service = name
		}
		httpHosts, tcpEndpoints := hostRules(stringValue(router["rule"]))
		if len(httpHosts) > 0 {
			routesByService[service] = append(routesByService[service], httpHosts...)
		}
		if len(tcpEndpoints) > 0 {
			tcpByService[service] = append(tcpByService[service], tcpEndpoints...)
		}
	}
}

func traefikServerURLs(value any) []string {
	var result []string
	servers := listValue(mapValue(mapValue(value)["loadBalancer"])["servers"])
	for _, server := range servers {
		if route := accessRoute(stringValue(mapValue(server)["url"]), ""); route != "" {
			result = append(result, route)
		}
	}
	return result
}

// traefikServerAddresses collects tcp.services backend addresses. These are
// private, shared, or dynamically selected server addresses declared as
// plain host:port literals with no scheme, so they become TCP endpoint
// evidence rather than being guessed into an HTTP route.
func traefikServerAddresses(value any) []TCPEndpoint {
	var result []TCPEndpoint
	servers := listValue(mapValue(mapValue(value)["loadBalancer"])["servers"])
	for _, server := range servers {
		if endpoint, ok := parseTCPAddress(stringValue(mapValue(server)["address"])); ok {
			result = append(result, endpoint)
		}
	}
	return result
}

func parseTCPAddress(address string) (TCPEndpoint, bool) {
	address = strings.TrimSpace(strings.Trim(address, `"'`))
	if address == "" {
		return TCPEndpoint{}, false
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host = strings.Trim(address, "[]")
		if host == "" {
			return TCPEndpoint{}, false
		}
		return TCPEndpoint{Host: host}, true
	}
	host = strings.Trim(host, "[]")
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return TCPEndpoint{Host: host, Port: port}, true
}
