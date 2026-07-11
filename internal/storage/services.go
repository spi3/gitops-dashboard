package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/routetarget"
)

type RuntimeServiceSource struct {
	Repository string
	SourcePath string
}

func (store *Store) EnsureRepositories(ctx context.Context, repos []config.RepositoryConfig) error {
	for _, repo := range repos {
		repoURL := store.redact(repo.URL)
		_, err := store.db.ExecContext(ctx, `
INSERT INTO repositories(name, url, default_ref, status)
VALUES(?, ?, ?, 'configured')
ON CONFLICT(name) DO UPDATE SET url=excluded.url, default_ref=excluded.default_ref
`, repo.Name, repoURL, repo.DefaultRef)
		if err != nil {
			return fmt.Errorf("upsert repository %s: %w", repo.Name, err)
		}
	}
	store.invalidateSummary()
	return nil
}

func (store *Store) ReplaceConfiguredServices(ctx context.Context, repositoryName, source string, services []core.Service) error {
	if source == "" {
		source = repositoryName
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `
INSERT INTO repositories(name, url, default_ref, last_scan_at, status, error)
VALUES(?, ?, 'configured', ?, 'configured', '')
ON CONFLICT(name) DO UPDATE SET
  url=excluded.url,
  default_ref=excluded.default_ref,
  last_scan_at=excluded.last_scan_at,
  status=excluded.status,
  error=''
`, repositoryName, source, now)
	if err != nil {
		return fmt.Errorf("upsert configured repository %s: %w", repositoryName, err)
	}

	currentRows, err := tx.QueryContext(ctx, `SELECT id FROM services WHERE repository=?`, repositoryName)
	if err != nil {
		return err
	}
	currentIDs := map[string]struct{}{}
	for currentRows.Next() {
		var id string
		if err := currentRows.Scan(&id); err != nil {
			_ = currentRows.Close()
			return err
		}
		currentIDs[id] = struct{}{}
	}
	if err := currentRows.Close(); err != nil {
		return err
	}
	newIDs := map[string]struct{}{}
	for _, service := range services {
		newIDs[service.ID] = struct{}{}
	}
	for id := range currentIDs {
		if _, ok := newIDs[id]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE repository=?`, repositoryName); err != nil {
		return err
	}
	for _, service := range services {
		if err := insertService(ctx, tx, service); err != nil {
			return err
		}
	}
	return store.commitAndInvalidateSummary(tx)
}

func (store *Store) ReplaceRuntimeServices(ctx context.Context, repositoryName, source, runtime string, services []core.Service) error {
	if source == "" {
		source = repositoryName
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `
INSERT INTO repositories(name, url, default_ref, last_scan_at, status, error)
VALUES(?, ?, 'configured', ?, 'configured', '')
ON CONFLICT(name) DO NOTHING
`, repositoryName, source, now)
	if err != nil {
		return fmt.Errorf("upsert configured repository %s: %w", repositoryName, err)
	}

	currentRows, err := tx.QueryContext(ctx, `SELECT id FROM services WHERE repository=? AND runtime=? AND source_path=?`, repositoryName, runtime, source)
	if err != nil {
		return err
	}
	currentIDs := map[string]struct{}{}
	for currentRows.Next() {
		var id string
		if err := currentRows.Scan(&id); err != nil {
			_ = currentRows.Close()
			return err
		}
		currentIDs[id] = struct{}{}
	}
	if err := currentRows.Close(); err != nil {
		return err
	}
	newIDs := map[string]struct{}{}
	for _, service := range services {
		newIDs[service.ID] = struct{}{}
	}
	for id := range currentIDs {
		if _, ok := newIDs[id]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE repository=? AND runtime=? AND source_path=?`, repositoryName, runtime, source); err != nil {
		return err
	}
	for _, service := range services {
		if err := insertService(ctx, tx, service); err != nil {
			return err
		}
	}
	return store.commitAndInvalidateSummary(tx)
}

