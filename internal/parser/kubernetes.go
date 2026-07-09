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

type kubeDocument struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   kubeMetadata      `yaml:"metadata"`
	Spec       yaml.Node         `yaml:"spec"`
	Data       map[string]string `yaml:"data"`
	Warnings   []string          `yaml:"-"`
}

type kubeMetadata struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace"`
	Labels    map[string]string `yaml:"labels"`
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
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
		docNode := documentContentNode(node)
		if docNode.Kind != yaml.MappingNode {
			continue
		}
		kind := stringValueFromYAMLNode(mappingValue(docNode, "kind"))
		if !supportedKubernetesKind(kind) {
			continue
		}
		doc := decodeKubeDocument(docNode, kind)
		resource, ok := parseKubeDoc(doc)
		if ok {
			resources = append(resources, resource)
		}
	}
	return resources, nil
}

func supportedKubernetesKind(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "HelmRelease", "Service", "Ingress", "PersistentVolumeClaim", "Namespace", "ConfigMap", "Secret":
		return true
	default:
		return false
	}
}

func documentContentNode(node yaml.Node) yaml.Node {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return *node.Content[0]
	}
	return node
}

func decodeKubeDocument(docNode yaml.Node, kind string) kubeDocument {
	doc := kubeDocument{
		APIVersion: stringValueFromYAMLNode(mappingValue(docNode, "apiVersion")),
		Kind:       kind,
		Spec:       mappingValue(docNode, "spec"),
	}
	metadataNode := mappingValue(docNode, "metadata")
	decodeKubeField(metadataNode, &doc.Metadata, &doc.Warnings, kind, "metadata")
	applyKubeMetadataFallback(metadataNode, &doc.Metadata)
	switch kind {
	case "ConfigMap", "Secret":
		decodeKubeField(mappingValue(docNode, "data"), &doc.Data, &doc.Warnings, kind, "data")
	}
	sort.Strings(doc.Warnings)
	return doc
}

func decodeKubeField(node yaml.Node, value any, warnings *[]string, kind, field string) {
	if node.Kind == 0 {
		return
	}
	if err := node.Decode(value); err != nil {
		*warnings = append(*warnings, fmt.Sprintf("unsupported Kubernetes %s %s shape: %v", kind, field, err))
	}
}

func applyKubeMetadataFallback(node yaml.Node, metadata *kubeMetadata) {
	node = resolveAlias(node)
	if node.Kind != yaml.MappingNode {
		return
	}
	if name := stringValueFromYAMLNode(mappingValue(node, "name")); name != "" {
		metadata.Name = name
	}
	if namespace := stringValueFromYAMLNode(mappingValue(node, "namespace")); namespace != "" {
		metadata.Namespace = namespace
	}
	if metadata.Labels == nil {
		var labels map[string]string
		if labelsNode := mappingValue(node, "labels"); labelsNode.Kind != 0 && labelsNode.Decode(&labels) == nil {
			metadata.Labels = labels
		}
	}
}

