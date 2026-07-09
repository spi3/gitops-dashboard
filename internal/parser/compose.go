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
	Image       string    `yaml:"image"`
	Build       yaml.Node `yaml:"build"`
	Ports       yaml.Node `yaml:"ports"`
	Volumes     yaml.Node `yaml:"volumes"`
	Networks    yaml.Node `yaml:"networks"`
	DependsOn   yaml.Node `yaml:"depends_on"`
	Environment yaml.Node `yaml:"environment"`
	Labels      yaml.Node `yaml:"labels"`
	Healthcheck yaml.Node `yaml:"healthcheck"`
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
			Build:     stringifyNode(raw.Build),
			Ports:     stringifyNodeSlice(raw.Ports),
			Volumes:   stringifyNodeSlice(raw.Volumes),
			Networks:  networks(raw.Networks),
			Exposure:  composeExposure(raw),
			DependsOn: dependsOn(raw.DependsOn),
			EnvVars:   envVars(raw.Environment),
		}
		service.Warnings = append(service.Warnings, composeShapeWarnings(name, raw)...)
		if !hasComposeHealthcheck(raw.Healthcheck) {
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

func envVars(value yaml.Node) []string {
	value = resolveAlias(value)
	switch value.Kind {
	case yaml.SequenceNode:
		var result []string
		for _, item := range value.Content {
			name := strings.SplitN(stringifyNode(*item), "=", 2)[0]
			if name != "" {
				result = append(result, name)
			}
		}
		sort.Strings(result)
		return result
	case yaml.MappingNode:
		var result []string
		for i := 0; i+1 < len(value.Content); i += 2 {
			if name := stringifyNode(*value.Content[i]); name != "" {
				result = append(result, name)
			}
		}
		sort.Strings(result)
		return result
	default:
		return nil
	}
}

func composeExposure(service composeService) []string {
	var result []string
	for _, port := range nodeSequence(service.Ports) {
		result = append(result, publishedPortRoutes(*port)...)
	}
	result = append(result, staticAddressRoutes(service.Networks, service.Ports)...)
	result = append(result, labelRoutes(service.Labels)...)
	return uniqueSorted(result)
}

func publishedPortRoutes(value yaml.Node) []string {
	value = resolveAlias(value)
	switch value.Kind {
	case yaml.ScalarNode:
		typed := value.Value
		host, published, ok := shortPublishedPort(typed)
		if !ok || host == "" || host == "0.0.0.0" {
			return nil
		}
		return []string{accessRoute(host, published)}
	case yaml.MappingNode:
		published := stringValueNode(mappingValue(value, "published"))
		host := stringValueNode(mappingValue(value, "host_ip"))
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

func staticAddressRoutes(networks yaml.Node, ports yaml.Node) []string {
	var addresses []string
	for _, network := range mappingValues(networks) {
		ip := stringValueNode(mappingValue(network, "ipv4_address"))
		if ip != "" {
			addresses = append(addresses, ip)
		}
	}
	var targetPorts []string
	for _, port := range nodeSequence(ports) {
		if target := targetPort(*port); target != "" {
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

func targetPort(value yaml.Node) string {
	value = resolveAlias(value)
	switch value.Kind {
	case yaml.ScalarNode:
		value := strings.TrimSpace(strings.SplitN(value.Value, "/", 2)[0])
		parts := strings.Split(value, ":")
		return parts[len(parts)-1]
	case yaml.MappingNode:
		return stringValueNode(mappingValue(value, "target"))
	default:
		return ""
	}
}

func labelRoutes(value yaml.Node) []string {
	value = resolveAlias(value)
	var result []string
	switch value.Kind {
	case yaml.SequenceNode:
		for _, item := range value.Content {
			result = append(result, hostRules(stringifyNode(*item))...)
		}
	case yaml.MappingNode:
		for _, item := range mappingValues(value) {
			result = append(result, hostRules(stringValueNode(item))...)
		}
	}
	return uniqueSorted(result)
}

func stringifyNode(value yaml.Node) string {
	value = resolveAlias(value)
	switch value.Kind {
	case 0:
		return ""
	case yaml.ScalarNode:
		return value.Value
	case yaml.MappingNode:
		if context := stringValueNode(mappingValue(value, "context")); context != "" {
			return context
		}
		return fmt.Sprint(nodeToMap(value))
	default:
		return fmt.Sprint(nodeToValue(value))
	}
}

func stringifyNodeSlice(values yaml.Node) []string {
	var result []string
	for _, value := range nodeSequence(values) {
		if text := stringifyNode(*value); text != "" {
			result = append(result, text)
		}
	}
	return result
}

func networks(value yaml.Node) []string {
	value = resolveAlias(value)
	switch value.Kind {
	case yaml.SequenceNode:
		return stringifyNodeSlice(value)
	case yaml.MappingNode:
		var result []string
		for i := 0; i+1 < len(value.Content); i += 2 {
			if name := stringifyNode(*value.Content[i]); name != "" {
				result = append(result, name)
			}
		}
		sort.Strings(result)
		return result
	default:
		return nil
	}
}

func dependsOn(value yaml.Node) []string {
	value = resolveAlias(value)
	switch value.Kind {
	case yaml.SequenceNode:
		return stringifyNodeSlice(value)
	case yaml.MappingNode:
		var result []string
		for i := 0; i+1 < len(value.Content); i += 2 {
			if name := stringifyNode(*value.Content[i]); name != "" {
				result = append(result, name)
			}
		}
		sort.Strings(result)
		return result
	default:
		return nil
	}
}

func hasComposeHealthcheck(value yaml.Node) bool {
	value = resolveAlias(value)
	return value.Kind != 0 && len(value.Content) > 0
}

func composeShapeWarnings(serviceName string, service composeService) []string {
	fields := []struct {
		name      string
		value     yaml.Node
		supported map[yaml.Kind]struct{}
	}{
		{name: "build", value: service.Build, supported: map[yaml.Kind]struct{}{yaml.ScalarNode: {}, yaml.MappingNode: {}}},
		{name: "ports", value: service.Ports, supported: map[yaml.Kind]struct{}{yaml.SequenceNode: {}}},
		{name: "volumes", value: service.Volumes, supported: map[yaml.Kind]struct{}{yaml.SequenceNode: {}}},
		{name: "networks", value: service.Networks, supported: map[yaml.Kind]struct{}{yaml.SequenceNode: {}, yaml.MappingNode: {}}},
		{name: "depends_on", value: service.DependsOn, supported: map[yaml.Kind]struct{}{yaml.SequenceNode: {}, yaml.MappingNode: {}}},
		{name: "environment", value: service.Environment, supported: map[yaml.Kind]struct{}{yaml.SequenceNode: {}, yaml.MappingNode: {}}},
		{name: "labels", value: service.Labels, supported: map[yaml.Kind]struct{}{yaml.SequenceNode: {}, yaml.MappingNode: {}}},
	}
	var warnings []string
	for _, field := range fields {
		value := resolveAlias(field.value)
		if value.Kind == 0 {
			continue
		}
		if _, ok := field.supported[value.Kind]; ok {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("unsupported compose services.%s.%s shape: %s", serviceName, field.name, yamlKindName(value.Kind)))
	}
	sort.Strings(warnings)
	return warnings
}

func nodeSequence(value yaml.Node) []*yaml.Node {
	value = resolveAlias(value)
	if value.Kind != yaml.SequenceNode {
		return nil
	}
	return value.Content
}

func mappingValue(value yaml.Node, key string) yaml.Node {
	value = resolveAlias(value)
	if value.Kind != yaml.MappingNode {
		return yaml.Node{}
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		if value.Content[i].Value == key {
			return *value.Content[i+1]
		}
	}
	return yaml.Node{}
}

func mappingValues(value yaml.Node) []yaml.Node {
	value = resolveAlias(value)
	if value.Kind != yaml.MappingNode {
		return nil
	}
	result := make([]yaml.Node, 0, len(value.Content)/2)
	for i := 1; i < len(value.Content); i += 2 {
		result = append(result, *value.Content[i])
	}
	return result
}

func stringValueNode(value yaml.Node) string {
	value = resolveAlias(value)
	if value.Kind == 0 {
		return ""
	}
	if value.Kind == yaml.ScalarNode {
		return value.Value
	}
	return stringValue(nodeToValue(value))
}

func nodeToMap(value yaml.Node) map[string]any {
	value = resolveAlias(value)
	result := map[string]any{}
	if value.Kind != yaml.MappingNode {
		return result
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		result[value.Content[i].Value] = nodeToValue(*value.Content[i+1])
	}
	return result
}

func nodeToValue(value yaml.Node) any {
	return nodeToValueSeen(value, nil, 0)
}

func nodeToValueSeen(value yaml.Node, seen map[*yaml.Node]struct{}, depth int) any {
	value = resolveAliasSeen(value, seen, depth)
	switch value.Kind {
	case yaml.ScalarNode:
		return value.Value
	case yaml.SequenceNode:
		result := make([]any, 0, len(value.Content))
		for _, item := range value.Content {
			result = append(result, nodeToValueSeen(*item, seen, depth+1))
		}
		return result
	case yaml.MappingNode:
		return nodeToMapSeen(value, seen, depth+1)
	default:
		return nil
	}
}

func nodeToMapSeen(value yaml.Node, seen map[*yaml.Node]struct{}, depth int) map[string]any {
	value = resolveAliasSeen(value, seen, depth)
	result := map[string]any{}
	if value.Kind != yaml.MappingNode {
		return result
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		result[value.Content[i].Value] = nodeToValueSeen(*value.Content[i+1], seen, depth+1)
	}
	return result
}

func resolveAlias(value yaml.Node) yaml.Node {
	return resolveAliasSeen(value, nil, 0)
}

func resolveAliasSeen(value yaml.Node, seen map[*yaml.Node]struct{}, depth int) yaml.Node {
	const maxAliasDepth = 64
	for value.Kind == yaml.AliasNode && value.Alias != nil {
		if depth >= maxAliasDepth {
			return yaml.Node{}
		}
		if seen == nil {
			seen = map[*yaml.Node]struct{}{}
		}
		if _, ok := seen[value.Alias]; ok {
			return yaml.Node{}
		}
		seen[value.Alias] = struct{}{}
		value = *value.Alias
		depth++
	}
	return value
}

func yamlKindName(kind yaml.Kind) string {
	switch kind {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return "unknown"
	}
}