func (store *Store) PruneRuntimeServices(ctx context.Context, runtime string, keep []RuntimeServiceSource) error {
	keepSources := map[string]struct{}{}
	for _, source := range keep {
		keepSources[runtimeSourceKey(source.Repository, source.SourcePath)] = struct{}{}
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT id, repository, source_path FROM services WHERE runtime=?`, runtime)
	if err != nil {
		return err
	}
	var removeIDs []string
	affectedRepositories := map[string]struct{}{}
	for rows.Next() {
		var id, repository, sourcePath string
		if err := rows.Scan(&id, &repository, &sourcePath); err != nil {
			_ = rows.Close()
			return err
		}
		if _, ok := keepSources[runtimeSourceKey(repository, sourcePath)]; ok {
			continue
		}
		removeIDs = append(removeIDs, id)
		affectedRepositories[repository] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range removeIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id=?`, id); err != nil {
			return err
		}
	}
	for repository := range affectedRepositories {
		if _, err := tx.ExecContext(ctx, `
DELETE FROM repositories
WHERE name=? AND default_ref='configured' AND NOT EXISTS (
  SELECT 1 FROM services WHERE repository=?
)
`, repository, repository); err != nil {
			return err
		}
	}
	return store.commitAndInvalidateSummary(tx)
}

func (store *Store) RuntimeServiceSourceCommit(ctx context.Context, repositoryName, source, runtime string) (string, bool, error) {
	var commit string
	var count int
	err := store.db.QueryRowContext(ctx, `
SELECT COALESCE(MAX(source_commit), ''), COUNT(*)
FROM services
WHERE repository=? AND source_path=? AND runtime=?
`, repositoryName, source, runtime).Scan(&commit, &count)
	if err != nil {
		return "", false, err
	}
	return commit, count > 0, nil
}

func runtimeSourceKey(repository, sourcePath string) string {
	return repository + "\x00" + sourcePath
}

func (store *Store) StartScan(ctx context.Context, repoName string) (int64, error) {
	result, err := store.db.ExecContext(ctx, `
	INSERT INTO scans(repository, status, started_at) VALUES(?, 'running', ?)
	`, repoName, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("start scan %s: %w", repoName, err)
	}
	store.invalidateSummary()
	return result.LastInsertId()
}

func (store *Store) FinishScan(ctx context.Context, scanID int64, repoName, commit string, services []core.Service, scanErr error) error {
	return store.FinishScanWithRouteTargetReplacements(ctx, scanID, repoName, commit, services, scanErr, nil, nil)
}

// FinishScanWithRouteTargetReplacements commits a successful discovery result and
// any evidence-backed route identity replacements atomically. Replacements are
// deliberately supplied by discovery rather than inferred from stored targets.
func (store *Store) FinishScanWithRouteTargetReplacements(ctx context.Context, scanID int64, repoName, commit string, services []core.Service, scanErr error, replacements []RouteTargetReplacement, httpTargets []config.HTTPRouteTarget) error {
	return store.FinishScanWithRouteTargetChanges(ctx, scanID, repoName, commit, services, scanErr, replacements, nil, httpTargets)
}

