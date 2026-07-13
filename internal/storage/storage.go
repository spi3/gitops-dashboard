package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/example/gitops-dashboard/internal/routetarget"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db                 *sql.DB
	dataDir            string
	redactionMu        sync.RWMutex
	redactionRawValues []string
	redactionTokenSet  map[string]struct{}
	redactionTokens    []string
	logger             *slog.Logger
	startupWarnings    []string
	decodeMu           sync.Mutex
	decodeFailures     map[decodeFailureKey]decodeFailure
	jsonProbeCursors   map[persistedJSONColumnKey]int64
	summaryMu          sync.RWMutex
	summaryVersion     uint64
	summaryCache       summaryCache
	freshnessMu        sync.RWMutex
	statusTTLs         map[string]time.Duration
	alertDedupeKey     []byte
	alertDedupeKeyPath string
	alertSinkNames     map[string]struct{}
	alertSinkAllowlist bool
	alertStateMu       sync.RWMutex
	// Health-alert cleanup attempts may finish out of order. Only the newest
	// completed attempt may update the retryable cleanup-lock component.
	healthAlertCleanupAttempt          uint64
	healthAlertCleanupCompletedAttempt uint64
	// afterSuccessfulHealthAlertCleanup is a narrow test seam for interleaving
	// cleanup attempts at the SQL/state-update boundary.
	afterSuccessfulHealthAlertCleanup func()
	// Durable and cleanup locks have different recovery contracts. Durable
	// locks require their specific repair path (usually operator action and a
	// restart); cleanup is retried after every inventory commit.
	alertStateDurableLockMsg string
	alertStateCleanupLockMsg string
	healthAlerts             HealthAlertProducerConfig
	resetAlertState          bool
	alertResetToken          string
	alertResetTokenFP        string
	// restoreAlertForeignKeys is a narrow test seam for pooled-connection
	// restoration failures during alert-table rebuilds.
	restoreAlertForeignKeys func(context.Context, *sql.Conn) error
}

var ErrStatusNotFound = errors.New("status result not found")

var errInvalidAlertDedupeKey = errors.New("invalid key material")

const (
	alertDedupeKeyFingerprintMetadata = "alert_dedupe_key_fingerprint"
	alertDedupeResetPendingMetadata   = "alert_dedupe_reset_pending"
	alertDedupeKeyLockMetadata        = "alert_dedupe_key_lock"
	defaultAlertDedupeResetToken      = "default"
)

type OpenOptions struct {
	Logger                      *slog.Logger
	RedactionValues             []string
	ResetAlertStateOnMissingKey bool
	ResetAlertStateToken        string
	AlertSinkNames              []string
	AlertSinkAllowlist          bool
	HealthAlerts                HealthAlertProducerConfig
}

// HealthAlertProducerConfig controls service-rollup health alerts. Targets are
// inputs to the rollup only; this avoids one incident producing an alert per
// monitor target. A zero-value config disables production for compatibility.
type HealthAlertProducerConfig struct {
	Enabled          bool
	Sinks            []string
	Debounce         time.Duration
	Cooldown         time.Duration
	StabilitySamples int
}

type alertDedupeKeyLoadResult struct {
	key                   []byte
	locked                bool
	lockMessage           string
	resetAlertState       bool
	resetTokenFingerprint string
}

type alertDedupeKeyResetMode int

const (
	alertDedupeKeyResetPreferExisting alertDedupeKeyResetMode = iota
	alertDedupeKeyResetCreateMissing
	alertDedupeKeyResetReplaceExisting
	alertDedupeKeyResetReplaceCorrupt
)

const (
	routeMonitorTarget          = routetarget.Parent
	routeTargetPrefix           = routetarget.Prefix
	statusHistoryWindow         = 7 * 24 * time.Hour
	routeMonitorLookupBatchSize = 500
)

func Open(path string) (*Store, error) {
	return OpenWithLogger(path, nil)
}

func OpenWithLogger(path string, logger *slog.Logger) (*Store, error) {
	return OpenWithOptions(path, OpenOptions{Logger: logger})
}

func OpenWithLoggerAndRedactionValues(path string, logger *slog.Logger, redactionValues ...string) (*Store, error) {
	return OpenWithOptions(path, OpenOptions{Logger: logger, RedactionValues: redactionValues})
}

