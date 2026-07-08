package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/hostinventory"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

type ReconcileStats struct {
	RepositoriesRemoved  int64
	ServicesRemoved      int64
	StatusResultsRemoved int64
	StatusHistoryRemoved int64
	AgentsRemoved        int64
}

func (stats ReconcileStats) Changed() bool {
	return stats.RepositoriesRemoved > 0 ||
		stats.ServicesRemoved > 0 ||
		stats.StatusResultsRemoved > 0 ||
		stats.StatusHistoryRemoved > 0 ||
		stats.AgentsRemoved > 0
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error {
	return store.db.Close()
}

func (store *Store) migrate(ctx context.Context) error {
	_, err := store.db.ExecContext(ctx, migrations)
	if err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	if err := store.ensureColumn(ctx, "services", "config_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	return nil
}

func (store *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := store.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition))
	return err
}

func (store *Store) EnsureRepositories(ctx context.Context, repos []config.RepositoryConfig) error {
	for _, repo := range repos {
		_, err := store.db.ExecContext(ctx, `
INSERT INTO repositories(name, url, default_ref, status)
VALUES(?, ?, ?, 'configured')
ON CONFLICT(name) DO UPDATE SET url=excluded.url, default_ref=excluded.default_ref
`, repo.Name, repo.URL, repo.DefaultRef)
		if err != nil {
			return fmt.Errorf("upsert repository %s: %w", repo.Name, err)
		}
	}
	return nil
}

