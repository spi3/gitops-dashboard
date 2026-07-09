package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/routetarget"
)

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
