package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/routetarget"
	"github.com/example/gitops-dashboard/internal/sanitizer"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db                 *sql.DB
	redactionMu        sync.RWMutex
	redactionRawValues []string
	redactionTokenSet  map[string]struct{}
	redactionTokens    []string
}

var ErrStatusNotFound = errors.New("status result not found")

const (
	routeMonitorTarget = routetarget.Parent
	routeTargetPrefix  = routetarget.Prefix
)

type RuntimeServiceSource struct {
	Repository string
	SourcePath string
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
	if err := store.RedactPersistedSensitiveValues(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error {
	return store.db.Close()
}

func (store *Store) AddRedactionValues(values ...string) {
	store.redactionMu.Lock()
	defer store.redactionMu.Unlock()
	if store.redactionTokenSet == nil {
		store.redactionTokenSet = map[string]struct{}{}
		for _, value := range store.redactionRawValues {
			store.redactionTokenSet[value] = struct{}{}
		}
	}
	changed := false
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := store.redactionTokenSet[value]; ok {
			continue
		}
		store.redactionTokenSet[value] = struct{}{}
		store.redactionRawValues = append(store.redactionRawValues, value)
		changed = true
	}
	if changed {
		store.redactionTokens = sanitizer.New(store.redactionRawValues...).Values()
	}
}

func (store *Store) redactionValues() []string {
	store.redactionMu.RLock()
	defer store.redactionMu.RUnlock()
	values := make([]string, len(store.redactionTokens))
	copy(values, store.redactionTokens)
	return values
}

func (store *Store) redact(value string) string {
	return sanitizer.Redact(value, store.redactionValues()...)
}

func (store *Store) RedactPersistedSensitiveValues(ctx context.Context) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	changed := false
	for _, target := range []struct {
		table  string
		column string
	}{
		{table: "repositories", column: "url"},
		{table: "repositories", column: "error"},
		{table: "scans", column: "error"},
		{table: "status_results", column: "message"},
		{table: "status_history", column: "message"},
	} {
		columnChanged, err := store.redactColumn(ctx, tx, target.table, target.column)
		if err != nil {
			return err
		}
		changed = changed || columnChanged
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return store.compactRedactedPages(ctx)
}

func (store *Store) redactColumn(ctx context.Context, tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT rowid, COALESCE(%s, '') FROM %s", column, table))
	if err != nil {
		return false, fmt.Errorf("query redaction target %s.%s: %w", table, column, err)
	}
	type rowValue struct {
		rowID int64
		value string
	}
	var updates []rowValue
	for rows.Next() {
		var current rowValue
		if err := rows.Scan(&current.rowID, &current.value); err != nil {
			_ = rows.Close()
			return false, err
		}
		redacted := store.redact(current.value)
		if redacted != current.value {
			updates = append(updates, rowValue{rowID: current.rowID, value: redacted})
		}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET %s=? WHERE rowid=?", table, column), update.value, update.rowID); err != nil {
			return false, fmt.Errorf("redact %s.%s: %w", table, column, err)
		}
	}
	return len(updates) > 0, nil
}

func (store *Store) compactRedactedPages(ctx context.Context) error {
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint redacted sqlite pages: %w", err)
	}
	if _, err := store.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum redacted sqlite pages: %w", err)
	}
	return nil
}