func (store *Store) ReconcileConfiguration(ctx context.Context, cfg config.Config) (ReconcileStats, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return ReconcileStats{}, err
	}
	defer func() { _ = tx.Rollback() }()

	for _, repo := range cfg.Repositories {
		_, err := tx.ExecContext(ctx, `
INSERT INTO repositories(name, url, default_ref, status)
VALUES(?, ?, ?, 'configured')
ON CONFLICT(name) DO UPDATE SET url=excluded.url, default_ref=excluded.default_ref
`, repo.Name, repo.URL, repo.DefaultRef)
		if err != nil {
			return ReconcileStats{}, fmt.Errorf("upsert repository %s: %w", repo.Name, err)
		}
	}

	stats := ReconcileStats{}
	allowedRepositories := configuredRepositories(cfg)
	pingSources := configuredPingSources(cfg.Runtime.Ping)
	for source := range pingSources {
		allowedRepositories[source.repository] = struct{}{}
	}

	services, err := tx.QueryContext(ctx, `SELECT id, repository, runtime, source_path FROM services`)
	if err != nil {
		return ReconcileStats{}, err
	}
	var staleServiceIDs []string
	for services.Next() {
		var id, repository, runtime, sourcePath string
		if err := services.Scan(&id, &repository, &runtime, &sourcePath); err != nil {
			_ = services.Close()
			return ReconcileStats{}, err
		}
		_, repositoryAllowed := allowedRepositories[repository]
		stale := !repositoryAllowed
		if !stale && runtime == "host" {
			_, configured := pingSources[serviceSourceKey{repository: repository, runtime: runtime, sourcePath: sourcePath}]
			stale = !configured
		}
		if stale {
			staleServiceIDs = append(staleServiceIDs, id)
		}
	}
	if err := services.Close(); err != nil {
		return ReconcileStats{}, err
	}
	for _, id := range staleServiceIDs {
		statusResults, statusHistory, err := deleteStatusForService(ctx, tx, id)
		if err != nil {
			return ReconcileStats{}, err
		}
		stats.StatusResultsRemoved += statusResults
		stats.StatusHistoryRemoved += statusHistory
		result, err := tx.ExecContext(ctx, `DELETE FROM services WHERE id=?`, id)
		if err != nil {
			return ReconcileStats{}, err
		}
		stats.ServicesRemoved += affectedRows(result)
	}

	repositories, err := tx.QueryContext(ctx, `SELECT name FROM repositories`)
	if err != nil {
		return ReconcileStats{}, err
	}
	var staleRepositories []string
	for repositories.Next() {
		var name string
		if err := repositories.Scan(&name); err != nil {
			_ = repositories.Close()
			return ReconcileStats{}, err
		}
		if _, ok := allowedRepositories[name]; !ok {
			staleRepositories = append(staleRepositories, name)
		}
	}
	if err := repositories.Close(); err != nil {
		return ReconcileStats{}, err
	}
	for _, name := range staleRepositories {
		if _, err := tx.ExecContext(ctx, `DELETE FROM scans WHERE repository=?`, name); err != nil {
			return ReconcileStats{}, err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM repositories WHERE name=?`, name)
		if err != nil {
			return ReconcileStats{}, err
		}
		stats.RepositoriesRemoved += affectedRows(result)
	}

	statusResults, err := deleteOrphanedStatusResults(ctx, tx)
	if err != nil {
		return ReconcileStats{}, err
	}
	stats.StatusResultsRemoved += statusResults
	statusHistory, err := deleteOrphanedStatusHistory(ctx, tx)
	if err != nil {
		return ReconcileStats{}, err
	}
	stats.StatusHistoryRemoved += statusHistory

	statusTargets := configuredStatusTargets(cfg)
	removedResults, err := deleteUnconfiguredStatusResults(ctx, tx, statusTargets)
	if err != nil {
		return ReconcileStats{}, err
	}
	stats.StatusResultsRemoved += removedResults
	removedHistory, err := deleteUnconfiguredStatusHistory(ctx, tx, statusTargets)
	if err != nil {
		return ReconcileStats{}, err
	}
	stats.StatusHistoryRemoved += removedHistory

	removedAgents, err := deleteUnconfiguredAgents(ctx, tx, configuredAgentTargets(cfg.Runtime.Docker))
	if err != nil {
		return ReconcileStats{}, err
	}
	stats.AgentsRemoved += removedAgents

	return stats, tx.Commit()
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

	currentIDs, err := currentServiceIDs(ctx, tx, `SELECT id FROM services WHERE repository=?`, repositoryName)
	if err != nil {
		return err
	}
	if err := deleteRemovedServiceStatuses(ctx, tx, currentIDs, services); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE repository=?`, repositoryName); err != nil {
		return err
	}
	for _, service := range services {
		if err := insertService(ctx, tx, service); err != nil {
			return err
		}
	}
	return tx.Commit()
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

	currentIDs, err := currentServiceIDs(ctx, tx, `SELECT id FROM services WHERE repository=? AND runtime=? AND source_path=?`, repositoryName, runtime, source)
	if err != nil {
		return err
	}
	if err := deleteRemovedServiceStatuses(ctx, tx, currentIDs, services); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE repository=? AND runtime=? AND source_path=?`, repositoryName, runtime, source); err != nil {
		return err
	}
	for _, service := range services {
		if err := insertService(ctx, tx, service); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (store *Store) StartScan(ctx context.Context, repoName string) (int64, error) {
	result, err := store.db.ExecContext(ctx, `
INSERT INTO scans(repository, status, started_at) VALUES(?, 'running', ?)
`, repoName, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("start scan %s: %w", repoName, err)
	}
	return result.LastInsertId()
}

func (store *Store) FinishScan(ctx context.Context, scanID int64, repoName, commit string, services []core.Service, scanErr error) error {
	status := "ok"
	errText := ""
	if scanErr != nil {
		status = "error"
		errText = redact(scanErr.Error())
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
		currentIDs, err := currentServiceIDs(ctx, tx, `SELECT id FROM services WHERE repository=?`, repoName)
		if err != nil {
			return err
		}
		if err := deleteRemovedServiceStatuses(ctx, tx, currentIDs, services); err != nil {
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
	return tx.Commit()
}

func insertService(ctx context.Context, tx *sql.Tx, service core.Service) error {
	service = normalizeService(service)
	_, err := tx.ExecContext(ctx, `
INSERT INTO services(
  id, name, repository, source_commit, source_path, runtime, kind, namespace,
  resource_name, environment, health, images_json, ports_json, dependencies_json,
  storage_json, exposure_json, config_json, warnings_json
) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, service.ID, service.Name, service.Repository, service.SourceCommit, service.SourcePath,
		service.Runtime, service.Kind, service.Namespace, service.ResourceName, service.Environment,
		string(service.Health), toJSON(service.Images), toJSON(service.Ports), toJSON(service.Dependencies),
		toJSON(service.Storage), toJSON(service.Exposure), toJSON(service.ConfigRefs), toJSON(service.Warnings))
	if err != nil {
		return fmt.Errorf("insert service %s: %w", service.ID, err)
	}
	return nil
}

func currentServiceIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

func deleteRemovedServiceStatuses(ctx context.Context, tx *sql.Tx, currentIDs map[string]struct{}, services []core.Service) error {
	newIDs := map[string]struct{}{}
	for _, service := range services {
		newIDs[service.ID] = struct{}{}
	}
	for id := range currentIDs {
		if _, ok := newIDs[id]; ok {
			continue
		}
		if _, _, err := deleteStatusForService(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

func deleteStatusForService(ctx context.Context, tx *sql.Tx, serviceID string) (int64, int64, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=?`, serviceID)
	if err != nil {
		return 0, 0, err
	}
	statusResults := affectedRows(result)
	result, err = tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=?`, serviceID)
	if err != nil {
		return 0, 0, err
	}
	return statusResults, affectedRows(result), nil
}

func deleteOrphanedStatusResults(ctx context.Context, tx *sql.Tx) (int64, error) {
	result, err := tx.ExecContext(ctx, `
DELETE FROM status_results
WHERE NOT EXISTS (
  SELECT 1 FROM services WHERE services.id=status_results.service_id
)
`)
	if err != nil {
		return 0, err
	}
	return affectedRows(result), nil
}

func deleteOrphanedStatusHistory(ctx context.Context, tx *sql.Tx) (int64, error) {
	result, err := tx.ExecContext(ctx, `
DELETE FROM status_history
WHERE NOT EXISTS (
  SELECT 1 FROM services WHERE services.id=status_history.service_id
)
`)
	if err != nil {
		return 0, err
	}
	return affectedRows(result), nil
}

func deleteUnconfiguredStatusResults(ctx context.Context, tx *sql.Tx, targets statusTargetConfig) (int64, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT status_results.service_id, status_results.target, services.runtime
FROM status_results
JOIN services ON services.id=status_results.service_id
`)
	if err != nil {
		return 0, err
	}
	type statusRow struct {
		serviceID string
		target    string
		runtime   string
	}
	var stale []statusRow
	for rows.Next() {
		var row statusRow
		if err := rows.Scan(&row.serviceID, &row.target, &row.runtime); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if !targets.allows(row.runtime, row.target) {
			stale = append(stale, row)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var removed int64
	for _, row := range stale {
		result, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=? AND target=?`, row.serviceID, row.target)
		if err != nil {
			return 0, err
		}
		removed += affectedRows(result)
	}
	return removed, nil
}

