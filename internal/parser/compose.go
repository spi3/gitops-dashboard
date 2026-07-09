package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type ComposeProject struct {
	Name     string
	Services []ComposeService
}

type ComposeService struct {
	Name      string
	Image     string
	Build     string
	Ports     []string
	Volumes   []string
	Networks  []string
	Exposure  []string
	DependsOn []string
	EnvVars   []string
	Warnings  []string
}

type composeFile struct {
	Name     string                    `yaml:"name"`
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string         `yaml:"image"`
	Build       any            `yaml:"build"`
	Ports       []any          `yaml:"ports"`
	Volumes     []any          `yaml:"volumes"`
	Networks    any            `yaml:"networks"`
	DependsOn   any            `yaml:"depends_on"`
	Environment any            `yaml:"environment"`
	Labels      any            `yaml:"labels"`
	Healthcheck map[string]any `yaml:"healthcheck"`
}

func IsComposeFile(path string) bool {
	switch filepath.Base(path) {
	case "compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml":
		return true
	default:
		return false
	}
}

func IsYAMLFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

func ParseCompose(path string) (ComposeProject, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ComposeProject{}, err
	}
	var file composeFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return ComposeProject{}, err
	}
	if len(file.Services) == 0 {
		return ComposeProject{}, nil
	}
	names := make([]string, 0, len(file.Services))
	for name := range file.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	project := ComposeProject{Name: normalizeComposeProjectName(file.Name)}
	for _, name := range names {
		raw := file.Services[name]
		service := ComposeService{
			Name:      name,
			Image:     raw.Image,
			Build:     stringify(raw.Build),
			Ports:     stringifySlice(raw.Ports),
			Volumes:   stringifySlice(raw.Volumes),
			Networks:  networks(raw.Networks),
			Exposure:  composeExposure(raw),
			DependsOn: dependsOn(raw.DependsOn),
			EnvVars:   envVars(raw.Environment),
		}
		if len(raw.Healthcheck) == 0 {
			service.Warnings = append(service.Warnings, "missing healthcheck")
		}
		project.Services = append(project.Services, service)
	}
	return project, nil
}

func normalizeComposeProjectName(name string) string {
	name = strings.TrimSpace(name)
	var literal strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] != '$' {
			literal.WriteByte(name[i])
			continue
		}
		if i+1 < len(name) && name[i+1] == '$' {
			literal.WriteByte('$')
			i++
			continue
		}
		return ""
	}
	return literal.String()
}

func envVars(value any) []string {
	switch typed := value.(type) {
	case []any:
		var result []string
		for _, item := range typed {
			name := strings.SplitN(stringify(item), "=", 2)[0]
			if name != "" {
				result = append(result, name)
			}
		}
		sort.Strings(result)
		return result
	case map[string]any:
		var result []string
		for name := range typed {
			result = append(result, name)
		}
		sort.Strings(result)
		return result
	default:
		return nil
	}
}

func composeExposure(service composeService) []string {
	var result []string
	for _, port := range service.Ports {
		result = append(result, publishedPortRoutes(port)...)
	}
	result = append(result, staticAddressRoutes(service.Networks, service.Ports)...)
	result = append(result, labelRoutes(service.Labels)...)
	return uniqueSorted(result)
}

func publishedPortRoutes(value any) []string {
	switch typed := value.(type) {
	case string:
		host, published, ok := shortPublishedPort(typed)
		if !ok || host == "" || host == "0.0.0.0" {
			return nil
		}
		return []string{accessRoute(host, published)}
	case map[string]any:
		published := stringValue(typed["published"])
		host := stringValue(typed["host_ip"])
		if published == "" || host == "" || host == "0.0.0.0" {
			return nil
		}
		return []string{accessRoute(host, published)}
	default:
		return nil
	}
}

func shortPublishedPort(value string) (string, string, bool) {
	value = strings.TrimSpace(strings.SplitN(value, "/", 2)[0])
	if !strings.Contains(value, ":") {
		return "", "", false
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 {
		return "", "", false
	}
	published := parts[len(parts)-2]
	host := ""
	if len(parts) > 2 {
		host = strings.Join(parts[:len(parts)-2], ":")
	}
	if published == "" {
		return "", "", false
	}
	return host, published, true
}

func staticAddressRoutes(networks any, ports []any) []string {
	var addresses []string
	for _, network := range mapValue(networks) {
		ip := stringValue(mapValue(network)["ipv4_address"])
		if ip != "" {
			addresses = append(addresses, ip)
		}
	}
	var targetPorts []string
	for _, port := range ports {
		if target := targetPort(port); target != "" {
			targetPorts = append(targetPorts, target)
		}
	}
	var result []string
	for _, address := range uniqueSorted(addresses) {
		if len(targetPorts) == 0 {
			result = append(result, accessRoute(address, ""))
			continue
		}
		for _, port := range uniqueSorted(targetPorts) {
			result = append(result, accessRoute(address, port))
		}
	}
	return result
}

func targetPort(value any) string {
	switch typed := value.(type) {
	case string:
		value := strings.TrimSpace(strings.SplitN(typed, "/", 2)[0])
		parts := strings.Split(value, ":")
		return parts[len(parts)-1]
	case map[string]any:
		return stringValue(typed["target"])
	default:
		return ""
	}
}

func labelRoutes(value any) []string {
	var result []string
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			result = append(result, hostRules(stringify(item))...)
		}
	case map[string]any:
		for _, item := range typed {
			result = append(result, hostRules(stringValue(item))...)
		}
	}
	return uniqueSorted(result)
}

func stringify(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case map[string]any:
		if context, ok := typed["context"].(string); ok {
			return context
		}
		return fmt.Sprint(typed)
	default:
		return fmt.Sprint(typed)
	}
}

func stringifySlice(values []any) []string {
	var result []string
	for _, value := range values {
		if text := stringify(value); text != "" {
			result = append(result, text)
		}
	}
	return result
}

func networks(value any) []string {
	switch typed := value.(type) {
	case []any:
		return stringifySlice(typed)
	case map[string]any:
		var result []string
		for name := range typed {
			result = append(result, name)
		}
		sort.Strings(result)
		return result
	default:
		return nil
	}
}

func dependsOn(value any) []string {
	switch typed := value.(type) {
	case []any:
		return stringifySlice(typed)
	case map[string]any:
		var result []string
		for name := range typed {
			result = append(result, name)
		}
		sort.Strings(result)
		return result
	default:
		return nil
	}
}
