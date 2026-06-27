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
	Services []ComposeService
}

type ComposeService struct {
	Name      string
	Image     string
	Build     string
	Ports     []string
	Volumes   []string
	Networks  []string
	DependsOn []string
	EnvVars   []string
	Warnings  []string
}

type composeFile struct {
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
	project := ComposeProject{}
	for _, name := range names {
		raw := file.Services[name]
		service := ComposeService{
			Name:      name,
			Image:     raw.Image,
			Build:     stringify(raw.Build),
			Ports:     stringifySlice(raw.Ports),
			Volumes:   stringifySlice(raw.Volumes),
			Networks:  networks(raw.Networks),
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