func (store *Store) migrate(ctx context.Context) error {
	_, err := store.db.ExecContext(ctx, migrations)
	if err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	if err := store.ensureColumn(ctx, "services", "config_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := store.ensureColumn(ctx, "services", "compose_project", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := store.ensureColumn(ctx, "status_results", "observed_images_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := store.canonicalizeStoredRouteTargets(ctx); err != nil {
		return err
	}
	return nil
}

func (store *Store) canonicalizeStoredRouteTargets(ctx context.Context) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := canonicalizeMonitorOverrides(ctx, tx); err != nil {
		return err
	}
	if err := canonicalizeStatusResults(ctx, tx); err != nil {
		return err
	}
	if err := canonicalizeStatusHistory(ctx, tx); err != nil {
		return err
	}
	if err := enforceActiveRouteOverrideStatuses(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

type monitorOverrideRouteAlias struct {
	serviceID     string
	target        string
	notApplicable int
	updatedAt     string
	canonical     string
}

func canonicalizeMonitorOverrides(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT service_id, target, not_applicable, updated_at
FROM monitor_overrides
WHERE target LIKE ?
`, routeTargetPrefix+"%")
	if err != nil {
		return fmt.Errorf("query route override aliases: %w", err)
	}
	var aliases []monitorOverrideRouteAlias
	for rows.Next() {
		var alias monitorOverrideRouteAlias
		if err := rows.Scan(&alias.serviceID, &alias.target, &alias.notApplicable, &alias.updatedAt); err != nil {
			_ = rows.Close()
			return err
		}
		canonical, ok := routetarget.CanonicalTarget(alias.target)
		if !ok || canonical == alias.target {
			continue
		}
		alias.canonical = canonical
		aliases = append(aliases, alias)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, alias := range aliases {
		var existingNotApplicable int
		var existingUpdatedAt string
		err := tx.QueryRowContext(ctx, `
SELECT not_applicable, updated_at
FROM monitor_overrides
WHERE service_id=? AND target=?
`, alias.serviceID, alias.canonical).Scan(&existingNotApplicable, &existingUpdatedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `
UPDATE monitor_overrides SET target=?
WHERE service_id=? AND target=?
`, alias.canonical, alias.serviceID, alias.target); err != nil {
				return fmt.Errorf("canonicalize route override %s/%s: %w", alias.serviceID, alias.target, err)
			}
		case err != nil:
			return err
		default:
			mergedNotApplicable := existingNotApplicable
			if alias.notApplicable > mergedNotApplicable {
				mergedNotApplicable = alias.notApplicable
			}
			mergedUpdatedAt := existingUpdatedAt
			if alias.updatedAt > mergedUpdatedAt {
				mergedUpdatedAt = alias.updatedAt
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE monitor_overrides SET not_applicable=?, updated_at=?
WHERE service_id=? AND target=?
`, mergedNotApplicable, mergedUpdatedAt, alias.serviceID, alias.canonical); err != nil {
				return fmt.Errorf("merge route override %s/%s: %w", alias.serviceID, alias.target, err)
			}
			if _, err := tx.ExecContext(ctx, `
DELETE FROM monitor_overrides
WHERE service_id=? AND target=?
`, alias.serviceID, alias.target); err != nil {
				return fmt.Errorf("delete route override alias %s/%s: %w", alias.serviceID, alias.target, err)
			}
		}
	}
	return nil
}

type statusResultRouteAlias struct {
	serviceID string
	target    string
	health    string
	message   string
	checkedAt string
	canonical string
}

func canonicalizeStatusResults(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT service_id, target, health, message, checked_at
FROM status_results
WHERE target LIKE ?
`, routeTargetPrefix+"%")
	if err != nil {
		return fmt.Errorf("query route status aliases: %w", err)
	}
	var aliases []statusResultRouteAlias
	for rows.Next() {
		var alias statusResultRouteAlias
		if err := rows.Scan(&alias.serviceID, &alias.target, &alias.health, &alias.message, &alias.checkedAt); err != nil {
			_ = rows.Close()
			return err
		}
		canonical, ok := routetarget.CanonicalTarget(alias.target)
		if !ok || canonical == alias.target {
			continue
		}
		alias.canonical = canonical
		aliases = append(aliases, alias)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, alias := range aliases {
		var existing statusResultRouteAlias
		err := tx.QueryRowContext(ctx, `
SELECT service_id, target, health, message, checked_at
FROM status_results
WHERE service_id=? AND target=?
`, alias.serviceID, alias.canonical).Scan(&existing.serviceID, &existing.target, &existing.health, &existing.message, &existing.checkedAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `
UPDATE status_results SET target=?
WHERE service_id=? AND target=?
`, alias.canonical, alias.serviceID, alias.target); err != nil {
				return fmt.Errorf("canonicalize route status %s/%s: %w", alias.serviceID, alias.target, err)
			}
		case err != nil:
			return err
		default:
			chosen := existing
			if shouldReplaceCanonicalStatus(alias, existing) {
				chosen = alias
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE status_results SET health=?, message=?, checked_at=?
WHERE service_id=? AND target=?
`, chosen.health, chosen.message, chosen.checkedAt, alias.serviceID, alias.canonical); err != nil {
				return fmt.Errorf("merge route status %s/%s: %w", alias.serviceID, alias.target, err)
			}
			if _, err := tx.ExecContext(ctx, `
DELETE FROM status_results
WHERE service_id=? AND target=?
`, alias.serviceID, alias.target); err != nil {
				return fmt.Errorf("delete route status alias %s/%s: %w", alias.serviceID, alias.target, err)
			}
		}
	}
	return nil
}

