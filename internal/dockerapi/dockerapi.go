package dockerapi

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

const DefaultHost = "unix:///var/run/docker.sock"

type Container struct {
	ID           string            `json:"Id"`
	Names        []string          `json:"Names"`
	Image        string            `json:"Image"`
	ImageID      string            `json:"ImageID"`
	RepoDigests  []string          `json:"RepoDigests"`
	Labels       map[string]string `json:"Labels"`
	State        string            `json:"State"`
	Status       string            `json:"Status"`
	RestartCount int               `json:"RestartCount"`
	Health       string            `json:"Health"`
}

// containerSummary is the response shape returned by Docker Engine's
// /containers/json endpoint. Docker documents Health as a string on some API
// versions, while current dockerd returns an object containing Status.
// Keep that wire-format difference at the API boundary and expose the
// normalized status string used by the monitor.
type containerSummary struct {
	ID           string            `json:"Id"`
	Names        []string          `json:"Names"`
	Image        string            `json:"Image"`
	ImageID      string            `json:"ImageID"`
	RepoDigests  []string          `json:"RepoDigests"`
	Labels       map[string]string `json:"Labels"`
	State        string            `json:"State"`
	Status       string            `json:"Status"`
	RestartCount int               `json:"RestartCount"`
	Health       containerHealth   `json:"Health"`
}

func (summary containerSummary) container() Container {
	return Container{
		ID:           summary.ID,
		Names:        summary.Names,
		Image:        summary.Image,
		ImageID:      summary.ImageID,
		RepoDigests:  summary.RepoDigests,
		Labels:       summary.Labels,
		State:        summary.State,
		Status:       summary.Status,
		RestartCount: summary.RestartCount,
		Health:       string(summary.Health),
	}
}

type containerHealth string

func (health *containerHealth) UnmarshalJSON(value []byte) error {
	if string(value) == "null" {
		*health = ""
		return nil
	}

	var status string
	if err := json.Unmarshal(value, &status); err == nil {
		*health = containerHealth(status)
		return nil
	}

	var detail struct {
		Status string `json:"Status"`
	}
	if err := json.Unmarshal(value, &detail); err != nil {
		return fmt.Errorf("decode Docker container health: %w", err)
	}
	*health = containerHealth(detail.Status)
	return nil
}

type ImageInspect struct {
	RepoDigests []string `json:"RepoDigests"`
}

func ListContainers(ctx context.Context, host string) ([]Container, error) {
	if host == "" {
		host = DefaultHost
	}
	client, baseURL, err := HTTPClient(host)
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
	var summaries []containerSummary
	if err := json.NewDecoder(resp.Body).Decode(&summaries); err != nil {
		return nil, err
	}
	containers := make([]Container, len(summaries))
	for i, summary := range summaries {
		containers[i] = summary.container()
	}
	return containers, nil
}

type ImageInspector struct {
	client  *http.Client
	baseURL string
	cache   map[string][]string
}

func NewImageInspector(host string) (*ImageInspector, error) {
	if host == "" {
		host = DefaultHost
	}
	client, baseURL, err := HTTPClient(host)
	if err != nil {
		return nil, err
	}
	return &ImageInspector{
		client:  client,
		baseURL: baseURL,
		cache:   map[string][]string{},
	}, nil
}

func (inspector *ImageInspector) RepoDigests(ctx context.Context, container Container) []string {
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
		return MergeRepoDigests(container.RepoDigests, digests)
	}
	digests := inspector.inspectRepoDigests(ctx, key)
	inspector.cache[key] = digests
	return MergeRepoDigests(container.RepoDigests, digests)
}

func (inspector *ImageInspector) inspectRepoDigests(ctx context.Context, key string) []string {
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
	var image ImageInspect
	if err := json.NewDecoder(resp.Body).Decode(&image); err != nil {
		return nil
	}
	return image.RepoDigests
}

func MergeRepoDigests(values ...[]string) []string {
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

func LiveContainer(state, status string) bool {
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

func HTTPClient(host string) (*http.Client, string, error) {
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
