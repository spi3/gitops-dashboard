package parser

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type ComposeProject struct {
	Name     string
	Services []ComposeService
}

type ComposeService struct {
	Name        string
	Image       string
	Build       string
	Ports       []string
	NetworkMode string
	Volumes     []string
	Networks    []string
	Exposure    []string
	DependsOn   []string
	EnvVars     []string
	Warnings    []string
}

type composeFile struct {
	Name     string                    `yaml:"name"`
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	Image       string    `yaml:"image"`
	Build       yaml.Node `yaml:"build"`
	Ports       yaml.Node `yaml:"ports"`
	Expose      yaml.Node `yaml:"expose"`
	NetworkMode yaml.Node `yaml:"network_mode"`
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
			Name:        name,
			Image:       raw.Image,
			Build:       stringifyNode(raw.Build),
			Ports:       stringifyNodeSlice(raw.Ports),
			NetworkMode: stringValueNode(raw.NetworkMode),
			Volumes:     stringifyNodeSlice(raw.Volumes),
			Networks:    networks(raw.Networks),
			Exposure:    composeExposure(raw),
			DependsOn:   dependsOn(raw.DependsOn),
			EnvVars:     envVars(raw.Environment),
		}
		service.Warnings = append(service.Warnings, composeShapeWarnings(name, raw)...)
		service.Warnings = append(service.Warnings, composePortWarnings(name, raw.Ports)...)
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
	// Host networking has no independent container address or published-port
	// boundary. Do not turn its declarations into a guessed host endpoint.
	if composeNetworkMode(service.NetworkMode) == "host" {
		return labelRoutes(service.Labels)
	}
	for _, port := range nodeSequence(service.Ports) {
		result = append(result, publishedPortRoutes(*port)...)
	}
	result = append(result, staticAddressRoutes(service.Networks, service.Ports, service.Expose)...)
	result = append(result, labelRoutes(service.Labels)...)
	return uniqueSorted(result)
}

func composeNetworkMode(value yaml.Node) string {
	return strings.ToLower(strings.TrimSpace(stringValueNode(value)))
}

func publishedPortRoutes(value yaml.Node) []string {
	value = resolveAlias(value)
	switch value.Kind {
	case yaml.ScalarNode:
		typed := value.Value
		host, published, target, protocol, ok := shortPublishedPort(typed)
		if !ok || host == "" || wildcardBind(host) || !httpRouteProtocol(protocol) {
			return nil
		}
		return publishedRoutes(host, published, target, true)
	case yaml.MappingNode:
		published := stringValueNode(mappingValue(value, "published"))
		target := stringValueNode(mappingValue(value, "target"))
		host := stringValueNode(mappingValue(value, "host_ip"))
		protocol := stringValueNode(mappingValue(value, "protocol"))
		if published == "" || host == "" || wildcardBind(host) || !httpRouteProtocol(protocol) {
			return nil
		}
		return publishedRoutes(host, published, target, false)
	default:
		return nil
	}
}

func shortPublishedPort(value string) (string, string, string, string, bool) {
	value, protocol := splitPortProtocol(value)
	if !strings.Contains(value, ":") {
		return "", "", "", "", false
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 {
		return "", "", "", "", false
	}
	target := parts[len(parts)-1]
	published := parts[len(parts)-2]
	host := ""
	if len(parts) > 2 {
		host = strings.Join(parts[:len(parts)-2], ":")
	}
	if published == "" || target == "" {
		return "", "", "", "", false
	}
	return host, published, target, protocol, true
}

func wildcardBind(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().IsUnspecified()
	}
	return host == "0.0.0.0" || host == "::"
}

func httpRouteProtocol(protocol string) bool {
	return protocol == "" || strings.EqualFold(strings.TrimSpace(protocol), "tcp")
}

