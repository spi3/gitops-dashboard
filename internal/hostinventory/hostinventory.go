package hostinventory

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/parser"
)

func RepositoryName(target config.PingTarget) string {
	if target.Repository != "" {
		return target.Repository
	}
	return "ping/" + target.EffectiveName()
}

func Source(target config.PingTarget) string {
	if target.AnsibleInventory != "" {
		return target.AnsibleInventory
	}
	if target.Host != "" {
		return target.Host
	}
	return "runtime.ping"
}

func ServicesForTarget(target config.PingTarget, inventoryPath, commit string) ([]core.Service, error) {
	hosts := map[string]parser.AnsibleHost{}
	if target.AnsibleInventory != "" {
		parsed, err := parser.ParseAnsibleHosts(inventoryPath)
		if err != nil {
			return nil, err
		}
		for _, host := range parsed {
			hosts[host.Name] = host
		}
	}
	if target.Host != "" {
		name := target.Host
		if target.AnsibleInventory == "" && target.Name != "" {
			name = target.Name
		}
		hosts[name] = parser.AnsibleHost{Name: name, Address: target.Host}
	}

	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)

	repository := RepositoryName(target)
	source := Source(target)
	environment := strings.TrimSpace(target.Environment)
	if environment == "" {
		environment = "infrastructure"
	}
	services := make([]core.Service, 0, len(names))
	for _, name := range names {
		host := hosts[name]
		address := strings.TrimSpace(host.Address)
		if address == "" {
			address = name
		}
		services = append(services, core.Service{
			ID:           hostServiceID(repository, name),
			Name:         name,
			Repository:   repository,
			SourceCommit: commit,
			SourcePath:   source,
			Runtime:      "host",
			Kind:         "Host",
			ResourceName: address,
			Environment:  environment,
			Health:       core.HealthUnknown,
			Exposure:     []string{fmt.Sprintf("host/%s", address)},
			ConfigRefs:   []string{fmt.Sprintf("ansible_host=%s", address)},
		})
	}
	return services, nil
}

func hostServiceID(repository, name string) string {
	h := sha1.New()
	for _, part := range []string{"host", repository, name} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
