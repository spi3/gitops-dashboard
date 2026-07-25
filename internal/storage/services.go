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

// replaceServiceRows is the shared core of the three service-replacement call
// sites (ReplaceConfiguredServices, ReplaceRuntimeServices, and the success
// branch of FinishScanWithRouteTargetChanges), run inside the caller's open
// transaction: load current service IDs+incarnations for the scope,
// optionally delete orphaned status rows for IDs about to disappear, delete
// the scoped services rows, then re-insert preserving each surviving ID's
// incarnation.
//
// pruneOrphanedStatus is the one deliberate divergence between call sites:
// true for ReplaceConfiguredServices/ReplaceRuntimeServices, false for
// FinishScanWithRouteTargetChanges — see its caller.
func replaceServiceRows(ctx context.Context, tx *sql.Tx, currentIDsQuery string, currentIDsArgs []any, deleteQuery string, deleteArgs []any, services []core.Service, pruneOrphanedStatus bool) error {
	currentRows, err := tx.QueryContext(ctx, currentIDsQuery, currentIDsArgs...)
	if err != nil {
		return err
	}
	currentIDs := map[string]string{}
	for currentRows.Next() {
		var id, incarnation string
		if err := currentRows.Scan(&id, &incarnation); err != nil {
			_ = currentRows.Close()
			return err
		}
		currentIDs[id] = incarnation
	}
	if err := currentRows.Close(); err != nil {
		return err
	}
	if pruneOrphanedStatus {
		newIDs := make(map[string]struct{}, len(services))
		for _, service := range services {
			newIDs[service.ID] = struct{}{}
		}
		for id := range currentIDs {
			if _, ok := newIDs[id]; ok {
				continue
			}
			if err := deleteStatusForService(ctx, tx, id); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, deleteQuery, deleteArgs...); err != nil {
		return err
	}
	for _, service := range services {
		if err := insertService(ctx, tx, service, currentIDs[service.ID]); err != nil {
			return err
		}
	}
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

	if err := replaceServiceRows(ctx, tx,
		`SELECT id, incarnation FROM services WHERE repository=?`, []any{repositoryName},
		`DELETE FROM services WHERE repository=?`, []any{repositoryName},
		services, true,
	); err != nil {
		return err
	}
	if err := store.commitAndInvalidateSummary(tx); err != nil {
		return err
	}
	store.reconcileHealthAlertStates(ctx)
	return nil
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

	if err := replaceServiceRows(ctx, tx,
		`SELECT id, incarnation FROM services WHERE repository=? AND runtime=? AND source_path=?`, []any{repositoryName, runtime, source},
		`DELETE FROM services WHERE repository=? AND runtime=? AND source_path=?`, []any{repositoryName, runtime, source},
		services, true,
	); err != nil {
		return err
	}
	if err := store.commitAndInvalidateSummary(tx); err != nil {
		return err
	}
	store.reconcileHealthAlertStates(ctx)
	return nil
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
		if err := deleteStatusForService(ctx, tx, id); err != nil {
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
	if err := store.commitAndInvalidateSummary(tx); err != nil {
		return err
	}
	store.reconcileHealthAlertStates(ctx)
	return nil
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
	var retainedReplacementIDs []string
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
		newIDs := make(map[string]struct{}, len(services))
		for _, service := range services {
			newIDs[service.ID] = struct{}{}
		}
		retainedReplacementIDs = routeReplacementServiceIDs(replacements, newIDs)
		// pruneOrphanedStatus=false (preserved, not a fix target — see
		// docs/tasks/TASK-0066-storage-dedup-and-file-organization.md):
		// unlike Replace{Configured,Runtime}Services, a successful scan does
		// not delete status rows for service IDs dropped from this result.
		if err := replaceServiceRows(ctx, tx,
			`SELECT id, incarnation FROM services WHERE repository=?`, []any{repoName},
			`DELETE FROM services WHERE repository=?`, []any{repoName},
			services, false,
		); err != nil {
			return err
		}
	}
	if err := store.commitAndInvalidateSummary(tx); err != nil {
		return err
	}
	store.reconcileHealthAlertStates(ctx)
	store.observeHealthAlerts(ctx, retainedReplacementIDs, time.Now().UTC())
	return nil
}

// reconcileHealthAlertStates runs only after core inventory commits. It is
// best-effort orphan hygiene: alert-state correctness is established by the
// producer's service-incarnation comparison, not by this deletion succeeding.
func (store *Store) reconcileHealthAlertStates(ctx context.Context) {
	attempt := store.beginHealthAlertCleanup()
	if _, err := store.db.ExecContext(ctx, `
DELETE FROM health_alert_states
WHERE NOT EXISTS (SELECT 1 FROM services WHERE services.id=health_alert_states.service_id)
`); err != nil {
		store.completeHealthAlertCleanup(attempt, fmt.Sprintf("alert state locked: health_alert_states cleanup unavailable: %v", err))
		return
	}
	if store.afterSuccessfulHealthAlertCleanup != nil {
		store.afterSuccessfulHealthAlertCleanup()
	}
	store.completeHealthAlertCleanup(attempt, "")
}

func insertService(ctx context.Context, tx *sql.Tx, service core.Service, incarnation string) error {
	service = normalizeService(service)
	_, err := tx.ExecContext(ctx, `
	INSERT INTO services(
	  id, name, repository, source_commit, source_path, runtime, kind, namespace,
	  compose_project, resource_name, environment, health, images_json, ports_json, dependencies_json,
	  storage_json, exposure_json, config_json, warnings_json, incarnation
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), lower(hex(randomblob(16)))))
	`, service.ID, service.Name, service.Repository, service.SourceCommit, service.SourcePath,
		service.Runtime, service.Kind, service.Namespace, service.ComposeProject, service.ResourceName, service.Environment,
		string(service.Health), toJSON(service.Images), toJSON(service.Ports), toJSON(service.Dependencies),
		toJSON(service.Storage), toJSON(service.Exposure), toJSON(service.ConfigRefs), toJSON(service.Warnings), incarnation)
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

// LatestTerminalScan is the latest terminal (status ok or error) scan row
// observed for a repository, selected by greatest id among terminal rows.
// Error is redacted using the storage layer's live redaction registry, so a
// secret registered after the row was persisted is still scrubbed on read.
type LatestTerminalScan struct {
	ID     int64
	Status string
	Error  string
}

// LatestTerminalScansByRepository returns, for each named repository that
// has one, its latest terminal scan. Running rows (and any other
// non-terminal status) are ignored in every position: they are neither
// returned nor allowed to hide an older terminal row. A repository with no
// terminal scan is simply absent from the result. This is deliberately
// narrower than Scans/the dashboard summary: it never consults repository
// summary status, started_at ordering, the newest row regardless of status,
// or the 50-row summary limit.
func (store *Store) LatestTerminalScansByRepository(ctx context.Context, repositoryNames []string) (map[string]LatestTerminalScan, error) {
	names := dedupeStrings(repositoryNames)
	result := make(map[string]LatestTerminalScan, len(names))
	if len(names) == 0 {
		return result, nil
	}
	args := make([]any, len(names))
	for i, name := range names {
		args[i] = name
	}
	rows, err := store.db.QueryContext(ctx, latestTerminalScansByRepositoryQuery(sqlPlaceholders(len(names))), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var repository, status, errText string
		var id int64
		if err := rows.Scan(&repository, &id, &status, &errText); err != nil {
			return nil, err
		}
		result[repository] = LatestTerminalScan{ID: id, Status: status, Error: strings.TrimSpace(store.redact(errText))}
	}
	return result, rows.Err()
}

// latestTerminalScansByRepositoryQuery builds the SQL LatestTerminalScansByRepository
// runs, parameterized only on its repository-name placeholder list. It is
// factored out so the query-plan test asserts against the exact statement
// production code executes rather than a hand-copied duplicate.
func latestTerminalScansByRepositoryQuery(placeholders string) string {
	return `
SELECT s.repository, s.id, s.status, COALESCE(s.error, '')
FROM scans s
WHERE s.repository IN (` + placeholders + `)
  AND s.status IN ('ok', 'error')
  AND s.id = (
    SELECT MAX(s2.id) FROM scans s2
    WHERE s2.repository = s.repository AND s2.status IN ('ok', 'error')
  )
`
}

// DefaultScanRetentionHorizon is how long a terminal scan row is kept once
// it is no longer the newest terminal row for its repository. Seven days
// keeps the dashboard's 50-row summary window populated at every supported
// scan interval (see docs/tasks/TASK-0068-scans-index-and-retention.md).
const DefaultScanRetentionHorizon = 7 * 24 * time.Hour

// DefaultScanRetentionBatchSize bounds a single retention delete when the
// caller does not configure one, keeping individual transactions small
// (PruneTerminalAlertEvents precedent).
const DefaultScanRetentionBatchSize = 500

// PruneTerminalScans deletes terminal (ok/error) scan rows started before
// the given horizon, in bounded batches. Running rows are never touched.
// Each repository's newest terminal row (by id, matching
// LatestTerminalScansByRepository's own selection) is excluded from every
// batch regardless of age: it is the alert evaluator's baseline input and
// must never be pruned away.
func (store *Store) PruneTerminalScans(ctx context.Context, horizon time.Duration, batchSize int) (int64, error) {
	if horizon <= 0 {
		return 0, nil
	}
	if batchSize <= 0 {
		batchSize = DefaultScanRetentionBatchSize
	}
	cutoff := time.Now().UTC().Add(-horizon).Format(time.RFC3339)
	var total int64
	for {
		result, err := retryAlertSQLiteBusy(ctx, func() (sql.Result, error) {
			return store.db.ExecContext(ctx, `
DELETE FROM scans
WHERE id IN (
  SELECT id FROM scans
  WHERE status IN ('ok', 'error') AND started_at < ?
    AND id NOT IN (SELECT MAX(id) FROM scans WHERE status IN ('ok', 'error') GROUP BY repository)
  ORDER BY id
  LIMIT ?
)`, cutoff, batchSize)
		})
		if err != nil {
			return total, fmt.Errorf("prune terminal scans: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return total, err
		}
		total += affected
		if affected > 0 {
			store.invalidateSummary()
		}
		if affected < int64(batchSize) {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
	}
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