func deleteUnconfiguredStatusHistory(ctx context.Context, tx *sql.Tx, targets statusTargetConfig) (int64, error) {
	rows, err := tx.QueryContext(ctx, `
SELECT status_history.id, status_history.target, services.runtime
FROM status_history
JOIN services ON services.id=status_history.service_id
`)
	if err != nil {
		return 0, err
	}
	type historyRow struct {
		id      int64
		target  string
		runtime string
	}
	var stale []historyRow
	for rows.Next() {
		var row historyRow
		if err := rows.Scan(&row.id, &row.target, &row.runtime); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if !targets.allows(row.runtime, row.target) {
			stale = append(stale, row)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var removed int64
	for _, row := range stale {
		result, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE id=?`, row.id)
		if err != nil {
			return 0, err
		}
		removed += affectedRows(result)
	}
	return removed, nil
}

func deleteUnconfiguredAgents(ctx context.Context, tx *sql.Tx, allowed map[string]struct{}) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT target FROM agents`)
	if err != nil {
		return 0, err
	}
	var stale []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if _, ok := allowed[target]; !ok {
			stale = append(stale, target)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var removed int64
	for _, target := range stale {
		result, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE target=?`, target)
		if err != nil {
			return 0, err
		}
		removed += affectedRows(result)
	}
	return removed, nil
}

type serviceSourceKey struct {
	repository string
	runtime    string
	sourcePath string
}

type statusTargetConfig struct {
	docker     map[string]struct{}
	kubernetes map[string]struct{}
	http       map[string]struct{}
	ping       map[string]struct{}
}

func (targets statusTargetConfig) allows(runtime, target string) bool {
	if _, ok := targets.http[target]; ok {
		return true
	}
	switch runtime {
	case "compose":
		_, ok := targets.docker[target]
		return ok
	case "kubernetes":
		_, ok := targets.kubernetes[target]
		return ok
	case "host":
		_, ok := targets.ping[target]
		return ok
	default:
		return false
	}
}

func configuredRepositories(cfg config.Config) map[string]struct{} {
	repositories := map[string]struct{}{}
	for _, repo := range cfg.Repositories {
		repositories[repo.Name] = struct{}{}
	}
	return repositories
}

func configuredPingSources(targets []config.PingTarget) map[serviceSourceKey]struct{} {
	sources := map[serviceSourceKey]struct{}{}
	for _, target := range targets {
		sources[serviceSourceKey{
			repository: hostinventory.RepositoryName(target),
			runtime:    "host",
			sourcePath: hostinventory.Source(target),
		}] = struct{}{}
	}
	return sources
}

func configuredStatusTargets(cfg config.Config) statusTargetConfig {
	targets := statusTargetConfig{
		docker:     map[string]struct{}{},
		kubernetes: map[string]struct{}{},
		http:       map[string]struct{}{},
		ping:       map[string]struct{}{},
	}
	for _, target := range cfg.Runtime.Docker {
		targets.docker[target.Name] = struct{}{}
	}
	for _, target := range cfg.Runtime.Kubernetes {
		targets.kubernetes[target.Name] = struct{}{}
	}
	for _, target := range cfg.Runtime.HTTP {
		name := target.Name
		if name == "" {
			name = "routes"
		}
		targets.http[name] = struct{}{}
	}
	for _, target := range cfg.Runtime.Ping {
		targets.ping[target.EffectiveName()] = struct{}{}
	}
	return targets
}

func configuredAgentTargets(targets []config.DockerTarget) map[string]struct{} {
	agents := map[string]struct{}{}
	for _, target := range targets {
		if target.Kind == "agent" {
			agents[target.Name] = struct{}{}
		}
	}
	return agents
}

func affectedRows(result sql.Result) int64 {
	rows, err := result.RowsAffected()
	if err != nil {
		return 0
	}
	return rows
}

func (store *Store) Summary(ctx context.Context) (core.DashboardSummary, error) {
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
		repo.Error = redact(repo.Error)
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
		scan.Error = redact(scan.Error)
		scans = append(scans, scan)
	}
	return scans, rows.Err()
}

func (store *Store) Services(ctx context.Context) ([]core.Service, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT id, name, repository, source_commit, source_path, runtime, kind, namespace,
       resource_name, environment, health, images_json, ports_json, dependencies_json,
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
		var health string
		var images, ports, dependencies, storageRefs, exposure, configRefs, warnings string
		err := rows.Scan(&service.ID, &service.Name, &service.Repository, &service.SourceCommit,
			&service.SourcePath, &service.Runtime, &service.Kind, &service.Namespace, &service.ResourceName,
			&service.Environment, &health, &images, &ports, &dependencies, &storageRefs, &exposure, &configRefs, &warnings)
		if err != nil {
			return nil, err
		}
		service.Health = core.HealthState(health)
		fromJSON(images, &service.Images)
		fromJSON(ports, &service.Ports)
		fromJSON(dependencies, &service.Dependencies)
		fromJSON(storageRefs, &service.Storage)
		fromJSON(exposure, &service.Exposure)
		fromJSON(configRefs, &service.ConfigRefs)
		fromJSON(warnings, &service.Warnings)
		service = normalizeService(service)
		services = append(services, service)
	}
	return services, rows.Err()
}

func (store *Store) StatusResults(ctx context.Context) ([]core.StatusResult, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT service_id, target, health, message, checked_at
FROM status_results ORDER BY checked_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var statuses []core.StatusResult
	for rows.Next() {
		var status core.StatusResult
		var health, checkedAt string
		if err := rows.Scan(&status.ServiceID, &status.Target, &health, &status.Message, &checkedAt); err != nil {
			return nil, err
		}
		status.Health = core.HealthState(health)
		status.Message = redact(status.Message)
		if parsed, err := time.Parse(time.RFC3339, checkedAt); err == nil {
			status.CheckedAt = parsed
		}
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}

func applyLatestStatus(services []core.Service, statuses []core.StatusResult) {
	latestByTarget := map[string]map[string]core.StatusResult{}
	for _, status := range statuses {
		targets, ok := latestByTarget[status.ServiceID]
		if !ok {
			targets = map[string]core.StatusResult{}
			latestByTarget[status.ServiceID] = targets
		}
		current, ok := targets[status.Target]
		if !ok || status.CheckedAt.After(current.CheckedAt) || (status.CheckedAt.Equal(current.CheckedAt) && healthPriority(status.Health) < healthPriority(current.Health)) {
			targets[status.Target] = status
		}
	}
	for i := range services {
		targets, ok := latestByTarget[services[i].ID]
		if !ok {
			continue
		}
		health := core.HealthUnknown
		for _, status := range targets {
			if healthPriority(status.Health) < healthPriority(health) {
				health = status.Health
			}
		}
		services[i].Health = health
	}
}

func healthPriority(health core.HealthState) int {
	switch health {
	case core.HealthError:
		return 0
	case core.HealthUnhealthy:
		return 1
	case core.HealthDegraded:
		return 2
	case core.HealthHealthy:
		return 3
	default:
		return 4
	}
}

func normalizeService(service core.Service) core.Service {
	if service.Images == nil {
		service.Images = []string{}
	}
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
	if service.ConfigRefs == nil {
		service.ConfigRefs = []string{}
	}
	if service.Warnings == nil {
		service.Warnings = []string{}
	}
	return service
}

func (store *Store) UpsertStatus(ctx context.Context, status core.StatusResult) error {
	checkedAt := status.CheckedAt.UTC().Format(time.RFC3339)
	message := redact(status.Message)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(service_id, target) DO UPDATE SET
  health=excluded.health, message=excluded.message, checked_at=excluded.checked_at
`, status.ServiceID, status.Target, string(status.Health), message, checkedAt)
	if err != nil {
		return fmt.Errorf("upsert status %s/%s: %w", status.ServiceID, status.Target, err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO status_history(service_id, target, health, message, checked_at)
VALUES(?, ?, ?, ?, ?)
`, status.ServiceID, status.Target, string(status.Health), message, checkedAt)
	if err != nil {
		return fmt.Errorf("insert status history %s/%s: %w", status.ServiceID, status.Target, err)
	}
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE checked_at < ?`, cutoff); err != nil {
		return fmt.Errorf("prune status history: %w", err)
	}
	return tx.Commit()
}

func (store *Store) UptimeStats(ctx context.Context) ([]core.UptimeStat, error) {
	windowStart := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	stats := map[string]*core.UptimeStat{}
	order := []string{}
	countRows, err := store.db.QueryContext(ctx, `
SELECT service_id, target,
       SUM(CASE WHEN health IN ('healthy', 'degraded') THEN 1 ELSE 0 END) AS up_count,
       COUNT(*) AS total_count
FROM status_history WHERE checked_at >= ?
GROUP BY service_id, target
`, windowStart)
	if err != nil {
		return nil, fmt.Errorf("query uptime counts: %w", err)
	}
	defer countRows.Close()
	for countRows.Next() {
		var serviceID, target string
		var upCount, totalCount int
		if err := countRows.Scan(&serviceID, &target, &upCount, &totalCount); err != nil {
			return nil, err
		}
		percent := 0.0
		if totalCount > 0 {
			percent = math.Round((100*float64(upCount)/float64(totalCount))*10) / 10
		}
		key := serviceID + "\x00" + target
		stats[key] = &core.UptimeStat{
			ServiceID:     serviceID,
			Target:        target,
			UptimePercent: percent,
			CheckCount:    totalCount,
			Samples:       []core.UptimeSample{},
		}
		order = append(order, key)
	}
	if err := countRows.Err(); err != nil {
		return nil, err
	}
	sampleRows, err := store.db.QueryContext(ctx, `
SELECT service_id, target, health, message, checked_at FROM (
  SELECT service_id, target, health, message, checked_at,
         ROW_NUMBER() OVER (PARTITION BY service_id, target ORDER BY checked_at DESC, id DESC) AS row_num
  FROM status_history WHERE checked_at >= ?
) WHERE row_num <= 40
ORDER BY service_id, target, checked_at ASC
`, windowStart)
	if err != nil {
		return nil, fmt.Errorf("query uptime samples: %w", err)
	}
	defer sampleRows.Close()
	for sampleRows.Next() {
		var serviceID, target, health, message, checkedAt string
		if err := sampleRows.Scan(&serviceID, &target, &health, &message, &checkedAt); err != nil {
			return nil, err
		}
		stat, ok := stats[serviceID+"\x00"+target]
		if !ok {
			continue
		}
		sample := core.UptimeSample{
			Health:  core.HealthState(health),
			Message: redact(message),
		}
		if parsed, err := time.Parse(time.RFC3339, checkedAt); err == nil {
			sample.CheckedAt = parsed
		}
		stat.Samples = append(stat.Samples, sample)
	}
	if err := sampleRows.Err(); err != nil {
		return nil, err
	}
	result := make([]core.UptimeStat, 0, len(order))
	for _, key := range order {
		result = append(result, *stats[key])
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ServiceID != result[j].ServiceID {
			return result[i].ServiceID < result[j].ServiceID
		}
		return result[i].Target < result[j].Target
	})
	return result, nil
}

func (store *Store) UpsertAgent(ctx context.Context, message core.AgentMessage) error {
	_, err := store.db.ExecContext(ctx, `
INSERT INTO agents(target, last_seen_at, status_json)
VALUES(?, ?, ?)
ON CONFLICT(target) DO UPDATE SET last_seen_at=excluded.last_seen_at, status_json=excluded.status_json
`, message.Target, time.Now().UTC().Format(time.RFC3339), toJSON(message.Containers))
	return err
}

func (store *Store) Agents(ctx context.Context) ([]core.AgentInfo, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT target, last_seen_at, status_json FROM agents ORDER BY target
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []core.AgentInfo
	for rows.Next() {
		var agent core.AgentInfo
		var statusJSON string
		if err := rows.Scan(&agent.Target, &agent.LastSeenAt, &statusJSON); err != nil {
			return nil, err
		}
		fromJSON(statusJSON, &agent.Containers)
		if agent.Containers == nil {
			agent.Containers = []core.ContainerStatus{}
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}

func toJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func fromJSON(data string, value any) {
	if data == "" {
		return
	}
	_ = json.Unmarshal([]byte(data), value)
}

func redact(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) > 1000 {
		value = value[:1000]
	}
	return value
}

const migrations = `
PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS repositories (
  name TEXT PRIMARY KEY,
  url TEXT NOT NULL,
  default_ref TEXT NOT NULL,
  last_commit TEXT,
  last_scan_at TEXT,
  status TEXT,
  error TEXT
);

CREATE TABLE IF NOT EXISTS scans (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repository TEXT NOT NULL,
  status TEXT NOT NULL,
  commit_sha TEXT,
  started_at TEXT NOT NULL,
  finished_at TEXT,
  error TEXT
);

CREATE TABLE IF NOT EXISTS services (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  repository TEXT NOT NULL,
  source_commit TEXT NOT NULL,
  source_path TEXT NOT NULL,
  runtime TEXT NOT NULL,
  kind TEXT NOT NULL,
  namespace TEXT NOT NULL,
  resource_name TEXT NOT NULL,
  environment TEXT NOT NULL,
  health TEXT NOT NULL,
  images_json TEXT NOT NULL,
  ports_json TEXT NOT NULL,
  dependencies_json TEXT NOT NULL,
  storage_json TEXT NOT NULL,
  exposure_json TEXT NOT NULL,
  config_json TEXT NOT NULL DEFAULT '[]',
  warnings_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS status_results (
  service_id TEXT NOT NULL,
  target TEXT NOT NULL,
  health TEXT NOT NULL,
  message TEXT NOT NULL,
  checked_at TEXT NOT NULL,
  PRIMARY KEY(service_id, target)
);

CREATE TABLE IF NOT EXISTS agents (
  target TEXT PRIMARY KEY,
  last_seen_at TEXT NOT NULL,
  status_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS status_history (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  service_id TEXT NOT NULL,
  target TEXT NOT NULL,
  health TEXT NOT NULL,
  message TEXT NOT NULL,
  checked_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_status_history_lookup
ON status_history(service_id, target, checked_at);
`
