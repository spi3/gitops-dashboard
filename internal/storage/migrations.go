package storage

import (
	"context"
	"fmt"
)

type migrationStep struct {
	id    string
	apply func(context.Context) (bool, error)
}

func (store *Store) migrate(ctx context.Context) error {
	changed := false
	for _, step := range store.migrationSteps() {
		stepChanged, err := step.apply(ctx)
		if err != nil {
			return fmt.Errorf("migration %s: %w", step.id, err)
		}
		changed = changed || stepChanged
	}
	if changed {
		return store.compactRedactedPages(ctx)
	}
	return nil
}

func (store *Store) migrationSteps() []migrationStep {
	return []migrationStep{
		{
			id: "001_base_schema",
			apply: func(ctx context.Context) (bool, error) {
				_, err := store.db.ExecContext(ctx, migrations)
				if err != nil {
					return false, fmt.Errorf("run schema SQL: %w", err)
				}
				return false, nil
			},
		},
		store.ensureColumnStep("002_services_config_json", "services", "config_json", "TEXT NOT NULL DEFAULT '[]'"),
		store.ensureColumnStep("003_services_compose_project", "services", "compose_project", "TEXT NOT NULL DEFAULT ''"),
		store.ensureColumnStep("004_status_results_observed_images_json", "status_results", "observed_images_json", "TEXT NOT NULL DEFAULT '[]'"),
		{
			id:    "005_validate_and_repair_persisted_json",
			apply: store.stripPersistedRouteUserinfo,
		},
		{
			id:    "006_canonicalize_route_status_targets",
			apply: store.canonicalizeStoredRouteTargets,
		},
	}
}

func (store *Store) ensureColumnStep(id, table, column, definition string) migrationStep {
	return migrationStep{
		id: id,
		apply: func(ctx context.Context) (bool, error) {
			return false, store.ensureColumn(ctx, table, column, definition)
		},
	}
}

func (store *Store) stripPersistedRouteUserinfo(ctx context.Context) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	changed, err := store.scanStartupPersistedJSON(ctx, tx)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
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

CREATE INDEX IF NOT EXISTS idx_status_history_checked_at_lookup
ON status_history(checked_at, service_id, target);

CREATE TABLE IF NOT EXISTS monitor_overrides (
  service_id TEXT NOT NULL,
  target TEXT NOT NULL,
  not_applicable INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL,
  PRIMARY KEY(service_id, target)
);
`
