package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type dockerContainer struct {
	ID           string            `json:"Id"`
	Names        []string          `json:"Names"`
	Image        string            `json:"Image"`
	ImageID      string            `json:"ImageID"`
	RepoDigests  []string          `json:"RepoDigests"`
	Labels       map[string]string `json:"Labels"`
	State        string            `json:"State"`
	Status       string            `json:"Status"`
	RestartCount int               `json:"RestartCount"`
}

func listDockerContainers(ctx context.Context, host string) ([]dockerContainer, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	client, baseURL, err := dockerHTTPClient(host)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/containers/json?all=1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("docker api status %s", resp.Status)
	}
	var containers []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, err
	}
	return containers, nil
}

type dockerImageInspector struct {
	client  *http.Client
	baseURL string
	cache   map[string][]string
}

type dockerImageInspect struct {
	RepoDigests []string `json:"RepoDigests"`
}

func newDockerImageInspector(host string) (*dockerImageInspector, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	client, baseURL, err := dockerHTTPClient(host)
	if err != nil {
		return nil, err
	}
	return &dockerImageInspector{
		client:  client,
		baseURL: baseURL,
		cache:   map[string][]string{},
	}, nil
}

func (inspector *dockerImageInspector) repoDigests(ctx context.Context, container dockerContainer) []string {
	if inspector == nil {
		return container.RepoDigests
	}
	key := strings.TrimSpace(container.ImageID)
	if key == "" {
		key = strings.TrimSpace(container.Image)
	}
	if key == "" {
		return container.RepoDigests
	}
	if digests, ok := inspector.cache[key]; ok {
		return mergeDockerRepoDigests(container.RepoDigests, digests)
	}
	digests := inspector.inspectRepoDigests(ctx, key)
	inspector.cache[key] = digests
	return mergeDockerRepoDigests(container.RepoDigests, digests)
}

func (inspector *dockerImageInspector) inspectRepoDigests(ctx context.Context, key string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, inspector.baseURL+"/images/"+url.PathEscape(key)+"/json", nil)
	if err != nil {
		return nil
	}
	resp, err := inspector.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil
	}
	var image dockerImageInspect
	if err := json.NewDecoder(resp.Body).Decode(&image); err != nil {
		return nil
	}
	return image.RepoDigests
}

func mergeDockerRepoDigests(values ...[]string) []string {
	seen := map[string]struct{}{}
	var result []string
	for _, list := range values {
		for _, value := range list {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

func liveDockerContainer(state, status string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running", "restarting", "paused":
		return true
	case "":
		normalizedStatus := strings.ToLower(strings.TrimSpace(status))
		return strings.HasPrefix(normalizedStatus, "up") || strings.HasPrefix(normalizedStatus, "restarting")
	default:
		return false
	}
}

func dockerHTTPClient(host string) (*http.Client, string, error) {
	parsed, err := url.Parse(host)
	if err != nil {
		return nil, "", err
	}
	if parsed.Scheme == "unix" {
		socketPath := parsed.Path
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socketPath)
			},
		}
		return &http.Client{Transport: transport, Timeout: 10 * time.Second}, "http://docker", nil
	}
	if parsed.Scheme == "tcp" {
		parsed.Scheme = "http"
	}
	if parsed.Scheme == "" {
		return nil, "", fmt.Errorf("docker host must be unix, tcp, http, or https")
	}
	return &http.Client{Timeout: 10 * time.Second}, strings.TrimRight(parsed.String(), "/"), nil
}