func parseKubeDoc(doc kubeDocument) (KubernetesResource, bool) {
	kind := doc.Kind
	if kind == "" {
		return KubernetesResource{}, false
	}
	name := doc.Metadata.Name
	if name == "" {
		return KubernetesResource{}, false
	}
	namespace := doc.Metadata.Namespace
	if namespace == "" {
		namespace = "default"
	}
	resource := KubernetesResource{
		Kind:      kind,
		Name:      name,
		Namespace: namespace,
		Labels:    doc.Metadata.Labels,
		Warnings:  append([]string{}, doc.Warnings...),
	}
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob":
		resource.parseWorkload(doc)
	case "HelmRelease":
		resource.parseHelmRelease(doc)
	case "Service":
		spec := serviceSpec(doc.Spec, &resource.Warnings, kind)
		resource.Ports = portsFromService(spec)
		resource.Selector = spec.Selector
		resource.Exposure = serviceExposure(spec)
		resource.Exposure = append(resource.Exposure, fmt.Sprintf("service/%s", name))
	case "Ingress":
		spec := ingressSpec(doc.Spec, &resource.Warnings, kind)
		resource.Backends, resource.Exposure = ingressRoutes(spec)
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

type kubePodTemplate struct {
	Metadata kubeMetadata `yaml:"metadata"`
	Spec     kubePodSpec  `yaml:"spec"`
}

type kubeWorkloadSpec struct {
	Template kubePodTemplate `yaml:"template"`
}

type kubeCronJobSpec struct {
	JobTemplate struct {
		Spec struct {
			Template kubePodTemplate `yaml:"template"`
		} `yaml:"spec"`
	} `yaml:"jobTemplate"`
}

type kubePodSpec struct {
	InitContainers []kubeContainer `yaml:"initContainers"`
	Containers     []kubeContainer `yaml:"containers"`
	Volumes        []kubeVolume    `yaml:"volumes"`
}

type kubeContainer struct {
	Image          string              `yaml:"image"`
	Ports          []kubeContainerPort `yaml:"ports"`
	ReadinessProbe yaml.Node           `yaml:"readinessProbe"`
	LivenessProbe  yaml.Node           `yaml:"livenessProbe"`
	EnvFrom        []kubeEnvFrom       `yaml:"envFrom"`
	Env            []kubeEnvVar        `yaml:"env"`
}

type kubeContainerPort struct {
	ContainerPort yaml.Node `yaml:"containerPort"`
}

type kubeEnvFrom struct {
	ConfigMapRef *kubeNamedRef `yaml:"configMapRef"`
	SecretRef    *kubeNamedRef `yaml:"secretRef"`
}

type kubeEnvVar struct {
	ValueFrom kubeEnvValueFrom `yaml:"valueFrom"`
}

type kubeEnvValueFrom struct {
	ConfigMapKeyRef *kubeNamedRef `yaml:"configMapKeyRef"`
	SecretKeyRef    *kubeNamedRef `yaml:"secretKeyRef"`
}

type kubeNamedRef struct {
	Name string `yaml:"name"`
}

type kubeVolume struct {
	PersistentVolumeClaim *struct {
		ClaimName string `yaml:"claimName"`
	} `yaml:"persistentVolumeClaim"`
}

func (resource *KubernetesResource) parseWorkload(doc kubeDocument) {
	template := podTemplateForWorkload(resource.Kind, doc.Spec, &resource.Warnings)
	resource.Labels = mergeStringMaps(resource.Labels, template.Metadata.Labels)
	podSpec := template.Spec
	containers := append(podSpec.InitContainers, podSpec.Containers...)
	for _, item := range containers {
		if item.Image != "" {
			resource.Images = append(resource.Images, item.Image)
		}
		for _, port := range item.Ports {
			if value := stringValueFromYAMLNode(port.ContainerPort); value != "" {
				resource.Ports = append(resource.Ports, value)
			}
		}
		if resource.Kind != "Job" && resource.Kind != "CronJob" {
			if item.ReadinessProbe.Kind == 0 {
				resource.Warnings = append(resource.Warnings, "missing readiness probe")
			}
			if item.LivenessProbe.Kind == 0 {
				resource.Warnings = append(resource.Warnings, "missing liveness probe")
			}
		}
		for _, envFrom := range item.EnvFrom {
			if envFrom.ConfigMapRef != nil && envFrom.ConfigMapRef.Name != "" {
				resource.ConfigRefs = append(resource.ConfigRefs, "configMapRef/"+envFrom.ConfigMapRef.Name)
			}
			if envFrom.SecretRef != nil && envFrom.SecretRef.Name != "" {
				resource.ConfigRefs = append(resource.ConfigRefs, "secretRef/"+envFrom.SecretRef.Name)
			}
		}
		for _, env := range item.Env {
			resource.ConfigRefs = append(resource.ConfigRefs, envValueRefs(env)...)
		}
	}
	for _, volume := range podSpec.Volumes {
		if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName != "" {
			resource.Storage = append(resource.Storage, volume.PersistentVolumeClaim.ClaimName)
		}
	}
	sort.Strings(resource.Dependencies)
	sort.Strings(resource.Images)
	sort.Strings(resource.Ports)
	sort.Strings(resource.Storage)
	sort.Strings(resource.ConfigRefs)
	sort.Strings(resource.Warnings)
}

func podTemplateForWorkload(kind string, specNode yaml.Node, warnings *[]string) kubePodTemplate {
	if kind == "CronJob" {
		var spec kubeCronJobSpec
		decodeKubeSpec(specNode, &spec, warnings, kind)
		return spec.JobTemplate.Spec.Template
	}
	var spec kubeWorkloadSpec
	decodeKubeSpec(specNode, &spec, warnings, kind)
	return spec.Template
}

func envValueRefs(env kubeEnvVar) []string {
	var result []string
	if env.ValueFrom.ConfigMapKeyRef != nil && env.ValueFrom.ConfigMapKeyRef.Name != "" {
		result = append(result, "configMapKeyRef/"+env.ValueFrom.ConfigMapKeyRef.Name)
	}
	if env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name != "" {
		result = append(result, "secretKeyRef/"+env.ValueFrom.SecretKeyRef.Name)
	}
	sort.Strings(result)
	return result
}

type kubeObjectRef struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
}

type kubeHelmReleaseSpec struct {
	Chart struct {
		Spec struct {
			Chart     string        `yaml:"chart"`
			SourceRef kubeObjectRef `yaml:"sourceRef"`
		} `yaml:"spec"`
	} `yaml:"chart"`
	ValuesFrom []kubeObjectRef `yaml:"valuesFrom"`
}