func shouldReplaceCanonicalStatus(candidate, existing statusResultRouteAlias) bool {
	if candidate.checkedAt != existing.checkedAt {
		return candidate.checkedAt > existing.checkedAt
	}
	candidatePriority := healthPriority(core.HealthState(candidate.health))
	existingPriority := healthPriority(core.HealthState(existing.health))
	if candidatePriority != existingPriority {
		return candidatePriority < existingPriority
	}
	return candidate.target < existing.target
}

type statusHistoryRouteAlias struct {
	id        int64
	target    string
	canonical string
}

func canonicalizeStatusHistory(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT id, target
FROM status_history
WHERE target LIKE ?
`, routeTargetPrefix+"%")
	if err != nil {
		return fmt.Errorf("query route history aliases: %w", err)
	}
	var aliases []statusHistoryRouteAlias
	for rows.Next() {
		var alias statusHistoryRouteAlias
		if err := rows.Scan(&alias.id, &alias.target); err != nil {
			_ = rows.Close()
			return err
		}
		canonical, ok := routetarget.CanonicalTarget(alias.target)
		if !ok || canonical == alias.target {
			continue
		}
		alias.canonical = canonical
		aliases = append(aliases, alias)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, alias := range aliases {
		if _, err := tx.ExecContext(ctx, `
UPDATE status_history SET target=?
WHERE id=?
`, alias.canonical, alias.id); err != nil {
			return fmt.Errorf("canonicalize route history %d/%s: %w", alias.id, alias.target, err)
		}
	}
	return nil
}

type activeRouteOverride struct {
	serviceID string
	target    string
	updatedAt string
}

func enforceActiveRouteOverrideStatuses(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT service_id, target, updated_at
FROM monitor_overrides
WHERE target LIKE ? AND not_applicable=1
`, routeTargetPrefix+"%")
	if err != nil {
		return fmt.Errorf("query active route overrides: %w", err)
	}
	var overrides []activeRouteOverride
	for rows.Next() {
		var override activeRouteOverride
		if err := rows.Scan(&override.serviceID, &override.target, &override.updatedAt); err != nil {
			_ = rows.Close()
			return err
		}
		overrides = append(overrides, override)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, override := range overrides {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(service_id, target) DO UPDATE SET
  health=excluded.health, message=excluded.message, checked_at=excluded.checked_at
`, override.serviceID, override.target, string(core.HealthNotApplicable), "not applicable", override.updatedAt); err != nil {
			return fmt.Errorf("force route override status %s/%s: %w", override.serviceID, override.target, err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE status_history
SET health=?, message=?
WHERE service_id=? AND target=?
`, string(core.HealthNotApplicable), "not applicable", override.serviceID, override.target); err != nil {
			return fmt.Errorf("force route override history %s/%s: %w", override.serviceID, override.target, err)
		}
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
	return tx.Commit()
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
	return tx.Commit()
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
	return result.LastInsertId()
}