func OpenWithOptions(path string, options OpenOptions) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	db, err := sql.Open("sqlite3", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Metadata and the reset ledger are alert-only state. A damaged historical
	// metadata table must not prevent the dashboard's core store from opening.
	metadataErr := ensureStorageMetadataTable(context.Background(), db)
	if metadataErr != nil {
		// Preserve the malformed alert metadata for manual inspection, then make
		// the core schema runnable. It remains latched for this process because
		// its historical alert fingerprint cannot be trusted.
		_ = repairAlertMetadataTable(context.Background(), db)
	}
	resetBookkeepingErr := metadataErr
	if resetBookkeepingErr == nil {
		resetBookkeepingErr = ensureAlertDedupeResetConsumedTable(context.Background(), db)
	}
	resetToken := normalizeAlertDedupeResetToken(options.ResetAlertStateToken)
	alertDedupeKeyPath := filepath.Join(filepath.Dir(path), "alert-dedupe.key")
	keyResult := alertDedupeKeyLoadResult{}
	if resetBookkeepingErr == nil {
		keyResult, err = loadOrCreateAlertDedupeKey(context.Background(), db, path, alertDedupeKeyPath, options.ResetAlertStateOnMissingKey, resetToken, logger)
		if err != nil {
			// The dedupe key and its bookkeeping are alert-only state. Keep the
			// core store available if a damaged or inaccessible key cannot be
			// initialized; alerting remains latched until an operator repairs it.
			keyResult = lockedAlertDedupeKeyResult(logger, alertDedupeKeyPath, err)
		}
	} else {
		keyResult.locked = true
		keyResult.lockMessage = fmt.Sprintf("alert state locked: initialize alert metadata/reset bookkeeping failed: %v", resetBookkeepingErr)
	}
	store := &Store{
		db:                       db,
		dataDir:                  filepath.Dir(path),
		logger:                   logger,
		alertDedupeKey:           keyResult.key,
		alertDedupeKeyPath:       alertDedupeKeyPath,
		alertSinkNames:           alertSinkNameSet(options.AlertSinkNames),
		alertSinkAllowlist:       options.AlertSinkAllowlist,
		healthAlerts:             normalizeHealthAlertProducerConfig(options.HealthAlerts),
		alertStateDurableLockMsg: keyResult.lockMessage,
		resetAlertState:          keyResult.resetAlertState,
		alertResetToken:          resetToken,
		alertResetTokenFP:        keyResult.resetTokenFingerprint,
	}
	if keyResult.locked && keyResult.lockMessage != "" {
		store.startupWarnings = append(store.startupWarnings, keyResult.lockMessage)
	}
	store.AddRedactionValues(options.RedactionValues...)
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := store.redactCorePersistedSensitiveValues(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("redact persisted core-sensitive values: %w", err)
	}
	if _, err := store.redactAlertPersistedSensitiveValues(context.Background()); err != nil {
		store.lockAlertState(fmt.Sprintf("alert state locked: redact persisted alert-sensitive values failed: %v", err))
	}
	if err := store.seedPersistedJSONDecodeFailures(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func normalizeHealthAlertProducerConfig(config HealthAlertProducerConfig) HealthAlertProducerConfig {
	if !config.Enabled {
		return HealthAlertProducerConfig{}
	}
	if config.StabilitySamples <= 0 {
		config.StabilitySamples = 2
	}
	return config
}

func alertSinkNameSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
		}
	}
	return set
}

