package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/example/gitops-dashboard/internal/routetarget"
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
	summaryMu          sync.RWMutex
	summaryVersion     uint64
	summaryCache       summaryCache
}

var ErrStatusNotFound = errors.New("status result not found")

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
