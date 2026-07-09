package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	logger             *slog.Logger
	startupWarnings    []string
	decodeMu           sync.Mutex
	decodeFailures     map[decodeFailureKey]decodeFailure
	jsonProbeCursors   map[persistedJSONColumnKey]int64
}

var ErrStatusNotFound = errors.New("status result not found")

const (
	routeMonitorTarget          = routetarget.Parent
	routeTargetPrefix           = routetarget.Prefix
	statusHistoryWindow         = 7 * 24 * time.Hour
	routeMonitorLookupBatchSize = 500
)

type RuntimeServiceSource struct {
	Repository string
	SourcePath string
}

type decodeFailureKey struct {
	table  string
	column string
	key    string
}

type decodeFailure struct {
	table      string
	column     string
	rowID      int64
	key        string
	displayKey string
	err        string
	observedAt time.Time
}

type persistedJSONColumnKey struct {
	table  string
	column string
}

const statusResultJSONKeySeparator = "\x1f"

func Open(path string) (*Store, error) {
	return OpenWithLogger(path, nil)
}

func OpenWithLogger(path string, logger *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	store := &Store{db: db, logger: logger}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.RedactPersistedSensitiveValues(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.seedPersistedJSONDecodeFailures(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) Close() error {
	return store.db.Close()
}

func (store *Store) Ping(ctx context.Context) error {
	if store == nil || store.db == nil {
		return errors.New("sqlite store is not open")
	}
	if err := store.db.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite ping: %w", err)
	}
	return nil
}

func (store *Store) StartupWarnings() []string {
	if store == nil || len(store.startupWarnings) == 0 {
		return nil
	}
	warnings := make([]string, len(store.startupWarnings))
	copy(warnings, store.startupWarnings)
	return warnings
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
	changed, err := store.stripPersistedRouteUserinfo(ctx)
	if err != nil {
		return err
	}
	targetChanged, err := store.canonicalizeStoredRouteTargets(ctx)
	if err != nil {
		return err
	}
	if changed || targetChanged {
		return store.compactRedactedPages(ctx)
	}
	return nil
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

type startupJSONColumn struct {
	table   string
	column  string
	keyExpr string
	handle  func(data, table, column, key string) (string, bool, error)
}

func startupJSONColumns() []startupJSONColumn {
	stringSlice := func(data, table, column, key string) (string, bool, error) {
		var values []string
		return "", false, fromJSON(data, &values, table, column, key)
	}
	exposure := func(data, table, column, key string) (string, bool, error) {
		var values []string
		if err := fromJSON(data, &values, table, column, key); err != nil {
			return "", false, err
		}
		sanitized := sanitizeExposure(values)
		if sameStrings(values, sanitized) {
			return "", false, nil
		}
		return toJSON(sanitized), true, nil
	}
	observedImages := func(data, table, column, key string) (string, bool, error) {
		var values []core.ObservedImage
		return "", false, fromJSON(data, &values, table, column, key)
	}
	containers := func(data, table, column, key string) (string, bool, error) {
		var values []core.ContainerStatus
		return "", false, fromJSON(data, &values, table, column, key)
	}

	return []startupJSONColumn{
		{table: "services", column: "images_json", keyExpr: "id", handle: stringSlice},
		{table: "services", column: "ports_json", keyExpr: "id", handle: stringSlice},
		{table: "services", column: "dependencies_json", keyExpr: "id", handle: stringSlice},
		{table: "services", column: "storage_json", keyExpr: "id", handle: stringSlice},
		{table: "services", column: "exposure_json", keyExpr: "id", handle: exposure},
		{table: "services", column: "config_json", keyExpr: "id", handle: stringSlice},
		{table: "services", column: "warnings_json", keyExpr: "id", handle: stringSlice},
		{table: "status_results", column: "observed_images_json", keyExpr: "service_id || char(31) || target", handle: observedImages},
		{table: "agents", column: "status_json", keyExpr: "target", handle: containers},
	}
}

func (store *Store) scanStartupPersistedJSON(ctx context.Context, tx *sql.Tx) (bool, error) {
	changed := false
	for _, column := range startupJSONColumns() {
		columnChanged, skipped, err := store.scanStartupJSONColumn(ctx, tx, column)
		if err != nil {
			return false, err
		}
		changed = changed || columnChanged
		if skipped == 0 {
			continue
		}
		warning := fmt.Sprintf("startup validation skipped %d corrupt %s.%s row(s)", skipped, column.table, column.column)
		store.startupWarnings = append(store.startupWarnings, warning)
		store.logger.Warn(
			"startup validation skipped corrupt persisted JSON rows",
			"table", column.table,
			"column", column.column,
			"count", skipped,
		)
	}
	return changed, nil
}

func (store *Store) scanStartupJSONColumn(ctx context.Context, tx *sql.Tx, column startupJSONColumn) (bool, int, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT rowid, %s, %s FROM %s", column.keyExpr, column.column, column.table))
	if err != nil {
		return false, 0, fmt.Errorf("query startup JSON validation target %s.%s: %w", column.table, column.column, err)
	}
	type rowValue struct {
		rowID int64
		key   string
		value string
	}
	var updates []rowValue
	skippedDecodeErrors := 0
	for rows.Next() {
		var current rowValue
		if err := rows.Scan(&current.rowID, &current.key, &current.value); err != nil {
			_ = rows.Close()
			return false, skippedDecodeErrors, err
		}
		displayKey := store.persistedJSONDisplayKey(column.table, current.key)
		updatedValue, valueChanged, err := column.handle(current.value, column.table, column.column, displayKey)
		if err != nil {
			skippedDecodeErrors++
			store.recordDecodeFailure(column.table, column.column, current.rowID, current.key, displayKey, err)
			attrs := []any{
				"table", column.table,
				"key", displayKey,
				"column", column.column,
				"error", err,
			}
			if column.table == "services" {
				attrs = append(attrs, "service_id", displayKey)
			}
			store.logger.Warn("skipping corrupt persisted JSON during startup validation", attrs...)
			continue
		}
		if valueChanged {
			updates = append(updates, rowValue{rowID: current.rowID, value: updatedValue})
		}
	}
	if err := rows.Close(); err != nil {
		return false, skippedDecodeErrors, err
	}
	for _, update := range updates {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET %s=? WHERE rowid=?", column.table, column.column), update.value, update.rowID); err != nil {
			return false, skippedDecodeErrors, fmt.Errorf("update startup JSON target %s.%s: %w", column.table, column.column, err)
		}
	}
	return len(updates) > 0, skippedDecodeErrors, nil
}

func (store *Store) ProbePersistedJSON(ctx context.Context, sampleLimit int) error {
	if err := store.CheckPersistedJSONFailures(ctx); err != nil {
		return err
	}
	if sampleLimit < 1 {
		sampleLimit = 1
	}
	for _, column := range startupJSONColumns() {
		if err := store.probePersistedJSONColumn(ctx, column, sampleLimit); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) probePersistedJSONColumn(ctx context.Context, column startupJSONColumn, sampleLimit int) error {
	cursorKey := persistedJSONColumnKey{table: column.table, column: column.column}
	lastRowID := store.persistedJSONProbeCursor(cursorKey)
	samples, err := store.persistedJSONProbeRows(ctx, column, "rowid > ?", []any{lastRowID}, sampleLimit)
	if err != nil {
		return err
	}
	if len(samples) < sampleLimit {
		wrapped, err := store.persistedJSONProbeRows(ctx, column, "rowid <= ?", []any{lastRowID}, sampleLimit-len(samples))
		if err != nil {
			return err
		}
		samples = append(samples, wrapped...)
	}
	for _, sample := range samples {
		if err := store.decodePersistedJSON(sample.value, column, sample.rowID, sample.key); err != nil {
			return err
		}
	}
	if len(samples) > 0 {
		store.setPersistedJSONProbeCursor(cursorKey, samples[len(samples)-1].rowID)
	}
	return nil
}

type persistedJSONProbeRow struct {
	rowID int64
	key   string
	value string
}

func (store *Store) persistedJSONProbeRows(ctx context.Context, column startupJSONColumn, where string, args []any, limit int) ([]persistedJSONProbeRow, error) {
	if limit < 1 {
		return nil, nil
	}
	query := fmt.Sprintf("SELECT rowid, %s, %s FROM %s WHERE %s ORDER BY rowid LIMIT ?", column.keyExpr, column.column, column.table, where)
	args = append(args, limit)
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query persisted JSON probe target %s.%s: %w", column.table, column.column, err)
	}
	defer rows.Close()
	var samples []persistedJSONProbeRow
	for rows.Next() {
		var sample persistedJSONProbeRow
		if err := rows.Scan(&sample.rowID, &sample.key, &sample.value); err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}

func (store *Store) persistedJSONProbeCursor(key persistedJSONColumnKey) int64 {
	store.decodeMu.Lock()
	defer store.decodeMu.Unlock()
	return store.jsonProbeCursors[key]
}

func (store *Store) setPersistedJSONProbeCursor(key persistedJSONColumnKey, rowID int64) {
	store.decodeMu.Lock()
	defer store.decodeMu.Unlock()
	if store.jsonProbeCursors == nil {
		store.jsonProbeCursors = map[persistedJSONColumnKey]int64{}
	}
	store.jsonProbeCursors[key] = rowID
}

func (store *Store) CheckPersistedJSONFailures(ctx context.Context) error {
	failures := store.persistedJSONFailures()
	if len(failures) == 0 {
		return nil
	}
	columns := startupJSONColumnLookup()
	for _, failure := range failures {
		column, ok := columns[persistedJSONColumnKey{table: failure.table, column: failure.column}]
		if !ok {
			store.clearDecodeFailure(failure.table, failure.column, failure.key)
			continue
		}
		err := store.validateDecodeFailure(ctx, failure, column)
		if err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) seedPersistedJSONDecodeFailures(ctx context.Context) error {
	store.resetDecodeFailures()
	for _, column := range startupJSONColumns() {
		if err := store.seedPersistedJSONDecodeFailuresColumn(ctx, column); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) seedPersistedJSONDecodeFailuresColumn(ctx context.Context, column startupJSONColumn) error {
	rows, err := store.db.QueryContext(ctx, fmt.Sprintf("SELECT rowid, %s, %s FROM %s ORDER BY rowid", column.keyExpr, column.column, column.table))
	if err != nil {
		return fmt.Errorf("query persisted JSON registry seed target %s.%s: %w", column.table, column.column, err)
	}
	defer rows.Close()
	for rows.Next() {
		var rowID int64
		var key, value string
		if err := rows.Scan(&rowID, &key, &value); err != nil {
			return err
		}
		_ = store.decodePersistedJSON(value, column, rowID, key)
	}
	return rows.Err()
}

func startupJSONColumnLookup() map[persistedJSONColumnKey]startupJSONColumn {
	columns := startupJSONColumns()
	lookup := make(map[persistedJSONColumnKey]startupJSONColumn, len(columns))
	for _, column := range columns {
		lookup[persistedJSONColumnKey{table: column.table, column: column.column}] = column
	}
	return lookup
}

func (store *Store) validateDecodeFailure(ctx context.Context, failure decodeFailure, column startupJSONColumn) error {
	where, args, ok := persistedJSONLookup(column.table, failure.key)
	if !ok {
		store.clearDecodeFailure(failure.table, failure.column, failure.key)
		return nil
	}
	var key, value string
	var rowID int64
	err := store.db.QueryRowContext(
		ctx,
		fmt.Sprintf("SELECT rowid, %s, %s FROM %s WHERE %s", column.keyExpr, column.column, column.table, where),
		args...,
	).Scan(&rowID, &key, &value)
	if errors.Is(err, sql.ErrNoRows) {
		store.clearDecodeFailure(failure.table, failure.column, failure.key)
		return nil
	}
	if err != nil {
		return err
	}
	if err := store.decodePersistedJSON(value, column, rowID, key); err != nil {
		return fmt.Errorf("persisted JSON decode failure table=%s column=%s key=%s: %s", failure.table, failure.column, failure.displayKey, store.redact(err.Error()))
	}
	return nil
}

func persistedJSONLookup(table, key string) (string, []any, bool) {
	switch table {
	case "services":
		return "id=?", []any{key}, true
	case "status_results":
		serviceID, target, ok := splitStatusResultJSONKey(key)
		if !ok {
			return "", nil, false
		}
		return "service_id=? AND target=?", []any{serviceID, target}, true
	case "agents":
		return "target=?", []any{key}, true
	default:
		return "", nil, false
	}
}

func statusResultJSONKey(serviceID, target string) string {
	return serviceID + statusResultJSONKeySeparator + target
}

func splitStatusResultJSONKey(key string) (string, string, bool) {
	serviceID, target, ok := strings.Cut(key, statusResultJSONKeySeparator)
	return serviceID, target, ok
}

func (store *Store) decodePersistedJSON(data string, column startupJSONColumn, rowID int64, key string) error {
	displayKey := store.persistedJSONDisplayKey(column.table, key)
	if _, _, err := column.handle(data, column.table, column.column, displayKey); err != nil {
		store.recordDecodeFailure(column.table, column.column, rowID, key, displayKey, err)
		return err
	}
	store.clearDecodeFailure(column.table, column.column, key)
	return nil
}

func (store *Store) fromPersistedJSON(data string, value any, table, column string, rowID int64, key string) error {
	displayKey := store.persistedJSONDisplayKey(table, key)
	if err := fromJSON(data, value, table, column, displayKey); err != nil {
		store.recordDecodeFailure(table, column, rowID, key, displayKey, err)
		return err
	}
	store.clearDecodeFailure(table, column, key)
	return nil
}

func (store *Store) recordDecodeFailure(table, column string, rowID int64, key, displayKey string, err error) {
	store.decodeMu.Lock()
	defer store.decodeMu.Unlock()
	if store.decodeFailures == nil {
		store.decodeFailures = map[decodeFailureKey]decodeFailure{}
	}
	failureKey := decodeFailureKey{table: table, column: column, key: key}
	store.decodeFailures[failureKey] = decodeFailure{
		table:      table,
		column:     column,
		rowID:      rowID,
		key:        key,
		displayKey: displayKey,
		err:        store.redact(err.Error()),
		observedAt: time.Now().UTC(),
	}
}

func (store *Store) clearDecodeFailure(table, column, key string) {
	store.decodeMu.Lock()
	defer store.decodeMu.Unlock()
	delete(store.decodeFailures, decodeFailureKey{table: table, column: column, key: key})
}

func (store *Store) resetDecodeFailures() {
	store.decodeMu.Lock()
	defer store.decodeMu.Unlock()
	store.decodeFailures = map[decodeFailureKey]decodeFailure{}
}

func (store *Store) persistedJSONFailures() []decodeFailure {
	store.decodeMu.Lock()
	defer store.decodeMu.Unlock()
	failures := make([]decodeFailure, 0, len(store.decodeFailures))
	for _, failure := range store.decodeFailures {
		failures = append(failures, failure)
	}
	sort.Slice(failures, func(i, j int) bool {
		if failures[i].table != failures[j].table {
			return failures[i].table < failures[j].table
		}
		if failures[i].column != failures[j].column {
			return failures[i].column < failures[j].column
		}
		return failures[i].displayKey < failures[j].displayKey
	})
	return failures
}

func (store *Store) persistedJSONDisplayKey(table, key string) string {
	if table == "status_results" {
		serviceID, target, ok := splitStatusResultJSONKey(key)
		if ok {
			key = serviceID + "/" + target
		}
	}
	return store.redact(routetarget.StripUserinfo(key))
}

func (store *Store) canonicalizeStoredRouteTargets(ctx context.Context) (bool, error) {
	return store.canonicalizeStoredRouteTargetsForNames(ctx, []string{routeMonitorTarget})
}

func (store *Store) CanonicalizeHTTPRouteTargets(ctx context.Context, targets []config.HTTPRouteTarget) error {
	changed, err := store.canonicalizeStoredRouteTargetsForNames(ctx, httpRouteTargetNames(targets))
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return store.compactRedactedPages(ctx)
}

func (store *Store) canonicalizeStoredRouteTargetsForNames(ctx context.Context, targetNames []string) (bool, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	changed := false
	for _, targetName := range targetNames {
		monitorOverridesChanged, err := canonicalizeMonitorOverrides(ctx, tx, targetName)
		if err != nil {
			return false, err
		}
		changed = changed || monitorOverridesChanged
		statusResultsChanged, err := canonicalizeStatusResults(ctx, tx, targetName)
		if err != nil {
			return false, err
		}
		changed = changed || statusResultsChanged
		statusHistoryChanged, err := canonicalizeStatusHistory(ctx, tx, targetName)
		if err != nil {
			return false, err
		}
		changed = changed || statusHistoryChanged
		if err := enforceActiveRouteOverrideStatuses(ctx, tx, targetName); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return changed, nil
}

func httpRouteTargetNames(targets []config.HTTPRouteTarget) []string {
	names := make([]string, 0, len(targets)+1)
	names = append(names, routeMonitorTarget)
	for _, target := range targets {
		name := strings.TrimSpace(target.Name)
		if name == "" {
			name = routeMonitorTarget
		}
		names = append(names, name)
	}
	return dedupeStrings(names)
}

func routeTargetPrefixForName(targetName string) string {
	targetName = strings.TrimSpace(targetName)
	if targetName == "" {
		targetName = routeMonitorTarget
	}
	return targetName + ": "
}

type monitorOverrideRouteAlias struct {
	serviceID     string
	target        string
	notApplicable int
	updatedAt     string
	canonical     string
}

func canonicalizeMonitorOverrides(ctx context.Context, tx *sql.Tx, targetName string) (bool, error) {
	targetPrefix := routeTargetPrefixForName(targetName)
	rows, err := tx.QueryContext(ctx, `
SELECT service_id, target, not_applicable, updated_at
FROM monitor_overrides
WHERE target LIKE ?
`, targetPrefix+"%")
	if err != nil {
		return false, fmt.Errorf("query route override aliases: %w", err)
	}
	var aliases []monitorOverrideRouteAlias
	sensitiveChanged := false
	for rows.Next() {
		var alias monitorOverrideRouteAlias
		if err := rows.Scan(&alias.serviceID, &alias.target, &alias.notApplicable, &alias.updatedAt); err != nil {
			_ = rows.Close()
			return false, err
		}
		canonical, ok := routetarget.CanonicalTargetForName(alias.target, targetName)
		if !ok || canonical == alias.target {
			continue
		}
		sensitiveChanged = sensitiveChanged || sanitizer.StripURLUserinfo(alias.target) != alias.target
		alias.canonical = canonical
		aliases = append(aliases, alias)
	}
	if err := rows.Close(); err != nil {
		return false, err
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
				return false, fmt.Errorf("canonicalize route override %s/%s: %w", alias.serviceID, alias.target, err)
			}
		case err != nil:
			return false, err
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
				return false, fmt.Errorf("merge route override %s/%s: %w", alias.serviceID, alias.target, err)
			}
			if _, err := tx.ExecContext(ctx, `
DELETE FROM monitor_overrides
WHERE service_id=? AND target=?
`, alias.serviceID, alias.target); err != nil {
				return false, fmt.Errorf("delete route override alias %s/%s: %w", alias.serviceID, alias.target, err)
			}
		}
	}
	return sensitiveChanged, nil
}

type statusResultRouteAlias struct {
	serviceID string
	target    string
	health    string
	message   string
	checkedAt string
	canonical string
}

func canonicalizeStatusResults(ctx context.Context, tx *sql.Tx, targetName string) (bool, error) {
	targetPrefix := routeTargetPrefixForName(targetName)
	rows, err := tx.QueryContext(ctx, `
SELECT service_id, target, health, message, checked_at
FROM status_results
WHERE target LIKE ?
`, targetPrefix+"%")
	if err != nil {
		return false, fmt.Errorf("query route status aliases: %w", err)
	}
	var aliases []statusResultRouteAlias
	sensitiveChanged := false
	for rows.Next() {
		var alias statusResultRouteAlias
		if err := rows.Scan(&alias.serviceID, &alias.target, &alias.health, &alias.message, &alias.checkedAt); err != nil {
			_ = rows.Close()
			return false, err
		}
		canonical, ok := routetarget.CanonicalTargetForName(alias.target, targetName)
		if !ok || canonical == alias.target {
			continue
		}
		sensitiveChanged = sensitiveChanged || sanitizer.StripURLUserinfo(alias.target) != alias.target
		alias.canonical = canonical
		aliases = append(aliases, alias)
	}
	if err := rows.Close(); err != nil {
		return false, err
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
				return false, fmt.Errorf("canonicalize route status %s/%s: %w", alias.serviceID, alias.target, err)
			}
		case err != nil:
			return false, err
		default:
			chosen := existing
			if shouldReplaceCanonicalStatus(alias, existing) {
				chosen = alias
			}
			if _, err := tx.ExecContext(ctx, `
UPDATE status_results SET health=?, message=?, checked_at=?
WHERE service_id=? AND target=?
`, chosen.health, chosen.message, chosen.checkedAt, alias.serviceID, alias.canonical); err != nil {
				return false, fmt.Errorf("merge route status %s/%s: %w", alias.serviceID, alias.target, err)
			}
			if _, err := tx.ExecContext(ctx, `
DELETE FROM status_results
WHERE service_id=? AND target=?
`, alias.serviceID, alias.target); err != nil {
				return false, fmt.Errorf("delete route status alias %s/%s: %w", alias.serviceID, alias.target, err)
			}
		}
	}
	return sensitiveChanged, nil
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

func canonicalizeStatusHistory(ctx context.Context, tx *sql.Tx, targetName string) (bool, error) {
	targetPrefix := routeTargetPrefixForName(targetName)
	rows, err := tx.QueryContext(ctx, `
SELECT id, target
FROM status_history
WHERE target LIKE ?
`, targetPrefix+"%")
	if err != nil {
		return false, fmt.Errorf("query route history aliases: %w", err)
	}
	var aliases []statusHistoryRouteAlias
	sensitiveChanged := false
	for rows.Next() {
		var alias statusHistoryRouteAlias
		if err := rows.Scan(&alias.id, &alias.target); err != nil {
			_ = rows.Close()
			return false, err
		}
		canonical, ok := routetarget.CanonicalTargetForName(alias.target, targetName)
		if !ok || canonical == alias.target {
			continue
		}
		sensitiveChanged = sensitiveChanged || sanitizer.StripURLUserinfo(alias.target) != alias.target
		alias.canonical = canonical
		aliases = append(aliases, alias)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, alias := range aliases {
		if _, err := tx.ExecContext(ctx, `
UPDATE status_history SET target=?
WHERE id=?
`, alias.canonical, alias.id); err != nil {
			return false, fmt.Errorf("canonicalize route history %d/%s: %w", alias.id, alias.target, err)
		}
	}
	return sensitiveChanged, nil
}

type activeRouteOverride struct {
	serviceID string
	target    string
	updatedAt string
}

func enforceActiveRouteOverrideStatuses(ctx context.Context, tx *sql.Tx, targetName string) error {
	targetPrefix := routeTargetPrefixForName(targetName)
	rows, err := tx.QueryContext(ctx, `
SELECT service_id, target, updated_at
FROM monitor_overrides
WHERE target LIKE ? AND not_applicable=1
`, targetPrefix+"%")
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

func (store *Store) StatusResults(ctx context.Context) ([]core.StatusResult, error) {
	parentRouteOverrides, err := store.parentRouteOverrideServices(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `
SELECT rowid, service_id, target, health, message, checked_at, observed_images_json
FROM status_results ORDER BY checked_at DESC
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var statuses []core.StatusResult
	for rows.Next() {
		var status core.StatusResult
		var rowID int64
		var health, checkedAt, observedImages string
		if err := rows.Scan(&rowID, &status.ServiceID, &status.Target, &health, &status.Message, &checkedAt, &observedImages); err != nil {
			return nil, err
		}
		status.Health = core.HealthState(health)
		status.Message = store.redact(status.Message)
		if err := store.fromPersistedJSON(observedImages, &status.ObservedImages, "status_results", "observed_images_json", rowID, statusResultJSONKey(status.ServiceID, status.Target)); err != nil {
			return nil, err
		}
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

	resolved, err := resolveMonitorOverrideTarget(ctx, store, tx, serviceID, target)
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

func resolveMonitorOverrideTarget(ctx context.Context, store *Store, tx *sql.Tx, serviceID, target string) (resolvedMonitorOverrideTarget, error) {
	resolved := resolvedMonitorOverrideTarget{target: strings.TrimSpace(target)}
	routes, err := serviceMonitorRoutes(ctx, store, tx, serviceID)
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

func serviceMonitorRoutes(ctx context.Context, store *Store, queryer serviceQuerier, serviceID string) ([]string, error) {
	var rowID int64
	var exposureJSON string
	err := queryer.QueryRowContext(ctx, `SELECT rowid, exposure_json FROM services WHERE id=?`, serviceID).Scan(&rowID, &exposureJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var exposure []string
	if err := store.fromPersistedJSON(exposureJSON, &exposure, "services", "exposure_json", rowID, serviceID); err != nil {
		return nil, err
	}
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
	exact, parent, err := monitorOverrideState(ctx, store, store.db, serviceID, target)
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

type RouteMonitorLookup struct {
	Overrides     map[string]struct{}
	StatusTargets map[string]struct{}
}

func (store *Store) RouteMonitorLookup(ctx context.Context, serviceIDs []string, exactTarget, targetPrefix string) (map[string]RouteMonitorLookup, error) {
	serviceIDs = dedupeStrings(serviceIDs)
	if len(serviceIDs) == 0 {
		return map[string]RouteMonitorLookup{}, nil
	}

	result := make(map[string]RouteMonitorLookup, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		result[serviceID] = RouteMonitorLookup{
			Overrides:     map[string]struct{}{},
			StatusTargets: map[string]struct{}{},
		}
	}

	for _, batch := range chunkStrings(serviceIDs, routeMonitorLookupBatchSize) {
		if err := store.routeMonitorLookupChunk(ctx, batch, exactTarget, targetPrefix, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (store *Store) routeMonitorLookupChunk(ctx context.Context, serviceIDs []string, exactTarget, targetPrefix string, result map[string]RouteMonitorLookup) error {
	placeholders := sqlPlaceholders(len(serviceIDs))

	overrideArgs := make([]any, 0, len(serviceIDs)+2)
	for _, serviceID := range serviceIDs {
		overrideArgs = append(overrideArgs, serviceID)
	}
	overrideArgs = append(overrideArgs, exactTarget, targetPrefix+"%")
	overrideRows, err := store.db.QueryContext(ctx, `
SELECT service_id, target
FROM monitor_overrides
WHERE service_id IN (`+placeholders+`) AND not_applicable = 1
  AND (target = ? OR target LIKE ?)
`, overrideArgs...)
	if err != nil {
		return err
	}
	for overrideRows.Next() {
		var serviceID, target string
		if err := overrideRows.Scan(&serviceID, &target); err != nil {
			_ = overrideRows.Close()
			return err
		}
		state, ok := result[serviceID]
		if !ok {
			continue
		}
		state.Overrides[target] = struct{}{}
		result[serviceID] = state
	}
	if err := overrideRows.Close(); err != nil {
		return err
	}

	statusArgs := make([]any, 0, len(serviceIDs)+2)
	for _, serviceID := range serviceIDs {
		statusArgs = append(statusArgs, serviceID)
	}
	statusArgs = append(statusArgs, exactTarget, targetPrefix+"%")
	statusRows, err := store.db.QueryContext(ctx, `
SELECT service_id, target
FROM status_results
WHERE service_id IN (`+placeholders+`) AND (target = ? OR target LIKE ?)
`, statusArgs...)
	if err != nil {
		return err
	}
	for statusRows.Next() {
		var serviceID, target string
		if err := statusRows.Scan(&serviceID, &target); err != nil {
			_ = statusRows.Close()
			return err
		}
		state, ok := result[serviceID]
		if !ok {
			continue
		}
		state.StatusTargets[target] = struct{}{}
		result[serviceID] = state
	}
	if err := statusRows.Close(); err != nil {
		return err
	}
	return nil
}

func (store *Store) PruneStatusTargetsFromKnown(ctx context.Context, serviceID, exactTarget, prefix string, keep []string, statusTargets map[string]struct{}, keepExact bool) error {
	keepTargets := make(map[string]struct{}, len(keep))
	for _, target := range keep {
		keepTargets[target] = struct{}{}
	}

	removeTargets := make([]string, 0, len(statusTargets))
	for target := range statusTargets {
		if target == exactTarget && exactTarget != "" {
			if keepExact {
				continue
			}
			removeTargets = append(removeTargets, target)
			continue
		}
		if prefix == "" || !strings.HasPrefix(target, prefix) {
			continue
		}
		if _, keep := keepTargets[target]; keep {
			continue
		}
		removeTargets = append(removeTargets, target)
	}
	if len(removeTargets) == 0 {
		return nil
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteByServiceAndTargets(ctx, tx, "status_results", serviceID, removeTargets); err != nil {
		return err
	}
	if err := deleteByServiceAndTargets(ctx, tx, "status_history", serviceID, removeTargets); err != nil {
		return err
	}
	return tx.Commit()
}

func sqlPlaceholders(count int) string {
	parts := make([]string, 0, count)
	for i := 0; i < count; i++ {
		parts = append(parts, "?")
	}
	return strings.Join(parts, ",")
}

func dedupeStrings(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	unique := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		unique = append(unique, item)
	}
	return unique
}

func chunkStrings(items []string, size int) [][]string {
	if size <= 0 {
		return [][]string{items}
	}
	chunks := make([][]string, 0, (len(items)+size-1)/size)
	for start := 0; start < len(items); start += size {
		end := start + size
		if end > len(items) {
			end = len(items)
		}
		chunks = append(chunks, items[start:end])
	}
	return chunks
}

func deleteByServiceAndTargets(ctx context.Context, tx *sql.Tx, table string, serviceID string, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	placeholders := sqlPlaceholders(len(targets))
	args := make([]any, 0, len(targets)+1)
	args = append(args, serviceID)
	for _, target := range targets {
		args = append(args, target)
	}
	query := "DELETE FROM " + table + " WHERE service_id=? AND target IN (" + placeholders + ")"
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("delete %s rows: %w", table, err)
	}
	return nil
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
	exactOverride, parentOverride, err := monitorOverrideState(ctx, store, tx, status.ServiceID, status.Target)
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
		if _, err := tx.ExecContext(ctx, `
DELETE FROM status_history WHERE service_id=? AND target=?
`, status.ServiceID, status.Target); err != nil {
			return fmt.Errorf("clear not applicable status history %s/%s: %w", status.ServiceID, status.Target, err)
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO status_history(service_id, target, health, message, checked_at)
VALUES(?, ?, ?, ?, ?)
`, status.ServiceID, status.Target, string(status.Health), message, checkedAt)
	if err != nil {
		return fmt.Errorf("insert status history %s/%s: %w", status.ServiceID, status.Target, err)
	}
	return tx.Commit()
}

func (store *Store) PruneStatusHistory(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-statusHistoryWindow).Format(time.RFC3339)
	if _, err := store.db.ExecContext(ctx, `DELETE FROM status_history WHERE checked_at < ?`, cutoff); err != nil {
		return fmt.Errorf("prune status history: %w", err)
	}
	return nil
}

type overrideQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func monitorOverrideState(ctx context.Context, store *Store, queryer overrideQuerier, serviceID, target string) (bool, bool, error) {
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
		routes, err := serviceMonitorRoutes(ctx, store, queryer, serviceID)
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
SELECT s.rowid, s.id, s.exposure_json
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
		var rowID int64
		var serviceID string
		var exposureJSON string
		if err := rows.Scan(&rowID, &serviceID, &exposureJSON); err != nil {
			return nil, err
		}
		var exposure []string
		if err := store.fromPersistedJSON(exposureJSON, &exposure, "services", "exposure_json", rowID, serviceID); err != nil {
			return nil, err
		}
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
SELECT rowid, target, last_seen_at, status_json FROM agents ORDER BY target
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var agents []core.AgentInfo
	for rows.Next() {
		var agent core.AgentInfo
		var rowID int64
		var statusJSON string
		if err := rows.Scan(&rowID, &agent.Target, &agent.LastSeenAt, &statusJSON); err != nil {
			return nil, err
		}
		if err := store.fromPersistedJSON(statusJSON, &agent.Containers, "agents", "status_json", rowID, agent.Target); err != nil {
			return nil, err
		}
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

func fromJSON(data string, value any, table, column, key string) error {
	if strings.TrimSpace(data) == "" {
		return fmt.Errorf("decode persisted JSON table=%s key=%s column=%s: empty JSON value", table, key, column)
	}
	if err := json.Unmarshal([]byte(data), value); err != nil {
		return fmt.Errorf("decode persisted JSON table=%s key=%s column=%s: %w", table, key, column, err)
	}
	return nil
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