func loadOrCreateAlertDedupeKey(ctx context.Context, db *sql.DB, dbPath, keyPath string, resetOnMissingKey bool, resetToken string, logger *slog.Logger) (alertDedupeKeyLoadResult, error) {
	resetPending, err := storageMetadataBool(ctx, db, alertDedupeResetPendingMetadata)
	if err != nil {
		return alertDedupeKeyLoadResult{}, err
	}
	key, err := readAlertDedupeKey(keyPath)
	if err == nil {
		if resetPending {
			return resetAlertDedupeKeyWithLock(ctx, db, keyPath, resetToken, alertDedupeKeyResetPreferExisting, logger, "alert dedupe key reset marker found; completing pending alert state reset")
		}
		result, err := verifyAlertDedupeKeyFingerprint(ctx, db, keyPath, key, resetOnMissingKey, resetToken, logger)
		if err != nil || result.locked || result.resetAlertState {
			return result, err
		}
		return alertDedupeKeyLoadResult{key: key}, nil
	}
	if errors.Is(err, errInvalidAlertDedupeKey) {
		key, retryErr := readAlertDedupeKeyWithRetry(keyPath)
		if retryErr == nil {
			if resetPending {
				return resetAlertDedupeKeyWithLock(ctx, db, keyPath, resetToken, alertDedupeKeyResetPreferExisting, logger, "alert dedupe key reset marker found; completing pending alert state reset")
			}
			result, err := verifyAlertDedupeKeyFingerprint(ctx, db, keyPath, key, resetOnMissingKey, resetToken, logger)
			if err != nil || result.locked || result.resetAlertState {
				return result, err
			}
			return alertDedupeKeyLoadResult{key: key}, nil
		}
		if resetPending || resetOnMissingKey {
			return resetAlertDedupeKeyWithLock(ctx, db, keyPath, resetToken, alertDedupeKeyResetReplaceCorrupt, logger, "alert dedupe key invalid; reset requested, moved corrupt key aside and generated a fresh key")
		}
		return lockedAlertDedupeKeyResult(logger, keyPath, fmt.Errorf("read alert dedupe key: %w", retryErr)), nil
	}
	if !os.IsNotExist(err) {
		return lockedAlertDedupeKeyResult(logger, keyPath, fmt.Errorf("read alert dedupe key: %w", err)), nil
	}
	if resetPending {
		return resetAlertDedupeKeyWithLock(ctx, db, keyPath, resetToken, alertDedupeKeyResetCreateMissing, logger, "alert dedupe key reset marker found without key file; generated replacement key and completing pending alert state reset")
	}
	hasKeyBackedAlertRows, err := databaseHasKeyBackedAlertRows(dbPath)
	if err != nil {
		return alertDedupeKeyLoadResult{}, fmt.Errorf("inspect alert state before creating dedupe key: %w", err)
	}
	hasFingerprint, err := storageMetadataExists(ctx, db, alertDedupeKeyFingerprintMetadata)
	if err != nil {
		return alertDedupeKeyLoadResult{}, err
	}
	if hasKeyBackedAlertRows && !resetOnMissingKey {
		message := fmt.Sprintf("alert state locked: dedupe key missing at %s; restore the key file, or set alerting.resetOnMissingKey: true once to reset alert state", keyPath)
		logger.Error(message)
		return alertDedupeKeyLoadResult{locked: true, lockMessage: message}, nil
	}
	if hasFingerprint && !resetOnMissingKey {
		if key, retryErr := readAlertDedupeKeyWithRetry(keyPath); retryErr == nil {
			return verifyAlertDedupeKeyFingerprint(ctx, db, keyPath, key, resetOnMissingKey, resetToken, logger)
		}
		message := fmt.Sprintf("alert state locked: dedupe key missing at %s; restore the key file, or set alerting.resetOnMissingKey: true once to reset alert state", keyPath)
		logger.Error(message)
		return alertDedupeKeyLoadResult{locked: true, lockMessage: message}, nil
	}
	if (hasKeyBackedAlertRows || hasFingerprint) && resetOnMissingKey {
		return resetAlertDedupeKeyWithLock(ctx, db, keyPath, resetToken, alertDedupeKeyResetCreateMissing, logger, "alert dedupe key missing; resetOnMissingKey requested, terminalizing old alert state and generating a fresh key")
	}
	key, err = createAlertDedupeKey(keyPath)
	if err != nil {
		return lockedAlertDedupeKeyResult(logger, keyPath, err), nil
	}
	if err := writeAlertDedupeKeyFingerprint(ctx, db, key); err != nil {
		return alertDedupeKeyLoadResult{}, err
	}
	return alertDedupeKeyLoadResult{key: key}, nil
}

