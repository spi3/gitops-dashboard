package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestLatestTerminalScansByRepositoryQueryPlanUsesIndex is the mandated
// query-plan assertion (survey 2026-07-25 IR-6/F8): every access to scans in
// LatestTerminalScansByRepository's plan, including the correlated MAX
// subquery, must use an index rather than a full table scan. It runs
// EXPLAIN QUERY PLAN against the exact SQL text the production query
// builder produces, so it fails if that query ever regresses off the index.
func TestLatestTerminalScansByRepositoryQueryPlanUsesIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	query := latestTerminalScansByRepositoryQuery(sqlPlaceholders(2))
	rows, err := store.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, "repo-a", "repo-b")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 {
		t.Fatal("query plan was empty")
	}
	for _, detail := range plan {
		if strings.Contains(detail, "SCAN") && !strings.Contains(detail, "USING") {
			t.Fatalf("query plan step %q is a full scan (no index used); plan = %v", detail, plan)
		}
	}
}

// TestScanIndexAppliesToFreshDatabase covers the migration requirement that
// the index applies to a freshly created database at store open.
func TestScanIndexAppliesToFreshDatabase(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertScanIndexShape(t, store)
}

// TestScanIndexMigrationRepairsSameNamedIndexWithDifferentDefinition is the
// alert-index reconciliation precedent applied to scans: an existing
// database whose idx_scans_repository_status_id index already exists, but
// with a different column set, must have it dropped and recreated to the
// current definition at store open, exactly like the alert indexes.
func TestScanIndexMigrationRepairsSameNamedIndexWithDifferentDefinition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE scans (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repository TEXT NOT NULL,
  status TEXT NOT NULL,
  commit_sha TEXT,
  started_at TEXT NOT NULL,
  finished_at TEXT,
  error TEXT
);
CREATE INDEX idx_scans_repository_status_id ON scans(repository);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertScanIndexShape(t, store)
}

func assertScanIndexShape(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	var tbl string
	var indexSQL string
	err := store.db.QueryRowContext(ctx, `SELECT tbl_name, sql FROM sqlite_master WHERE type='index' AND name='idx_scans_repository_status_id'`).Scan(&tbl, &indexSQL)
	if err != nil {
		t.Fatalf("idx_scans_repository_status_id missing: %v", err)
	}
	if tbl != "scans" {
		t.Fatalf("index table = %q, want scans", tbl)
	}
	columns, err := indexColumnsTx(ctx, store.db, "idx_scans_repository_status_id")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"repository", "status", "id"}
	if !stringSlicesEqual(columns, want) {
		t.Fatalf("index columns = %v, want %v", columns, want)
	}
}

// TestDefaultScanRetentionHorizonMeetsSevenDayFloor documents and enforces
// the chosen default: at least 7 days, matching the UI's 50-row summary
// window and the existing status_history retention precedent.
func TestDefaultScanRetentionHorizonMeetsSevenDayFloor(t *testing.T) {
	t.Parallel()
	if DefaultScanRetentionHorizon < 7*24*time.Hour {
		t.Fatalf("DefaultScanRetentionHorizon = %s, want at least 7 days", DefaultScanRetentionHorizon)
	}
}

