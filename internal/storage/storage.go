package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
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
	return core.DashboardSummary{
		Repositories: repos,
		Services:     services,
		Scans:        scans,
		Statuses:     statuses,
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
	latest := map[string]core.StatusResult{}
	for _, status := range statuses {
		current, ok := latest[status.ServiceID]
		if !ok || status.CheckedAt.After(current.CheckedAt) {
			latest[status.ServiceID] = status
		}
	}
	for i := range services {
		if status, ok := latest[services[i].ID]; ok {
			services[i].Health = status.Health
		}
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
	_, err := store.db.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(service_id, target) DO UPDATE SET
  health=excluded.health, message=excluded.message, checked_at=excluded.checked_at
`, status.ServiceID, status.Target, string(status.Health), redact(status.Message), status.CheckedAt.UTC().Format(time.RFC3339))
	return err
}

func (store *Store) UpsertAgent(ctx context.Context, message core.AgentMessage) error {
	_, err := store.db.ExecContext(ctx, `
INSERT INTO agents(target, last_seen_at, status_json)
VALUES(?, ?, ?)
ON CONFLICT(target) DO UPDATE SET last_seen_at=excluded.last_seen_at, status_json=excluded.status_json
`, message.Target, time.Now().UTC().Format(time.RFC3339), toJSON(message.Containers))
	return err
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
`