func verifyAlertDedupeKeyFingerprint(ctx context.Context, db *sql.DB, keyPath string, key []byte, resetOnMissingKey bool, resetToken string, logger *slog.Logger) (alertDedupeKeyLoadResult, error) {
	stored, ok, err := storageMetadataValue(ctx, db, alertDedupeKeyFingerprintMetadata)
	if err != nil {
		return alertDedupeKeyLoadResult{}, err
	}
	current := alertDedupeKeyFingerprint(key)
	if !ok || stored == "" {
		hasKeyBackedAlertRows, err := databaseHasKeyBackedAlertRowsDB(ctx, db)
		if err != nil {
			return alertDedupeKeyLoadResult{}, fmt.Errorf("inspect alert state before writing dedupe key fingerprint: %w", err)
		}
		if hasKeyBackedAlertRows {
			if !resetOnMissingKey {
				message := fmt.Sprintf("alert state locked: dedupe key fingerprint missing for key-backed alert rows at %s; restore storage metadata with the correct key file, or set alerting.resetOnMissingKey: true once to reset alert state", keyPath)
				logger.Error(message)
				return alertDedupeKeyLoadResult{locked: true, lockMessage: message}, nil
			}
			return resetAlertDedupeKeyWithLock(ctx, db, keyPath, resetToken, alertDedupeKeyResetReplaceExisting, logger, "alert dedupe key fingerprint missing; resetOnMissingKey requested, terminalizing old alert state and generating a fresh key")
		}
		if err := writeAlertDedupeKeyFingerprint(ctx, db, key); err != nil {
			return alertDedupeKeyLoadResult{}, err
		}
		return alertDedupeKeyLoadResult{key: key}, nil
	}
	if stored == current {
		return alertDedupeKeyLoadResult{key: key}, nil
	}
	if !resetOnMissingKey {
		message := fmt.Sprintf("alert state locked: dedupe key mismatch at %s; restore the correct key file, or set alerting.resetOnMissingKey: true once to reset alert state", keyPath)
		logger.Error(message)
		return alertDedupeKeyLoadResult{locked: true, lockMessage: message}, nil
	}
	return resetAlertDedupeKeyWithLock(ctx, db, keyPath, resetToken, alertDedupeKeyResetReplaceExisting, logger, "alert dedupe key mismatch; resetOnMissingKey requested, terminalizing old alert state and generating a fresh key")
}

