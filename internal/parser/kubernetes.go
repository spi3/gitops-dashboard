package parser

import (
	"bytes"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

type KubernetesResource struct {
	Kind       string
	Name       string
	Namespace  string
	Images     []string
	Ports      []string
	Storage    []string
	Exposure   []string
	ConfigRefs []string
	Warnings   []string
}

func (resource KubernetesResource) IsWorkload() bool {
	switch resource.Kind {
	case "Deployment", "StatefulSet", "DaemonSet":
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
		var doc map[string]any
		if err := decoder.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
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
	resource := KubernetesResource{Kind: kind, Name: name, Namespace: namespace}
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet":
		resource.parseWorkload(doc)
	case "Service":
		resource.Ports = portsFromService(doc)
		resource.Exposure = append(resource.Exposure, fmt.Sprintf("service/%s", name))
	case "Ingress":
		resource.Exposure = ingressHosts(doc)
	case "PersistentVolumeClaim":
		resource.Storage = append(resource.Storage, name)
	case "Namespace", "ConfigMap", "Secret":
	default:
		return KubernetesResource{}, false
	}
	return resource, true
}

func (resource *KubernetesResource) parseWorkload(doc map[string]any) {
	spec := mapValue(doc["spec"])
	template := mapValue(spec["template"])
	podSpec := mapValue(mapValue(template["spec"])["containers"])
	_ = podSpec
	containers := listValue(mapValue(template["spec"])["containers"])
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
		if _, ok := container["readinessProbe"]; !ok {
			resource.Warnings = append(resource.Warnings, "missing readiness probe")
		}
		if _, ok := container["livenessProbe"]; !ok {
			resource.Warnings = append(resource.Warnings, "missing liveness probe")
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
	}
	for _, volume := range listValue(mapValue(template["spec"])["volumes"]) {
		volumeMap := mapValue(volume)
		if claim := mapValue(volumeMap["persistentVolumeClaim"]); len(claim) > 0 {
			if name, ok := claim["claimName"].(string); ok {
				resource.Storage = append(resource.Storage, name)
			}
		}
	}
	sort.Strings(resource.Images)
	sort.Strings(resource.Ports)
	sort.Strings(resource.Storage)
	sort.Strings(resource.ConfigRefs)
	sort.Strings(resource.Warnings)
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

func ingressHosts(doc map[string]any) []string {
	var result []string
	for _, rule := range listValue(mapValue(doc["spec"])["rules"]) {
		ruleMap := mapValue(rule)
		if host, ok := ruleMap["host"].(string); ok {
			result = append(result, host)
		}
	}
	return result
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