// FinishScanWithRouteTargetChanges atomically commits discovery, proven
// replacements, and ambiguity exclusions.
func (store *Store) FinishScanWithRouteTargetChanges(ctx context.Context, scanID int64, repoName, commit string, services []core.Service, scanErr error, replacements []RouteTargetReplacement, exclusions []RouteTargetExclusion, httpTargets []config.HTTPRouteTarget) error {
	status := "ok"
	errText := ""
	if scanErr != nil {
		status = "error"
		errText = store.redact(scanErr.Error())
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
UPDATE scans SET status=?, commit_sha=?, finished_at=?, error=? WHERE id=?
`, status, commit, time.Now().UTC().Format(time.RFC3339), errText, scanID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE repositories SET last_commit=?, last_scan_at=?, status=?, error=? WHERE name=?
`, commit, time.Now().UTC().Format(time.RFC3339), status, errText, repoName)
	if err != nil {
		return err
	}
	if scanErr == nil {
		// A successful scan is authoritative for ambiguity state. Retain only
		// identities that remain ambiguous in this result, so resolved or vanished
		// routes return to ordinary stale-pruning behavior.
		if _, err := tx.ExecContext(ctx, `DELETE FROM route_target_exclusions WHERE service_id IN (SELECT id FROM services WHERE repository=?)`, repoName); err != nil {
			return fmt.Errorf("reconcile route target exclusions: %w", err)
		}
		if err := setRouteTargetExclusions(ctx, tx, exclusions); err != nil {
			return err
		}
		if err := store.migrateRouteTargetReplacements(ctx, tx, replacements, httpRouteTargetNames(httpTargets)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE repository=?`, repoName); err != nil {
			return err
		}
		for _, service := range services {
			if err := insertService(ctx, tx, service); err != nil {
				return err
			}
		}
	}
	return store.commitAndInvalidateSummary(tx)
}

func insertService(ctx context.Context, tx *sql.Tx, service core.Service) error {
	service = normalizeService(service)
	_, err := tx.ExecContext(ctx, `
INSERT INTO services(
	  id, name, repository, source_commit, source_path, runtime, kind, namespace,
	  compose_project, resource_name, environment, health, images_json, ports_json, dependencies_json,
	  storage_json, exposure_json, config_json, warnings_json
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, service.ID, service.Name, service.Repository, service.SourceCommit, service.SourcePath,
		service.Runtime, service.Kind, service.Namespace, service.ComposeProject, service.ResourceName, service.Environment,
		string(service.Health), toJSON(service.Images), toJSON(service.Ports), toJSON(service.Dependencies),
		toJSON(service.Storage), toJSON(service.Exposure), toJSON(service.ConfigRefs), toJSON(service.Warnings))
	if err != nil {
		return fmt.Errorf("insert service %s: %w", service.ID, err)
	}
	return nil
}

func (store *Store) Summary(ctx context.Context) (core.DashboardSummary, error) {
	if summary, ok := store.cachedSummary(); ok {
		return summary, nil
	}
	version := store.currentSummaryVersion()
	summary, err := store.buildSummary(ctx)
	if err != nil {
		return core.DashboardSummary{}, err
	}
	store.cacheSummary(version, summary)
	return cloneDashboardSummary(summary), nil
}

func (store *Store) buildSummary(ctx context.Context) (core.DashboardSummary, error) {
	repos, err := store.Repositories(ctx)
	if err != nil {
		return core.DashboardSummary{}, err
	}
	services, err := store.Services(ctx)
	if err != nil {
		return core.DashboardSummary{}, err
	}
	scans, err := store.Scans(ctx)
	if err != nil {
		return core.DashboardSummary{}, err
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		return core.DashboardSummary{}, err
	}
	uptime, err := store.UptimeStats(ctx)
	if err != nil {
		return core.DashboardSummary{}, err
	}
	applyLatestStatus(services, statuses)
	if repos == nil {
		repos = []core.Repository{}
	}
	if services == nil {
		services = []core.Service{}
	}
	if scans == nil {
		scans = []core.Scan{}
	}
	if statuses == nil {
		statuses = []core.StatusResult{}
	}
	if uptime == nil {
		uptime = []core.UptimeStat{}
	}
	core.ApplyImageVersionComparisons(services, statuses)
	return core.DashboardSummary{
		Repositories: repos,
		Services:     services,
		Scans:        scans,
		Statuses:     statuses,
		Uptime:       uptime,
		GeneratedAt:  time.Now().UTC(),
	}, nil
}

func (store *Store) Repositories(ctx context.Context) ([]core.Repository, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT name, url, default_ref, COALESCE(last_commit, ''), COALESCE(last_scan_at, ''), COALESCE(status, ''), COALESCE(error, '')
FROM repositories ORDER BY name
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repos []core.Repository
	for rows.Next() {
		var repo core.Repository
		if err := rows.Scan(&repo.Name, &repo.URL, &repo.DefaultRef, &repo.LastCommit, &repo.LastScanAt, &repo.Status, &repo.Error); err != nil {
			return nil, err
		}
		repo.URL = store.redact(repo.URL)
		repo.Error = store.redact(repo.Error)
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

func (store *Store) Scans(ctx context.Context) ([]core.Scan, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT id, repository, status, COALESCE(commit_sha, ''), started_at, COALESCE(finished_at, ''), COALESCE(error, '')
FROM scans ORDER BY started_at DESC LIMIT 50
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scans []core.Scan
	for rows.Next() {
		var scan core.Scan
		if err := rows.Scan(&scan.ID, &scan.Repository, &scan.Status, &scan.CommitSHA, &scan.StartedAt, &scan.FinishedAt, &scan.Error); err != nil {
			return nil, err
		}
		scan.Error = store.redact(scan.Error)
		scans = append(scans, scan)
	}
	return scans, rows.Err()
}

func (store *Store) Services(ctx context.Context) ([]core.Service, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT rowid, id, name, repository, source_commit, source_path, runtime, kind, namespace,
       compose_project, resource_name, environment, health, images_json, ports_json, dependencies_json,
       storage_json, exposure_json, config_json, warnings_json
FROM services ORDER BY repository, runtime, name
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var services []core.Service
	for rows.Next() {
		var service core.Service
		var rowID int64
		var health string
		var images, ports, dependencies, storageRefs, exposure, configRefs, warnings string
		err := rows.Scan(&rowID, &service.ID, &service.Name, &service.Repository, &service.SourceCommit,
			&service.SourcePath, &service.Runtime, &service.Kind, &service.Namespace, &service.ComposeProject, &service.ResourceName,
			&service.Environment, &health, &images, &ports, &dependencies, &storageRefs, &exposure, &configRefs, &warnings)
		if err != nil {
			return nil, err
		}
		service.Health = core.HealthState(health)
		if err := store.fromPersistedJSON(images, &service.Images, "services", "images_json", rowID, service.ID); err != nil {
			return nil, err
		}
		if err := store.fromPersistedJSON(ports, &service.Ports, "services", "ports_json", rowID, service.ID); err != nil {
			return nil, err
		}
		if err := store.fromPersistedJSON(dependencies, &service.Dependencies, "services", "dependencies_json", rowID, service.ID); err != nil {
			return nil, err
		}
		if err := store.fromPersistedJSON(storageRefs, &service.Storage, "services", "storage_json", rowID, service.ID); err != nil {
			return nil, err
		}
		if err := store.fromPersistedJSON(exposure, &service.Exposure, "services", "exposure_json", rowID, service.ID); err != nil {
			return nil, err
		}
		if err := store.fromPersistedJSON(configRefs, &service.ConfigRefs, "services", "config_json", rowID, service.ID); err != nil {
			return nil, err
		}
		if err := store.fromPersistedJSON(warnings, &service.Warnings, "services", "warnings_json", rowID, service.ID); err != nil {
			return nil, err
		}
		service = normalizeService(service)
		services = append(services, service)
	}
	return services, rows.Err()
}

func normalizeService(service core.Service) core.Service {
	service.ComposeProject = strings.TrimSpace(service.ComposeProject)
	if service.Images == nil {
		service.Images = []string{}
	}
	core.NormalizeServiceImageMetadata(&service)
	if service.Ports == nil {
		service.Ports = []string{}
	}
	if service.Dependencies == nil {
		service.Dependencies = []string{}
	}
	if service.Storage == nil {
		service.Storage = []string{}
	}
	if service.Exposure == nil {
		service.Exposure = []string{}
	}
	service.Exposure = sanitizeExposure(service.Exposure)
	service.MonitorRoutes = monitorRoutesFromExposure(service.Exposure)
	if service.ConfigRefs == nil {
		service.ConfigRefs = []string{}
	}
	if service.Warnings == nil {
		service.Warnings = []string{}
	}
	return service
}

func sanitizeExposure(exposure []string) []string {
	sanitized := make([]string, len(exposure))
	for i, value := range exposure {
		sanitized[i] = routetarget.StripUserinfo(value)
	}
	return sanitized
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
