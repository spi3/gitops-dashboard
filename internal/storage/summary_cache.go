package storage

import (
	"database/sql"
	"time"

	"github.com/example/gitops-dashboard/internal/core"
)

type summaryCache struct {
	valid    bool
	version  uint64
	cachedAt time.Time
	summary  core.DashboardSummary
}

const summaryCacheTTL = time.Minute

func (store *Store) cachedSummary() (core.DashboardSummary, bool) {
	store.summaryMu.RLock()
	defer store.summaryMu.RUnlock()
	if !store.summaryCache.valid || store.summaryCache.version != store.summaryVersion {
		return core.DashboardSummary{}, false
	}
	if time.Since(store.summaryCache.cachedAt) >= summaryCacheTTL {
		return core.DashboardSummary{}, false
	}
	summary := cloneDashboardSummary(store.summaryCache.summary)
	summary.GeneratedAt = time.Now().UTC()
	return summary, true
}

func (store *Store) currentSummaryVersion() uint64 {
	store.summaryMu.RLock()
	defer store.summaryMu.RUnlock()
	return store.summaryVersion
}

func (store *Store) cacheSummary(version uint64, summary core.DashboardSummary) {
	store.summaryMu.Lock()
	defer store.summaryMu.Unlock()
	if store.summaryVersion != version {
		return
	}
	store.summaryCache = summaryCache{
		valid:    true,
		version:  version,
		cachedAt: time.Now().UTC(),
		summary:  cloneDashboardSummary(summary),
	}
}

func (store *Store) invalidateSummary() {
	store.summaryMu.Lock()
	defer store.summaryMu.Unlock()
	store.summaryVersion++
	store.summaryCache = summaryCache{}
}

func (store *Store) commitAndInvalidateSummary(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	store.invalidateSummary()
	return nil
}

func cloneDashboardSummary(summary core.DashboardSummary) core.DashboardSummary {
	summary.Repositories = cloneSlice(summary.Repositories)
	summary.Services = cloneServices(summary.Services)
	summary.Scans = cloneSlice(summary.Scans)
	summary.Statuses = cloneStatusResults(summary.Statuses)
	summary.Uptime = cloneUptimeStats(summary.Uptime)
	summary.Agents = cloneAgentInfo(summary.Agents)
	return summary
}

func cloneServices(services []core.Service) []core.Service {
	if services == nil {
		return nil
	}
	cloned := make([]core.Service, len(services))
	for i, service := range services {
		cloned[i] = service
		cloned[i].Images = cloneSlice(service.Images)
		cloned[i].DesiredImages = cloneSlice(service.DesiredImages)
		cloned[i].ImageVersionChecks = cloneImageVersionChecks(service.ImageVersionChecks)
		cloned[i].Ports = cloneSlice(service.Ports)
		cloned[i].Dependencies = cloneSlice(service.Dependencies)
		cloned[i].Storage = cloneSlice(service.Storage)
		cloned[i].Exposure = cloneSlice(service.Exposure)
		cloned[i].MonitorRoutes = cloneSlice(service.MonitorRoutes)
		cloned[i].ConfigRefs = cloneSlice(service.ConfigRefs)
		cloned[i].Warnings = cloneSlice(service.Warnings)
	}
	return cloned
}

func cloneStatusResults(statuses []core.StatusResult) []core.StatusResult {
	if statuses == nil {
		return nil
	}
	cloned := make([]core.StatusResult, len(statuses))
	for i, status := range statuses {
		cloned[i] = status
		cloned[i].ObservedImages = cloneObservedImages(status.ObservedImages)
	}
	return cloned
}

func cloneImageVersionChecks(checks []core.ImageVersionCheck) []core.ImageVersionCheck {
	if checks == nil {
		return nil
	}
	cloned := make([]core.ImageVersionCheck, len(checks))
	for i, check := range checks {
		cloned[i] = check
		if check.Observed != nil {
			observed := cloneObservedImages([]core.ObservedImage{*check.Observed})[0]
			cloned[i].Observed = &observed
		}
	}
	return cloned
}

func cloneObservedImages(images []core.ObservedImage) []core.ObservedImage {
	if images == nil {
		return nil
	}
	cloned := make([]core.ObservedImage, len(images))
	for i, image := range images {
		cloned[i] = image
		cloned[i].RepoDigests = cloneSlice(image.RepoDigests)
	}
	return cloned
}

func cloneUptimeStats(stats []core.UptimeStat) []core.UptimeStat {
	if stats == nil {
		return nil
	}
	cloned := make([]core.UptimeStat, len(stats))
	for i, stat := range stats {
		cloned[i] = stat
		cloned[i].Samples = cloneSlice(stat.Samples)
	}
	return cloned
}

func cloneAgentInfo(agents []core.AgentInfo) []core.AgentInfo {
	if agents == nil {
		return nil
	}
	cloned := make([]core.AgentInfo, len(agents))
	for i, agent := range agents {
		cloned[i] = agent
		cloned[i].Containers = cloneSlice(agent.Containers)
		for j := range cloned[i].Containers {
			if cloned[i].Containers[j].Labels != nil {
				labels := map[string]string{}
				for key, value := range cloned[i].Containers[j].Labels {
					labels[key] = value
				}
				cloned[i].Containers[j].Labels = labels
			}
		}
	}
	return cloned
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	cloned := make([]T, len(values))
	copy(cloned, values)
	return cloned
}