func resetAlertDedupeKeyWithLock(ctx context.Context, db *sql.DB, keyPath, resetToken string, mode alertDedupeKeyResetMode, logger *slog.Logger, logMessage string) (alertDedupeKeyLoadResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return alertDedupeKeyLoadResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := acquireAlertDedupeKeyLockTx(ctx, tx); err != nil {
		return alertDedupeKeyLoadResult{}, err
	}

	pendingValue, resetPending, err := storageMetadataValueTx(ctx, tx, alertDedupeResetPendingMetadata)
	if err != nil {
		return alertDedupeKeyLoadResult{}, err
	}
	pendingTokenFingerprint := alertDedupeResetPendingTokenFingerprint(pendingValue, resetToken)
	currentKey, currentKeyErr := readAlertDedupeKey(keyPath)
	if currentKeyErr == nil {
		stored, ok, err := storageMetadataValueTx(ctx, tx, alertDedupeKeyFingerprintMetadata)
		if err != nil {
			return alertDedupeKeyLoadResult{}, err
		}
		if ok && stored == alertDedupeKeyFingerprint(currentKey) {
			if err := tx.Commit(); err != nil {
				return alertDedupeKeyLoadResult{}, err
			}
			if err := verifyAlertDedupeKeyFileAndFingerprint(ctx, db, keyPath, currentKey); err != nil {
				return lockedAlertDedupeKeyResult(logger, keyPath, err), nil
			}
			if resetPending {
				logger.Error("alert dedupe key reset marker found; completing pending alert state reset", "path", keyPath)
			}
			return alertDedupeKeyLoadResult{key: currentKey, resetAlertState: resetPending, resetTokenFingerprint: pendingTokenFingerprint}, nil
		}
		if mode == alertDedupeKeyResetPreferExisting {
			resetTokenFingerprint := pendingTokenFingerprint
			if !resetPending {
				resetTokenFingerprint = alertDedupeResetTokenFingerprint(resetToken)
			}
			if err := setStorageMetadataTx(ctx, tx, alertDedupeResetPendingMetadata, resetTokenFingerprint); err != nil {
				return alertDedupeKeyLoadResult{}, err
			}
			if err := writeAlertDedupeKeyFingerprintTx(ctx, tx, currentKey); err != nil {
				return alertDedupeKeyLoadResult{}, err
			}
			if err := verifyAlertDedupeKeyFileAndFingerprintTx(ctx, tx, keyPath, currentKey); err != nil {
				return lockedAlertDedupeKeyResult(logger, keyPath, err), nil
			}
			if err := tx.Commit(); err != nil {
				return alertDedupeKeyLoadResult{}, err
			}
			if err := verifyAlertDedupeKeyFileAndFingerprint(ctx, db, keyPath, currentKey); err != nil {
				return lockedAlertDedupeKeyResult(logger, keyPath, err), nil
			}
			logger.Error(logMessage, "path", keyPath)
			return alertDedupeKeyLoadResult{key: currentKey, resetAlertState: true, resetTokenFingerprint: resetTokenFingerprint}, nil
		}
	}
	// Another opener may have completed the repair while this one waited for
	// the transaction lock. Only consult the one-shot ledger if repair is still
	// required after observing the current key and fingerprint under that lock.
	if !resetPending {
		consumed, err := alertDedupeResetTokenConsumedTx(ctx, tx, resetToken)
		if err != nil {
			return alertDedupeKeyLoadResult{}, err
		}
		if consumed {
			message := fmt.Sprintf("alert state locked: alerting.resetOnMissingKey was already consumed for resetToken fingerprint %s; change alerting.resetToken to re-arm reset, or repair the key file", alertDedupeResetTokenFingerprintPrefix(resetToken))
			logger.Error(message)
			return alertDedupeKeyLoadResult{locked: true, lockMessage: message}, nil
		}
	}

	resetTokenFingerprint := pendingTokenFingerprint
	if !resetPending {
		resetTokenFingerprint = alertDedupeResetTokenFingerprint(resetToken)
	}
	if err := setStorageMetadataTx(ctx, tx, alertDedupeResetPendingMetadata, resetTokenFingerprint); err != nil {
		return alertDedupeKeyLoadResult{}, err
	}
	var key []byte
	switch mode {
	case alertDedupeKeyResetPreferExisting:
		if currentKeyErr == nil {
			key = currentKey
			break
		}
		if errors.Is(currentKeyErr, errInvalidAlertDedupeKey) {
			key, err = replaceCorruptAlertDedupeKey(keyPath)
		} else {
			key, err = createAlertDedupeKey(keyPath)
		}
	case alertDedupeKeyResetCreateMissing:
		if currentKeyErr == nil {
			if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
				return lockedAlertDedupeKeyResult(logger, keyPath, fmt.Errorf("replace alert dedupe key: %w", err)), nil
			}
		}
		key, err = createAlertDedupeKey(keyPath)
	case alertDedupeKeyResetReplaceExisting:
		if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
			return lockedAlertDedupeKeyResult(logger, keyPath, fmt.Errorf("replace alert dedupe key: %w", err)), nil
		}
		key, err = createAlertDedupeKey(keyPath)
	case alertDedupeKeyResetReplaceCorrupt:
		if currentKeyErr == nil {
			if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
				return lockedAlertDedupeKeyResult(logger, keyPath, fmt.Errorf("replace alert dedupe key: %w", err)), nil
			}
			key, err = createAlertDedupeKey(keyPath)
		} else {
			key, err = replaceCorruptAlertDedupeKey(keyPath)
		}
	default:
		return alertDedupeKeyLoadResult{}, fmt.Errorf("unknown alert dedupe key reset mode %d", mode)
	}
	if err != nil {
		return lockedAlertDedupeKeyResult(logger, keyPath, err), nil
	}
	if err := writeAlertDedupeKeyFingerprintTx(ctx, tx, key); err != nil {
		return alertDedupeKeyLoadResult{}, err
	}
	if err := verifyAlertDedupeKeyFileAndFingerprintTx(ctx, tx, keyPath, key); err != nil {
		return lockedAlertDedupeKeyResult(logger, keyPath, err), nil
	}
	if err := tx.Commit(); err != nil {
		return alertDedupeKeyLoadResult{}, err
	}
	if err := verifyAlertDedupeKeyFileAndFingerprint(ctx, db, keyPath, key); err != nil {
		return lockedAlertDedupeKeyResult(logger, keyPath, err), nil
	}
	logger.Error(logMessage, "path", keyPath)
	return alertDedupeKeyLoadResult{key: key, resetAlertState: true, resetTokenFingerprint: resetTokenFingerprint}, nil
}

func lockedAlertDedupeKeyResult(logger *slog.Logger, keyPath string, cause error) alertDedupeKeyLoadResult {
	message := fmt.Sprintf("alert state locked: dedupe key unavailable at %s: %v; repair the key file, restore the correct key file, or set alerting.resetOnMissingKey: true once to reset alert state", keyPath, cause)
	logger.Error(message)
	return alertDedupeKeyLoadResult{locked: true, lockMessage: message}
}

func replaceCorruptAlertDedupeKey(keyPath string) ([]byte, error) {
	corruptPath := keyPath + ".corrupt"
	if err := os.Remove(corruptPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("replace corrupt alert dedupe key: %w", err)
	}
	if err := os.Rename(keyPath, corruptPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("move corrupt alert dedupe key aside: %w", err)
	}
	if err := syncDirectory(filepath.Dir(keyPath)); err != nil {
		return nil, err
	}
	return createAlertDedupeKey(keyPath)
}

