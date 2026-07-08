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

type Service struct {
	ID           string      `json:"id"`
	Name         string      `json:"name"`
	Repository   string      `json:"repository"`
	SourceCommit string      `json:"sourceCommit"`
	SourcePath   string      `json:"sourcePath"`
	Runtime      string      `json:"runtime"`
	Kind         string      `json:"kind"`
	Namespace    string      `json:"namespace"`
	ResourceName string      `json:"resourceName"`
	Environment  string      `json:"environment"`
	Health       HealthState `json:"health"`
	Images       []string    `json:"images"`
	Ports        []string    `json:"ports"`
	Dependencies []string    `json:"dependencies"`
	Storage      []string    `json:"storage"`
	Exposure     []string    `json:"exposure"`
	ConfigRefs   []string    `json:"configRefs"`
	Warnings     []string    `json:"warnings"`
}

type StatusResult struct {
	ServiceID string      `json:"serviceId"`
	Target    string      `json:"target"`
	Health    HealthState `json:"health"`
	Message   string      `json:"message"`
	CheckedAt time.Time   `json:"checkedAt"`
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
	GeneratedAt  time.Time      `json:"generatedAt"`
}

type AgentInfo struct {
	Target     string            `json:"target"`
	LastSeenAt string            `json:"lastSeenAt"`
	Configured bool              `json:"configured"`
	Containers []ContainerStatus `json:"containers"`
}

type AgentMessage struct {
	Target     string            `json:"target"`
	CheckedAt  time.Time         `json:"checkedAt"`
	Containers []ContainerStatus `json:"containers"`
}

type ContainerStatus struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Image        string `json:"image"`
	State        string `json:"state"`
	Status       string `json:"status"`
	Health       string `json:"health"`
	RestartCount int    `json:"restartCount"`
}