func (resource *KubernetesResource) parseHelmRelease(doc kubeDocument) {
	var spec kubeHelmReleaseSpec
	decodeKubeSpec(doc.Spec, &spec, &resource.Warnings, resource.Kind)
	if spec.Chart.Spec.Chart != "" {
		resource.Dependencies = append(resource.Dependencies, "chart/"+spec.Chart.Spec.Chart)
	}
	sourceRef := spec.Chart.Spec.SourceRef
	if sourceRef.Kind != "" && sourceRef.Name != "" {
		resource.ConfigRefs = append(resource.ConfigRefs, sourceRef.Kind+"/"+sourceRef.Name)
	}
	for _, valueRef := range spec.ValuesFrom {
		if valueRef.Kind != "" && valueRef.Name != "" {
			resource.ConfigRefs = append(resource.ConfigRefs, valueRef.Kind+"/"+valueRef.Name)
		}
	}
	sort.Strings(resource.Warnings)
	sort.Strings(resource.ConfigRefs)
	sort.Strings(resource.Dependencies)
}

type kubeServiceSpec struct {
	Selector       map[string]string `yaml:"selector"`
	Type           string            `yaml:"type"`
	LoadBalancerIP string            `yaml:"loadBalancerIP"`
	ExternalIPs    []string          `yaml:"externalIPs"`
	Ports          []kubeServicePort `yaml:"ports"`
}

type kubeServicePort struct {
	Port yaml.Node `yaml:"port"`
}

func serviceSpec(specNode yaml.Node, warnings *[]string, kind string) kubeServiceSpec {
	var spec kubeServiceSpec
	decodeKubeSpec(specNode, &spec, warnings, kind)
	return spec
}

func portsFromService(spec kubeServiceSpec) []string {
	var result []string
	for _, port := range spec.Ports {
		if value := stringValueFromYAMLNode(port.Port); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func serviceExposure(spec kubeServiceSpec) []string {
	var addresses []string
	if spec.LoadBalancerIP != "" {
		addresses = append(addresses, spec.LoadBalancerIP)
	}
	addresses = append(addresses, spec.ExternalIPs...)
	var ports []string
	for _, port := range spec.Ports {
		if value := stringValueFromYAMLNode(port.Port); value != "" {
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

type kubeIngressSpec struct {
	Rules          []kubeIngressRule  `yaml:"rules"`
	TLS            []kubeIngressTLS   `yaml:"tls"`
	DefaultBackend kubeIngressBackend `yaml:"defaultBackend"`
}

type kubeIngressRule struct {
	Host string          `yaml:"host"`
	HTTP kubeIngressHTTP `yaml:"http"`
}

type kubeIngressHTTP struct {
	Paths []kubeIngressPath `yaml:"paths"`
}

type kubeIngressPath struct {
	Path    string             `yaml:"path"`
	Backend kubeIngressBackend `yaml:"backend"`
	Service kubeIngressService `yaml:"service"`
}

type kubeIngressBackend struct {
	Service kubeIngressService `yaml:"service"`
}

type kubeIngressService struct {
	Name string `yaml:"name"`
}

type kubeIngressTLS struct {
	Hosts []string `yaml:"hosts"`
}

func ingressSpec(specNode yaml.Node, warnings *[]string, kind string) kubeIngressSpec {
	var spec kubeIngressSpec
	decodeKubeSpec(specNode, &spec, warnings, kind)
	return spec
}

func ingressRoutes(spec kubeIngressSpec) ([]string, []string) {
	var backends []string
	var routes []string
	tlsHosts := ingressTLSHosts(spec)
	for _, rule := range spec.Rules {
		if rule.Host == "" {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			backends = append(backends, backendServiceName(path)...)
			routes = append(routes, ingressAccessRoute(rule.Host, path.Path, tlsHosts[rule.Host]))
		}
	}
	if spec.DefaultBackend.Service.Name != "" {
		backends = append(backends, spec.DefaultBackend.Service.Name)
	}
	return uniqueSorted(backends), uniqueSorted(routes)
}

func ingressTLSHosts(spec kubeIngressSpec) map[string]bool {
	result := map[string]bool{}
	for _, tls := range spec.TLS {
		for _, host := range tls.Hosts {
			if host != "" {
				result[host] = true
			}
		}
	}
	return result
}

func backendServiceName(path kubeIngressPath) []string {
	if path.Backend.Service.Name != "" {
		return []string{path.Backend.Service.Name}
	}
	if path.Service.Name != "" {
		return []string{path.Service.Name}
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

func configMapRoutes(doc kubeDocument) []string {
	var result []string
	for _, text := range doc.Data {
		result = append(result, routesFromEmbeddedYAML(text)...)
	}
	return uniqueSorted(result)
}

func decodeKubeSpec(spec yaml.Node, value any, warnings *[]string, kind string) {
	if spec.Kind == 0 {
		return
	}
	if err := spec.Decode(value); err != nil {
		*warnings = append(*warnings, fmt.Sprintf("unsupported Kubernetes %s spec shape: %v", kind, err))
	}
}

func stringValueFromYAMLNode(node yaml.Node) string {
	if node.Kind == 0 {
		return ""
	}
	if node.Kind == yaml.ScalarNode {
		return node.Value
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return ""
	}
	return stringValue(value)
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