func createAlertDedupeKey(keyPath string) ([]byte, error) {
	return createAlertDedupeKeyWithFile(keyPath, func(path string, flag int, perm os.FileMode) (alertDedupeKeyFile, error) {
		return os.OpenFile(path, flag, perm)
	}, os.Remove)
}

type alertDedupeKeyFile interface {
	WriteString(string) (int, error)
	Sync() error
	Close() error
}

func createAlertDedupeKeyWithFile(keyPath string, openFile func(string, int, os.FileMode) (alertDedupeKeyFile, error), remove func(string) error) (key []byte, err error) {
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate alert dedupe key: %w", err)
	}
	file, err := openFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return readAlertDedupeKeyWithRetry(keyPath)
	}
	if err != nil {
		return nil, fmt.Errorf("write alert dedupe key: %w", err)
	}
	created := false
	defer func() {
		if !created {
			err = errors.Join(err, file.Close(), remove(keyPath))
		}
	}()
	if _, err := file.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return nil, fmt.Errorf("write alert dedupe key: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync alert dedupe key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close alert dedupe key: %w", err)
	}
	created = true
	if err := syncDirectory(filepath.Dir(keyPath)); err != nil {
		return nil, err
	}
	return key, nil
}

func databaseHasKeyBackedAlertRows(path string) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	db, err := sql.Open("sqlite3", sqliteDSN(path))
	if err != nil {
		return false, err
	}
	defer db.Close()
	return databaseHasKeyBackedAlertRowsDB(context.Background(), db)
}

func databaseHasKeyBackedAlertRowsDB(ctx context.Context, db *sql.DB) (bool, error) {
	for _, table := range []string{"alert_events", "alert_events_legacy"} {
		exists, err := sqliteTableExists(ctx, db, table)
		if err != nil {
			return false, err
		}
		if !exists {
			continue
		}
		hasHashColumn, err := sqliteTableHasColumn(ctx, db, table, "dedupe_hash")
		if err != nil {
			return false, err
		}
		if !hasHashColumn {
			continue
		}
		var count int
		if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE COALESCE(dedupe_hash, '')<>''`, table)).Scan(&count); err != nil {
			return false, err
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
}

func ensureStorageMetadataTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS storage_metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
)`)
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(storage_metadata)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasValue := false
	hasKey := false
	keyPrimaryOrdinal := 0
	primaryKeyColumnCount := 0
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if pk > 0 {
			primaryKeyColumnCount++
		}
		switch name {
		case "value":
			hasValue = true
		case "key":
			hasKey = true
			keyPrimaryOrdinal = pk
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasKey || !hasValue {
		return errors.New("storage_metadata is missing key or value column")
	}
	if keyPrimaryOrdinal == 1 && primaryKeyColumnCount == 1 {
		return nil
	}
	unique, err := sqliteTableHasUniqueKeyIndex(ctx, db, "storage_metadata", "key")
	if err != nil {
		return err
	}
	if !unique {
		return errors.New("storage_metadata key column is not primary or unique")
	}
	return nil
}

func sqliteTableHasUniqueKeyIndex(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA index_list("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var seq, unique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return false, err
		}
		if unique != 1 || partial != 0 {
			continue
		}
		indexRows, err := db.QueryContext(ctx, "PRAGMA index_info("+name+")")
		if err != nil {
			return false, err
		}
		var columns []string
		for indexRows.Next() {
			var seqno, cid int
			var indexColumn string
			if err := indexRows.Scan(&seqno, &cid, &indexColumn); err != nil {
				_ = indexRows.Close()
				return false, err
			}
			columns = append(columns, indexColumn)
		}
		err = errors.Join(indexRows.Err(), indexRows.Close())
		if err != nil {
			return false, err
		}
		if len(columns) == 1 && columns[0] == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func repairAlertMetadataTable(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `ALTER TABLE storage_metadata RENAME TO storage_metadata_alert_legacy`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE storage_metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	return tx.Commit()
}

func ensureAlertDedupeResetConsumedTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS alert_dedupe_reset_consumed (
  token_fingerprint TEXT PRIMARY KEY,
  key_fingerprint TEXT NOT NULL,
  consumed_at TEXT NOT NULL,
  consumed_at_ns INTEGER NOT NULL
)`)
	return err
}

func storageMetadataValue(ctx context.Context, db *sql.DB, key string) (string, bool, error) {
	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM storage_metadata WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func storageMetadataValueTx(ctx context.Context, tx *sql.Tx, key string) (string, bool, error) {
	var value string
	err := tx.QueryRowContext(ctx, `SELECT value FROM storage_metadata WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func storageMetadataExists(ctx context.Context, db *sql.DB, key string) (bool, error) {
	_, ok, err := storageMetadataValue(ctx, db, key)
	return ok, err
}

func storageMetadataBool(ctx context.Context, db *sql.DB, key string) (bool, error) {
	value, ok, err := storageMetadataValue(ctx, db, key)
	if err != nil || !ok {
		return false, err
	}
	return value != "" && value != "0" && !strings.EqualFold(value, "false"), nil
}

func storageMetadataBoolTx(ctx context.Context, tx *sql.Tx, key string) (bool, error) {
	value, ok, err := storageMetadataValueTx(ctx, tx, key)
	if err != nil || !ok {
		return false, err
	}
	return value != "" && value != "0" && !strings.EqualFold(value, "false"), nil
}

func setStorageMetadata(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO storage_metadata(key, value)
VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value
`, key, value)
	return err
}

func setStorageMetadataTx(ctx context.Context, tx *sql.Tx, key, value string) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO storage_metadata(key, value)
VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value
`, key, value)
	return err
}

func deleteStorageMetadata(ctx context.Context, db *sql.DB, key string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM storage_metadata WHERE key=?`, key)
	return err
}

func deleteStorageMetadataTx(ctx context.Context, tx *sql.Tx, key string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM storage_metadata WHERE key=?`, key)
	return err
}

