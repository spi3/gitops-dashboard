package parser

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type AnsibleHost struct {
	Name    string
	Address string
}

func ParseAnsibleHosts(path string) ([]AnsibleHost, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ansible inventory %s: %w", path, err)
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse ansible inventory %s: %w", path, err)
	}
	hosts := map[string]AnsibleHost{}
	collectAnsibleHosts(raw, hosts)
	result := make([]AnsibleHost, 0, len(hosts))
	for _, host := range hosts {
		result = append(result, host)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func collectAnsibleHosts(value any, hosts map[string]AnsibleHost) {
	group := mapValue(value)
	if len(group) == 0 {
		return
	}
	if rawHosts := mapValue(group["hosts"]); len(rawHosts) > 0 {
		for name, rawHost := range rawHosts {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			hostVars := mapValue(rawHost)
			address := strings.TrimSpace(stringValue(hostVars["ansible_host"]))
			if address == "" {
				address = name
			}
			if existing, ok := hosts[name]; ok {
				if existing.Address == existing.Name && address != name {
					existing.Address = address
					hosts[name] = existing
				}
				continue
			}
			hosts[name] = AnsibleHost{Name: name, Address: address}
		}
	}
	for _, child := range mapValue(group["children"]) {
		collectAnsibleHosts(child, hosts)
	}
	for key, child := range group {
		if key == "children" || key == "hosts" || key == "vars" {
			continue
		}
		childGroup := mapValue(child)
		if _, ok := childGroup["hosts"]; ok {
			collectAnsibleHosts(child, hosts)
			continue
		}
		if _, ok := childGroup["children"]; ok {
			collectAnsibleHosts(child, hosts)
		}
	}
}