func splitPortProtocol(value string) (string, string) {
	parts := strings.SplitN(strings.TrimSpace(value), "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func publishedRoutes(host, published, target string, allowTargetRange bool) []string {
	// Long syntax defines target as one container port. Short syntax alone
	// permits matching published and target ranges.
	if !allowTargetRange && isPortRange(target) {
		return nil
	}
	publishedPorts := composePortRange(published)
	targetPorts := composePortRange(target)
	// Compose treats unequal port ranges as an allocation pool: Docker selects
	// one published port for the target. That runtime-selected host port is not
	// known from the declaration, so do not invent published-host routes.
	if len(publishedPorts) == 0 || len(publishedPorts) != len(targetPorts) {
		return nil
	}
	var result []string
	for _, port := range publishedPorts {
		result = append(result, accessRoute(host, port))
	}
	return result
}

func staticAddressRoutes(networks, ports, expose yaml.Node) []string {
	var addresses []string
	for _, network := range mappingValues(networks) {
		for _, key := range []string{"ipv4_address", "ipv6_address"} {
			if ip := stringValueNode(mappingValue(network, key)); ip != "" {
				addresses = append(addresses, ip)
			}
		}
	}
	targetPorts := composeTargetPorts(ports, expose)
	var result []string
	for _, address := range uniqueSorted(addresses) {
		if len(targetPorts) == 0 {
			// Retain the declared address as inventory, but make its
			// non-monitorable status explicit rather than guessing port 80.
			result = append(result, "address/"+address)
			continue
		}
		for _, port := range uniqueSorted(targetPorts) {
			result = append(result, accessRoute(address, port))
		}
	}
	return uniqueSorted(result)
}

func composeTargetPorts(ports, expose yaml.Node) []string {
	var result []string
	for _, source := range []yaml.Node{ports, expose} {
		for _, port := range nodeSequence(source) {
			result = append(result, targetPorts(*port)...)
		}
	}
	return uniqueSorted(result)
}

func targetPorts(value yaml.Node) []string {
	value = resolveAlias(value)
	var target, protocol string
	switch value.Kind {
	case yaml.ScalarNode:
		raw, parsedProtocol := splitPortProtocol(value.Value)
		protocol = parsedProtocol
		parts := strings.Split(raw, ":")
		target = parts[len(parts)-1]
	case yaml.MappingNode:
		target = stringValueNode(mappingValue(value, "target"))
		protocol = stringValueNode(mappingValue(value, "protocol"))
		// Compose long syntax accepts one target/container port, not a range.
		if isPortRange(target) {
			return nil
		}
	default:
		return nil
	}
	if !httpRouteProtocol(protocol) {
		return nil
	}
	return composePortRange(target)
}

func isPortRange(value string) bool {
	return strings.Contains(strings.TrimSpace(value), "-")
}

func composePortRange(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if !strings.Contains(value, "-") {
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil || port == 0 {
			return nil
		}
		return []string{value}
	}
	parts := strings.Split(value, "-")
	if len(parts) != 2 {
		return nil
	}
	start, startErr := strconv.ParseUint(parts[0], 10, 16)
	end, endErr := strconv.ParseUint(parts[1], 10, 16)
	if startErr != nil || endErr != nil || start == 0 || end < start {
		return nil
	}
	// Compose permits equivalent host:container port ranges. Each target port is
	// a declared container listener, so retaining every member avoids inventing
	// a synthetic single-port route. https://docs.docker.com/reference/compose-file/services/#ports
	result := make([]string, 0, end-start+1)
	for port := start; port <= end; port++ {
		result = append(result, strconv.FormatUint(port, 10))
	}
	return result
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

func composePortWarnings(serviceName string, ports yaml.Node) []string {
	var warnings []string
	for _, port := range nodeSequence(ports) {
		port := resolveAlias(*port)
		if port.Kind != yaml.MappingNode {
			continue
		}
		if target := stringValueNode(mappingValue(port, "target")); isPortRange(target) {
			warnings = append(warnings, fmt.Sprintf("invalid compose services.%s.ports target range skipped: long syntax target must be a single container port", serviceName))
		}
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
