package core

import "time"

type HealthState string

const (
	HealthHealthy       HealthState = "healthy"
	HealthDegraded      HealthState = "degraded"
	HealthUnhealthy     HealthState = "unhealthy"
	HealthUnknown       HealthState = "unknown"
	HealthError         HealthState = "error"
	HealthNotApplicable HealthState = "not_applicable"
)

type Repository struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	DefaultRef string `json:"defaultRef"`
	LastCommit string `json:"lastCommit"`
	LastScanAt string `json:"lastScanAt"`
	Status     string `json:"status"`
	Error      string `json:"error"`
}

type Scan struct {
	ID         int64  `json:"id"`
	Repository string `json:"repository"`
	Status     string `json:"status"`
	CommitSHA  string `json:"commitSha"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
	Error      string `json:"error"`
}

type BuildInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
}

type ImageVersionState string

const (
	ImageVersionMatching   ImageVersionState = "matching"
	ImageVersionMismatched ImageVersionState = "mismatched"
	ImageVersionUnknown    ImageVersionState = "unknown"
	ImageVersionMutable    ImageVersionState = "mutable"
)

type ImageReference struct {
	Original   string `json:"original"`
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest"`
}

type ObservedImage struct {
	Target      string           `json:"target"`
	Runtime     string           `json:"runtime"`
	Reference   ImageReference   `json:"reference"`
	ImageID     string           `json:"imageId"`
	RepoDigests []ImageReference `json:"repoDigests"`
}

type ImageVersionCheck struct {
	Desired  ImageReference    `json:"desired"`
	Observed *ObservedImage    `json:"observed,omitempty"`
	State    ImageVersionState `json:"state"`
	Message  string            `json:"message"`
}

type Service struct {
	ID                 string              `json:"id"`
	Name               string              `json:"name"`
	Repository         string              `json:"repository"`
	SourceCommit       string              `json:"sourceCommit"`
	SourcePath         string              `json:"sourcePath"`
	Runtime            string              `json:"runtime"`
	ComposeProject     string              `json:"composeProject,omitempty"`
	Kind               string              `json:"kind"`
	Namespace          string              `json:"namespace"`
	ResourceName       string              `json:"resourceName"`
	Environment        string              `json:"environment"`
	Health             HealthState         `json:"health"`
	Images             []string            `json:"images"`
	DesiredImages      []ImageReference    `json:"desiredImages"`
	ImageVersionState  ImageVersionState   `json:"imageVersionState"`
	ImageVersionChecks []ImageVersionCheck `json:"imageVersionChecks"`
	Ports              []string            `json:"ports"`
	Dependencies       []string            `json:"dependencies"`
	Storage            []string            `json:"storage"`
	Exposure           []string            `json:"exposure"`
	MonitorRoutes      []string            `json:"monitorRoutes"`
	ConfigRefs         []string            `json:"configRefs"`
	Warnings           []string            `json:"warnings"`
}

type StatusResult struct {
	ServiceID      string          `json:"serviceId"`
	Target         string          `json:"target"`
	Health         HealthState     `json:"health"`
	Message        string          `json:"message"`
	CheckedAt      time.Time       `json:"checkedAt"`
	ExpiresAt      time.Time       `json:"expiresAt,omitempty"`
	ObservedImages []ObservedImage `json:"observedImages"`
}

type UptimeSample struct {
	Health    HealthState `json:"health"`
	CheckedAt time.Time   `json:"checkedAt"`
	Message   string      `json:"message"`
}

type UptimeStat struct {
	ServiceID     string         `json:"serviceId"`
	Target        string         `json:"target"`
	UptimePercent float64        `json:"uptimePercent"`
	CheckCount    int            `json:"checkCount"`
	Samples       []UptimeSample `json:"samples"`
}

type DashboardSummary struct {
	Repositories []Repository   `json:"repositories"`
	Services     []Service      `json:"services"`
	Scans        []Scan         `json:"scans"`
	Statuses     []StatusResult `json:"statuses"`
	Uptime       []UptimeStat   `json:"uptime"`
	Agents       []AgentInfo    `json:"agents"`
	Version      BuildInfo      `json:"version"`
	GeneratedAt  time.Time      `json:"generatedAt"`
}

type AgentInfo struct {
	Target     string            `json:"target"`
	LastSeenAt string            `json:"lastSeenAt"`
	StaleAfter string            `json:"staleAfter,omitempty"`
	Configured bool              `json:"configured"`
	Containers []ContainerStatus `json:"containers"`
}

type AgentMessage struct {
	Target    string    `json:"target"`
	CheckedAt time.Time `json:"checkedAt"`
	// StaleAfter is server-owned reporting policy, never agent input.
	StaleAfter time.Time         `json:"-"`
	Containers []ContainerStatus `json:"containers"`
}

const (
	DockerComposeProjectLabel = "com.docker.compose.project"
	DockerComposeServiceLabel = "com.docker.compose.service"
)

type ContainerStatus struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Image        string            `json:"image"`
	ImageID      string            `json:"imageId"`
	RepoDigests  []string          `json:"repoDigests"`
	Labels       map[string]string `json:"labels,omitempty"`
	State        string            `json:"state"`
	Status       string            `json:"status"`
	Health       string            `json:"health"`
	RestartCount int               `json:"restartCount"`
}

func FilterDockerComposeLabels(labels map[string]string) map[string]string {
	filtered := map[string]string{}
	for _, key := range []string{DockerComposeProjectLabel, DockerComposeServiceLabel} {
		if value, ok := labels[key]; ok {
			filtered[key] = value
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func FilterAgentMessageDockerLabels(message AgentMessage) AgentMessage {
	for i := range message.Containers {
		message.Containers[i].Labels = FilterDockerComposeLabels(message.Containers[i].Labels)
	}
	return message
}