func insertScanRow(t *testing.T, store *Store, id int64, repository, status, startedAt string) {
	t.Helper()
	ctx := context.Background()
	finishedAt := any(nil)
	if status != "running" {
		finishedAt = startedAt
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO scans(id, repository, status, commit_sha, started_at, finished_at, error)
VALUES(?, ?, ?, '', ?, ?, '')
`, id, repository, status, startedAt, finishedAt); err != nil {
		t.Fatalf("insert scan row %d: %v", id, err)
	}
}

// TestPruneTerminalScansRemovesOldTerminalRowsOnly covers the retention
// shape (PruneTerminalAlertEvents precedent): only terminal rows older than
// the horizon are removed; a running row is never touched regardless of
// age, and a recent terminal row inside the horizon is kept.
func TestPruneTerminalScansRemovesOldTerminalRowsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	recent := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

	insertScanRow(t, store, 1, "repo-old-then-recent", "ok", old)
	insertScanRow(t, store, 2, "repo-old-then-recent", "ok", recent)
	insertScanRow(t, store, 3, "repo-old-running", "running", old)
	insertScanRow(t, store, 4, "repo-recent-terminal", "error", recent)

	horizon := time.Since(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	pruned, err := store.PruneTerminalScans(ctx, horizon, 500)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (only the superseded old terminal row)", pruned)
	}

	remaining := map[int64]bool{}
	rows, err := store.db.QueryContext(ctx, `SELECT id FROM scans`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		remaining[id] = true
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if remaining[1] {
		t.Fatal("old, superseded terminal row survived prune, want removed")
	}
	if !remaining[2] {
		t.Fatal("newest terminal row for repo-old-then-recent was pruned, want kept")
	}
	if !remaining[3] {
		t.Fatal("old running row was pruned, want kept (retention never touches non-terminal rows)")
	}
	if !remaining[4] {
		t.Fatal("recent terminal row was pruned, want kept (inside horizon)")
	}
}

// TestPruneTerminalScansBoundsBatchSize covers bounded-batch deletion while
// still respecting the newest-row invariant: of many old terminal rows for
// one repository, every row is prunable except the single newest by id,
// which must survive across as many batches as it takes to clear the rest.
func TestPruneTerminalScansBoundsBatchSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	const total = 8
	for i := int64(1); i <= total; i++ {
		insertScanRow(t, store, i, "repo-batch", "ok", old)
	}

	horizon := time.Since(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	pruned, err := store.PruneTerminalScans(ctx, horizon, 3)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != total-1 {
		t.Fatalf("pruned = %d, want %d (all but the newest row, across batches)", pruned, total-1)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM scans`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining scans = %d, want 1", remaining)
	}
	var survivorID int64
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM scans`).Scan(&survivorID); err != nil {
		t.Fatal(err)
	}
	if survivorID != total {
		t.Fatalf("surviving row id = %d, want %d (newest by id)", survivorID, total)
	}
}

// TestPruneTerminalScansNeverDeletesNewestTerminalRowPerRepository is the
// mandated hard-invariant test: a repository whose ONLY terminal row is far
// older than the horizon must survive pruning, and
// LatestTerminalScansByRepository must return identical results
// immediately before and after any prune across a mixed multi-repository
// dataset, regardless of how old each repository's newest terminal row is.
func TestPruneTerminalScansNeverDeletesNewestTerminalRowPerRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ancient := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	recent := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)

	// repo-only-ancient-terminal: its ONLY terminal row is far older than
	// the horizon (the case the spec calls out explicitly) and must survive.
	insertScanRow(t, store, 1, "repo-only-ancient-terminal", "error", ancient)

	// repo-many-old-terminal: several old terminal rows; only the newest
	// (highest id) may survive.
	insertScanRow(t, store, 2, "repo-many-old-terminal", "ok", old)
	insertScanRow(t, store, 3, "repo-many-old-terminal", "error", old)
	insertScanRow(t, store, 4, "repo-many-old-terminal", "ok", old)

	// repo-running-only: never had a terminal scan; must remain absent from
	// LatestTerminalScansByRepository throughout.
	insertScanRow(t, store, 5, "repo-running-only", "running", ancient)

	// repo-recent-terminal: inside the horizon, trivially kept, included so
	// the before/after comparison spans both pruned and unpruned repositories.
	insertScanRow(t, store, 6, "repo-recent-terminal", "ok", recent)

	repoNames := []string{
		"repo-only-ancient-terminal", "repo-many-old-terminal", "repo-running-only", "repo-never-scanned", "repo-recent-terminal",
	}
	before, err := store.LatestTerminalScansByRepository(ctx, repoNames)
	if err != nil {
		t.Fatal(err)
	}

	horizon := time.Since(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if _, err := store.PruneTerminalScans(ctx, horizon, 500); err != nil {
		t.Fatal(err)
	}

	after, err := store.LatestTerminalScansByRepository(ctx, repoNames)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("LatestTerminalScansByRepository changed across prune:\nbefore = %+v\nafter  = %+v", before, after)
	}

	assertLatestTerminalScan(t, after, "repo-only-ancient-terminal", 1, "error")
	assertLatestTerminalScan(t, after, "repo-many-old-terminal", 4, "ok")
	assertLatestTerminalScan(t, after, "repo-recent-terminal", 6, "ok")
	if _, ok := after["repo-running-only"]; ok {
		t.Fatal("repo-running-only present in LatestTerminalScansByRepository, want absent (no terminal scan)")
	}

	if got := countRows(t, store, `SELECT COUNT(*) FROM scans WHERE id=1`); got != 1 {
		t.Fatal("repo-only-ancient-terminal's only terminal row was pruned, want kept regardless of age")
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM scans WHERE id IN (2,3)`); got != 0 {
		t.Fatal("superseded old terminal rows for repo-many-old-terminal survived prune, want removed")
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM scans WHERE id=5`); got != 1 {
		t.Fatal("running row was pruned, want kept (retention never touches non-terminal rows)")
	}
}

// TestScansPreChangeFixtureOpensMigratesAndPrunes is the standing-invariant
// 3 pre-change fixture: a database created by the release before this task,
// with a populated scans table (running + ok + error rows across multiple
// repositories) and no scans index, must open, migrate (gain the index),
// operate normally, and prune per the retention rules, with scan history
// visible in the summary and the evaluator's baseline results preserved.
func TestScansPreChangeFixtureOpensMigratesAndPrunes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE scans (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  repository TEXT NOT NULL,
  status TEXT NOT NULL,
  commit_sha TEXT,
  started_at TEXT NOT NULL,
  finished_at TEXT,
  error TEXT
);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	recent := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	if _, err := db.ExecContext(ctx, `
INSERT INTO scans(id, repository, status, commit_sha, started_at, finished_at, error)
VALUES
  (1, 'repo-a', 'ok', 'aaa', ?, ?, ''),
  (2, 'repo-a', 'ok', 'bbb', ?, ?, ''),
  (3, 'repo-b', 'error', 'ccc', ?, ?, 'clone failed'),
  (4, 'repo-b', 'running', '', ?, NULL, '')
`, old, old, recent, recent, old, old, recent, recent); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	assertScanIndexShape(t, store)

	baseline, err := store.LatestTerminalScansByRepository(ctx, []string{"repo-a", "repo-b"})
	if err != nil {
		t.Fatal(err)
	}
	assertLatestTerminalScan(t, baseline, "repo-a", 2, "ok")
	assertLatestTerminalScan(t, baseline, "repo-b", 3, "error")

	scans, err := store.Scans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(scans) != 4 {
		t.Fatalf("summary scans = %d, want 4 (all pre-existing rows visible)", len(scans))
	}

	horizon := time.Since(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	pruned, err := store.PruneTerminalScans(ctx, horizon, 500)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (only repo-a's superseded old terminal row)", pruned)
	}

	afterPrune, err := store.LatestTerminalScansByRepository(ctx, []string{"repo-a", "repo-b"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(baseline, afterPrune) {
		t.Fatalf("evaluator baseline changed after prune: before = %+v, after = %+v", baseline, afterPrune)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM scans WHERE id=1`); got != 0 {
		t.Fatal("repo-a's superseded old terminal row survived prune, want removed")
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM scans WHERE id=4 AND status='running'`); got != 1 {
		t.Fatal("repo-b's running row was pruned or altered, want kept untouched")
	}
}