func writeAlertDedupeKeyFingerprint(ctx context.Context, db *sql.DB, key []byte) error {
	return setStorageMetadata(ctx, db, alertDedupeKeyFingerprintMetadata, alertDedupeKeyFingerprint(key))
}

func writeAlertDedupeKeyFingerprintTx(ctx context.Context, tx *sql.Tx, key []byte) error {
	return setStorageMetadataTx(ctx, tx, alertDedupeKeyFingerprintMetadata, alertDedupeKeyFingerprint(key))
}

func writeAlertDedupeResetConsumedTx(ctx context.Context, tx *sql.Tx, resetToken string, key []byte) error {
	return writeAlertDedupeResetFingerprintConsumedTx(ctx, tx, alertDedupeResetTokenFingerprint(resetToken), key)
}

func writeAlertDedupeResetFingerprintConsumedTx(ctx context.Context, tx *sql.Tx, tokenFingerprint string, key []byte) error {
	now := time.Now().UTC()
	_, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO alert_dedupe_reset_consumed(token_fingerprint, key_fingerprint, consumed_at, consumed_at_ns)
VALUES(?, ?, ?, ?)
`, tokenFingerprint, alertDedupeKeyFingerprint(key), now.Format(time.RFC3339Nano), now.UnixNano())
	return err
}

func alertDedupeResetTokenConsumedTx(ctx context.Context, tx *sql.Tx, resetToken string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_dedupe_reset_consumed WHERE token_fingerprint=?`, alertDedupeResetTokenFingerprint(resetToken)).Scan(&count)
	return count > 0, err
}

func alertDedupeKeyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])
}

func normalizeAlertDedupeResetToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return defaultAlertDedupeResetToken
	}
	return token
}

func alertDedupeResetTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(normalizeAlertDedupeResetToken(token)))
	return hex.EncodeToString(sum[:])
}

func alertDedupeResetTokenFingerprintPrefix(token string) string {
	fingerprint := alertDedupeResetTokenFingerprint(token)
	if len(fingerprint) < 8 {
		return fingerprint
	}
	return fingerprint[:8]
}

func alertDedupeResetPendingTokenFingerprint(value, fallbackToken string) string {
	value = strings.TrimSpace(value)
	if len(value) == sha256.Size*2 {
		for _, r := range value {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return alertDedupeResetTokenFingerprint(fallbackToken)
			}
		}
		return strings.ToLower(value)
	}
	return alertDedupeResetTokenFingerprint(fallbackToken)
}

func acquireAlertDedupeKeyLockTx(ctx context.Context, tx *sql.Tx) error {
	return setStorageMetadataTx(ctx, tx, alertDedupeKeyLockMetadata, "1")
}