func (store *Store) FinishScan(ctx context.Context, scanID int64, repoName, commit string, services []core.Service, scanErr error) error {
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
SELECT id, name, repository, source_commit, source_path, runtime, kind, namespace,
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
		var health string
		var images, ports, dependencies, storageRefs, exposure, configRefs, warnings string
		err := rows.Scan(&service.ID, &service.Name, &service.Repository, &service.SourceCommit,
			&service.SourcePath, &service.Runtime, &service.Kind, &service.Namespace, &service.ComposeProject, &service.ResourceName,
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
	parentRouteOverrides, err := store.parentRouteOverrideServices(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `
SELECT service_id, target, health, message, checked_at, observed_images_json
FROM status_results ORDER BY checked_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var statuses []core.StatusResult
	for rows.Next() {
		var status core.StatusResult
		var health, checkedAt, observedImages string
		if err := rows.Scan(&status.ServiceID, &status.Target, &health, &status.Message, &checkedAt, &observedImages); err != nil {
			return nil, err
		}
		status.Health = core.HealthState(health)
		status.Message = store.redact(status.Message)
		fromJSON(observedImages, &status.ObservedImages)
		if status.ObservedImages == nil {
			status.ObservedImages = []core.ObservedImage{}
		}
		if parsed, err := time.Parse(time.RFC3339, checkedAt); err == nil {
			status.CheckedAt = parsed
		}
		if parentRouteOverrides[status.ServiceID] && strings.HasPrefix(status.Target, routeTargetPrefix) {
			continue
		}
		statuses = append(statuses, status)
	}
	return statuses, rows.Err()
}

func (store *Store) SetMonitorNotApplicable(ctx context.Context, serviceID, target string, notApplicable bool) error {
	serviceID = strings.TrimSpace(serviceID)
	target = strings.TrimSpace(target)
	if serviceID == "" || target == "" {
		return fmt.Errorf("serviceId and target are required")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	resolved, err := resolveMonitorOverrideTarget(ctx, tx, serviceID, target)
	if err != nil {
		return err
	}
	target = resolved.target

	var exists int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM status_results WHERE service_id=? AND target=?
`, serviceID, target).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		if !notApplicable {
			return ErrStatusNotFound
		}
		if !resolved.syntheticRouteTarget {
			return ErrStatusNotFound
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if notApplicable {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO monitor_overrides(service_id, target, not_applicable, updated_at)
VALUES(?, ?, 1, ?)
ON CONFLICT(service_id, target) DO UPDATE SET not_applicable=1, updated_at=excluded.updated_at
`, serviceID, target, now); err != nil {
			return fmt.Errorf("set monitor override %s/%s: %w", serviceID, target, err)
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(service_id, target) DO UPDATE SET
  health=excluded.health, message=excluded.message, checked_at=excluded.checked_at,
  observed_images_json='[]'
`, serviceID, target, string(core.HealthNotApplicable), "not applicable", now); err != nil {
			return fmt.Errorf("update status override %s/%s: %w", serviceID, target, err)
		}
		if resolved.syntheticRouteParent {
			if _, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=? AND target LIKE ?`, serviceID, routeTargetPrefix+"%"); err != nil {
				return fmt.Errorf("clear child route statuses %s/%s: %w", serviceID, target, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=? AND target LIKE ?`, serviceID, routeTargetPrefix+"%"); err != nil {
				return fmt.Errorf("clear child route history %s/%s: %w", serviceID, target, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
DELETE FROM status_history WHERE service_id=? AND target=?
`, serviceID, target); err != nil {
			return fmt.Errorf("clear ignored status history %s/%s: %w", serviceID, target, err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM monitor_overrides WHERE service_id=? AND target=?`, serviceID, target); err != nil {
			return fmt.Errorf("delete monitor override %s/%s: %w", serviceID, target, err)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE status_results
SET health=?, message=?, checked_at=?
WHERE service_id=? AND target=? AND health=?
`, string(core.HealthUnknown), "monitor enabled; waiting for next check", now, serviceID, target, string(core.HealthNotApplicable)); err != nil {
			return fmt.Errorf("reset status override %s/%s: %w", serviceID, target, err)
		}
	}
	return tx.Commit()
}

type resolvedMonitorOverrideTarget struct {
	target               string
	syntheticRouteTarget bool
	syntheticRouteParent bool
}

func resolveMonitorOverrideTarget(ctx context.Context, tx *sql.Tx, serviceID, target string) (resolvedMonitorOverrideTarget, error) {
	resolved := resolvedMonitorOverrideTarget{target: strings.TrimSpace(target)}
	routes, err := serviceMonitorRoutes(ctx, tx, serviceID)
	if err != nil {
		return resolved, err
	}
	if resolved.target == routeMonitorTarget {
		if len(routes) > 0 {
			resolved.syntheticRouteTarget = true
			resolved.syntheticRouteParent = true
		}
		return resolved, nil
	}
	route, ok := routetarget.RouteFromTarget(resolved.target)
	if !ok {
		return resolved, nil
	}
	resolved.target = routetarget.Target(route)
	for _, configuredRoute := range routes {
		if configuredRoute == route {
			resolved.target = routetarget.Target(configuredRoute)
			resolved.syntheticRouteTarget = true
			return resolved, nil
		}
	}
	return resolved, nil
}

type serviceQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func serviceMonitorRoutes(ctx context.Context, queryer serviceQuerier, serviceID string) ([]string, error) {
	var exposureJSON string
	err := queryer.QueryRowContext(ctx, `SELECT exposure_json FROM services WHERE id=?`, serviceID).Scan(&exposureJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var exposure []string
	fromJSON(exposureJSON, &exposure)
	return monitorRoutesFromExposure(exposure), nil
}

func monitorRoutesFromExposure(exposure []string) []string {
	return routetarget.Routes(exposure)
}

func (store *Store) MonitorNotApplicable(ctx context.Context, serviceID, target string) (bool, error) {
	serviceID = strings.TrimSpace(serviceID)
	target = canonicalStatusTarget(target)
	if serviceID == "" || target == "" {
		return false, nil
	}
	exact, parent, err := monitorOverrideState(ctx, store.db, serviceID, target)
	if err != nil {
		return false, err
	}
	return exact || parent, nil
}

func (store *Store) PruneStatusTargets(ctx context.Context, serviceID, exactTarget, prefix string, keep []string) error {
	keepTargets := map[string]struct{}{}
	for _, target := range keep {
		keepTargets[target] = struct{}{}
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT target FROM status_results WHERE service_id=?`, serviceID)
	if err != nil {
		return err
	}
	var removeTargets []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			_ = rows.Close()
			return err
		}
		if target != exactTarget && (prefix == "" || !strings.HasPrefix(target, prefix)) {
			continue
		}
		if _, ok := keepTargets[target]; ok {
			continue
		}
		removeTargets = append(removeTargets, target)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, target := range removeTargets {
		if _, err := tx.ExecContext(ctx, `DELETE FROM status_results WHERE service_id=? AND target=?`, serviceID, target); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=? AND target=?`, serviceID, target); err != nil {
			return err
		}
	}
	return tx.Commit()
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
		statuses := make([]core.StatusResult, 0, len(targets))
		for _, status := range targets {
			statuses = append(statuses, status)
		}
		services[i].Health = aggregateTargetHealth(statuses)
	}
}

func aggregateTargetHealth(statuses []core.StatusResult) core.HealthState {
	applicable := make([]core.StatusResult, 0, len(statuses))
	for _, status := range statuses {
		if status.Health != core.HealthNotApplicable {
			applicable = append(applicable, status)
		}
	}
	statuses = applicable
	if len(statuses) == 0 {
		return core.HealthUnknown
	}
	if len(statuses) == 1 {
		return statuses[0].Health
	}
	allHealthy := true
	anyHealthy := false
	allUnknown := true
	worst := core.HealthUnknown
	for _, status := range statuses {
		if status.Health == core.HealthHealthy {
			anyHealthy = true
		} else {
			allHealthy = false
		}
		if status.Health != core.HealthUnknown {
			allUnknown = false
		}
		if healthPriority(status.Health) < healthPriority(worst) {
			worst = status.Health
		}
	}
	if allHealthy {
		return core.HealthHealthy
	}
	if anyHealthy {
		return core.HealthDegraded
	}
	if allUnknown {
		return core.HealthUnknown
	}
	return worst
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
	case core.HealthNotApplicable:
		return 5
	default:
		return 4
	}
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
	service.MonitorRoutes = monitorRoutesFromExposure(service.Exposure)
	if service.ConfigRefs == nil {
		service.ConfigRefs = []string{}
	}
	if service.Warnings == nil {
		service.Warnings = []string{}
	}
	return service
}

func (store *Store) UpsertStatus(ctx context.Context, status core.StatusResult) error {
	status.ServiceID = strings.TrimSpace(status.ServiceID)
	status.Target = canonicalStatusTarget(status.Target)
	if status.ObservedImages == nil {
		status.ObservedImages = []core.ObservedImage{}
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	exactOverride, parentOverride, err := monitorOverrideState(ctx, tx, status.ServiceID, status.Target)
	if err != nil {
		return err
	}
	if parentOverride && routetarget.IsChildTarget(status.Target) {
		return tx.Commit()
	}
	if exactOverride {
		status.Health = core.HealthNotApplicable
		status.Message = "not applicable"
		status.ObservedImages = []core.ObservedImage{}
	}
	checkedAt := status.CheckedAt.UTC().Format(time.RFC3339)
	message := store.redact(status.Message)
	_, err = tx.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at, observed_images_json)
VALUES(?, ?, ?, ?, ?, ?)
ON CONFLICT(service_id, target) DO UPDATE SET
  health=excluded.health, message=excluded.message, checked_at=excluded.checked_at,
  observed_images_json=excluded.observed_images_json
`, status.ServiceID, status.Target, string(status.Health), message, checkedAt, toJSON(status.ObservedImages))
	if err != nil {
		return fmt.Errorf("upsert status %s/%s: %w", status.ServiceID, status.Target, err)
	}
	if status.Health == core.HealthNotApplicable {
		return tx.Commit()
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

type overrideQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func monitorOverrideState(ctx context.Context, queryer overrideQuerier, serviceID, target string) (bool, bool, error) {
	target = canonicalStatusTarget(target)
	childTarget := routetarget.IsChildTarget(target)
	query := `
SELECT target
FROM monitor_overrides
WHERE service_id=? AND not_applicable=1 AND target=?
`
	args := []any{serviceID, target}
	if childTarget {
		query = `
SELECT target
FROM monitor_overrides
WHERE service_id=? AND not_applicable=1 AND target IN (?, ?)
`
		args = []any{serviceID, target, routeMonitorTarget}
	}
	rows, err := queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	exactOverride := false
	parentOverride := false
	for rows.Next() {
		var overrideTarget string
		if err := rows.Scan(&overrideTarget); err != nil {
			return false, false, err
		}
		switch {
		case overrideTarget == target:
			exactOverride = true
		case childTarget && overrideTarget == routeMonitorTarget:
			parentOverride = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	if parentOverride {
		routes, err := serviceMonitorRoutes(ctx, queryer, serviceID)
		if err != nil {
			return false, false, err
		}
		parentOverride = len(routes) > 0
	}
	return exactOverride, parentOverride, nil
}

func canonicalStatusTarget(target string) string {
	canonical, ok := routetarget.CanonicalTarget(target)
	if ok {
		return canonical
	}
	return strings.TrimSpace(target)
}

func (store *Store) parentRouteOverrideServices(ctx context.Context) (map[string]bool, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT s.id, s.exposure_json
FROM services s
JOIN monitor_overrides o ON o.service_id=s.id
WHERE o.target=? AND o.not_applicable=1
`, routeMonitorTarget)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	services := map[string]bool{}
	for rows.Next() {
		var serviceID string
		var exposureJSON string
		if err := rows.Scan(&serviceID, &exposureJSON); err != nil {
			return nil, err
		}
		var exposure []string
		fromJSON(exposureJSON, &exposure)
		if len(monitorRoutesFromExposure(exposure)) > 0 {
			services[serviceID] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return services, nil
}

func (store *Store) UptimeStats(ctx context.Context) ([]core.UptimeStat, error) {
	windowStart := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	parentRouteOverrides, err := store.parentRouteOverrideServices(ctx)
	if err != nil {
		return nil, err
	}
	stats := map[string]*core.UptimeStat{}
	order := []string{}
	countRows, err := store.db.QueryContext(ctx, `
SELECT h.service_id, h.target,
       SUM(CASE WHEN h.health IN ('healthy', 'degraded') THEN 1 ELSE 0 END) AS up_count,
       COUNT(*) AS total_count
FROM status_history h
WHERE h.checked_at >= ?
  AND h.health != 'not_applicable'
  AND NOT EXISTS (
    SELECT 1 FROM monitor_overrides o
    WHERE o.service_id=h.service_id AND o.target=h.target AND o.not_applicable=1
  )
GROUP BY h.service_id, h.target
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
		if parentRouteOverrides[serviceID] && strings.HasPrefix(target, routeTargetPrefix) {
			continue
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
  SELECT h.service_id, h.target, h.health, h.message, h.checked_at,
         ROW_NUMBER() OVER (PARTITION BY h.service_id, h.target ORDER BY h.checked_at DESC, h.id DESC) AS row_num
  FROM status_history h
  WHERE h.checked_at >= ?
    AND h.health != 'not_applicable'
    AND NOT EXISTS (
      SELECT 1 FROM monitor_overrides o
      WHERE o.service_id=h.service_id AND o.target=h.target AND o.not_applicable=1
    )
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
		if parentRouteOverrides[serviceID] && strings.HasPrefix(target, routeTargetPrefix) {
			continue
		}
		stat, ok := stats[serviceID+"\x00"+target]
		if !ok {
			continue
		}
		sample := core.UptimeSample{
			Health:  core.HealthState(health),
			Message: store.redact(message),
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
	message = core.FilterAgentMessageDockerLabels(message)
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
	  compose_project TEXT NOT NULL DEFAULT '',
	  warnings_json TEXT NOT NULL
	);

CREATE TABLE IF NOT EXISTS status_results (
  service_id TEXT NOT NULL,
  target TEXT NOT NULL,
  health TEXT NOT NULL,
  message TEXT NOT NULL,
  checked_at TEXT NOT NULL,
  observed_images_json TEXT NOT NULL DEFAULT '[]',
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

CREATE TABLE IF NOT EXISTS monitor_overrides (
  service_id TEXT NOT NULL,
  target TEXT NOT NULL,
  not_applicable INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(service_id, target)
);
`
