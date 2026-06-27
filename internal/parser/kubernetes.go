package parser

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type KubernetesResource struct {
	Kind         string
	Name         string
	Namespace    string
	SourcePath   string
	Labels       map[string]string
	Selector     map[string]string
	Backends     []string
	Images       []string
	Ports        []string
	Dependencies []string
	Storage      []string
	Exposure     []string
	ConfigRefs   []string
	Warnings     []string
}

func (resource KubernetesResource) IsWorkload() bool {
	switch resource.Kind {
	case "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "HelmRelease":
		return true
	default:
		return false
	}
}

func ParseKubernetes(path string) ([]KubernetesResource, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var resources []KubernetesResource
	for {
		var raw any
		if err := decoder.Decode(&raw); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
		doc := mapValue(raw)
		if len(doc) == 0 {
			continue
		}
		resource, ok := parseKubeDoc(doc)
		if ok {
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

func parseKubeDoc(doc map[string]any) (KubernetesResource, bool) {
	kind, _ := doc["kind"].(string)
	if kind == "" {
		return KubernetesResource{}, false
	}
	meta := mapValue(doc["metadata"])
	name, _ := meta["name"].(string)
	if name == "" {
		return KubernetesResource{}, false
	}
	namespace, _ := meta["namespace"].(string)
	if namespace == "" {
		namespace = "default"
	}
	resource := KubernetesResource{
		Kind:      kind,
		Name:      name,
		Namespace: namespace,
		Labels:    stringMap(meta["labels"]),
	}
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob":
		resource.parseWorkload(doc)
	case "HelmRelease":
		resource.parseHelmRelease(doc)
	case "Service":
		resource.Ports = portsFromService(doc)
		resource.Selector = stringMap(mapValue(doc["spec"])["selector"])
		resource.Exposure = serviceExposure(doc)
		resource.Exposure = append(resource.Exposure, fmt.Sprintf("service/%s", name))
	case "Ingress":
		resource.Backends, resource.Exposure = ingressRoutes(doc)
	case "PersistentVolumeClaim":
		resource.Storage = append(resource.Storage, name)
	case "Namespace", "ConfigMap", "Secret":
		if kind == "ConfigMap" {
			resource.Exposure = configMapRoutes(doc)
		}
	default:
		return KubernetesResource{}, false
	}
	return resource, true
}

func (resource *KubernetesResource) parseWorkload(doc map[string]any) {
	spec := mapValue(doc["spec"])
	template := mapValue(spec["template"])
	if resource.Kind == "CronJob" {
		jobSpec := mapValue(mapValue(spec["jobTemplate"])["spec"])
		template = mapValue(jobSpec["template"])
	}
	resource.Labels = mergeStringMaps(resource.Labels, stringMap(mapValue(template["metadata"])["labels"]))
	podSpec := podSpecForWorkload(resource.Kind, doc)
	containers := append(listValue(podSpec["initContainers"]), listValue(podSpec["containers"])...)
	for _, item := range containers {
		container := mapValue(item)
		if image, ok := container["image"].(string); ok {
			resource.Images = append(resource.Images, image)
		}
		for _, port := range listValue(container["ports"]) {
			portMap := mapValue(port)
			if value, ok := portMap["containerPort"]; ok {
				resource.Ports = append(resource.Ports, fmt.Sprint(value))
			}
		}
		if resource.Kind != "Job" && resource.Kind != "CronJob" {
			if _, ok := container["readinessProbe"]; !ok {
				resource.Warnings = append(resource.Warnings, "missing readiness probe")
			}
			if _, ok := container["livenessProbe"]; !ok {
				resource.Warnings = append(resource.Warnings, "missing liveness probe")
			}
		}
		for _, envFrom := range listValue(container["envFrom"]) {
			envMap := mapValue(envFrom)
			for key := range envMap {
				ref := mapValue(envMap[key])
				if name, ok := ref["name"].(string); ok {
					resource.ConfigRefs = append(resource.ConfigRefs, key+"/"+name)
				}
			}
		}
		for _, env := range listValue(container["env"]) {
			resource.ConfigRefs = append(resource.ConfigRefs, envValueRefs(env)...)
		}
	}
	for _, volume := range listValue(podSpec["volumes"]) {
		volumeMap := mapValue(volume)
		if claim := mapValue(volumeMap["persistentVolumeClaim"]); len(claim) > 0 {
			if name, ok := claim["claimName"].(string); ok {
				resource.Storage = append(resource.Storage, name)
			}
		}
	}
	sort.Strings(resource.Dependencies)
	sort.Strings(resource.Images)
	sort.Strings(resource.Ports)
	sort.Strings(resource.Storage)
	sort.Strings(resource.ConfigRefs)
	sort.Strings(resource.Warnings)
}

func podSpecForWorkload(kind string, doc map[string]any) map[string]any {
	spec := mapValue(doc["spec"])
	if kind == "CronJob" {
		jobTemplate := mapValue(spec["jobTemplate"])
		jobSpec := mapValue(jobTemplate["spec"])
		template := mapValue(jobSpec["template"])
		return mapValue(template["spec"])
	}
	template := mapValue(spec["template"])
	return mapValue(template["spec"])
}

func envValueRefs(value any) []string {
	env := mapValue(value)
	valueFrom := mapValue(env["valueFrom"])
	var result []string
	for key, raw := range valueFrom {
		ref := mapValue(raw)
		if name, ok := ref["name"].(string); ok {
			result = append(result, key+"/"+name)
		}
	}
	sort.Strings(result)
	return result
}

func (resource *KubernetesResource) parseHelmRelease(doc map[string]any) {
	spec := mapValue(doc["spec"])
	chart := mapValue(spec["chart"])
	chartSpec := mapValue(chart["spec"])
	if name, ok := chartSpec["chart"].(string); ok && name != "" {
		resource.Dependencies = append(resource.Dependencies, "chart/"+name)
	}
	sourceRef := mapValue(chartSpec["sourceRef"])
	sourceKind, _ := sourceRef["kind"].(string)
	sourceName, _ := sourceRef["name"].(string)
	if sourceKind != "" && sourceName != "" {
		resource.ConfigRefs = append(resource.ConfigRefs, sourceKind+"/"+sourceName)
	}
	for _, item := range listValue(spec["valuesFrom"]) {
		valueRef := mapValue(item)
		kind, _ := valueRef["kind"].(string)
		name, _ := valueRef["name"].(string)
		if kind != "" && name != "" {
			resource.ConfigRefs = append(resource.ConfigRefs, kind+"/"+name)
		}
	}
	sort.Strings(resource.ConfigRefs)
	sort.Strings(resource.Dependencies)
}

func portsFromService(doc map[string]any) []string {
	var result []string
	for _, port := range listValue(mapValue(doc["spec"])["ports"]) {
		portMap := mapValue(port)
		if value, ok := portMap["port"]; ok {
			result = append(result, fmt.Sprint(value))
		}
	}
	return result
}

func serviceExposure(doc map[string]any) []string {
	spec := mapValue(doc["spec"])
	var addresses []string
	if ip := stringValue(spec["loadBalancerIP"]); ip != "" {
		addresses = append(addresses, ip)
	}
	for _, value := range listValue(spec["externalIPs"]) {
		if ip := stringValue(value); ip != "" {
			addresses = append(addresses, ip)
		}
	}
	var ports []string
	for _, port := range listValue(spec["ports"]) {
		if value := stringValue(mapValue(port)["port"]); value != "" {
			ports = append(ports, value)
		}
	}
	var result []string
	for _, address := range uniqueSorted(addresses) {
		if len(ports) == 0 {
			result = append(result, accessRoute(address, ""))
			continue
		}
		for _, port := range uniqueSorted(ports) {
			result = append(result, accessRoute(address, port))
		}
	}
	return uniqueSorted(result)
}

func ingressRoutes(doc map[string]any) ([]string, []string) {
	var backends []string
	var routes []string
	tlsHosts := ingressTLSHosts(doc)
	for _, rule := range listValue(mapValue(doc["spec"])["rules"]) {
		ruleMap := mapValue(rule)
		if host, ok := ruleMap["host"].(string); ok {
			for _, path := range listValue(mapValue(ruleMap["http"])["paths"]) {
				pathMap := mapValue(path)
				backends = append(backends, backendServiceName(pathMap)...)
				routePath := stringValue(pathMap["path"])
				routes = append(routes, ingressAccessRoute(host, routePath, tlsHosts[host]))
			}
		}
	}
	if defaultBackend := mapValue(mapValue(doc["spec"])["defaultBackend"]); len(defaultBackend) > 0 {
		backends = append(backends, backendServiceName(defaultBackend)...)
	}
	return uniqueSorted(backends), uniqueSorted(routes)
}

func ingressTLSHosts(doc map[string]any) map[string]bool {
	result := map[string]bool{}
	for _, tls := range listValue(mapValue(doc["spec"])["tls"]) {
		for _, host := range listValue(mapValue(tls)["hosts"]) {
			if value := stringValue(host); value != "" {
				result[value] = true
			}
		}
	}
	return result
}

func backendServiceName(path map[string]any) []string {
	service := mapValue(mapValue(path["backend"])["service"])
	if len(service) == 0 {
		service = mapValue(path["service"])
	}
	if name := stringValue(service["name"]); name != "" {
		return []string{name}
	}
	return nil
}

func ingressAccessRoute(host, path string, hasTLS bool) string {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if hasTLS || !strings.HasSuffix(host, ".lan") {
		return "https://" + host + path
	}
	return "http://" + host + path
}

func configMapRoutes(doc map[string]any) []string {
	var result []string
	for _, value := range mapValue(doc["data"]) {
		text, ok := value.(string)
		if !ok {
			continue
		}
		result = append(result, routesFromEmbeddedYAML(text)...)
	}
	return uniqueSorted(result)
}

func routesFromEmbeddedYAML(value string) []string {
	var raw any
	if err := yaml.Unmarshal([]byte(value), &raw); err != nil {
		return nil
	}
	return routesFromValue("", raw)
}

func routesFromValue(key string, value any) []string {
	key = strings.ToLower(key)
	switch typed := value.(type) {
	case map[string]any:
		var result []string
		for childKey, childValue := range typed {
			result = append(result, routesFromValue(childKey, childValue)...)
		}
		return result
	case []any:
		var result []string
		for _, item := range typed {
			result = append(result, routesFromValue(key, item)...)
		}
		return result
	case string:
		if key == "host" || key == "hosts" || key == "url" || key == "root_url" || key == "externalurl" {
			return []string{accessRoute(typed, "")}
		}
	}
	return nil
}

func mapValue(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	if typed, ok := value.(map[any]any); ok {
		result := map[string]any{}
		for key, val := range typed {
			if text, ok := key.(string); ok {
				result[text] = val
			}
		}
		return result
	}
	return map[string]any{}
}

func listValue(value any) []any {
	if typed, ok := value.([]any); ok {
		return typed
	}
	return nil
}

func stringMap(value any) map[string]string {
	raw := mapValue(value)
	if len(raw) == 0 {
		return nil
	}
	result := map[string]string{}
	for key, value := range raw {
		text := stringValue(value)
		if text != "" {
			result[key] = text
		}
	}
	return result
}

func mergeStringMaps(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	result := map[string]string{}
	for key, value := range base {
		result[key] = value
	}
	for key, value := range override {
		result[key] = value
	}
	return result
}