func verifyAlertDedupeKeyFileAndFingerprint(ctx context.Context, db *sql.DB, keyPath string, expectedKey []byte) error {
	key, err := readAlertDedupeKey(keyPath)
	if err != nil {
		return err
	}
	if alertDedupeKeyFingerprint(key) != alertDedupeKeyFingerprint(expectedKey) {
		return errors.New("alert dedupe key changed during reset")
	}
	stored, ok, err := storageMetadataValue(ctx, db, alertDedupeKeyFingerprintMetadata)
	if err != nil {
		return err
	}
	if !ok || stored != alertDedupeKeyFingerprint(key) {
		return errors.New("alert dedupe key fingerprint does not match key file")
	}
	return nil
}

func verifyAlertDedupeKeyFileAndFingerprintTx(ctx context.Context, tx *sql.Tx, keyPath string, expectedKey []byte) error {
	key, err := readAlertDedupeKey(keyPath)
	if err != nil {
		return err
	}
	if alertDedupeKeyFingerprint(key) != alertDedupeKeyFingerprint(expectedKey) {
		return errors.New("alert dedupe key changed during reset")
	}
	stored, ok, err := storageMetadataValueTx(ctx, tx, alertDedupeKeyFingerprintMetadata)
	if err != nil {
		return err
	}
	if !ok || stored != alertDedupeKeyFingerprint(key) {
		return errors.New("alert dedupe key fingerprint does not match key file")
	}
	return nil
}

func sqliteDSN(path string) string {
	if strings.Contains(path, "?") {
		return path + "&_foreign_keys=on&_busy_timeout=5000"
	}
	return path + "?_foreign_keys=on&_busy_timeout=5000"
}

func sqliteTableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func sqliteTableHasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func readAlertDedupeKey(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("insecure permissions on %s: alert dedupe key must be readable only by owner (chmod 0600)", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != 32 {
		return nil, errInvalidAlertDedupeKey
	}
	return key, nil
}

func readAlertDedupeKeyWithRetry(path string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		key, err := readAlertDedupeKey(path)
		if err == nil {
			return key, nil
		}
		lastErr = err
		if !errors.Is(err, errInvalidAlertDedupeKey) && !os.IsNotExist(err) {
			return nil, err
		}
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}
	return nil, lastErr
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open alert dedupe key dir: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync alert dedupe key dir: %w", err)
	}
	return nil
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
	if store == nil {
		return nil
	}
	store.alertStateMu.RLock()
	defer store.alertStateMu.RUnlock()
	if len(store.startupWarnings) == 0 {
		return nil
	}
	warnings := make([]string, len(store.startupWarnings))
	copy(warnings, store.startupWarnings)
	return warnings
}

func (store *Store) lockAlertState(message string) {
	if store == nil || strings.TrimSpace(message) == "" {
		return
	}
	store.alertStateMu.Lock()
	defer store.alertStateMu.Unlock()
	if store.alertStateDurableLockMsg == "" {
		store.alertStateDurableLockMsg = message
	}
	for _, warning := range store.startupWarnings {
		if warning == message {
			return
		}
	}
	store.startupWarnings = append(store.startupWarnings, message)
	if store.logger != nil {
		store.logger.Error(message)
	}
}

func (store *Store) beginHealthAlertCleanup() uint64 {
	store.alertStateMu.Lock()
	defer store.alertStateMu.Unlock()
	store.healthAlertCleanupAttempt++
	return store.healthAlertCleanupAttempt
}

// completeHealthAlertCleanup applies a cleanup result only if it is newer
// than every result already applied. This prevents an older success that
// resumes late from clearing a newer failure's retryable lock.
func (store *Store) completeHealthAlertCleanup(attempt uint64, message string) {
	store.alertStateMu.Lock()
	defer store.alertStateMu.Unlock()
	if attempt <= store.healthAlertCleanupCompletedAttempt {
		return
	}
	store.healthAlertCleanupCompletedAttempt = attempt
	store.alertStateCleanupLockMsg = message
	if message == "" {
		return
	}
	for _, warning := range store.startupWarnings {
		if warning == message {
			return
		}
	}
	store.startupWarnings = append(store.startupWarnings, message)
	if store.logger != nil {
		store.logger.Error(message)
	}
}

func (store *Store) isAlertStateLocked() bool {
	store.alertStateMu.RLock()
	defer store.alertStateMu.RUnlock()
	return store.alertStateDurableLockMsg != "" || store.alertStateCleanupLockMsg != ""
}

func (store *Store) alertStateLockMessage() string {
	store.alertStateMu.RLock()
	defer store.alertStateMu.RUnlock()
	if store.alertStateDurableLockMsg != "" {
		return store.alertStateDurableLockMsg
	}
	return store.alertStateCleanupLockMsg
}
