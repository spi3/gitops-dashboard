package parser

import (
	"os"

	"gopkg.in/yaml.v3"
)

type TraefikRoute struct {
	Service string
	Routes  []string
}

func ParseTraefikRoutes(path string) ([]TraefikRoute, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	doc := mapValue(raw)
	if len(doc) == 0 {
		return nil, nil
	}
	httpConfig := mapValue(doc["http"])
	tcpConfig := mapValue(doc["tcp"])
	routesByService := map[string][]string{}
	collectTraefikRouters(routesByService, mapValue(httpConfig["routers"]))
	collectTraefikRouters(routesByService, mapValue(tcpConfig["routers"]))
	for serviceName, service := range mapValue(httpConfig["services"]) {
		for _, route := range traefikServerURLs(service) {
			routesByService[serviceName] = append(routesByService[serviceName], route)
		}
	}
	for serviceName, service := range mapValue(tcpConfig["services"]) {
		for _, route := range traefikServerAddresses(service) {
			routesByService[serviceName] = append(routesByService[serviceName], route)
		}
	}
	var result []TraefikRoute
	for service, routes := range routesByService {
		result = append(result, TraefikRoute{Service: service, Routes: uniqueSorted(routes)})
	}
	return result, nil
}

func collectTraefikRouters(routesByService map[string][]string, routers map[string]any) {
	for name, rawRouter := range routers {
		router := mapValue(rawRouter)
		service := stringValue(router["service"])
		if service == "" {
			service = name
		}
		routesByService[service] = append(routesByService[service], hostRules(stringValue(router["rule"]))...)
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

func traefikServerAddresses(value any) []string {
	var result []string
	servers := listValue(mapValue(mapValue(value)["loadBalancer"])["servers"])
	for _, server := range servers {
		if route := accessRoute(stringValue(mapValue(server)["address"]), ""); route != "" {
			result = append(result, route)
		}
	}
	return result
}
