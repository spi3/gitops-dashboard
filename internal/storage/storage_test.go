package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example/gitops-dashboard/internal/config"
	"github.com/example/gitops-dashboard/internal/core"
)

func TestStorePersistsSummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repos := []config.RepositoryConfig{{Name: "repo", URL: "https://example.invalid/repo.git", DefaultRef: "main"}}
	if err := store.EnsureRepositories(ctx, repos); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	services := []core.Service{{
		ID:           "svc",
		Name:         "api",
		Repository:   "repo",
		SourceCommit: "abc123",
		SourcePath:   "prod/compose.yaml",
		Runtime:      "compose",
		Environment:  "production",
		Health:       core.HealthUnknown,
		Images:       []string{"example/api:v1.0.0"},
		Warnings:     []string{"missing healthcheck"},
	}}
	if err := store.FinishScan(ctx, scanID, "repo", "abc123", services, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc",
		Target:    "local",
		Health:    core.HealthHealthy,
		Message:   "running",
		CheckedAt: time.Date(2026, 6, 27, 16, 0, 0, 0, time.UTC),
		ObservedImages: []core.ObservedImage{
			core.NewObservedImage("local", "docker", "example/api:v1.0.0", "sha256:local", nil),
		},
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Repositories) != 1 || summary.Repositories[0].LastCommit != "abc123" {
		t.Fatalf("repositories = %#v", summary.Repositories)
	}
	if len(summary.Services) != 1 || summary.Services[0].Images[0] != "example/api:v1.0.0" {
		t.Fatalf("services = %#v", summary.Services)
	}
	if summary.Services[0].ImageVersionState != core.ImageVersionMatching {
		t.Fatalf("image version state = %s, want matching; checks=%#v", summary.Services[0].ImageVersionState, summary.Services[0].ImageVersionChecks)
	}
	if len(summary.Statuses[0].ObservedImages) != 1 || summary.Statuses[0].ObservedImages[0].Reference.Tag != "v1.0.0" {
		t.Fatalf("observed images = %#v, want persisted image metadata", summary.Statuses[0].ObservedImages)
	}
	if len(summary.Scans) != 1 || summary.Scans[0].CommitSHA != "abc123" {
		t.Fatalf("scans = %#v", summary.Scans)
	}
	if len(summary.Statuses) != 1 || summary.Statuses[0].Target != "local" {
		t.Fatalf("statuses = %#v", summary.Statuses)
	}
	if summary.Services[0].Health != core.HealthHealthy {
		t.Fatalf("service health = %s, want healthy", summary.Services[0].Health)
	}
	service := summary.Services[0]
	listFields := map[string][]string{
		"images":       service.Images,
		"ports":        service.Ports,
		"dependencies": service.Dependencies,
		"storage":      service.Storage,
		"exposure":     service.Exposure,
		"configRefs":   service.ConfigRefs,
		"warnings":     service.Warnings,
	}
	for name, values := range listFields {
		if values == nil {
			t.Fatalf("%s field is nil, want empty slice", name)
		}
	}
}

func TestHealthAlertProducerTransitionSequences(t *testing.T) {
	ctx := context.Background()
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "dashboard.db"), OpenOptions{HealthAlerts: HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, Cooldown: time.Hour, StabilitySamples: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	write := func(health core.HealthState) {
		t.Helper()
		if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "target", Health: health, CheckedAt: now}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
	}
	events := func() []AlertEvent {
		t.Helper()
		got, err := store.ListUndeliveredAlertEvents(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	write(core.HealthHealthy) // silent baseline
	write(core.HealthUnhealthy)
	if got := events(); len(got) != 0 {
		t.Fatalf("events after unstable edge = %#v", got)
	}
	write(core.HealthUnhealthy)
	if got := events(); len(got) != 1 || got[0].Kind != "health.transition" || got[0].OldState != "healthy" || got[0].NewState != "unhealthy" {
		t.Fatalf("failure event = %#v", got)
	}
	write(core.HealthUnhealthy)
	if got := events(); len(got) != 1 {
		t.Fatalf("steady state events = %#v", got)
	}
	write(core.HealthHealthy)
	write(core.HealthHealthy)
	if got := events(); len(got) != 2 || got[1].Kind != "health.recovery" || !strings.Contains(got[1].Reason, "recovered after 4m0s") {
		t.Fatalf("recovery event = %#v", got)
	}
}

func TestHealthAlertStateLegacyRebuildResetsInflightCandidate(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE health_alert_states (
  service_id TEXT NOT NULL PRIMARY KEY,
  stable_health TEXT NOT NULL,
  candidate_health TEXT NOT NULL,
  candidate_samples INTEGER NOT NULL,
  failure_started_at TEXT NOT NULL DEFAULT ''
);
INSERT INTO health_alert_states(service_id, stable_health, candidate_health, candidate_samples, failure_started_at)
VALUES('svc', 'healthy', 'unhealthy', 1, '2026-07-10T12:00:00Z');`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenWithOptions(path, OpenOptions{HealthAlerts: HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, Debounce: 2 * time.Minute, StabilitySamples: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var candidate string
	var samples int
	if err := store.db.QueryRowContext(ctx, `SELECT candidate_health, candidate_samples FROM health_alert_states WHERE service_id='svc'`).Scan(&candidate, &samples); err != nil {
		t.Fatal(err)
	}
	if candidate != string(core.HealthHealthy) || samples != 0 {
		t.Fatalf("rebuilt candidate = %q/%d, want healthy/0", candidate, samples)
	}
	base := time.Date(2026, 7, 10, 12, 1, 0, 0, time.UTC)
	for _, at := range []time.Time{base, base.Add(2 * time.Minute)} {
		if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "target", Health: core.HealthUnhealthy, CheckedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if events, err := store.ListUndeliveredAlertEvents(ctx, 0); err != nil || len(events) != 0 {
		t.Fatalf("legacy state without an incarnation must establish a silent fresh baseline: %#v, %v", events, err)
	}
}

func TestHealthAlertStateMalformedCanonicalSchemaLatches(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE health_alert_states (
  service_id TEXT NOT NULL PRIMARY KEY,
  stable_health TEXT NOT NULL,
  candidate_health TEXT NOT NULL,
  candidate_samples TEXT NOT NULL,
  candidate_started_at TEXT NOT NULL DEFAULT '',
  candidate_observation_id TEXT NOT NULL DEFAULT '',
  failure_started_at TEXT NOT NULL DEFAULT ''
);`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !store.isAlertStateLocked() || !strings.Contains(strings.Join(store.StartupWarnings(), "\n"), "health_alert_states has an incompatible schema") {
		t.Fatalf("malformed health state schema did not latch: %#v", store.StartupWarnings())
	}
}

func TestHealthAlertStateCanonicalSchemaWithTriggerLatches(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, healthAlertStatesCreateSQL+`
CREATE TRIGGER health_alert_states_abort_delete
BEFORE DELETE ON health_alert_states
BEGIN
  SELECT RAISE(ABORT, 'alert cleanup blocked');
END;`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !store.isAlertStateLocked() || !strings.Contains(strings.Join(store.StartupWarnings(), "\n"), "health_alert_states has unsupported triggers") {
		t.Fatalf("triggered health state schema did not latch: %#v", store.StartupWarnings())
	}
}

func TestCleanupHealthAlertStateTriggerKeepsCoreInventory(t *testing.T) {
	ctx := context.Background()
	service := core.Service{ID: "svc", Name: "service", Repository: "repo", SourcePath: "services.yml", Runtime: "configured", Kind: "Service", Health: core.HealthUnknown}

	for _, trigger := range []string{"ABORT", "ROLLBACK"} {
		trigger := trigger
		for _, tc := range []struct {
			name  string
			run   func(t *testing.T, store *Store)
			check func(t *testing.T, store *Store)
		}{
			{
				name: "replace configured services",
				run: func(t *testing.T, store *Store) {
					if err := store.ReplaceConfiguredServices(ctx, "repo", "services.yml", []core.Service{service}); err != nil {
						t.Fatal(err)
					}
					seedInventoryStatus(t, store)
					installHealthAlertDeleteTrigger(t, store, trigger)
					if err := store.ReplaceConfiguredServices(ctx, "repo", "services.yml", nil); err != nil {
						t.Fatal(err)
					}
				},
				check: assertInventoryStatusRemoved,
			},
			{
				name: "replace runtime services",
				run: func(t *testing.T, store *Store) {
					if err := store.ReplaceRuntimeServices(ctx, "repo", "services.yml", "configured", []core.Service{service}); err != nil {
						t.Fatal(err)
					}
					seedInventoryStatus(t, store)
					installHealthAlertDeleteTrigger(t, store, trigger)
					if err := store.ReplaceRuntimeServices(ctx, "repo", "services.yml", "configured", nil); err != nil {
						t.Fatal(err)
					}
				},
				check: assertInventoryStatusRemoved,
			},
			{
				name: "prune runtime services",
				run: func(t *testing.T, store *Store) {
					if err := store.ReplaceRuntimeServices(ctx, "repo", "services.yml", "configured", []core.Service{service}); err != nil {
						t.Fatal(err)
					}
					seedInventoryStatus(t, store)
					installHealthAlertDeleteTrigger(t, store, trigger)
					if err := store.PruneRuntimeServices(ctx, "configured", nil); err != nil {
						t.Fatal(err)
					}
				},
				check: assertInventoryStatusRemoved,
			},
			{
				name: "finish scan inventory replacement",
				run: func(t *testing.T, store *Store) {
					if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{Name: "repo", URL: "https://example.test/repo", DefaultRef: "main"}}); err != nil {
						t.Fatal(err)
					}
					scanID, err := store.StartScan(ctx, "repo")
					if err != nil {
						t.Fatal(err)
					}
					if err := store.FinishScan(ctx, scanID, "repo", "first", []core.Service{service}, nil); err != nil {
						t.Fatal(err)
					}
					seedInventoryStatus(t, store)
					installHealthAlertDeleteTrigger(t, store, trigger)
					scanID, err = store.StartScan(ctx, "repo")
					if err != nil {
						t.Fatal(err)
					}
					if err := store.FinishScan(ctx, scanID, "repo", "second", nil, nil); err != nil {
						t.Fatal(err)
					}
				},
				check: func(t *testing.T, store *Store) {
					t.Helper()
					if got := countRows(t, store, `SELECT COUNT(*) FROM scans WHERE commit_sha='second' AND status='ok'`); got != 1 {
						t.Fatalf("committed scan rows = %d, want 1", got)
					}
					if got := countRows(t, store, `SELECT COUNT(*) FROM repositories WHERE name='repo' AND last_commit='second' AND status='ok'`); got != 1 {
						t.Fatalf("committed repository rows = %d, want 1", got)
					}
				},
			},
		} {
			t.Run(trigger+"/"+tc.name, func(t *testing.T) {
				store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				tc.run(t, store)
				if !store.isAlertStateLocked() {
					t.Fatalf("alert state must latch after cleanup trigger %s", trigger)
				}
				if got := countRows(t, store, `SELECT COUNT(*) FROM services WHERE id='svc'`); got != 0 {
					t.Fatalf("core service rows = %d, want 0", got)
				}
				tc.check(t, store)
			})
		}
	}
}

func TestReusedServiceIDResetsAlertStateAfterFailedCleanup(t *testing.T) {
	for _, restartBeforeObservation := range []bool{false, true} {
		restartBeforeObservation := restartBeforeObservation
		name := "same process"
		if restartBeforeObservation {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "dashboard.db")
			service := core.Service{ID: "svc", Name: "service", Repository: "repo", SourcePath: "services.yml", Runtime: "configured", Kind: "Service", Health: core.HealthUnknown}
			options := OpenOptions{HealthAlerts: HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, StabilitySamples: 1}}
			store, err := OpenWithOptions(path, options)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ReplaceConfiguredServices(ctx, "repo", "services.yml", []core.Service{service}); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "target", Health: core.HealthUnhealthy, CheckedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`CREATE TRIGGER health_alert_states_abort_delete
BEFORE DELETE ON health_alert_states
BEGIN
  SELECT RAISE(ABORT, 'alert cleanup blocked');
END;`); err != nil {
				t.Fatal(err)
			}
			if err := store.ReplaceConfiguredServices(ctx, "repo", "services.yml", nil); err != nil {
				t.Fatal(err)
			}
			if !store.isAlertStateLocked() {
				t.Fatal("alert state must latch after forced cleanup failure")
			}
			// Replay the same nonempty inventory without repairing the failed
			// cleanup. The orphan is no longer selectable for deletion, so hygiene
			// succeeds but must not be required to reset producer state.
			if err := store.ReplaceConfiguredServices(ctx, "repo", "services.yml", []core.Service{service}); err != nil {
				t.Fatal(err)
			}
			if store.isAlertStateLocked() {
				t.Fatal("successful hygiene pass must resume alert observation")
			}
			if got := countRows(t, store, `SELECT COUNT(*) FROM health_alert_states WHERE service_id='svc'`); got != 1 {
				t.Fatalf("reused service ID retained stale alert state rows = %d, want 1 before producer replacement", got)
			}
			if restartBeforeObservation {
				// The trigger deliberately simulates failed hygiene above. Remove it
				// only before reopening, because startup correctly rejects unknown
				// alert-table triggers.
				if _, err := store.db.Exec(`DROP TRIGGER health_alert_states_abort_delete`); err != nil {
					t.Fatal(err)
				}
			}
			if restartBeforeObservation {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store, err = OpenWithOptions(path, options)
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "target", Health: core.HealthHealthy, CheckedAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			if got, err := store.ListUndeliveredAlertEvents(ctx, 0); err != nil {
				t.Fatal(err)
			} else if len(got) != 0 {
				t.Fatalf("reused service ID emitted an event instead of a silent fresh baseline: %#v", got)
			}
			if got := countRows(t, store, `SELECT COUNT(*) FROM health_alert_states WHERE service_id='svc' AND stable_health='healthy' AND service_incarnation=(SELECT incarnation FROM services WHERE id='svc')`); got != 1 {
				t.Fatalf("reused service ID fresh baseline rows = %d, want 1", got)
			}
		})
	}
}

func TestDurableAlertLockSurvivesRecoveredHealthAlertCleanup(t *testing.T) {
	ctx := context.Background()
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "dashboard.db"), OpenOptions{
		HealthAlerts: HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, StabilitySamples: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	service := core.Service{ID: "svc", Name: "service", Repository: "repo", SourcePath: "services.yml", Runtime: "configured", Kind: "Service", Health: core.HealthUnknown}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "services.yml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: service.ID, Target: "target", Health: core.HealthUnhealthy, CheckedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueAlertEvent(ctx, AlertEvent{Kind: "test", ServiceID: service.ID, NewState: "unhealthy", DedupeKey: "durable-cleanup-lock"}, []string{"test"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}

	store.lockAlertState("alert state locked: test durable migration failure; operator reset required")
	if _, err := store.db.Exec(`CREATE TRIGGER health_alert_states_abort_delete
BEFORE DELETE ON health_alert_states
BEGIN
  SELECT RAISE(ABORT, 'alert cleanup blocked');
END;`); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "services.yml", nil); err != nil {
		t.Fatal(err)
	}
	if !store.isAlertStateLocked() {
		t.Fatal("durable and cleanup components must lock alerting after cleanup failure")
	}
	// The replacement makes the failed-cleanup row non-orphaned, so this later
	// pass succeeds with zero rows deleted while the cleanup trigger remains.
	if err := store.ReplaceConfiguredServices(ctx, "repo", "services.yml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if !store.isAlertStateLocked() {
		t.Fatal("successful zero-row cleanup must not clear a durable alert lock")
	}

	if _, err := store.ClaimPendingAlertDeliveries(ctx, "worker-b", time.Minute, 1); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("claim after cleanup recovery err = %v, want ErrAlertStateLocked", err)
	}
	if _, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, ""); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("record after cleanup recovery err = %v, want ErrAlertStateLocked", err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: service.ID, Target: "target", Health: core.HealthHealthy, CheckedAt: time.Now().UTC().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events`); got != 1 {
		t.Fatalf("production after cleanup recovery created %d alert events, want only the pre-lock event", got)
	}
}

func TestConcurrentHealthAlertCleanupFailureRemainsLocked(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	olderPaused := make(chan struct{})
	releaseOlder := make(chan struct{})
	var pauseOlder sync.Once
	store.afterSuccessfulHealthAlertCleanup = func() {
		pauseOlder.Do(func() {
			close(olderPaused)
			<-releaseOlder
		})
	}

	olderDone := make(chan struct{})
	go func() {
		store.reconcileHealthAlertStates(ctx)
		close(olderDone)
	}()
	<-olderPaused

	if _, err := store.db.Exec(`INSERT INTO health_alert_states(service_id, stable_health, candidate_health, candidate_samples) VALUES('orphan', 'healthy', 'healthy', 0);
CREATE TRIGGER health_alert_states_abort_delete
BEFORE DELETE ON health_alert_states
BEGIN
  SELECT RAISE(ABORT, 'newer cleanup blocked');
END;`); err != nil {
		t.Fatal(err)
	}
	store.reconcileHealthAlertStates(ctx)
	if !store.isAlertStateLocked() {
		t.Fatal("newer cleanup failure must lock alerting before the older success is released")
	}
	close(releaseOlder)
	<-olderDone
	if !store.isAlertStateLocked() {
		t.Fatal("newer cleanup failure must remain locked after the older success completes")
	}
}

func assertInventoryStatusRemoved(t *testing.T, store *Store) {
	t.Helper()
	if got := countRows(t, store, `SELECT COUNT(*) FROM status_results WHERE service_id='svc'`); got != 0 {
		t.Fatalf("core status result rows = %d, want 0", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM status_history WHERE service_id='svc'`); got != 0 {
		t.Fatalf("core status history rows = %d, want 0", got)
	}
}

func seedInventoryStatus(t *testing.T, store *Store) {
	t.Helper()
	if err := store.UpsertStatus(context.Background(), core.StatusResult{
		ServiceID: "svc",
		Target:    "target",
		Health:    core.HealthHealthy,
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func installHealthAlertDeleteTrigger(t *testing.T, store *Store, trigger string) {
	t.Helper()
	if trigger != "ABORT" && trigger != "ROLLBACK" {
		t.Fatalf("unsupported trigger action %q", trigger)
	}
	if _, err := store.db.Exec(fmt.Sprintf(`INSERT INTO health_alert_states(service_id, stable_health, candidate_health, candidate_samples) VALUES('svc', 'healthy', 'healthy', 0);
CREATE TRIGGER health_alert_states_abort_delete
BEFORE DELETE ON health_alert_states
BEGIN
  SELECT RAISE(%s, 'alert cleanup blocked');
END;`, trigger)); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceConfiguredServicesRemovesCoreInventoryWhenAlertStateUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unprepare func(t *testing.T, path string)
	}{
		{
			name: "startup locked without health alert states table",
			unprepare: func(t *testing.T, path string) {
				t.Helper()
				db, err := sql.Open("sqlite3", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`DROP TABLE health_alert_states`); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(filepath.Dir(path), "alert-dedupe.key")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "incompatible health alert states table without service id",
			unprepare: func(t *testing.T, path string) {
				t.Helper()
				db, err := sql.Open("sqlite3", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`DROP TABLE health_alert_states; CREATE TABLE health_alert_states (state TEXT NOT NULL)`); err != nil {
					_ = db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "dashboard.db")
			store, err := OpenWithOptions(path, OpenOptions{HealthAlerts: HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, StabilitySamples: 1}})
			if err != nil {
				t.Fatal(err)
			}
			service := core.Service{ID: "svc", Name: "service", Repository: "configured", SourcePath: "services.yml", Runtime: "configured", Kind: "Service", Health: core.HealthUnknown}
			if err := store.ReplaceConfiguredServices(ctx, "configured", "services.yml", []core.Service{service}); err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			// Persist a keyed alert row so removing the key latches startup before
			// alert-only migrations can recreate the table.
			if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: service.ID, Target: "target", Health: core.HealthHealthy, CheckedAt: time.Now().UTC()}); err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: service.ID, Target: "target", Health: core.HealthUnhealthy, CheckedAt: time.Now().UTC().Add(time.Second)}); err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			tc.unprepare(t, path)

			store, err = OpenWithOptions(path, OpenOptions{HealthAlerts: HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, StabilitySamples: 1}})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if !store.isAlertStateLocked() {
				t.Fatal("alert state must remain latched")
			}
			if err := store.ReplaceConfiguredServices(ctx, "configured", "services.yml", nil); err != nil {
				t.Fatalf("remove configured service with unavailable alert state: %v", err)
			}
			if got := countRows(t, store, `SELECT COUNT(*) FROM services WHERE id='svc'`); got != 0 {
				t.Fatalf("core service rows = %d, want 0", got)
			}
		})
	}
}

func TestRouteReplacementObservesMergedHealthAfterCommit(t *testing.T) {
	ctx := context.Background()
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "dashboard.db"), OpenOptions{HealthAlerts: HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, StabilitySamples: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldRoute, newRoute := "https://old.example.test", "https://new.example.test"
	oldTarget, newTarget := "routes: "+oldRoute, "routes: "+newRoute
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: oldTarget, Health: core.HealthUnhealthy, CheckedAt: base}); err != nil {
		t.Fatal(err)
	}
	// Insert the collision directly: it is the replacement transaction, not a
	// monitor write, that must observe the resulting recovered rollup.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO status_results(service_id, target, health, message, checked_at) VALUES(?, ?, ?, '', ?)`, "svc", newTarget, string(core.HealthHealthy), base.Add(time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateRouteTargetReplacements(ctx, []RouteTargetReplacement{{ServiceID: "svc", OldRoute: oldRoute, NewRoute: newRoute}}, nil); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListUndeliveredAlertEvents(ctx, 0)
	if err != nil || len(events) != 1 || events[0].Kind != "health.recovery" {
		t.Fatalf("replacement recovery event = %#v, %v", events, err)
	}
}

func TestFinishScanRouteReplacementObservesMergedHealthAfterCommit(t *testing.T) {
	ctx := context.Background()
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "dashboard.db"), OpenOptions{HealthAlerts: HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, StabilitySamples: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{Name: "repo", URL: "https://example.test/repo", DefaultRef: "main"}}); err != nil {
		t.Fatal(err)
	}
	oldRoute, newRoute := "https://old.example.test", "https://new.example.test"
	service := core.Service{ID: "svc", Name: "svc", Repository: "repo", Runtime: "compose", Exposure: []string{oldRoute}}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "repo", "old", []core.Service{service}, nil); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "routes: " + oldRoute, Health: core.HealthUnhealthy, CheckedAt: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO status_results(service_id, target, health, message, checked_at) VALUES(?, ?, ?, '', ?)`, "svc", "routes: "+newRoute, string(core.HealthHealthy), base.Add(time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	service.Exposure = []string{newRoute}
	scanID, err = store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScanWithRouteTargetReplacements(ctx, scanID, "repo", "new", []core.Service{service}, nil, []RouteTargetReplacement{{ServiceID: "svc", OldRoute: oldRoute, NewRoute: newRoute}}, nil); err != nil {
		t.Fatal(err)
	}
	events, err := store.ListUndeliveredAlertEvents(ctx, 0)
	if err != nil || len(events) != 1 || events[0].Kind != "health.recovery" {
		t.Fatalf("scan replacement recovery event = %#v, %v", events, err)
	}
}

func TestHealthAlertProducerCooldownDisabledAndNotApplicable(t *testing.T) {
	ctx := context.Background()
	newStore := func(config HealthAlertProducerConfig) *Store {
		t.Helper()
		store, err := OpenWithOptions(filepath.Join(t.TempDir(), "dashboard.db"), OpenOptions{HealthAlerts: config})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	write := func(store *Store, health core.HealthState, at time.Time) {
		t.Helper()
		if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "target", Health: health, CheckedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	disabled := newStore(HealthAlertProducerConfig{})
	write(disabled, core.HealthHealthy, time.Now().UTC())
	write(disabled, core.HealthUnhealthy, time.Now().UTC().Add(time.Minute))
	if got, err := disabled.ListUndeliveredAlertEvents(ctx, 0); err != nil || len(got) != 0 {
		t.Fatalf("disabled events = %#v, %v", got, err)
	}
	store := newStore(HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, Cooldown: time.Hour, StabilitySamples: 1})
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	write(store, core.HealthHealthy, base)
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "ignored", Health: core.HealthNotApplicable, CheckedAt: base.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ListUndeliveredAlertEvents(ctx, 0); err != nil || len(got) != 0 {
		t.Fatalf("not-applicable events = %#v, %v", got, err)
	}
	write(store, core.HealthUnhealthy, base.Add(2*time.Minute))
	deliveries, err := store.ClaimPendingAlertDeliveries(ctx, "worker", time.Minute, 10)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %#v, %v", deliveries, err)
	}
	if _, err := store.RecordAlertDispatchResult(ctx, deliveries[0].Dispatch.ID, "worker", deliveries[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, ""); err != nil {
		t.Fatal(err)
	}
	write(store, core.HealthHealthy, base.Add(3*time.Minute))
	write(store, core.HealthUnhealthy, base.Add(4*time.Minute))
	if got, err := store.ListUndeliveredAlertEvents(ctx, 0); err != nil || len(got) != 1 {
		t.Fatalf("cooldown events = %#v, %v", got, err)
	}
}

func TestHealthAlertProducerDebounceRejectsReplayedObservationAndKeepsSilentFailureStart(t *testing.T) {
	ctx := context.Background()
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "dashboard.db"), OpenOptions{HealthAlerts: HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, Debounce: 2 * time.Minute, StabilitySamples: 2}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	write := func(health core.HealthState, at time.Time) {
		t.Helper()
		if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "target", Health: health, CheckedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	write(core.HealthHealthy, base)
	write(core.HealthUnhealthy, base.Add(time.Minute))
	write(core.HealthUnhealthy, base.Add(time.Minute)) // same aggregate identity: no second confirmation
	write(core.HealthUnhealthy, base.Add(2*time.Minute))
	if events, err := store.ListUndeliveredAlertEvents(ctx, 0); err != nil || len(events) != 0 {
		t.Fatalf("premature events = %#v, %v", events, err)
	}
	write(core.HealthUnhealthy, base.Add(3*time.Minute))
	events, err := store.ListUndeliveredAlertEvents(ctx, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("confirmed events = %#v, %v", events, err)
	}

	failing, err := OpenWithOptions(filepath.Join(t.TempDir(), "failing.db"), OpenOptions{HealthAlerts: HealthAlertProducerConfig{Enabled: true, Sinks: []string{"test"}, StabilitySamples: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer failing.Close()
	if err := failing.UpsertStatus(ctx, core.StatusResult{ServiceID: "failed-first", Target: "target", Health: core.HealthUnhealthy, CheckedAt: base}); err != nil {
		t.Fatal(err)
	}
	if err := failing.UpsertStatus(ctx, core.StatusResult{ServiceID: "failed-first", Target: "target", Health: core.HealthHealthy, CheckedAt: base.Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	events, err = failing.ListUndeliveredAlertEvents(ctx, 0)
	if err != nil || len(events) != 1 || !strings.Contains(events[0].Reason, "recovered after 5m0s") {
		t.Fatalf("silent failing baseline recovery = %#v, %v", events, err)
	}
}

func TestFinishScanMigratesRouteTargetIdentityAcrossState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{Name: "repo", URL: "https://example.test/repo", DefaultRef: "main"}}); err != nil {
		t.Fatal(err)
	}
	oldRoute, newRoute := "http://10.10.10.127", "http://10.10.10.127:8080"
	service := core.Service{ID: "svc", Name: "svc", Repository: "repo", Runtime: "compose", Exposure: []string{oldRoute}}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "repo", "old", []core.Service{service}, nil); err != nil {
		t.Fatal(err)
	}
	oldTarget, newTarget := "custom: "+oldRoute, "custom: "+newRoute
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO monitor_overrides(service_id,target,not_applicable,updated_at) VALUES(?,?,1,?)`, []any{"svc", oldTarget, "2026-07-10T10:00:00Z"}},
		{`INSERT INTO status_results(service_id,target,health,message,checked_at) VALUES(?,?, 'healthy','old',?)`, []any{"svc", oldTarget, "2026-07-10T09:00:00Z"}},
		{`INSERT INTO status_results(service_id,target,health,message,checked_at) VALUES(?,?, 'unhealthy','newer',?)`, []any{"svc", newTarget, "2026-07-10T11:00:00Z"}},
		{`INSERT INTO status_history(service_id,target,health,message,checked_at) VALUES(?,?, 'healthy','old history',?)`, []any{"svc", oldTarget, "2026-07-10T09:00:00Z"}},
	} {
		if _, err := store.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	service.Exposure = []string{newRoute}
	scanID, err = store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	err = store.FinishScanWithRouteTargetReplacements(ctx, scanID, "repo", "new", []core.Service{service}, nil,
		[]RouteTargetReplacement{{ServiceID: "svc", OldRoute: oldRoute, NewRoute: newRoute}}, []config.HTTPRouteTarget{{Name: "custom"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM monitor_overrides WHERE service_id='svc' AND target='custom: http://10.10.10.127'`); got != 0 {
		t.Fatalf("old override rows = %d, want 0", got)
	}
	var override, health, historyHealth, historyMessage string
	if err := store.db.QueryRowContext(ctx, `SELECT not_applicable, (SELECT health FROM status_results WHERE service_id='svc' AND target=?) FROM monitor_overrides WHERE service_id='svc' AND target=?`, newTarget, newTarget).Scan(&override, &health); err != nil {
		t.Fatal(err)
	}
	if override != "1" || health != string(core.HealthUnhealthy) {
		t.Fatalf("migrated override/status = %q/%q, want active/unhealthy payload preserved", override, health)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT health, message FROM status_history WHERE service_id='svc' AND target=?`, newTarget).Scan(&historyHealth, &historyMessage); err != nil {
		t.Fatal(err)
	}
	if historyHealth != string(core.HealthHealthy) || historyMessage != "old history" {
		t.Fatalf("migrated history = %q/%q, want byte-equivalent payload", historyHealth, historyMessage)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM status_history WHERE service_id='svc' AND target='custom: http://10.10.10.127:8080'`); got != 1 {
		t.Fatalf("retargeted history = %d, want 1", got)
	}
}

func TestRouteTargetMigrationRetargetsOnlyMutableAlertsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldTarget, newTarget := "routes: https://app.example.test", "routes: https://app.example.test:8443"
	for _, status := range []string{AlertEventStatusPending, AlertEventStatusFailed, AlertEventStatusDelivered} {
		key := "health:svc:" + oldTarget + ":" + status
		if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_events(kind,service_id,target,new_state,dedupe_key,dedupe_hash,created_at,created_at_ns,status) VALUES('health','svc',?,?,?,?,?,?,?)`, oldTarget, "unhealthy", key, store.alertDedupeHash(key), "2026-07-10T10:00:00Z", int64(1), status); err != nil {
			t.Fatal(err)
		}
	}
	replacement := []RouteTargetReplacement{{ServiceID: "svc", OldRoute: "https://app.example.test", NewRoute: "https://app.example.test:8443"}}
	if err := store.MigrateRouteTargetReplacements(ctx, replacement, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateRouteTargetReplacements(ctx, replacement, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT target, dedupe_key, status FROM alert_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for _, want := range []struct{ target, status string }{{newTarget, AlertEventStatusPending}, {newTarget, AlertEventStatusFailed}, {oldTarget, AlertEventStatusDelivered}} {
		if !rows.Next() {
			t.Fatal("missing alert row")
		}
		var target, key, status string
		if err := rows.Scan(&target, &key, &status); err != nil {
			t.Fatal(err)
		}
		if target != want.target || status != want.status {
			t.Fatalf("alert = %q/%q, want %q/%q", target, status, want.target, want.status)
		}
		if status != AlertEventStatusDelivered && !strings.Contains(key, newTarget) {
			t.Fatalf("mutable dedupe key = %q, want new identity", key)
		}
	}
}

func TestRouteTargetMigrationScopesAlertsToService(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldTarget, newTarget := "routes: https://app.example.test", "routes: https://app.example.test:8443"
	for _, serviceID := range []string{"migrated", "other"} {
		key := "health:" + serviceID + ":" + oldTarget
		if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_events(kind,service_id,target,new_state,dedupe_key,dedupe_hash,created_at,created_at_ns,status) VALUES('health',?,?,?,?,?,?,?,?)`, serviceID, oldTarget, "unhealthy", key, store.alertDedupeHash(key), "2026-07-10T10:00:00Z", int64(1), AlertEventStatusPending); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MigrateRouteTargetReplacements(ctx, []RouteTargetReplacement{{ServiceID: "migrated", OldRoute: "https://app.example.test", NewRoute: "https://app.example.test:8443"}}, nil); err != nil {
		t.Fatal(err)
	}
	for serviceID, wantTarget := range map[string]string{"migrated": newTarget, "other": oldTarget} {
		var target string
		if err := store.db.QueryRowContext(ctx, `SELECT target FROM alert_events WHERE service_id=?`, serviceID).Scan(&target); err != nil {
			t.Fatal(err)
		}
		if target != wantTarget {
			t.Fatalf("alert target for %s = %q, want %q", serviceID, target, wantTarget)
		}
	}
}

func TestRouteTargetMigrationDefersAlertsUntilAlertSafetyValidationRecovers(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		breakKey func(*Store)
	}{
		{name: "missing key", breakKey: func(store *Store) { store.alertDedupeKey = nil }},
		{name: "mismatched key", breakKey: func(store *Store) { store.alertDedupeKey = []byte("not-the-sidecar-key") }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "dashboard.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			oldTarget, newTarget := "routes: https://app.example.test", "routes: https://app.example.test:8443"
			key := "health:svc:" + oldTarget
			if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_events(kind,service_id,target,new_state,dedupe_key,dedupe_hash,created_at,created_at_ns,status) VALUES('health','svc',?,?,?,?,?,?,?)`, oldTarget, "unhealthy", key, store.alertDedupeHash(key), "2026-07-10T10:00:00Z", int64(1), AlertEventStatusPending); err != nil {
				t.Fatal(err)
			}
			var before string
			if err := store.db.QueryRowContext(ctx, `SELECT target || '|' || dedupe_key || '|' || dedupe_hash || '|' || status FROM alert_events WHERE service_id='svc'`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			scenario.breakKey(store)
			if err := store.MigrateRouteTargetReplacements(ctx, []RouteTargetReplacement{{ServiceID: "svc", OldRoute: "https://app.example.test", NewRoute: "https://app.example.test:8443"}}, nil); err != nil {
				t.Fatalf("route migration should not be held hostage by alert safety validation: %v", err)
			}
			var after string
			if err := store.db.QueryRowContext(ctx, `SELECT target || '|' || dedupe_key || '|' || dedupe_hash || '|' || status FROM alert_events WHERE service_id='svc'`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatalf("alert row changed while alert state was unsafe: got %q, want byte-identical %q", after, before)
			}
			if got := countRows(t, store, `SELECT COUNT(*) FROM deferred_alert_route_reconciliations WHERE service_id='svc' AND old_target='routes: https://app.example.test' AND new_target='routes: https://app.example.test:8443'`); got != 1 {
				t.Fatalf("deferred reconciliations = %d, want 1", got)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.MigrateRouteTargetReplacements(ctx, nil, nil); err != nil {
				t.Fatalf("apply deferred alert reconciliation: %v", err)
			}
			var target string
			if err := store.db.QueryRowContext(ctx, `SELECT target FROM alert_events WHERE service_id='svc'`).Scan(&target); err != nil {
				t.Fatal(err)
			}
			if target != newTarget {
				t.Fatalf("deferred alert target = %q, want %q", target, newTarget)
			}
		})
	}
}

func TestFinishScanRouteTargetMigrationReconcilesPendingAlertCollision(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{Name: "repo", URL: "https://example.test/repo", DefaultRef: "main"}}); err != nil {
		t.Fatal(err)
	}
	oldRoute, newRoute := "https://app.example.test", "https://app.example.test:8443"
	oldTarget, newTarget := "routes: "+oldRoute, "routes: "+newRoute
	service := core.Service{ID: "svc", Name: "svc", Repository: "repo", Runtime: "compose", Exposure: []string{oldRoute}}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "repo", "old", []core.Service{service}, nil); err != nil {
		t.Fatal(err)
	}
	for _, event := range []struct{ target, key, sink string }{
		{oldTarget, "health:svc:" + oldTarget, "old-sink"},
		{newTarget, "health:svc:" + newTarget, "new-sink"},
	} {
		result, err := store.db.ExecContext(ctx, `INSERT INTO alert_events(kind,service_id,target,new_state,dedupe_key,dedupe_hash,created_at,created_at_ns,status) VALUES('health','svc',?,?,?,?,?,?,?)`, event.target, "unhealthy", event.key, store.alertDedupeHash(event.key), "2026-07-10T10:00:00Z", int64(1), AlertEventStatusPending)
		if err != nil {
			t.Fatal(err)
		}
		eventID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_dispatches(event_id,sink,status,updated_at,updated_at_ns) VALUES(?,?,?, ?, ?)`, eventID, event.sink, AlertDispatchStatusPending, "2026-07-10T10:00:00Z", int64(1)); err != nil {
			t.Fatal(err)
		}
	}
	service.Exposure = []string{newRoute}
	scanID, err = store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScanWithRouteTargetReplacements(ctx, scanID, "repo", "new", []core.Service{service}, nil, []RouteTargetReplacement{{ServiceID: "svc", OldRoute: oldRoute, NewRoute: newRoute}}, nil); err != nil {
		t.Fatalf("FinishScan collision reconciliation: %v", err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE target='routes: https://app.example.test:8443' AND status='pending'`); got != 1 {
		t.Fatalf("pending destination events = %d, want 1", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_dispatches d JOIN alert_events e ON e.id=d.event_id WHERE e.target='routes: https://app.example.test:8443' AND e.status='pending' AND d.status='pending'`); got != 2 {
		t.Fatalf("keeper pending dispatches = %d, want merged 2", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE target='routes: https://app.example.test:8443' AND status='reset'`); got != 1 {
		t.Fatalf("terminalized duplicate events = %d, want 1", got)
	}
}

func TestRouteTargetMigrationReplaysDeferredAlertChainsToFixedPoint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// The second edge sorts before the first: a one-pass lexical replay would
	// discard a->c before a later z->a migration can reach it.
	oldRoute, middleRoute, newRoute := "https://z.example.test", "https://a.example.test", "https://c.example.test"
	oldTarget, newTarget := "routes: "+oldRoute, "routes: "+newRoute
	key := "health:svc:" + oldTarget
	if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_events(kind,service_id,target,new_state,dedupe_key,dedupe_hash,created_at,created_at_ns,status) VALUES('health','svc',?,?,?,?,?,?,?)`, oldTarget, "unhealthy", key, store.alertDedupeHash(key), "2026-07-10T10:00:00Z", int64(1), AlertEventStatusPending); err != nil {
		t.Fatal(err)
	}
	store.alertDedupeKey = nil
	replacements := []RouteTargetReplacement{
		{ServiceID: "svc", OldRoute: oldRoute, NewRoute: middleRoute},
		{ServiceID: "svc", OldRoute: middleRoute, NewRoute: newRoute},
	}
	if err := store.MigrateRouteTargetReplacements(ctx, replacements, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.MigrateRouteTargetReplacements(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	var target string
	if err := store.db.QueryRowContext(ctx, `SELECT target FROM alert_events WHERE service_id='svc'`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	if target != newTarget {
		t.Fatalf("deferred chain target = %q, want %q", target, newTarget)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM deferred_alert_route_reconciliations`); got != 0 {
		t.Fatalf("deferred chain rows = %d, want 0", got)
	}
}

func TestRouteTargetMigrationCollisionPreservesClaimedDestinationDispatch(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldRoute, newRoute := "https://old.example.test", "https://new.example.test"
	oldTarget, newTarget := "routes: "+oldRoute, "routes: "+newRoute
	var oldEvent, destinationEvent int64
	for _, event := range []struct {
		target string
		status string
	}{{oldTarget, AlertEventStatusPending}, {newTarget, AlertEventStatusPending}} {
		key := "health:svc:" + event.target
		result, err := store.db.ExecContext(ctx, `INSERT INTO alert_events(kind,service_id,target,new_state,dedupe_key,dedupe_hash,created_at,created_at_ns,status) VALUES('health','svc',?,?,?,?,?,?,?)`, event.target, "unhealthy", key, store.alertDedupeHash(key), "2026-07-10T10:00:00Z", int64(1), event.status)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if event.target == oldTarget {
			oldEvent = id
		} else {
			destinationEvent = id
		}
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_dispatches(event_id,sink,status,updated_at,updated_at_ns) VALUES(?, 'discord', ?, ?, ?)`, oldEvent, AlertDispatchStatusPending, "2026-07-10T10:00:00Z", int64(1)); err != nil {
		t.Fatal(err)
	}
	lease := time.Now().UTC().Add(time.Hour)
	result, err := store.db.ExecContext(ctx, `INSERT INTO alert_dispatches(event_id,sink,status,worker_id,claim_id,lease_expires_at,lease_expires_at_ns,updated_at,updated_at_ns) VALUES(?, 'discord', ?, 'worker-a', 'claim-a', ?, ?, ?, ?)`, destinationEvent, AlertDispatchStatusInFlight, lease.Format(time.RFC3339Nano), lease.UnixNano(), "2026-07-10T10:00:00Z", int64(1))
	if err != nil {
		t.Fatal(err)
	}
	claimedID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateRouteTargetReplacements(ctx, []RouteTargetReplacement{{ServiceID: "svc", OldRoute: oldRoute, NewRoute: newRoute}}, nil); err != nil {
		t.Fatal(err)
	}
	var eventID int64
	var status, workerID, claimID string
	if err := store.db.QueryRowContext(ctx, `SELECT event_id, status, worker_id, claim_id FROM alert_dispatches WHERE id=?`, claimedID).Scan(&eventID, &status, &workerID, &claimID); err != nil {
		t.Fatal(err)
	}
	if eventID != destinationEvent || status != AlertDispatchStatusInFlight || workerID != "worker-a" || claimID != "claim-a" {
		t.Fatalf("claimed dispatch = event=%d status=%q worker=%q claim=%q, want preserved destination claim", eventID, status, workerID, claimID)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM deferred_alert_route_reconciliations`); got != 1 {
		t.Fatalf("deferred collision edges = %d, want 1", got)
	}
}

func TestRouteTargetMigrationDefersClaimedCollisionWithoutRewritingTerminalKeeper(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldRoute, newRoute := "https://old.example.test", "https://new.example.test"
	oldTarget, newTarget := "routes: "+oldRoute, "routes: "+newRoute
	var keeperID, destinationID int64
	for _, target := range []string{oldTarget, newTarget} {
		key := "health:svc:" + target
		result, err := store.db.ExecContext(ctx, `INSERT INTO alert_events(kind,service_id,target,new_state,dedupe_key,dedupe_hash,created_at,created_at_ns,status) VALUES('health','svc',?,?,?,?,?,?,?)`, target, "unhealthy", key, store.alertDedupeHash(key), "2026-07-10T10:00:00Z", int64(1), AlertEventStatusPending)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if target == oldTarget {
			keeperID = id
		} else {
			destinationID = id
		}
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_dispatches(event_id,sink,status,attempts,last_error,delivered_at,delivered_at_ns,updated_at,updated_at_ns) VALUES(?, 'discord', 'delivered', 3, 'delivery audit', '2026-07-10T10:00:01Z', 2, '2026-07-10T10:00:01Z', 2), (?, 'webhook', 'pending', 0, '', NULL, NULL, '2026-07-10T10:00:02Z', 3)`, keeperID, keeperID); err != nil {
		t.Fatal(err)
	}
	lease := time.Now().UTC().Add(time.Hour)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_dispatches(event_id,sink,status,worker_id,claim_id,lease_expires_at,lease_expires_at_ns,attempts,last_error,updated_at,updated_at_ns) VALUES(?, 'discord', 'in_flight', 'worker-a', 'claim-a', ?, ?, 2, 'claimed delivery', '2026-07-10T10:00:03Z', 4)`, destinationID, lease.Format(time.RFC3339Nano), lease.UnixNano()); err != nil {
		t.Fatal(err)
	}
	var before string
	if err := store.db.QueryRowContext(ctx, `SELECT printf('%d|%d|%s|%s|%s|%d|%s|%s|%d|%s|%d', id,event_id,status,worker_id,claim_id,attempts,last_error,COALESCE(delivered_at,''),COALESCE(delivered_at_ns,0),updated_at,updated_at_ns) FROM alert_dispatches WHERE event_id=? AND sink='discord'`, keeperID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateRouteTargetReplacements(ctx, []RouteTargetReplacement{{ServiceID: "svc", OldRoute: oldRoute, NewRoute: newRoute}}, nil); err != nil {
		t.Fatalf("committed migration with claimed collision: %v", err)
	}
	var after string
	if err := store.db.QueryRowContext(ctx, `SELECT printf('%d|%d|%s|%s|%s|%d|%s|%s|%d|%s|%d', id,event_id,status,worker_id,claim_id,attempts,last_error,COALESCE(delivered_at,''),COALESCE(delivered_at_ns,0),updated_at,updated_at_ns) FROM alert_dispatches WHERE event_id=? AND sink='discord'`, keeperID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("terminal keeper dispatch changed: got %q, want byte-identical %q", after, before)
	}
	var deferred int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deferred_alert_route_reconciliations WHERE service_id='svc' AND old_target=? AND new_target=?`, oldTarget, newTarget).Scan(&deferred); err != nil {
		t.Fatal(err)
	}
	if deferred != 1 {
		t.Fatalf("deferred collision edges = %d, want 1", deferred)
	}
}

func TestFinishScanDefersDeadLetteredKeeperSameSinkPendingDestination(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{Name: "repo", URL: "https://example.test/repo", DefaultRef: "main"}}); err != nil {
		t.Fatal(err)
	}
	oldRoute, newRoute := "https://old.example.test", "https://new.example.test"
	oldTarget, newTarget := "routes: "+oldRoute, "routes: "+newRoute
	service := core.Service{ID: "svc", Name: "svc", Repository: "repo", Runtime: "compose", Exposure: []string{oldRoute}}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "repo", "old", []core.Service{service}, nil); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, target := range []string{oldTarget, newTarget} {
		key := "health:svc:" + target
		result, err := store.db.ExecContext(ctx, `INSERT INTO alert_events(kind,service_id,target,new_state,dedupe_key,dedupe_hash,created_at,created_at_ns,status) VALUES('health','svc',?,?,?,?,?,?,?)`, target, "unhealthy", key, store.alertDedupeHash(key), "2026-07-10T10:00:00Z", int64(1), AlertEventStatusPending)
		if err != nil {
			t.Fatal(err)
		}
		ids[target], err = result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_dispatches(event_id,sink,status,attempts,last_error,updated_at,updated_at_ns) VALUES(?, 'discord', 'dead_lettered', 2, 'terminal diagnostic', '2026-07-10T10:00:01Z', 2), (?, 'webhook', 'pending', 0, '', '2026-07-10T10:00:02Z', 3), (?, 'discord', 'pending', 0, '', '2026-07-10T10:00:03Z', 4)`, ids[oldTarget], ids[oldTarget], ids[newTarget]); err != nil {
		t.Fatal(err)
	}
	terminalRow := func(eventID int64, sink string) string {
		var row string
		if err := store.db.QueryRowContext(ctx, `SELECT printf('%d|%d|%s|%s|%d|%s|%s|%d|%s|%d', id,event_id,status,worker_id,attempts,last_error,COALESCE(delivered_at,''),COALESCE(delivered_at_ns,0),updated_at,updated_at_ns) FROM alert_dispatches WHERE event_id=? AND sink=?`, eventID, sink).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	deadLetteredBefore := terminalRow(ids[oldTarget], "discord")
	service.Exposure = []string{newRoute}
	scanID, err = store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScanWithRouteTargetReplacements(ctx, scanID, "repo", "new", []core.Service{service}, nil, []RouteTargetReplacement{{ServiceID: "svc", OldRoute: oldRoute, NewRoute: newRoute}}, nil); err != nil {
		t.Fatalf("scan with dead-lettered keeper collision: %v", err)
	}
	if got := terminalRow(ids[oldTarget], "discord"); got != deadLetteredBefore {
		t.Fatalf("dead-lettered keeper changed: got %q, want byte-identical %q", got, deadLetteredBefore)
	}
	var pendingDestination int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_dispatches WHERE event_id=? AND sink='discord' AND status='pending'`, ids[newTarget]).Scan(&pendingDestination); err != nil {
		t.Fatal(err)
	}
	if pendingDestination != 1 {
		t.Fatalf("pending destination dispatches = %d, want claimable 1", pendingDestination)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM deferred_alert_route_reconciliations`); got != 1 {
		t.Fatalf("deferred collision edges = %d, want 1", got)
	}
	deliveries, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 2 {
		t.Fatalf("claimable deliveries = %d, want keeper webhook plus destination discord", len(deliveries))
	}
	for _, delivery := range deliveries {
		if _, err := store.RecordAlertDispatchResult(ctx, delivery.Dispatch.ID, "worker-a", delivery.Dispatch.ClaimID, AlertDispatchStatusDelivered, ""); err != nil {
			t.Fatal(err)
		}
	}
	destinationDeliveredBefore := terminalRow(ids[newTarget], "discord")
	scanID, err = store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "repo", "settled", []core.Service{service}, nil); err != nil {
		t.Fatalf("settled reconciliation scan: %v", err)
	}
	if got := terminalRow(ids[oldTarget], "discord"); got != deadLetteredBefore {
		t.Fatalf("dead-lettered keeper changed after reconciliation: got %q, want %q", got, deadLetteredBefore)
	}
	if got := terminalRow(ids[newTarget], "discord"); got != destinationDeliveredBefore {
		t.Fatalf("delivered destination changed after reconciliation: got %q, want %q", got, destinationDeliveredBefore)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM deferred_alert_route_reconciliations`); got != 0 {
		t.Fatalf("deferred collision edges = %d, want 0 after terminal reconciliation", got)
	}
}

func TestFinishScanDefersDualClaimedSameSinkCollisionUntilTerminal(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{Name: "repo", URL: "https://example.test/repo", DefaultRef: "main"}}); err != nil {
		t.Fatal(err)
	}
	oldRoute, newRoute := "https://old.example.test", "https://new.example.test"
	oldTarget, newTarget := "routes: "+oldRoute, "routes: "+newRoute
	service := core.Service{ID: "svc", Name: "svc", Repository: "repo", Runtime: "compose", Exposure: []string{oldRoute}}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "repo", "old", []core.Service{service}, nil); err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, target := range []string{oldTarget, newTarget} {
		key := "health:svc:" + target
		result, err := store.db.ExecContext(ctx, `INSERT INTO alert_events(kind,service_id,target,new_state,dedupe_key,dedupe_hash,created_at,created_at_ns,status) VALUES('health','svc',?,?,?,?,?,?,?)`, target, "unhealthy", key, store.alertDedupeHash(key), "2026-07-10T10:00:00Z", int64(1), AlertEventStatusPending)
		if err != nil {
			t.Fatal(err)
		}
		ids[target], err = result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
	}
	lease := time.Now().UTC().Add(time.Hour)
	for target, claimID := range map[string]string{oldTarget: "claim-old", newTarget: "claim-new"} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_dispatches(event_id,sink,status,worker_id,claim_id,lease_expires_at,lease_expires_at_ns,updated_at,updated_at_ns) VALUES(?, 'discord', 'in_flight', 'worker-a', ?, ?, ?, '2026-07-10T10:00:00Z', 1)`, ids[target], claimID, lease.Format(time.RFC3339Nano), lease.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	service.Exposure = []string{newRoute}
	scanID, err = store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScanWithRouteTargetReplacements(ctx, scanID, "repo", "new", []core.Service{service}, nil, []RouteTargetReplacement{{ServiceID: "svc", OldRoute: oldRoute, NewRoute: newRoute}}, nil); err != nil {
		t.Fatalf("scan with dual claimed collision: %v", err)
	}
	for _, target := range []string{oldTarget, newTarget} {
		var targetAfter, hash, status, claimID string
		if err := store.db.QueryRowContext(ctx, `SELECT e.target,e.dedupe_hash,d.status,d.claim_id FROM alert_events e JOIN alert_dispatches d ON d.event_id=e.id WHERE e.id=?`, ids[target]).Scan(&targetAfter, &hash, &status, &claimID); err != nil {
			t.Fatal(err)
		}
		if targetAfter != target || hash != store.alertDedupeHash("health:svc:"+target) || status != AlertDispatchStatusInFlight || claimID == "" {
			t.Fatalf("claimed event after deferred scan = target:%q hash:%q status:%q claim:%q, want untouched", targetAfter, hash, status, claimID)
		}
	}
	if deliveries, err := store.ClaimPendingAlertDeliveries(ctx, "worker-b", time.Minute, 10); err != nil || len(deliveries) != 0 {
		t.Fatalf("claim after deferred scan = %#v, err=%v; want no newly claimable dispatch", deliveries, err)
	}
	var destinationDispatchID int64
	if err := store.db.QueryRowContext(ctx, `SELECT id FROM alert_dispatches WHERE event_id=?`, ids[newTarget]).Scan(&destinationDispatchID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordAlertDispatchResult(ctx, destinationDispatchID, "worker-a", "claim-new", AlertDispatchStatusDelivered, ""); err != nil {
		t.Fatalf("terminalize destination claim: %v", err)
	}
	scanID, err = store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "repo", "reconciled", []core.Service{service}, nil); err != nil {
		t.Fatalf("fixed-point reconciliation scan: %v", err)
	}
	var reconciled int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alert_events WHERE id=? AND target=? AND status='pending'`, ids[oldTarget], newTarget).Scan(&reconciled); err != nil {
		t.Fatal(err)
	}
	if reconciled != 1 {
		t.Fatalf("reconciled keeper events = %d, want 1", reconciled)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM deferred_alert_route_reconciliations`); got != 0 {
		t.Fatalf("deferred collision edges = %d, want 0 after terminal reconciliation", got)
	}
}

func TestRouteTargetExclusionRetainsOnlyAmbiguousStaleTarget(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldTarget, otherTarget := "routes: https://old.example.test", "routes: https://gone.example.test"
	for _, target := range []string{oldTarget, otherTarget} {
		if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: target, Health: core.HealthHealthy, Message: target, CheckedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetRouteTargetExclusions(ctx, []RouteTargetExclusion{{ServiceID: "svc", OldRoute: "https://old.example.test"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneStatusTargetsFromKnown(ctx, "svc", "routes", "routes: ", nil, map[string]struct{}{oldTarget: {}, otherTarget: {}}, false); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM status_history WHERE service_id='svc' AND target='routes: https://old.example.test'`); got != 1 {
		t.Fatalf("ambiguous history rows = %d, want 1", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM status_history WHERE service_id='svc' AND target='routes: https://gone.example.test'`); got != 0 {
		t.Fatalf("ordinary stale history rows = %d, want 0", got)
	}
}

func TestFinishScanReconcilesResolvedRouteTargetExclusion(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{Name: "repo", URL: "https://example.test/repo", DefaultRef: "main"}}); err != nil {
		t.Fatal(err)
	}
	oldRoute, keptRoute, droppedRoute := "https://app.example.test", "https://app.example.test:8443", "https://app.example.test:9443"
	oldTarget, keptTarget, staleTarget := "routes: "+oldRoute, "routes: "+keptRoute, "routes: https://gone.example.test"
	service := core.Service{ID: "svc", Name: "svc", Repository: "repo", Runtime: "compose", Exposure: []string{oldRoute}}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "repo", "one", []core.Service{service}, nil); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{oldTarget, staleTarget} {
		if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: target, Health: core.HealthHealthy, Message: "observed " + target, CheckedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	service.Exposure = []string{keptRoute, droppedRoute}
	scanID, err = store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScanWithRouteTargetChanges(ctx, scanID, "repo", "two", []core.Service{service}, nil, nil, []RouteTargetExclusion{{ServiceID: "svc", OldRoute: oldRoute}}, nil); err != nil {
		t.Fatal(err)
	}
	service.Exposure = []string{keptRoute}
	scanID, err = store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScanWithRouteTargetChanges(ctx, scanID, "repo", "three", []core.Service{service}, nil, []RouteTargetReplacement{{ServiceID: "svc", OldRoute: oldRoute, NewRoute: keptRoute}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM route_target_exclusions WHERE service_id='svc' AND old_route='https://app.example.test'`); got != 0 {
		t.Fatalf("resolved exclusions = %d, want 0", got)
	}
	if err := store.PruneStatusTargetsFromKnown(ctx, "svc", "routes", "routes: ", []string{keptTarget}, map[string]struct{}{keptTarget: {}, staleTarget: {}}, false); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM status_history WHERE service_id='svc' AND target='routes: https://gone.example.test'`); got != 0 {
		t.Fatalf("stale history rows = %d, want 0", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM status_history WHERE service_id='svc' AND target='routes: https://app.example.test:8443'`); got != 1 {
		t.Fatalf("migrated history rows = %d, want 1", got)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Services) != 1 || summary.Services[0].Health != core.HealthHealthy {
		t.Fatalf("aggregate service health = %#v, want healthy", summary.Services)
	}
}

func TestSummaryReturnsDecodeErrorWithRowContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "repo",
		URL:        "https://example.invalid/repo.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, scanID, "repo", "abc123", []core.Service{{
		ID:           "svc",
		Name:         "api",
		Repository:   "repo",
		SourceCommit: "abc123",
		SourcePath:   "prod/compose.yaml",
		Runtime:      "compose",
		Health:       core.HealthUnknown,
		Images:       []string{"example/api:v1"},
	}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, "{", "svc"); err != nil {
		t.Fatal(err)
	}

	_, err = store.Summary(ctx)
	if err == nil {
		t.Fatal("Summary returned nil error, want decode failure")
	}
	for _, want := range []string{"decode persisted JSON", "table=services", "key=svc", "column=images_json"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
}

func TestProbePersistedJSONRotatesBoundedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "repo",
		URL:        "https://example.invalid/repo.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	services := []core.Service{
		{
			ID:           "svc-a",
			Name:         "api-a",
			Repository:   "repo",
			SourceCommit: "abc123",
			SourcePath:   "prod/compose.yaml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
			Images:       []string{"example/api-a:v1"},
		},
		{
			ID:           "svc-b",
			Name:         "api-b",
			Repository:   "repo",
			SourceCommit: "abc123",
			SourcePath:   "prod/compose.yaml",
			Runtime:      "compose",
			Health:       core.HealthUnknown,
			Images:       []string{"example/api-b:v1"},
		},
	}
	if err := store.FinishScan(ctx, scanID, "repo", "abc123", services, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE services SET images_json=? WHERE id=?`, "{", "svc-b"); err != nil {
		t.Fatal(err)
	}

	if err := store.ProbePersistedJSON(ctx, 1); err != nil {
		t.Fatalf("one-row persisted JSON probe error = %v, want bounded sample to skip second row", err)
	}
	err = store.ProbePersistedJSON(ctx, 1)
	if err == nil {
		t.Fatal("rotated one-row persisted JSON probe returned nil error, want sampled decode failure")
	}
	for _, want := range []string{"decode persisted JSON", "table=services", "key=svc-b", "column=images_json"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
}

func TestOpenSkipsCorruptPersistedJSONDuringStartupScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, migrations); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO services(
	id, name, repository, source_commit, source_path, runtime, kind, namespace,
	compose_project, resource_name, environment, health, images_json, ports_json, dependencies_json,
	storage_json, exposure_json, config_json, warnings_json
) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, "svc-corrupt", "api", "repo", "abc123", "prod/compose.yaml", "compose", "", "", "", "", "production", string(core.HealthUnknown), "{", "[]", "[]", "[]", "[]", "[]", "[]"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO agents(target, last_seen_at, status_json)
VALUES(?, ?, ?)
`, "agent-corrupt", time.Now().UTC().Format(time.RFC3339), "{"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	credentialedTarget := "routes: https://user:P%40ss@app.example.test:443"
	if _, err := db.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at, observed_images_json)
VALUES(?, ?, ?, ?, ?, ?)
`, "svc-corrupt", credentialedTarget, string(core.HealthError), "failed", time.Now().UTC().Format(time.RFC3339), "{"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	store, err := OpenWithLogger(dbPath, slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("OpenWithLogger failed on corrupt persisted JSON: %v", err)
	}
	defer store.Close()
	warnings := store.StartupWarnings()
	if len(warnings) != 3 {
		t.Fatalf("startup warnings = %#v", warnings)
	}
	for _, want := range []string{
		"startup validation skipped 1 corrupt services.images_json row",
		"startup validation skipped 1 corrupt status_results.observed_images_json row",
		"startup validation skipped 1 corrupt agents.status_json row",
	} {
		if !strings.Contains(strings.Join(warnings, "\n"), want) {
			t.Fatalf("startup warnings = %#v, want %q", warnings, want)
		}
	}
	logText := logs.String()
	for _, want := range []string{
		"skipping corrupt persisted JSON during startup validation",
		"service_id=svc-corrupt",
		"column=images_json",
		"key=svc-corrupt/routes: https://app.example.test:443",
		"column=observed_images_json",
		"key=agent-corrupt",
		"column=status_json",
		"count=1",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("logs = %q, want %q", logText, want)
		}
	}
	for _, leaked := range []string{"user:P%40ss", "P%40ss", "https://user:"} {
		if strings.Contains(logText, leaked) || strings.Contains(strings.Join(warnings, "\n"), leaked) {
			t.Fatalf("startup validation leaked credential fragment %q\nlogs=%q\nwarnings=%#v", leaked, logText, warnings)
		}
	}
	if _, err := store.Summary(ctx); err == nil || !strings.Contains(err.Error(), "column=images_json") {
		t.Fatalf("Summary error = %v, want strict images_json decode failure", err)
	}
	if _, err := store.Agents(ctx); err == nil || !strings.Contains(err.Error(), "column=status_json") {
		t.Fatalf("Agents error = %v, want strict status_json decode failure", err)
	}
}

func TestStoreRedactsKnownTokensWhenPersistingScanAndStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "storage-secret-token-t008"
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.AddRedactionValues(token)
	repos := []config.RepositoryConfig{{Name: "repo", URL: "https://deploy:" + token + "@example.com/repo.git", DefaultRef: "main"}}
	if err := store.EnsureRepositories(ctx, repos); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	scanErr := errors.New("git clone https://x-access-token:" + token + "@example.com/org/repo.git failed with " + token)
	if err := store.FinishScan(ctx, scanID, "repo", "", nil, scanErr); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc",
		Target:    "local",
		Health:    core.HealthError,
		Message:   "git fetch https://user:" + token + "@example.com/org/repo.git failed with " + token,
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT COALESCE(url, '') FROM repositories`,
		`SELECT COALESCE(error, '') FROM scans`,
		`SELECT COALESCE(error, '') FROM repositories`,
		`SELECT COALESCE(message, '') FROM status_results`,
		`SELECT COALESCE(message, '') FROM status_history`,
	} {
		rows, err := store.db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			assertRedacted(t, value, token)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range summary.Repositories {
		assertRedacted(t, repo.URL, token)
		assertRedacted(t, repo.Error, token)
	}
	for _, scan := range summary.Scans {
		assertRedacted(t, scan.Error, token)
	}
	for _, status := range summary.Statuses {
		assertRedacted(t, status.Message, token)
	}
}

func TestStoreAddRedactionValuesDedupesRawValues(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	token := "storage/secret-token"
	store.AddRedactionValues(token)
	firstSize := len(store.redactionTokens)
	if firstSize == 0 {
		t.Fatal("redaction token set is empty")
	}
	store.AddRedactionValues(token)
	if len(store.redactionTokens) != firstSize {
		t.Fatalf("redaction token set size = %d, want %d after duplicate add", len(store.redactionTokens), firstSize)
	}
	for _, value := range store.redactionTokens {
		if strings.Contains(value, "%252F") {
			t.Fatalf("redaction token was re-escaped: %#v", store.redactionTokens)
		}
	}
}

func TestRedactPersistedSensitiveValuesCompactsDatabaseFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "physical-secret-token-t008"
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store.AddRedactionValues(token)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO repositories(name, url, default_ref, status, error)
VALUES(?, ?, 'main', 'error', ?)
`, "repo", "https://deploy:"+token+"@example.com/repo.git", "clone failed: "+token); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO scans(repository, status, started_at, error)
VALUES(?, 'error', ?, ?)
`, "repo", time.Now().UTC().Format(time.RFC3339), "scan failed: "+token); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if !sqliteFilesContainToken(t, dbPath, token) {
		t.Fatal("test setup did not write token bytes into sqlite files")
	}
	if err := store.RedactPersistedSensitiveValues(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if sqliteFilesContainToken(t, dbPath, token) {
		t.Fatal("sqlite files still contain token bytes after persisted redaction cleanup")
	}
}

func TestCanonicalizeRouteCredentialsCompactsPersistedPages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	secret := "route-password-t026"
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := "routes: https://user:" + secret + "@app.example.test"
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at, observed_images_json)
VALUES(?, ?, 'error', 'failed', ?, '[]')`, "svc-route", target, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if !sqliteFilesContainToken(t, dbPath, secret) {
		t.Fatal("test setup did not write credential into sqlite files")
	}
	if err := store.CanonicalizeHTTPRouteTargets(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if sqliteFilesContainToken(t, dbPath, secret) {
		t.Fatal("credential remains in sqlite files after canonicalization compaction")
	}
}

func TestOpenDegradesWhenAlertMetadataPrimaryKeyIsComposite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE storage_metadata (key TEXT, value TEXT NOT NULL, PRIMARY KEY(key, value))`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO storage_metadata(key, value) VALUES ('duplicate-key', 'first'), ('duplicate-key', 'second')`); err != nil {
		t.Fatalf("insert duplicate composite-primary-key metadata rows: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed for alert-only metadata key-constraint skew: %v", err)
	}
	defer store.Close()
	if !strings.Contains(strings.Join(store.StartupWarnings(), "\n"), "alert state locked") {
		t.Fatalf("warnings = %#v, want alert-state lock", store.StartupWarnings())
	}
	if _, _, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:composite-metadata-key",
	}, []string{"discord"}, time.Hour); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("enqueue err = %v, want ErrAlertStateLocked", err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM storage_metadata_alert_legacy WHERE key='duplicate-key'`); got != 2 {
		t.Fatalf("quarantined duplicate metadata rows = %d, want 2", got)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO repositories(name, url, default_ref) VALUES ('core-still-available', 'https://example.test/repo', 'main')`); err != nil {
		t.Fatalf("core repository write after metadata skew: %v", err)
	}
}

func TestCreateAlertDedupeKeySurfacesPartialSidecarCleanupFailures(t *testing.T) {
	writeErr := errors.New("injected key write failure")
	closeErr := errors.New("injected key close failure")
	removeErr := errors.New("injected partial-sidecar removal failure")
	file := &failingAlertDedupeKeyFile{writeErr: writeErr, closeErr: closeErr}
	removed := false
	_, err := createAlertDedupeKeyWithFile("partial-alert-dedupe.key", func(string, int, os.FileMode) (alertDedupeKeyFile, error) {
		return file, nil
	}, func(string) error {
		removed = true
		return removeErr
	})
	if !errors.Is(err, writeErr) || !errors.Is(err, closeErr) || !errors.Is(err, removeErr) {
		t.Fatalf("create error = %v, want joined write, close, and removal errors", err)
	}
	if !removed {
		t.Fatal("partial sidecar removal was not attempted")
	}
}

func TestAlertRedactionFailureDoesNotRollbackCoreRedaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secret := "core-secret-survives-alert-error"
	if _, err := store.db.ExecContext(ctx, `INSERT INTO repositories(name, url, default_ref) VALUES ('core-redaction', ?, 'main')`, "https://"+secret+"@example.test/repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE alert_events; CREATE TABLE alert_events (id TEXT PRIMARY KEY, reason TEXT NOT NULL) WITHOUT ROWID; INSERT INTO alert_events(id, reason) VALUES ('one', ?);`, secret); err != nil {
		t.Fatal(err)
	}
	store.AddRedactionValues(secret)
	if _, err := store.redactCorePersistedSensitiveValues(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.redactAlertPersistedSensitiveValues(ctx); err == nil {
		t.Fatal("alert redaction succeeded, want rowid failure")
	}
	var url string
	if err := store.db.QueryRowContext(ctx, `SELECT url FROM repositories WHERE name='core-redaction'`).Scan(&url); err != nil {
		t.Fatal(err)
	}
	assertRedacted(t, url, secret)
}

func TestReplaceConfiguredServicesPreservesStableStatusHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "host-1",
		Name:        "serenity",
		Repository:  "ping/hosts",
		SourcePath:  "hosts.yml",
		Runtime:     "host",
		Kind:        "Host",
		Health:      core.HealthUnknown,
		Environment: "infrastructure",
	}
	if err := store.ReplaceConfiguredServices(ctx, "ping/hosts", "hosts.yml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "host-1",
		Target:    "ping/hosts",
		Health:    core.HealthHealthy,
		Message:   "pong",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceConfiguredServices(ctx, "ping/hosts", "hosts.yml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='host-1'"); got != 1 {
		t.Fatalf("stable host history rows = %d, want 1", got)
	}
	if err := store.ReplaceConfiguredServices(ctx, "ping/hosts", "hosts.yml", nil); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='host-1'"); got != 0 {
		t.Fatalf("removed host history rows = %d, want 0", got)
	}
}

func TestReplaceRuntimeServicesPreservesOtherRepositoryServices(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "kube",
		URL:        "https://example.invalid/kube.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "kube")
	if err != nil {
		t.Fatal(err)
	}
	composeService := core.Service{
		ID:           "compose-1",
		Name:         "web",
		Repository:   "kube",
		SourceCommit: "abc123",
		SourcePath:   "docker_files/web/docker-compose.yml",
		Runtime:      "compose",
		Kind:         "Service",
		Health:       core.HealthUnknown,
	}
	if err := store.FinishScan(ctx, scanID, "kube", "abc123", []core.Service{composeService}, nil); err != nil {
		t.Fatal(err)
	}
	hostService := core.Service{
		ID:           "host-1",
		Name:         "serenity",
		Repository:   "kube",
		SourceCommit: "abc123",
		SourcePath:   "infrastructure/inventory/hosts.yml",
		Runtime:      "host",
		Kind:         "Host",
		Health:       core.HealthUnknown,
	}
	if err := store.ReplaceRuntimeServices(ctx, "kube", "infrastructure/inventory/hosts.yml", "host", []core.Service{hostService}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "host-1",
		Target:    "homelab",
		Health:    core.HealthHealthy,
		Message:   "pong",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRuntimeServices(ctx, "kube", "infrastructure/inventory/hosts.yml", "host", nil); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Services) != 1 || summary.Services[0].ID != "compose-1" {
		t.Fatalf("services = %#v, want only compose service preserved", summary.Services)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='host-1'"); got != 0 {
		t.Fatalf("removed host history rows = %d, want 0", got)
	}
}

func TestPruneRuntimeServicesRemovesStaleSources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.EnsureRepositories(ctx, []config.RepositoryConfig{{
		Name:       "kube",
		URL:        "https://example.invalid/kube.git",
		DefaultRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}
	scanID, err := store.StartScan(ctx, "kube")
	if err != nil {
		t.Fatal(err)
	}
	keptHost := core.Service{
		ID:           "host-kept",
		Name:         "serenity",
		Repository:   "kube",
		SourceCommit: "abc123",
		SourcePath:   "infrastructure/inventory/hosts.yml",
		Runtime:      "host",
		Kind:         "Host",
		Health:       core.HealthUnknown,
	}
	composeService := core.Service{
		ID:           "compose-1",
		Name:         "gitops-dashboard",
		Repository:   "kube",
		SourceCommit: "abc123",
		SourcePath:   "docker_files/serenity/gitops-dashboard/docker-compose.yml",
		Runtime:      "compose",
		Kind:         "Service",
		Health:       core.HealthUnknown,
	}
	if err := store.FinishScan(ctx, scanID, "kube", "abc123", []core.Service{keptHost, composeService}, nil); err != nil {
		t.Fatal(err)
	}
	staleHost := core.Service{
		ID:           "host-stale",
		Name:         "serenity",
		Repository:   "ping/ansible-hosts",
		SourceCommit: "",
		SourcePath:   "/ansible/hosts.yml",
		Runtime:      "host",
		Kind:         "Host",
		Health:       core.HealthUnknown,
	}
	if err := store.ReplaceRuntimeServices(ctx, "ping/ansible-hosts", "/ansible/hosts.yml", "host", []core.Service{staleHost}); err != nil {
		t.Fatal(err)
	}
	for _, serviceID := range []string{"host-kept", "host-stale"} {
		if err := store.UpsertStatus(ctx, core.StatusResult{
			ServiceID: serviceID,
			Target:    "ansible-hosts",
			Health:    core.HealthHealthy,
			Message:   "pong",
			CheckedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	err = store.PruneRuntimeServices(ctx, "host", []RuntimeServiceSource{{
		Repository: "kube",
		SourcePath: "infrastructure/inventory/hosts.yml",
	}})
	if err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	servicesByID := map[string]core.Service{}
	for _, service := range summary.Services {
		servicesByID[service.ID] = service
	}
	if _, ok := servicesByID["host-kept"]; !ok {
		t.Fatalf("kept host missing from services: %#v", summary.Services)
	}
	if _, ok := servicesByID["compose-1"]; !ok {
		t.Fatalf("compose service missing from services: %#v", summary.Services)
	}
	if _, ok := servicesByID["host-stale"]; ok {
		t.Fatalf("stale host still present: %#v", summary.Services)
	}
	for _, repo := range summary.Repositories {
		if repo.Name == "ping/ansible-hosts" {
			t.Fatalf("stale configured repository still present: %#v", summary.Repositories)
		}
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='host-kept'"); got != 1 {
		t.Fatalf("kept host history rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='host-stale'"); got != 0 {
		t.Fatalf("stale host history rows = %d, want 0", got)
	}
}

func countRows(t *testing.T, store *Store, query string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(), query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func alertResetConsumedCount(t *testing.T, store *Store, resetToken string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM alert_dedupe_reset_consumed WHERE token_fingerprint=?`, alertDedupeResetTokenFingerprint(resetToken)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestUptimeTracksHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc", Target: "local", Health: core.HealthHealthy, Message: "up", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc", Target: "local", Health: core.HealthUnhealthy, Message: "down", CheckedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results"); got != 1 {
		t.Fatalf("status_results rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history"); got != 2 {
		t.Fatalf("status_history rows = %d, want 2", got)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 {
		t.Fatalf("uptime = %#v", summary.Uptime)
	}
	stat := summary.Uptime[0]
	if stat.ServiceID != "svc" || stat.Target != "local" {
		t.Fatalf("stat identity = %#v", stat)
	}
	if stat.CheckCount != 2 {
		t.Fatalf("checkCount = %d, want 2", stat.CheckCount)
	}
	if stat.UptimePercent != 50.0 {
		t.Fatalf("uptimePercent = %v, want 50.0", stat.UptimePercent)
	}
	if len(stat.Samples) != 2 {
		t.Fatalf("samples len = %d, want 2", len(stat.Samples))
	}
	if !stat.Samples[0].CheckedAt.Before(stat.Samples[1].CheckedAt) {
		t.Fatalf("samples not ascending: %v then %v", stat.Samples[0].CheckedAt, stat.Samples[1].CheckedAt)
	}
	if stat.Samples[0].Health != core.HealthHealthy || stat.Samples[1].Health != core.HealthUnhealthy {
		t.Fatalf("sample healths = %s, %s", stat.Samples[0].Health, stat.Samples[1].Health)
	}
}

func TestSummaryHealthAggregatesAcrossTargets(t *testing.T) {
	t.Parallel()
	statusTime := time.Date(2026, 6, 27, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		statuses []core.StatusResult
		want     core.HealthState
	}{
		{
			name: "mixed target results degrade",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "docker", Health: core.HealthUnknown, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "routes", Health: core.HealthHealthy, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "docker", Health: core.HealthUnknown, CheckedAt: statusTime.Add(time.Minute)},
			},
			want: core.HealthDegraded,
		},
		{
			name: "all target results healthy",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "docker", Health: core.HealthHealthy, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "routes", Health: core.HealthHealthy, CheckedAt: statusTime},
			},
			want: core.HealthHealthy,
		},
		{
			name: "single failed target stays failed",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "routes", Health: core.HealthError, CheckedAt: statusTime},
			},
			want: core.HealthError,
		},
		{
			name: "all non-passing targets use worst status",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "docker", Health: core.HealthUnhealthy, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "routes", Health: core.HealthError, CheckedAt: statusTime},
			},
			want: core.HealthError,
		},
		{
			name: "not applicable targets are ignored",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "routes: http://10.10.10.20", Health: core.HealthNotApplicable, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "routes: https://app.example.test", Health: core.HealthHealthy, CheckedAt: statusTime},
			},
			want: core.HealthHealthy,
		},
		{
			name: "only not applicable targets leave health unknown",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "routes: http://10.10.10.20", Health: core.HealthNotApplicable, CheckedAt: statusTime},
			},
			want: core.HealthUnknown,
		},
		{
			name: "all routes not applicable use other monitor targets",
			statuses: []core.StatusResult{
				{ServiceID: "svc", Target: "routes: http://10.10.10.20", Health: core.HealthNotApplicable, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "routes: https://app.example.test", Health: core.HealthNotApplicable, CheckedAt: statusTime},
				{ServiceID: "svc", Target: "docker", Health: core.HealthUnhealthy, CheckedAt: statusTime},
			},
			want: core.HealthUnhealthy,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			services := []core.Service{{ID: "svc", Health: core.HealthUnknown}}
			applyLatestStatus(services, tc.statuses)
			if services[0].Health != tc.want {
				t.Fatalf("health = %s, want %s", services[0].Health, tc.want)
			}
		})
	}
}

func TestSummaryCacheInvalidatesOnStatusWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{{
		ID:         "svc",
		Name:       "web",
		Repository: "repo",
		SourcePath: "prod/compose.yaml",
		Runtime:    "compose",
		Kind:       "Service",
		Health:     core.HealthUnknown,
	}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc",
		Target:    "local",
		Health:    core.HealthHealthy,
		Message:   "first",
		CheckedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 || summary.Uptime[0].CheckCount != 1 {
		t.Fatalf("first uptime = %#v, want one check", summary.Uptime)
	}
	store.summaryMu.RLock()
	cacheValid := store.summaryCache.valid
	store.summaryMu.RUnlock()
	if !cacheValid {
		t.Fatal("summary cache was not populated")
	}

	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc",
		Target:    "local",
		Health:    core.HealthUnhealthy,
		Message:   "second",
		CheckedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	store.summaryMu.RLock()
	cacheValid = store.summaryCache.valid
	store.summaryMu.RUnlock()
	if cacheValid {
		t.Fatal("summary cache remained valid after status write")
	}
	summary, err = store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 || summary.Uptime[0].CheckCount != 2 || summary.Uptime[0].UptimePercent != 50 {
		t.Fatalf("rebuilt uptime = %#v, want two checks at 50%%", summary.Uptime)
	}
}

func TestSummaryCacheExpiresRollingUptimeWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(t.TempDir() + "/dashboard.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{{
		ID:         "svc",
		Name:       "web",
		Repository: "repo",
		SourcePath: "prod/compose.yaml",
		Runtime:    "compose",
		Kind:       "Service",
		Health:     core.HealthUnknown,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc",
		Target:    "local",
		Health:    core.HealthHealthy,
		Message:   "inside window",
		CheckedAt: time.Now().UTC().Add(-23 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 {
		t.Fatalf("cached uptime = %#v, want one in-window stat", summary.Uptime)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE status_history SET checked_at=?`, time.Now().UTC().Add(-25*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	store.summaryMu.Lock()
	store.summaryCache.cachedAt = time.Now().UTC().Add(-summaryCacheTTL - time.Second)
	store.summaryMu.Unlock()

	summary, err = store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 0 {
		t.Fatalf("expired-cache uptime = %#v, want rolling window recomputed empty", summary.Uptime)
	}
}

func TestSetMonitorNotApplicableRemovesTargetFromHealthAndUptime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "svc-app",
		Name:        "app",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	goodTarget := "routes: https://app.example.test"
	badTarget := "routes: http://10.10.10.20"
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-app", Target: goodTarget, Health: core.HealthHealthy, Message: "ok", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-app", Target: badTarget, Health: core.HealthError, Message: "dial failed", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].Health != core.HealthDegraded {
		t.Fatalf("pre-override service health = %s, want degraded", summary.Services[0].Health)
	}
	if len(summary.Uptime) != 2 {
		t.Fatalf("pre-override uptime = %#v, want two targets", summary.Uptime)
	}

	if err := store.SetMonitorNotApplicable(ctx, "svc-app", badTarget, true); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-app", Target: badTarget, Health: core.HealthError, Message: "dial failed again", CheckedAt: base.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	summary, err = store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].Health != core.HealthHealthy {
		t.Fatalf("post-override service health = %s, want healthy", summary.Services[0].Health)
	}
	statusByTarget := map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		statusByTarget[status.Target] = status
	}
	if statusByTarget[badTarget].Health != core.HealthNotApplicable {
		t.Fatalf("ignored status = %#v, want not_applicable", statusByTarget[badTarget])
	}
	if len(summary.Uptime) != 1 || summary.Uptime[0].Target != goodTarget {
		t.Fatalf("post-override uptime = %#v, want only good target", summary.Uptime)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='svc-app' AND target='routes: http://10.10.10.20'"); got != 0 {
		t.Fatalf("ignored status history rows = %d, want 0", got)
	}

	if err := store.SetMonitorNotApplicable(ctx, "svc-app", badTarget, false); err != nil {
		t.Fatal(err)
	}
	summary, err = store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statusByTarget = map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		statusByTarget[status.Target] = status
	}
	if statusByTarget[badTarget].Health != core.HealthUnknown {
		t.Fatalf("re-enabled status = %#v, want unknown until next check", statusByTarget[badTarget])
	}
}

func TestSetMonitorNotApplicableClearsImageVersionContribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "svc-api",
		Name:        "api",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Health:      core.HealthUnknown,
		Images:      []string{"example/api:v1.0.0"},
		Environment: "production",
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	status := core.StatusResult{
		ServiceID: "svc-api",
		Target:    "docker",
		Health:    core.HealthHealthy,
		Message:   "running",
		CheckedAt: time.Now().UTC(),
		ObservedImages: []core.ObservedImage{
			core.NewObservedImage("docker", "docker", "example/api:v1.0.0", "sha256:api", nil),
		},
	}
	if err := store.UpsertStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc-api", "docker", true); err != nil {
		t.Fatal(err)
	}
	status.Message = "running after override"
	status.CheckedAt = status.CheckedAt.Add(time.Minute)
	if err := store.UpsertStatus(ctx, status); err != nil {
		t.Fatal(err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].ImageVersionState != core.ImageVersionUnknown {
		t.Fatalf("image version state = %s, want unknown; checks=%#v", summary.Services[0].ImageVersionState, summary.Services[0].ImageVersionChecks)
	}
	if len(summary.Services[0].ImageVersionChecks) != 1 || summary.Services[0].ImageVersionChecks[0].Observed != nil {
		t.Fatalf("image checks = %#v, want no observed image contribution", summary.Services[0].ImageVersionChecks)
	}
	statusByTarget := map[string]core.StatusResult{}
	for _, item := range summary.Statuses {
		statusByTarget[item.Target] = item
	}
	if statusByTarget["docker"].Health != core.HealthNotApplicable {
		t.Fatalf("docker status = %#v, want not_applicable", statusByTarget["docker"])
	}
	if len(statusByTarget["docker"].ObservedImages) != 0 {
		t.Fatalf("observed images = %#v, want cleared on override", statusByTarget["docker"].ObservedImages)
	}
}

func TestParentRouteOverrideSuppressesLateChildStatusWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "svc-app",
		Name:        "app",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure:    []string{"https://app.example.test"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}

	if err := store.SetMonitorNotApplicable(ctx, "svc-app", "routes", true); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-app",
		Target:    "routes: https://app.example.test",
		Health:    core.HealthUnhealthy,
		Message:   "late check failed",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Services[0].Health != core.HealthUnknown {
		t.Fatalf("service health = %s, want unknown", summary.Services[0].Health)
	}
	if len(summary.Uptime) != 0 {
		t.Fatalf("uptime = %#v, want no child route contributions", summary.Uptime)
	}
	statusByTarget := map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		statusByTarget[status.Target] = status
	}
	if statusByTarget["routes"].Health != core.HealthNotApplicable {
		t.Fatalf("parent status = %#v, want not_applicable", statusByTarget["routes"])
	}
	if _, ok := statusByTarget["routes: https://app.example.test"]; ok {
		t.Fatalf("late child route status was stored despite parent override: %#v", statusByTarget["routes: https://app.example.test"])
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE service_id='svc-app' AND target='routes: https://app.example.test'"); got != 0 {
		t.Fatalf("child status rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='svc-app' AND target='routes: https://app.example.test'"); got != 0 {
		t.Fatalf("child history rows = %d, want 0", got)
	}
}

func TestSetMonitorNotApplicableAcceptsConfiguredSyntheticRouteTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "svc-app",
		Name:        "app",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure: []string{
			"https://app.example.test",
			"ssh://app.example.test:22",
		},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}

	target := "routes: https://app.example.test"
	if err := store.SetMonitorNotApplicable(ctx, "svc-app", target, true); err != nil {
		t.Fatalf("configured route override failed: %v", err)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statusByTarget := map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		statusByTarget[status.Target] = status
	}
	if statusByTarget[target].Health != core.HealthNotApplicable {
		t.Fatalf("synthetic route status = %#v, want not_applicable", statusByTarget[target])
	}

	if err := store.SetMonitorNotApplicable(ctx, "svc-app", "routes: ssh://app.example.test:22", true); !errors.Is(err, ErrStatusNotFound) {
		t.Fatalf("ssh route override error = %v, want ErrStatusNotFound", err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc-app", "routes: https://missing.example.test", true); !errors.Is(err, ErrStatusNotFound) {
		t.Fatalf("missing route override error = %v, want ErrStatusNotFound", err)
	}
}

func TestSetMonitorNotApplicableCanonicalizesSyntheticRouteTargets(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		target string
	}{
		{name: "trailing slash", target: "routes: https://app.example.test/"},
		{name: "default port", target: "routes: https://app.example.test:443"},
		{name: "case variant", target: "routes: HTTPS://APP.EXAMPLE.TEST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			service := core.Service{
				ID:          "svc-app",
				Name:        "app",
				Repository:  "repo",
				SourcePath:  "prod/compose.yaml",
				Runtime:     "compose",
				Kind:        "Service",
				Environment: "production",
				Health:      core.HealthUnknown,
				Exposure:    []string{"https://app.example.test"},
			}
			if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
				t.Fatal(err)
			}

			const canonicalTarget = "routes: https://app.example.test"
			if err := store.SetMonitorNotApplicable(ctx, "svc-app", tc.target, true); err != nil {
				t.Fatal(err)
			}
			if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE service_id='svc-app' AND target='routes: https://app.example.test'"); got != 1 {
				t.Fatalf("canonical override rows = %d, want 1", got)
			}
			if tc.target != canonicalTarget {
				var rawRows int
				if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM monitor_overrides WHERE service_id=? AND target=?`, "svc-app", tc.target).Scan(&rawRows); err != nil {
					t.Fatal(err)
				}
				if rawRows != 0 {
					t.Fatalf("raw override rows = %d, want 0", rawRows)
				}
			}
			ignored, err := store.MonitorNotApplicable(ctx, "svc-app", canonicalTarget)
			if err != nil {
				t.Fatal(err)
			}
			if !ignored {
				t.Fatalf("canonical target was not ignored")
			}
			ignored, err = store.MonitorNotApplicable(ctx, "svc-app", tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if !ignored {
				t.Fatalf("alias target was not ignored")
			}

			if err := store.SetMonitorNotApplicable(ctx, "svc-app", tc.target, false); err != nil {
				t.Fatal(err)
			}
			ignored, err = store.MonitorNotApplicable(ctx, "svc-app", canonicalTarget)
			if err != nil {
				t.Fatal(err)
			}
			if ignored {
				t.Fatalf("canonical target remained ignored after enabling alias")
			}
		})
	}
}

func TestMigrateCanonicalizesStoredRouteTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	service := core.Service{
		ID:          "svc-app",
		Name:        "app",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure:    []string{"https://app.example.test"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE services
SET exposure_json='["https://user:pass@app.example.test/","ssh://git@github.com/org/repo.git"]'
WHERE id='svc-app';
INSERT INTO monitor_overrides(service_id, target, not_applicable, updated_at) VALUES
  ('svc-app', 'routes: https://app.example.test', 0, '2026-01-02T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/', 1, '2026-01-01T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test:443', 0, '2026-01-03T00:00:00Z'),
  ('svc-app', 'routes: https://user:P%40ss@app.example.test:443', 0, '2026-01-06T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/admin/', 1, '2026-01-04T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/a%2Fb', 1, '2026-01-05T00:00:00Z');
INSERT INTO status_results(service_id, target, health, message, checked_at) VALUES
  ('svc-app', 'routes: https://app.example.test', 'healthy', 'canonical ok', '2026-01-01T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/', 'degraded', 'slash degraded', '2026-01-02T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test:443', 'error', 'default port failed', '2026-01-03T00:00:00Z'),
  ('svc-app', 'routes: https://user:P%40ss@app.example.test:443', 'degraded', 'https://user:P%40ss@app.example.test failed', '2026-01-06T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/admin/', 'healthy', 'admin slash ok', '2026-01-04T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/a%2Fb', 'healthy', 'escaped slash ok', '2026-01-05T00:00:00Z');
INSERT INTO status_history(service_id, target, health, message, checked_at) VALUES
  ('svc-app', 'routes: https://app.example.test/', 'degraded', 'slash degraded', '2026-01-02T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test:443', 'error', 'default port failed', '2026-01-03T00:00:00Z'),
  ('svc-app', 'routes: https://user:P%40ss@app.example.test:443', 'degraded', 'https://user:P%40ss@app.example.test failed', '2026-01-06T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/admin/', 'healthy', 'admin slash ok', '2026-01-04T00:00:00Z'),
  ('svc-app', 'routes: https://app.example.test/a%2Fb', 'healthy', 'escaped slash ok', '2026-01-05T00:00:00Z');
`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target IN ('routes: https://app.example.test/', 'routes: https://app.example.test:443')"); got != 0 {
		t.Fatalf("alias override rows = %d, want 0", got)
	}
	var notApplicable int
	var updatedAt string
	if err := store.db.QueryRowContext(ctx, `
SELECT not_applicable, updated_at
FROM monitor_overrides
WHERE service_id='svc-app' AND target='routes: https://app.example.test'
`).Scan(&notApplicable, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if notApplicable != 1 || updatedAt != "2026-01-06T00:00:00Z" {
		t.Fatalf("merged override = not_applicable:%d updated_at:%s, want 1 and latest timestamp", notApplicable, updatedAt)
	}
	ignored, err := store.MonitorNotApplicable(ctx, "svc-app", "routes: https://app.example.test:443")
	if err != nil {
		t.Fatal(err)
	}
	if !ignored {
		t.Fatalf("default-port alias was not preserved as an active override")
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target='routes: https://app.example.test/admin/'"); got != 1 {
		t.Fatalf("non-root trailing-slash override rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target='routes: https://app.example.test/admin'"); got != 0 {
		t.Fatalf("non-root slashless override rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target='routes: https://app.example.test/a%2Fb'"); got != 1 {
		t.Fatalf("escaped route override rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target='routes: https://app.example.test/a/b'"); got != 0 {
		t.Fatalf("decoded route override rows = %d, want 0", got)
	}

	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target IN ('routes: https://app.example.test/', 'routes: https://app.example.test:443')"); got != 0 {
		t.Fatalf("alias status rows = %d, want 0", got)
	}
	var health, message, checkedAt string
	if err := store.db.QueryRowContext(ctx, `
SELECT health, message, checked_at
FROM status_results
WHERE service_id='svc-app' AND target='routes: https://app.example.test'
`).Scan(&health, &message, &checkedAt); err != nil {
		t.Fatal(err)
	}
	if health != "not_applicable" || message != "not applicable" || checkedAt != "2026-01-06T00:00:00Z" {
		t.Fatalf("merged status = %s/%s/%s, want active override to force not_applicable", health, message, checkedAt)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target='routes: https://app.example.test/admin/'"); got != 1 {
		t.Fatalf("non-root trailing-slash status rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target='routes: https://app.example.test/admin'"); got != 0 {
		t.Fatalf("non-root slashless status rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target='routes: https://app.example.test/a%2Fb'"); got != 1 {
		t.Fatalf("escaped route status rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target='routes: https://app.example.test/a/b'"); got != 0 {
		t.Fatalf("decoded route status rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target IN ('routes: https://app.example.test/', 'routes: https://app.example.test:443')"); got != 0 {
		t.Fatalf("alias history rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test'"); got != 3 {
		t.Fatalf("canonical history rows = %d, want 3", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test' AND health='not_applicable'"); got != 0 {
		t.Fatalf("canonical history rows rewritten by override = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test/admin/'"); got != 1 {
		t.Fatalf("non-root trailing-slash history rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test/admin'"); got != 0 {
		t.Fatalf("non-root slashless history rows = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test/a%2Fb'"); got != 1 {
		t.Fatalf("escaped route history rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target='routes: https://app.example.test/a/b'"); got != 0 {
		t.Fatalf("decoded route history rows = %d, want 0", got)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Services) != 1 || len(summary.Services[0].MonitorRoutes) != 1 || summary.Services[0].MonitorRoutes[0] != "https://app.example.test" {
		t.Fatalf("monitor routes = %#v, want canonical route in summary", summary.Services)
	}
	for _, value := range summary.Services[0].Exposure {
		if strings.Contains(value, "@") {
			t.Fatalf("service exposure contains URL userinfo after migration: %#v", summary.Services[0].Exposure)
		}
	}
	if summary.Services[0].Health != core.HealthUnknown {
		t.Fatalf("service health = %s, want unknown because all route rows are ignored", summary.Services[0].Health)
	}
	if len(summary.Uptime) != 0 {
		t.Fatalf("uptime = %#v, want ignored route excluded", summary.Uptime)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target LIKE '%@%'"); got != 0 {
		t.Fatalf("override rows with URL userinfo = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target LIKE '%@%' OR message LIKE '%@%'"); got != 0 {
		t.Fatalf("status rows with URL userinfo = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target LIKE '%@%' OR message LIKE '%@%'"); got != 0 {
		t.Fatalf("history rows with URL userinfo = %d, want 0", got)
	}
}

func TestMigrateCanonicalizesConfiguredCustomRouteTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	service := core.Service{
		ID:          "svc-app",
		Name:        "app",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure:    []string{"https://app.example.test"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO monitor_overrides(service_id, target, not_applicable, updated_at) VALUES
  ('svc-app', 'svc-http: https://user:P%40ss@app.example.test:443', 1, '2026-01-06T00:00:00Z');
INSERT INTO status_results(service_id, target, health, message, checked_at) VALUES
  ('svc-app', 'svc-http: https://user:P%40ss@app.example.test:443', 'degraded', 'https://user:P%40ss@app.example.test failed', '2026-01-06T00:00:00Z');
INSERT INTO status_history(service_id, target, health, message, checked_at) VALUES
  ('svc-app', 'svc-http: https://user:P%40ss@app.example.test:443', 'healthy', 'https://user:P%40ss@app.example.test ok', '2026-01-06T00:00:00Z');
`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.CanonicalizeHTTPRouteTargets(ctx, []config.HTTPRouteTarget{{Name: "svc-http"}}); err != nil {
		t.Fatal(err)
	}

	const canonicalTarget = "svc-http: https://app.example.test"
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target='svc-http: https://user:P%40ss@app.example.test:443'"); got != 0 {
		t.Fatalf("raw custom override rows = %d, want 0", got)
	}
	var notApplicable int
	var updatedAt string
	if err := store.db.QueryRowContext(ctx, `
SELECT not_applicable, updated_at
FROM monitor_overrides
WHERE service_id='svc-app' AND target=?
`, canonicalTarget).Scan(&notApplicable, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if notApplicable != 1 || updatedAt != "2026-01-06T00:00:00Z" {
		t.Fatalf("custom override = not_applicable:%d updated_at:%s, want active canonical override", notApplicable, updatedAt)
	}
	lookup, err := store.RouteMonitorLookup(ctx, []string{"svc-app"}, "svc-http", "svc-http: ")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lookup["svc-app"].Overrides[canonicalTarget]; !ok {
		t.Fatalf("custom canonical override missing from route monitor lookup: %#v", lookup["svc-app"].Overrides)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE service_id='svc-app' AND target='svc-http: https://app.example.test' AND health='not_applicable' AND message='not applicable'"); got != 1 {
		t.Fatalf("canonical custom not_applicable status rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='svc-app' AND target='svc-http: https://app.example.test' AND health='healthy' AND message='https://app.example.test ok'"); got != 1 {
		t.Fatalf("canonical custom observed history rows = %d, want 1", got)
	}
	if err := store.MigrateRouteTargetReplacements(ctx, []RouteTargetReplacement{{ServiceID: "svc-app", OldRoute: "https://app.example.test", NewRoute: "https://app.example.test:8443"}}, []config.HTTPRouteTarget{{Name: "svc-http"}}); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='svc-app' AND target='svc-http: https://app.example.test:8443' AND health='healthy' AND message='https://app.example.test ok'"); got != 1 {
		t.Fatalf("migrated custom observed history rows = %d, want 1", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM monitor_overrides WHERE target LIKE '%@%'"); got != 0 {
		t.Fatalf("custom override rows with URL userinfo = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_results WHERE target LIKE '%@%' OR message LIKE '%@%'"); got != 0 {
		t.Fatalf("custom status rows with URL userinfo = %d, want 0", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE target LIKE '%@%' OR message LIKE '%@%'"); got != 0 {
		t.Fatalf("custom history rows with URL userinfo = %d, want 0", got)
	}
}

func TestStoreStripsURLUserinfoFromServiceExposureAndMonitorRoutes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{{
		ID:          "svc-auth-route",
		Name:        "auth-route",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure: []string{
			"https://user:pass@app.example.test/admin",
			"ssh://git@github.com/org/repo.git",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	var rawExposure string
	if err := store.db.QueryRowContext(ctx, `SELECT exposure_json FROM services WHERE id='svc-auth-route'`).Scan(&rawExposure); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawExposure, "user:pass@") || strings.Contains(rawExposure, "git@") {
		t.Fatalf("stored exposure contains URL userinfo: %s", rawExposure)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	service := summary.Services[0]
	for _, value := range service.Exposure {
		if strings.Contains(value, "@") {
			t.Fatalf("summary exposure contains URL userinfo: %#v", service.Exposure)
		}
	}
	if len(service.MonitorRoutes) != 1 || service.MonitorRoutes[0] != "https://app.example.test/admin" {
		t.Fatalf("monitor routes = %#v, want stripped HTTP route", service.MonitorRoutes)
	}
}

func TestRouteMonitorLookupBatchesStatusTargetsAndOverrides(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.db.ExecContext(ctx, `
INSERT INTO monitor_overrides(service_id, target, not_applicable, updated_at)
VALUES
  ('svc-a', 'routes', 1, '2026-01-01T00:00:00Z'),
  ('svc-a', 'routes: https://app.example.test', 1, '2026-01-01T00:00:00Z'),
  ('svc-a', 'docker', 1, '2026-01-01T00:00:00Z'),
  ('svc-b', 'routes: https://api.example.test', 1, '2026-01-01T00:00:00Z')
`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at)
VALUES
  ('svc-a', 'routes', 'healthy', 'parent', '2026-01-01T00:00:00Z'),
  ('svc-a', 'routes: https://app.example.test', 'healthy', 'child', '2026-01-01T00:00:00Z'),
  ('svc-a', 'routes: https://old.example.test', 'healthy', 'stale', '2026-01-01T00:00:00Z'),
  ('svc-a', 'docker', 'healthy', 'other', '2026-01-01T00:00:00Z'),
  ('svc-b', 'routes: https://api.example.test', 'healthy', 'child', '2026-01-01T00:00:00Z'),
  ('svc-b', 'routes: https://other.example.test', 'healthy', 'other', '2026-01-01T00:00:00Z')
`); err != nil {
		t.Fatal(err)
	}

	lookup, err := store.RouteMonitorLookup(ctx, []string{"svc-a", "svc-b", "svc-a", "", "svc-c"}, "routes", "routes: ")
	if err != nil {
		t.Fatal(err)
	}
	svcA, ok := lookup["svc-a"]
	if !ok {
		t.Fatal("missing svc-a state")
	}
	if len(svcA.Overrides) != 2 {
		t.Fatalf("svc-a overrides = %#v, want 2", svcA.Overrides)
	}
	if _, ok := svcA.Overrides["routes"]; !ok {
		t.Fatal("svc-a overrides missing parent route override")
	}
	if _, ok := svcA.Overrides["routes: https://app.example.test"]; !ok {
		t.Fatal("svc-a overrides missing exact child override")
	}
	if _, ok := svcA.StatusTargets["routes"]; !ok {
		t.Fatal("svc-a status target missing parent route target")
	}
	if _, ok := svcA.StatusTargets["routes: https://app.example.test"]; !ok {
		t.Fatal("svc-a status target missing configured child target")
	}
	if _, ok := svcA.StatusTargets["routes: https://old.example.test"]; !ok {
		t.Fatal("svc-a status target missing stale child target")
	}
	if _, ok := svcA.StatusTargets["docker"]; ok {
		t.Fatal("svc-a status target contained non-route target")
	}
	if _, ok := svcA.Overrides["docker"]; ok {
		t.Fatal("svc-a override lookup included non-route target")
	}

	svcB, ok := lookup["svc-b"]
	if !ok {
		t.Fatal("missing svc-b state")
	}
	if _, ok := svcB.Overrides["routes: https://api.example.test"]; len(svcB.Overrides) != 1 || !ok {
		t.Fatal("svc-b override set did not include only expected child override")
	}
	if len(svcB.StatusTargets) != 2 {
		t.Fatalf("svc-b status targets = %#v, want 2", svcB.StatusTargets)
	}
	if _, ok := svcB.StatusTargets["routes: https://api.example.test"]; !ok {
		t.Fatal("svc-b status target missing configured child target")
	}
	if _, ok := svcB.StatusTargets["routes: https://other.example.test"]; !ok {
		t.Fatal("svc-b status target missing stale child target")
	}

	svcC, ok := lookup["svc-c"]
	if !ok {
		t.Fatal("missing svc-c state")
	}
	if len(svcC.Overrides) != 0 || len(svcC.StatusTargets) != 0 {
		t.Fatalf("svc-c lookup state = %#v, want empty maps", svcC)
	}
	if len(lookup) != 3 {
		t.Fatalf("lookup map = %#v, want 3 services after dedupe", len(lookup))
	}
}

func TestRouteMonitorLookupUsesCustomHTTPParentPrefixAndBatchesLargeSets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	target := "svc-http"
	prefix := target + ": "

	for i := 0; i < 1100; i++ {
		serviceID := fmt.Sprintf("svc-%04d", i)
		_, err := store.db.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at)
VALUES (?, ?, 'healthy', 'ok', '2026-01-01T00:00:00Z')
`, serviceID, target)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.db.ExecContext(ctx, `
INSERT INTO status_results(service_id, target, health, message, checked_at)
VALUES (?, ?, 'healthy', 'child ok', '2026-01-01T00:00:00Z')
`, serviceID, prefix+"https://example.test/"+serviceID)
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = store.db.ExecContext(ctx, `
INSERT INTO monitor_overrides(service_id, target, not_applicable, updated_at)
VALUES ('svc-0000', ?, 1, '2026-01-01T00:00:00Z'),
       ('svc-0000', ?, 1, '2026-01-01T00:00:00Z')
`, target, prefix+"https://app.example.test")
	if err != nil {
		t.Fatal(err)
	}

	serviceIDs := make([]string, 1100)
	for i := 0; i < 1100; i++ {
		serviceIDs[i] = fmt.Sprintf("svc-%04d", i)
	}
	lookup, err := store.RouteMonitorLookup(ctx, serviceIDs, target, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(lookup) != 1100 {
		t.Fatalf("lookup size = %d, want 1100", len(lookup))
	}
	svc0, ok := lookup["svc-0000"]
	if !ok {
		t.Fatal("svc-0000 missing from lookup")
	}
	if _, ok := svc0.Overrides[target]; !ok {
		t.Fatal("svc-0000 missing exact parent override for configured target")
	}
	if _, ok := svc0.Overrides[prefix+"https://app.example.test"]; !ok {
		t.Fatal("svc-0000 missing exact child override for configured target")
	}
	if _, ok := svc0.StatusTargets[target]; !ok {
		t.Fatal("svc-0000 missing configured target from status")
	}
	if _, ok := svc0.StatusTargets[prefix+"https://example.test/svc-0000"]; !ok {
		t.Fatal("svc-0000 missing configured child status target")
	}
}

func TestStatusHistoryCheckedAtIndexMigrationIsIdempotent(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	countIndexes := func() int {
		var count int
		if err := store.db.QueryRowContext(context.Background(), `
SELECT COUNT(*)
FROM sqlite_master
WHERE type='index' AND name='idx_status_history_checked_at_lookup'
`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if got := countIndexes(); got != 1 {
		t.Fatalf("status_history checked_at index count after first migrate = %d, want 1", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := countIndexes(); got != 1 {
		t.Fatalf("status_history checked_at index count after second migrate = %d, want 1", got)
	}
}

func TestSetMonitorNotApplicableHandlesRoutesParentAndLiteralTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := core.Service{
		ID:          "svc-routes",
		Name:        "routes",
		Repository:  "repo",
		SourcePath:  "prod/compose.yaml",
		Runtime:     "compose",
		Kind:        "Service",
		Environment: "production",
		Health:      core.HealthUnknown,
		Exposure:    []string{"ssh://routes.example.test:22"},
	}
	if err := store.ReplaceConfiguredServices(ctx, "repo", "prod/compose.yaml", []core.Service{service}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc-routes", "routes", true); !errors.Is(err, ErrStatusNotFound) {
		t.Fatalf("synthetic ordinary routes error = %v, want ErrStatusNotFound", err)
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-routes", Target: "routes", Health: core.HealthHealthy, Message: "ok", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	childTarget := "routes: https://app.example.test"
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-routes", Target: childTarget, Health: core.HealthHealthy, Message: "child ok", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc-routes", "routes", true); err != nil {
		t.Fatal(err)
	}

	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statusByTarget := map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		statusByTarget[status.Target] = status
	}
	if statusByTarget["routes"].Health != core.HealthNotApplicable {
		t.Fatalf("ordinary routes status = %#v, want not_applicable", statusByTarget["routes"])
	}
	if statusByTarget[childTarget].Health != core.HealthHealthy {
		t.Fatalf("child route status = %#v, want preserved healthy", statusByTarget[childTarget])
	}

	parentService := service
	parentService.ID = "svc-routes-http"
	parentService.Exposure = []string{"https://routes.example.test"}
	if err := store.ReplaceConfiguredServices(ctx, "repo-http", "prod/compose.yaml", []core.Service{parentService}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-routes-http", Target: "routes", Health: core.HealthHealthy, Message: "ok", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	httpChildTarget := "routes: https://routes.example.test"
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc-routes-http", Target: httpChildTarget, Health: core.HealthHealthy, Message: "child ok", CheckedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMonitorNotApplicable(ctx, "svc-routes-http", "routes", true); err != nil {
		t.Fatal(err)
	}
	summary, err = store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	statusByTarget = map[string]core.StatusResult{}
	for _, status := range summary.Statuses {
		if status.ServiceID == "svc-routes-http" {
			statusByTarget[status.Target] = status
		}
	}
	if statusByTarget["routes"].Health != core.HealthNotApplicable {
		t.Fatalf("parent routes status = %#v, want not_applicable", statusByTarget["routes"])
	}
	if _, ok := statusByTarget[httpChildTarget]; ok {
		t.Fatalf("HTTP child route status = %#v, want removed by parent override", statusByTarget[httpChildTarget])
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history WHERE service_id='svc-routes-http' AND target='routes: https://routes.example.test'"); got != 0 {
		t.Fatalf("HTTP child route history rows = %d, want 0", got)
	}
}

func TestUptimeHealthClassification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	healths := []core.HealthState{
		core.HealthHealthy, core.HealthDegraded, core.HealthUnhealthy, core.HealthUnknown, core.HealthError,
	}
	for i, health := range healths {
		if err := store.UpsertStatus(ctx, core.StatusResult{
			ServiceID: "svc", Target: "local", Health: health, Message: "check",
			CheckedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 {
		t.Fatalf("uptime = %#v", summary.Uptime)
	}
	stat := summary.Uptime[0]
	if stat.CheckCount != 5 {
		t.Fatalf("checkCount = %d, want 5", stat.CheckCount)
	}
	// healthy and degraded count as up, so 2 of 5 checks are up.
	if stat.UptimePercent != 40.0 {
		t.Fatalf("uptimePercent = %v, want 40.0", stat.UptimePercent)
	}
}

func TestUptimeSamplesCappedAndOrdered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	for i := 0; i < 45; i++ {
		if err := store.UpsertStatus(ctx, core.StatusResult{
			ServiceID: "svc", Target: "local", Health: core.HealthHealthy, Message: "up",
			CheckedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 {
		t.Fatalf("uptime = %#v", summary.Uptime)
	}
	stat := summary.Uptime[0]
	if stat.CheckCount != 45 {
		t.Fatalf("checkCount = %d, want 45", stat.CheckCount)
	}
	if len(stat.Samples) != 40 {
		t.Fatalf("samples len = %d, want 40", len(stat.Samples))
	}
	for i := 1; i < len(stat.Samples); i++ {
		if stat.Samples[i].CheckedAt.Before(stat.Samples[i-1].CheckedAt) {
			t.Fatalf("samples not ascending at index %d", i)
		}
	}
	// The oldest 5 of 45 are dropped, so the earliest retained sample is the 6th insert.
	if wantFirst := base.Add(5 * time.Minute); !stat.Samples[0].CheckedAt.Equal(wantFirst) {
		t.Fatalf("first sample = %v, want %v", stat.Samples[0].CheckedAt, wantFirst)
	}
	if wantLast := base.Add(44 * time.Minute); !stat.Samples[len(stat.Samples)-1].CheckedAt.Equal(wantLast) {
		t.Fatalf("last sample = %v, want %v", stat.Samples[len(stat.Samples)-1].CheckedAt, wantLast)
	}
}

func TestUptimePruneAndWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Second)
	// Insert directly so these rows survive to be observed; UpsertStatus would prune the old one in its own transaction.
	// Prune now runs as a separate maintenance pass after monitor runs.
	old := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)
	stale := now.Add(-30 * time.Hour).Format(time.RFC3339)
	for _, checkedAt := range []string{old, stale} {
		if _, err := store.db.ExecContext(ctx, `
INSERT INTO status_history(service_id, target, health, message, checked_at)
VALUES('svc', 'local', 'healthy', 'up', ?)`, checkedAt); err != nil {
			t.Fatal(err)
		}
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history"); got != 2 {
		t.Fatalf("pre-prune history rows = %d, want 2", got)
	}
	// A fresh check triggers the 7-day prune and adds a sample inside the window.
	if err := store.UpsertStatus(ctx, core.StatusResult{
		ServiceID: "svc", Target: "local", Health: core.HealthHealthy, Message: "up", CheckedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneStatusHistory(ctx); err != nil {
		t.Fatal(err)
	}
	// The 10-day-old row is pruned; the 30-hour-old row and the fresh row remain.
	if got := countRows(t, store, "SELECT COUNT(*) FROM status_history"); got != 2 {
		t.Fatalf("post-prune history rows = %d, want 2", got)
	}
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Uptime) != 1 {
		t.Fatalf("uptime = %#v", summary.Uptime)
	}
	// Only the fresh row is inside the 24h window; the 30-hour-old row is excluded from stats.
	if summary.Uptime[0].CheckCount != 1 {
		t.Fatalf("checkCount = %d, want 1", summary.Uptime[0].CheckCount)
	}
	if len(summary.Uptime[0].Samples) != 1 {
		t.Fatalf("samples len = %d, want 1", len(summary.Uptime[0].Samples))
	}
}

func TestUpsertAgentThenAgentsRoundtrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertAgent(ctx, core.AgentMessage{
		Target: "serenity",
		Containers: []core.ContainerStatus{{
			ID:    "abc123",
			Name:  "/stack-web-1",
			Image: "example/web:v1",
			Labels: map[string]string{
				core.DockerComposeProjectLabel:                  "stack",
				core.DockerComposeServiceLabel:                  "web",
				"traefik.http.middlewares.auth.basicauth.users": "admin:$2y$05$secret",
			},
			State: "running",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAgent(ctx, core.AgentMessage{Target: "albert"}); err != nil {
		t.Fatal(err)
	}
	agents, err := store.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("agents = %#v, want 2", agents)
	}
	if agents[0].Target != "albert" || agents[1].Target != "serenity" {
		t.Fatalf("agents not sorted by target: %#v", agents)
	}
	if agents[0].Containers == nil {
		t.Fatal("albert containers is nil, want empty slice")
	}
	if len(agents[0].Containers) != 0 {
		t.Fatalf("albert containers = %#v, want empty", agents[0].Containers)
	}
	if agents[0].LastSeenAt == "" {
		t.Fatal("albert last_seen_at is empty, want set")
	}
	serenity := agents[1]
	if len(serenity.Containers) != 1 || serenity.Containers[0].Name != "/stack-web-1" {
		t.Fatalf("serenity containers = %#v", serenity.Containers)
	}
	labels := serenity.Containers[0].Labels
	if len(labels) != 2 ||
		labels[core.DockerComposeProjectLabel] != "stack" ||
		labels[core.DockerComposeServiceLabel] != "web" {
		t.Fatalf("serenity labels = %#v, want only compose identity labels", labels)
	}
	if _, ok := labels["traefik.http.middlewares.auth.basicauth.users"]; ok {
		t.Fatalf("serenity labels leaked sensitive Docker label: %#v", labels)
	}
	if serenity.LastSeenAt == "" {
		t.Fatal("serenity last_seen_at is empty, want set")
	}
}

func TestStatusResultsDegradeExpiredTargetStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "target", Health: core.HealthHealthy, CheckedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Health != core.HealthUnknown || !strings.Contains(statuses[0].Message, "stale") {
		t.Fatalf("expired status = %#v, want stale unknown", statuses)
	}
}

func TestStatusResultsExpireAtTTLBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	boundary := time.Now().UTC()
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "target", Health: core.HealthHealthy, CheckedAt: boundary.Add(-time.Minute), ExpiresAt: boundary}); err != nil {
		t.Fatal(err)
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Health != core.HealthUnknown {
		t.Fatalf("health at expiry boundary = %s, want unknown", statuses[0].Health)
	}
}

func TestStatusResultsUseParentTTLForRouteChildrenAndLegacyRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetStatusTTL("routes", time.Minute)
	checkedAt := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO status_results(service_id,target,health,message,checked_at,expires_at) VALUES(?,?,?,?,?, '')`, "svc", "routes: https://example.test", "healthy", "old", checkedAt.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Health != core.HealthUnknown {
		t.Fatalf("legacy route status = %#v, want expired unknown", statuses)
	}
}

func TestAgentsUseLegacyTargetTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetStatusTTL("agent-a", time.Minute)
	seen := time.Now().UTC().Add(-2 * time.Minute)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO agents(target,last_seen_at,stale_after,status_json) VALUES(?,?, '', '[]')`, "agent-a", seen.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	agents, err := store.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].StaleAfter == "" {
		t.Fatalf("legacy agent = %#v, want derived staleAfter", agents)
	}
}

func TestLegacyAgentStatusUsesReceiptInsteadOfFutureCheckedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetStatusTTL("agent-a", time.Minute)
	seen := time.Now().UTC().Add(-2 * time.Minute)
	future := time.Now().UTC().Add(24 * time.Hour)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO agents(target,last_seen_at,stale_after,status_json) VALUES(?,?, '', '[]')`, "agent-a", seen.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO status_results(service_id,target,health,message,checked_at,expires_at,observed_images_json) VALUES(?,?, 'healthy','legacy',?,'','[]')`, "svc", "agent-a", future.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].Health != core.HealthUnknown {
		t.Fatalf("future legacy agent status = %#v", statuses[0])
	}
}

func TestFreshnessMigrationRebuildsIncompatibleExistingColumn(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE status_results (service_id TEXT NOT NULL, target TEXT NOT NULL, health TEXT NOT NULL, message TEXT NOT NULL, checked_at TEXT NOT NULL, expires_at INTEGER, PRIMARY KEY(service_id,target))`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.validateFreshnessTable(context.Background(), "status_results"); err != nil {
		t.Fatalf("rebuilt status_results schema: %v", err)
	}
}

func TestFreshnessMigrationRebuildsCorrectColumnWithWrongPrimaryKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE agents (target TEXT NOT NULL, last_seen_at TEXT NOT NULL, stale_after TEXT NOT NULL DEFAULT '', status_json TEXT NOT NULL, PRIMARY KEY(last_seen_at))`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.validateFreshnessTable(context.Background(), "agents"); err != nil {
		t.Fatalf("rebuilt agents schema: %v", err)
	}
}

func TestFreshnessMigrationRebuildsEverySchemaDriftDimension(t *testing.T) {
	fixtures := map[string]string{
		"type":          `CREATE TABLE status_results (service_id TEXT NOT NULL, target TEXT NOT NULL, health TEXT NOT NULL, message TEXT NOT NULL, checked_at TEXT NOT NULL, expires_at INTEGER NOT NULL DEFAULT '', observed_images_json TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(service_id,target))`,
		"nullability":   `CREATE TABLE status_results (service_id TEXT NOT NULL, target TEXT NOT NULL, health TEXT NOT NULL, message TEXT NOT NULL, checked_at TEXT NOT NULL, expires_at TEXT DEFAULT '', observed_images_json TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(service_id,target))`,
		"default":       `CREATE TABLE status_results (service_id TEXT NOT NULL, target TEXT NOT NULL, health TEXT NOT NULL, message TEXT NOT NULL, checked_at TEXT NOT NULL, expires_at TEXT NOT NULL DEFAULT 'later', observed_images_json TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(service_id,target))`,
		"primary key":   `CREATE TABLE status_results (service_id TEXT NOT NULL, target TEXT NOT NULL, health TEXT NOT NULL, message TEXT NOT NULL, checked_at TEXT NOT NULL, expires_at TEXT NOT NULL DEFAULT '', observed_images_json TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(target,service_id))`,
		"index":         `CREATE TABLE status_results (service_id TEXT NOT NULL, target TEXT NOT NULL, health TEXT NOT NULL, message TEXT NOT NULL, checked_at TEXT NOT NULL, expires_at TEXT NOT NULL DEFAULT '', observed_images_json TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(service_id,target)); CREATE INDEX drift_index ON status_results(health)`,
		"constraint":    `CREATE TABLE status_results (service_id TEXT NOT NULL, target TEXT NOT NULL, health TEXT NOT NULL CHECK(health <> 'unhealthy'), message TEXT NOT NULL, checked_at TEXT NOT NULL, expires_at TEXT NOT NULL DEFAULT '', observed_images_json TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(service_id,target))`,
		"foreign key":   `CREATE TABLE status_results (service_id TEXT NOT NULL, target TEXT NOT NULL, health TEXT NOT NULL, message TEXT NOT NULL, checked_at TEXT NOT NULL, expires_at TEXT NOT NULL DEFAULT '', observed_images_json TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(service_id,target), FOREIGN KEY(service_id) REFERENCES services(id))`,
		"trigger":       `CREATE TABLE status_results (service_id TEXT NOT NULL, target TEXT NOT NULL, health TEXT NOT NULL, message TEXT NOT NULL, checked_at TEXT NOT NULL, expires_at TEXT NOT NULL DEFAULT '', observed_images_json TEXT NOT NULL DEFAULT '[]', PRIMARY KEY(service_id,target)); CREATE TRIGGER drift_trigger AFTER INSERT ON status_results BEGIN SELECT 1; END`,
		"hidden column": `CREATE TABLE status_results (service_id TEXT NOT NULL, target TEXT NOT NULL, health TEXT NOT NULL, message TEXT NOT NULL, checked_at TEXT NOT NULL, expires_at TEXT NOT NULL DEFAULT '', observed_images_json TEXT NOT NULL DEFAULT '[]', generated TEXT GENERATED ALWAYS AS (service_id || target) VIRTUAL, PRIMARY KEY(service_id,target))`,
	}
	for name, schema := range fixtures {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dashboard.db")
			db, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(schema); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.validateFreshnessTable(context.Background(), "status_results"); err != nil {
				t.Fatalf("schema drift %s survived migration: %v", name, err)
			}
		})
	}
}

func TestFreshnessMigrationKeepsLatestConflictAndRecoveryRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE agents (target TEXT NOT NULL, last_seen_at TEXT NOT NULL, stale_after TEXT NOT NULL DEFAULT '', status_json TEXT NOT NULL, PRIMARY KEY(last_seen_at)); INSERT INTO agents VALUES ('agent-a','2026-07-12T12:00:00Z','','[]'),('agent-a','2026-07-12T12:01:00Z','','[]')`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agents, err := store.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].LastSeenAt != "2026-07-12T12:01:00Z" {
		t.Fatalf("canonical agents = %#v, want latest receipt", agents)
	}
	var recovered int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM freshness_recovery WHERE source_table='agents' AND canonical_key='agent-a'`).Scan(&recovered); err != nil {
		t.Fatal(err)
	}
	if recovered != 2 {
		t.Fatalf("recovered agent rows=%d, want 2", recovered)
	}
}

func TestLegacyOrphanAgentProjectionExpiresWithoutConfiguredTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	received := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	checked := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO agents(target,last_seen_at,stale_after,status_json) VALUES(?,?, '', '[]')`, "removed-agent", received); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO status_results(service_id,target,health,message,checked_at,expires_at,observed_images_json) VALUES('svc','removed-agent','healthy','legacy',?,'','[]')`, checked); err != nil {
		t.Fatal(err)
	}
	statuses, err := store.StatusResults(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Health != core.HealthUnknown {
		t.Fatalf("orphan agent status=%#v, want expired unknown", statuses)
	}
}

func TestNotApplicableExpiredStatusRemainsCacheable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "target", Health: core.HealthNotApplicable, CheckedAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Summary(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.cachedSummary(); !ok {
		t.Fatal("expired not-applicable status bypassed summary cache")
	}
}

func TestSummaryCachesMaterializedStaleStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertStatus(ctx, core.StatusResult{ServiceID: "svc", Target: "target", Health: core.HealthHealthy, CheckedAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Summary(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.cachedSummary(); !ok {
		t.Fatal("stale summary was not cacheable after materialization")
	}
}

func TestUptimeEmptyIsSlice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	summary, err := store.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Uptime == nil {
		t.Fatal("uptime is nil, want empty slice")
	}
	if len(summary.Uptime) != 0 {
		t.Fatalf("uptime = %#v, want empty", summary.Uptime)
	}
}

func TestAlertMigrationsAreIdempotent(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dbPath)
	if err != nil {
		t.Fatalf("second Open failed: %v", err)
	}
	defer store.Close()
	for _, table := range []string{"alert_events", "alert_dispatches"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("table %s count = %d, want 1", table, count)
		}
	}
}

func TestAlertMigrationRepairsOldEventTableWithoutStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  old_state TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL
);
INSERT INTO alert_events(kind, service_id, new_state, dedupe_key, created_at)
VALUES('health_transition', 'svc', 'unhealthy', 'legacy-key', '2026-07-09T12:00:00Z');
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, column := range []string{"status", "dedupe_hash"} {
		if !tableHasColumn(t, store, "alert_events", column) {
			t.Fatalf("alert_events missing repaired column %s", column)
		}
	}
	var status, hash string
	if err := store.db.QueryRowContext(ctx, `SELECT status, dedupe_hash FROM alert_events WHERE dedupe_key='legacy-key'`).Scan(&status, &hash); err != nil {
		t.Fatal(err)
	}
	if status != AlertEventStatusFailed || hash == "" {
		t.Fatalf("legacy row status/hash = %q/%q, want failed and non-empty hash", status, hash)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO alert_events(kind, service_id, new_state, dedupe_key, dedupe_hash, created_at, status)
VALUES('health_transition', 'svc-2', 'unhealthy', 'legacy-key', ?, '2026-07-09T12:01:00Z', 'failed')
`, store.alertDedupeHash("another-raw-key")); err != nil {
		t.Fatalf("legacy unique dedupe_key constraint was not rebuilt away: %v", err)
	}
	fresh, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "legacy-key",
		CreatedAt: time.Date(2026, 7, 9, 12, 2, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted || fresh.ID == 0 {
		t.Fatalf("fresh event inserted=%v event=%#v, want dispatchless legacy row not to suppress", inserted, fresh)
	}
}

func TestAlertMigrationRemovesLegacyDedupeKeyUniqueBeforeRedaction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  old_state TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL
);
INSERT INTO alert_events(kind, service_id, new_state, dedupe_key, created_at)
VALUES
  ('health_transition', 'svc-a', 'unhealthy', 'health:svc:secret-a', '2026-07-09T12:00:00Z'),
  ('health_transition', 'svc-b', 'unhealthy', 'health:svc:secret-b', '2026-07-09T12:01:00Z');
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := OpenWithOptions(dbPath, OpenOptions{RedactionValues: []string{"secret-a", "secret-b"}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE dedupe_key='health:svc:[REDACTED]'`); got != 2 {
		t.Fatalf("redacted dedupe_key rows = %d, want two rows after unique constraint removal", got)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO alert_events(kind, service_id, new_state, dedupe_key, dedupe_hash, created_at, status)
VALUES('health_transition', 'svc-c', 'unhealthy', 'health:svc:[REDACTED]', ?, '2026-07-09T12:02:00Z', 'failed')
`, store.alertDedupeHash("health:svc:third")); err != nil {
		t.Fatalf("dedupe_key unique constraint still active after migration: %v", err)
	}
}

func TestAlertSchemaOnlyMigrationDoesNotCompact(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  dedupe_key TEXT NOT NULL,
  created_at TEXT NOT NULL
);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	compactions := 0
	compactRedactedPagesObserver = func() { compactions++ }
	defer func() { compactRedactedPagesObserver = nil }()
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !tableHasColumn(t, store, "alert_events", "status") {
		t.Fatal("alert_events status column was not repaired")
	}
	if compactions != 0 {
		t.Fatalf("compactions = %d, want schema-only migration to skip checkpoint/VACUUM", compactions)
	}
}

func TestAlertIndexMigrationRepairsSameNamedFullUniqueIndex(t *testing.T) {
	t.Parallel()
	testAlertIndexMigrationRepairsSameNamedPendingDedupeIndex(t, `CREATE UNIQUE INDEX idx_alert_events_pending_dedupe_hash ON alert_events(dedupe_hash)`)
}

func TestAlertIndexMigrationRepairsSameNamedNonUniqueIndex(t *testing.T) {
	t.Parallel()
	testAlertIndexMigrationRepairsSameNamedPendingDedupeIndex(t, `CREATE INDEX idx_alert_events_pending_dedupe_hash ON alert_events(dedupe_hash)`)
}

func testAlertIndexMigrationRepairsSameNamedPendingDedupeIndex(t *testing.T, staleIndexSQL string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL DEFAULT '',
  service_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  old_state TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL DEFAULT '',
  dedupe_hash TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  created_at_ns INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending'
);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, staleIndexSQL); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertPendingDedupeIndexShape(t, store)
}

func assertPendingDedupeIndexShape(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	rows, err := store.db.QueryContext(ctx, `PRAGMA index_list(alert_events)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		if name != "idx_alert_events_pending_dedupe_hash" {
			continue
		}
		found = true
		if unique != 1 || partial != 1 {
			t.Fatalf("pending dedupe index unique/partial = %d/%d, want 1/1", unique, partial)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("pending dedupe index missing")
	}
	var sqlText string
	if err := store.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_alert_events_pending_dedupe_hash'`).Scan(&sqlText); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sqlText, "WHERE status='pending' AND dedupe_hash<>''") {
		t.Fatalf("pending dedupe index sql = %q, want partial predicate", sqlText)
	}
}

func TestAlertMigrationRebuildPreservesDispatchForeignKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
PRAGMA foreign_keys=ON;
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  old_state TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at TEXT,
  updated_at TEXT NOT NULL,
  UNIQUE(event_id, sink),
  FOREIGN KEY(event_id) REFERENCES alert_events(id) ON DELETE CASCADE
);
INSERT INTO alert_events(id, kind, service_id, new_state, dedupe_key, created_at, status)
VALUES(1, 'health_transition', 'svc', 'unhealthy', 'legacy-key', '2026-07-09T12:00:00Z', 'pending');
INSERT INTO alert_dispatches(event_id, sink, status, updated_at)
VALUES(1, 'discord', 'pending', '2026-07-09T12:00:00Z');
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var dispatchSQL string
	if err := store.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='alert_dispatches'`).Scan(&dispatchSQL); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dispatchSQL, "alert_events_legacy") || !strings.Contains(dispatchSQL, "REFERENCES alert_events(id)") {
		t.Fatalf("alert_dispatches schema = %s, want FK to alert_events", dispatchSQL)
	}
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 9, 12, 5, 0, 0, time.UTC)
	if _, err := conn.ExecContext(ctx, `INSERT INTO alert_dispatches(event_id, sink, status, updated_at, updated_at_ns) VALUES(1, 'webhook', 'pending', ?, ?)`, now.Format(time.RFC3339Nano), now.UnixNano()); err != nil {
		t.Fatalf("valid alert_dispatch insert failed after migration: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO alert_dispatches(event_id, sink, status, updated_at, updated_at_ns) VALUES(999, 'invalid', 'pending', ?, ?)`, now.Format(time.RFC3339Nano), now.UnixNano()); err == nil {
		t.Fatal("invalid alert_dispatch insert succeeded, want FK failure")
	}
}

func TestRebuildAlertTablesDiscardsConnectionWhenForeignKeyRestorationFails(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", sqliteDSN(filepath.Join(t.TempDir(), "dashboard.db")))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	restoreErr := errors.New("injected foreign-key restoration failure")
	store := &Store{
		db: db,
		restoreAlertForeignKeys: func(context.Context, *sql.Conn) error {
			return restoreErr
		},
	}
	err = store.rebuildAlertTables(ctx)
	if !errors.Is(err, restoreErr) {
		t.Fatalf("rebuild error = %v, want restoration failure", err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var enabled int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 1 {
		t.Fatalf("reusable connection foreign_keys = %d, want 1", enabled)
	}
}

func TestAlertMigrationRepairsDispatchTableMissingUniqueConstraint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  dedupe_key TEXT NOT NULL,
  created_at TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  updated_at TEXT NOT NULL,
  FOREIGN KEY(event_id) REFERENCES alert_events(id) ON DELETE CASCADE
);
INSERT INTO alert_events(id, kind, service_id, new_state, dedupe_key, created_at, status)
VALUES(1, 'health_transition', 'svc', 'unhealthy', 'missing-unique', '2026-07-09T12:00:00Z', 'pending');
INSERT INTO alert_dispatches(event_id, sink, status, updated_at)
VALUES(1, 'discord', 'pending', '2026-07-09T12:00:00Z');
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if ok, err := store.alertDispatchesHasUniqueEventSink(ctx); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("alert_dispatches missing UNIQUE(event_id, sink) after migration")
	}
	if ok, err := store.alertDispatchesHasAlertEventsFK(ctx); err != nil {
		t.Fatal(err)
	} else if !ok {
		t.Fatal("alert_dispatches missing FK to alert_events after migration")
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO alert_dispatches(event_id, sink, status, updated_at, updated_at_ns) VALUES(1, 'discord', 'pending', '2026-07-09T12:01:00Z', ?)`, time.Date(2026, 7, 9, 12, 1, 0, 0, time.UTC).UnixNano()); err == nil {
		t.Fatal("duplicate event/sink insert succeeded after migration, want unique constraint")
	}
}

func TestAlertDispatchConstraintRebuildMergesDuplicatesByStatusPriority(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  dedupe_key TEXT NOT NULL,
  created_at TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at TEXT,
  delivered_at_ns INTEGER,
  updated_at TEXT NOT NULL,
  updated_at_ns INTEGER NOT NULL DEFAULT 0
);
INSERT INTO alert_events(id, kind, service_id, new_state, dedupe_key, created_at, created_at_ns, status)
VALUES(1, 'health_transition', 'svc', 'unhealthy', 'constraint-merge', '2026-07-09T12:00:00Z', 100, 'delivered');
INSERT INTO alert_dispatches(id, event_id, sink, status, attempts, last_error, delivered_at, delivered_at_ns, updated_at, updated_at_ns)
VALUES
  (1, 1, 'discord', 'delivered', 5, 'older delivered diagnostic', '2026-07-09T12:00:10Z', 110, '2026-07-09T12:00:10Z', 110),
  (2, 1, 'discord', 'pending', 2, 'newer pending diagnostic', NULL, NULL, '2026-07-09T12:00:20Z', 120),
  (3, 1, 'webhook', 'pending', 7, 'older pending diagnostic', NULL, NULL, '2026-07-09T12:00:10Z', 110),
  (4, 1, 'webhook', 'delivered', 1, 'newer delivered diagnostic', '2026-07-09T12:00:20Z', 120, '2026-07-09T12:00:20Z', 120);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, want := range []struct {
		sink      string
		id        int64
		status    string
		attempts  int
		lastError string
	}{
		{sink: "discord", id: 1, status: AlertDispatchStatusDelivered, attempts: 5, lastError: "older delivered diagnostic"},
		{sink: "webhook", id: 4, status: AlertDispatchStatusDelivered, attempts: 1, lastError: "newer delivered diagnostic"},
	} {
		var id int64
		var status string
		var attempts int
		var lastError string
		if err := store.db.QueryRowContext(ctx, `SELECT id, status, attempts, last_error FROM alert_dispatches WHERE event_id=1 AND sink=?`, want.sink).Scan(&id, &status, &attempts, &lastError); err != nil {
			t.Fatal(err)
		}
		if id != want.id || status != want.status || attempts != want.attempts || lastError != want.lastError {
			t.Fatalf("%s dispatch = id:%d status:%q attempts:%d error:%q, want id:%d status:%q attempts:%d error:%q", want.sink, id, status, attempts, lastError, want.id, want.status, want.attempts, want.lastError)
		}
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_dispatches WHERE event_id=1`); got != 2 {
		t.Fatalf("merged dispatch rows = %d, want one per sink", got)
	}
	if got := alertEventStatus(t, store, 1); got != AlertEventStatusDelivered {
		t.Fatalf("event status = %q, want delivered after sticky delivered dispatch merge", got)
	}
}

func TestAlertDispatchForeignKeyEnforcedByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO alert_dispatches(event_id, sink, status, updated_at, updated_at_ns)
VALUES(999, 'discord', 'pending', ?, ?)
`, now.Format(time.RFC3339Nano), now.UnixNano()); err == nil {
		t.Fatal("invalid alert_dispatch insert succeeded, want default FK enforcement")
	}
}

func TestAlertMigrationReconcilesDuplicatePendingLegacyDedupeHashes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
	CREATE TABLE alert_events_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  old_state TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL,
  dedupe_hash TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  claim_id TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  lease_expires_at_ns INTEGER,
  next_attempt_at_ns INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at TEXT,
  delivered_at_ns INTEGER,
  updated_at TEXT NOT NULL,
  updated_at_ns INTEGER NOT NULL DEFAULT 0,
  UNIQUE(event_id, sink),
  FOREIGN KEY(event_id) REFERENCES alert_events_legacy(id) ON DELETE CASCADE
);
INSERT INTO alert_events_legacy(id, kind, service_id, new_state, dedupe_key, dedupe_hash, created_at, created_at_ns, status)
VALUES
  (1, 'health_transition', 'svc-old', 'unhealthy', 'dup-old', 'dup-hash', '2026-07-09T12:00:00Z', 100, 'pending'),
  (2, 'health_transition', 'svc-new', 'unhealthy', 'dup-new', 'dup-hash', '2026-07-09T12:01:00Z', 200, 'pending');
INSERT INTO alert_dispatches_legacy(event_id, sink, status, updated_at, updated_at_ns)
VALUES
  (1, 'discord', 'pending', '2026-07-09T12:00:00Z', 100),
  (2, 'webhook', 'pending', '2026-07-09T12:01:00Z', 200);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, table := range []string{"alert_events_legacy", "alert_dispatches_legacy"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("legacy table %s still exists after duplicate-dedupe migration", table)
		}
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE status='pending' AND dedupe_hash='dup-hash'`); got != 1 {
		t.Fatalf("pending duplicate dedupe rows = %d, want 1", got)
	}
	if got := alertEventStatus(t, store, 1); got != AlertEventStatusFailed {
		t.Fatalf("older duplicate event status = %q, want failed", got)
	}
	if got := alertEventStatus(t, store, 2); got != AlertEventStatusPending {
		t.Fatalf("newest duplicate event status = %q, want pending", got)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT sink FROM alert_dispatches WHERE event_id=2 ORDER BY sink`)
	if err != nil {
		t.Fatal(err)
	}
	var sinks []string
	for rows.Next() {
		var sink string
		if err := rows.Scan(&sink); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		sinks = append(sinks, sink)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(sinks) != 2 || sinks[0] != "discord" || sinks[1] != "webhook" {
		t.Fatalf("newest event dispatch sinks = %#v, want merged discord/webhook", sinks)
	}
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO alert_events(kind, service_id, new_state, dedupe_key, dedupe_hash, created_at, created_at_ns, status)
VALUES('health_transition', 'svc-dup', 'unhealthy', 'dup-third', 'dup-hash', '2026-07-09T12:02:00Z', 300, 'pending')
`); err == nil {
		t.Fatal("duplicate pending dedupe insert succeeded, want repaired unique index")
	}
}

func TestAlertMigrationMergesDuplicatePendingDedupeSameSinkByActionableStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  old_state TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL,
  dedupe_hash TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  claim_id TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  lease_expires_at_ns INTEGER,
  next_attempt_at_ns INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at TEXT,
  delivered_at_ns INTEGER,
  updated_at TEXT NOT NULL,
  updated_at_ns INTEGER NOT NULL DEFAULT 0,
  UNIQUE(event_id, sink),
  FOREIGN KEY(event_id) REFERENCES alert_events(id) ON DELETE CASCADE
);
INSERT INTO alert_events(id, kind, service_id, new_state, dedupe_key, dedupe_hash, created_at, created_at_ns, status)
VALUES
  (1, 'health_transition', 'svc-old', 'unhealthy', 'dup-old', 'same-sink-hash', '2026-07-09T12:00:00Z', 100, 'pending'),
  (2, 'health_transition', 'svc-new', 'unhealthy', 'dup-new', 'same-sink-hash', '2026-07-09T12:01:00Z', 200, 'pending');
INSERT INTO alert_dispatches(id, event_id, sink, status, last_error, delivered_at, delivered_at_ns, updated_at, updated_at_ns)
VALUES
  (1, 1, 'discord', 'in_flight', 'still actionable', NULL, NULL, '2026-07-09T12:00:00Z', 100),
  (2, 2, 'discord', 'pending', 'newer but less actionable', NULL, NULL, '2026-07-09T12:01:00Z', 200);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := alertEventStatus(t, store, 1); got != AlertEventStatusFailed {
		t.Fatalf("older event status = %q, want failed", got)
	}
	if got := alertEventStatus(t, store, 2); got != AlertEventStatusPending {
		t.Fatalf("keeper event status = %q, want pending from merged actionable dispatch", got)
	}
	var dispatchID int64
	var status, lastError string
	if err := store.db.QueryRowContext(ctx, `SELECT id, status, last_error FROM alert_dispatches WHERE event_id=2 AND sink='discord'`).Scan(&dispatchID, &status, &lastError); err != nil {
		t.Fatal(err)
	}
	if dispatchID != 1 || status != AlertDispatchStatusInFlight || lastError != "still actionable" {
		t.Fatalf("merged dispatch = id:%d %q/%q, want original in-flight dispatch ID 1 preserved", dispatchID, status, lastError)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_dispatches WHERE event_id=1`); got != 0 {
		t.Fatalf("duplicate event dispatch rows = %d, want moved/deleted", got)
	}
}

func TestAlertMigrationDeliveredDispatchWinsDuplicatePending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  dedupe_key TEXT NOT NULL,
  dedupe_hash TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  claim_id TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  lease_expires_at_ns INTEGER,
  next_attempt_at_ns INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at TEXT,
  delivered_at_ns INTEGER,
  updated_at TEXT NOT NULL,
  updated_at_ns INTEGER NOT NULL DEFAULT 0,
  UNIQUE(event_id, sink),
  FOREIGN KEY(event_id) REFERENCES alert_events(id) ON DELETE CASCADE
);
INSERT INTO alert_events(id, kind, service_id, new_state, dedupe_key, dedupe_hash, created_at, created_at_ns, status)
VALUES
  (1, 'health_transition', 'svc-old', 'unhealthy', 'dup-old', 'delivered-pending-hash', '2026-07-09T12:00:00Z', 100, 'pending'),
  (2, 'health_transition', 'svc-new', 'unhealthy', 'dup-new', 'delivered-pending-hash', '2026-07-09T12:01:00Z', 200, 'pending');
INSERT INTO alert_dispatches(id, event_id, sink, status, last_error, delivered_at, delivered_at_ns, updated_at, updated_at_ns)
VALUES
  (1, 1, 'discord', 'pending', '', NULL, NULL, '2026-07-09T12:00:00Z', 100),
  (2, 2, 'discord', 'delivered', '', '2026-07-09T12:01:10Z', 210, '2026-07-09T12:01:10Z', 210),
  (3, 2, 'webhook', 'pending', '', NULL, NULL, '2026-07-09T12:01:20Z', 220);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var keptID int64
	var keptStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT id, status FROM alert_dispatches WHERE event_id=2 AND sink='discord'`).Scan(&keptID, &keptStatus); err != nil {
		t.Fatal(err)
	}
	if keptID != 2 || keptStatus != AlertDispatchStatusDelivered {
		t.Fatalf("keeper dispatch = id:%d status:%q, want delivered dispatch id 2 preserved", keptID, keptStatus)
	}
	var duplicateStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM alert_dispatches WHERE id=1`).Scan(&duplicateStatus); err != nil {
		t.Fatal(err)
	}
	if duplicateStatus != AlertDispatchStatusReset {
		t.Fatalf("losing pending dispatch status = %q, want reset", duplicateStatus)
	}
	if got := alertEventStatus(t, store, 2); got != AlertEventStatusPending {
		t.Fatalf("keeper event status = %q, want pending because webhook is still active", got)
	}
	if got, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 10); err != nil {
		t.Fatalf("claim after delivered/pending merge failed: %v", err)
	} else if len(got) != 1 || got[0].Dispatch.Sink != "webhook" {
		t.Fatalf("claim after delivered/pending merge = %#v, want only unrelated webhook dispatch", got)
	}
}

func TestAlertMigrationDeliveredDispatchWinsDuplicateDeadLettered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  dedupe_key TEXT NOT NULL,
  dedupe_hash TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  claim_id TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  lease_expires_at_ns INTEGER,
  next_attempt_at_ns INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at TEXT,
  delivered_at_ns INTEGER,
  updated_at TEXT NOT NULL,
  updated_at_ns INTEGER NOT NULL DEFAULT 0
);
INSERT INTO alert_events(id, kind, service_id, new_state, dedupe_key, dedupe_hash, created_at, created_at_ns, status)
VALUES(1, 'health_transition', 'svc', 'unhealthy', 'dup', 'delivered-dead-hash', '2026-07-09T12:00:00Z', 100, 'pending');
INSERT INTO alert_dispatches(id, event_id, sink, status, last_error, delivered_at, delivered_at_ns, updated_at, updated_at_ns)
VALUES
  (1, 1, 'discord', 'delivered', '', '2026-07-09T12:00:10Z', 110, '2026-07-09T12:00:10Z', 110),
  (2, 1, 'discord', 'dead_lettered', 'terminal diagnostic', NULL, NULL, '2026-07-09T12:01:10Z', 210);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var keptID int64
	var keptStatus string
	if err := store.db.QueryRowContext(ctx, `SELECT id, status FROM alert_dispatches WHERE event_id=1 AND sink='discord'`).Scan(&keptID, &keptStatus); err != nil {
		t.Fatal(err)
	}
	if keptID != 1 || keptStatus != AlertDispatchStatusDelivered {
		t.Fatalf("keeper dispatch = id:%d status:%q, want delivered dispatch id 1 preserved", keptID, keptStatus)
	}
	if got := alertEventStatus(t, store, 1); got != AlertEventStatusDelivered {
		t.Fatalf("keeper event status = %q, want delivered", got)
	}
	if got, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 10); err != nil || len(got) != 0 {
		t.Fatalf("claim after delivered/dead-lettered merge = %#v, err=%v; want no claimable dispatches", got, err)
	}
}

func TestAlertMigrationLeavesTerminalDedupeHistoryUntouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  old_state TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL,
  dedupe_hash TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  claim_id TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  lease_expires_at_ns INTEGER,
  next_attempt_at_ns INTEGER NOT NULL DEFAULT 0,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at TEXT,
  delivered_at_ns INTEGER,
  updated_at TEXT NOT NULL,
  updated_at_ns INTEGER NOT NULL DEFAULT 0,
  UNIQUE(event_id, sink),
  FOREIGN KEY(event_id) REFERENCES alert_events(id) ON DELETE CASCADE
);
INSERT INTO alert_events(id, kind, service_id, new_state, dedupe_key, dedupe_hash, created_at, created_at_ns, status)
VALUES
  (1, 'health_transition', 'svc-old', 'unhealthy', 'delivered-history', 'history-hash', '2026-07-09T12:00:00Z', 100, 'delivered'),
  (2, 'health_transition', 'svc-new', 'unhealthy', 'new-pending', 'history-hash', '2026-07-09T12:01:00Z', 200, 'pending');
INSERT INTO alert_dispatches(id, event_id, sink, status, last_error, delivered_at, delivered_at_ns, updated_at, updated_at_ns)
VALUES
  (1, 1, 'discord', 'delivered', '', '2026-07-09T12:00:10Z', 110, '2026-07-09T12:00:10Z', 110),
  (2, 2, 'discord', 'pending', '', NULL, NULL, '2026-07-09T12:01:00Z', 200);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := alertEventStatus(t, store, 1); got != AlertEventStatusDelivered {
		t.Fatalf("terminal history status = %q, want delivered", got)
	}
	if got := alertEventStatus(t, store, 2); got != AlertEventStatusPending {
		t.Fatalf("new event status = %q, want pending", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE dedupe_hash='history-hash'`); got != 2 {
		t.Fatalf("events with shared history hash = %d, want delivered history plus pending event", got)
	}
	var status string
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM alert_dispatches WHERE event_id=1 AND sink='discord'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != AlertDispatchStatusDelivered {
		t.Fatalf("terminal history dispatch status = %q, want delivered", status)
	}
}

func TestAlertMigrationResumesInterruptedAlertTableRebuild(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  repository TEXT NOT NULL DEFAULT '',
  agent TEXT NOT NULL DEFAULT '',
  old_state TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  lease_expires_at TEXT,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  delivered_at TEXT,
  updated_at TEXT NOT NULL,
  UNIQUE(event_id, sink),
  FOREIGN KEY(event_id) REFERENCES alert_events_legacy(id) ON DELETE CASCADE
);
CREATE TABLE alert_events (id INTEGER PRIMARY KEY AUTOINCREMENT, partial TEXT NOT NULL DEFAULT '');
CREATE TABLE alert_dispatches (id INTEGER PRIMARY KEY AUTOINCREMENT, event_id INTEGER NOT NULL DEFAULT 0, sink TEXT NOT NULL DEFAULT '');
INSERT INTO alert_events_legacy(id, kind, service_id, new_state, dedupe_key, created_at, status)
VALUES(7, 'health_transition', 'svc', 'unhealthy', 'resume-key', '2026-07-09T12:00:00.9Z', 'pending');
INSERT INTO alert_dispatches_legacy(id, event_id, sink, status, updated_at)
VALUES(11, 7, 'discord', 'pending', '2026-07-09T12:00:00.9Z');
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, table := range []string{"alert_events_legacy", "alert_dispatches_legacy"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("legacy table %s still exists after resumed rebuild", table)
		}
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE id=7`); got != 1 {
		t.Fatalf("resumed rebuild event rows = %d, want 1", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_dispatches WHERE id=11 AND event_id=7`); got != 1 {
		t.Fatalf("resumed rebuild dispatch rows = %d, want 1", got)
	}
}

func TestAlertMigrationLocksWhenCurrentAndLegacyTablesBothContainRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL DEFAULT '',
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL DEFAULT 0,
  sink TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  updated_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL DEFAULT '',
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL DEFAULT 0,
  sink TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  updated_at TEXT NOT NULL DEFAULT ''
);
INSERT INTO alert_events_legacy(id, kind, service_id, new_state, dedupe_key, created_at, status)
VALUES(7, 'health_transition', 'legacy-svc', 'unhealthy', 'legacy-key', '2026-07-09T12:00:00Z', 'pending');
INSERT INTO alert_dispatches_legacy(id, event_id, sink, status, updated_at)
VALUES(17, 7, 'discord', 'pending', '2026-07-09T12:00:00Z');
INSERT INTO alert_events(id, kind, service_id, new_state, dedupe_key, created_at, status)
VALUES(8, 'health_transition', 'current-svc', 'unhealthy', 'current-key', '2026-07-09T12:01:00Z', 'pending');
INSERT INTO alert_dispatches(id, event_id, sink, status, updated_at)
VALUES(18, 8, 'webhook', 'pending', '2026-07-09T12:01:00Z');
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open failed for conflicted alert tables: %v", err)
	}
	warnings := strings.Join(store.StartupWarnings(), "\n")
	if !strings.Contains(warnings, "alert state locked") || !strings.Contains(warnings, "both alert_dispatches and alert_dispatches_legacy with rows") {
		t.Fatalf("startup warnings = %q, want alert table conflict guidance", warnings)
	}
	if _, _, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:conflicted-alert-tables",
	}, []string{"discord"}, time.Hour); !errors.Is(err, ErrAlertStateLocked) {
		_ = store.Close()
		t.Fatalf("enqueue err = %v, want ErrAlertStateLocked", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"alert_events", "alert_events_legacy", "alert_dispatches", "alert_dispatches_legacy"} {
		var count int
		if err := db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s rows after failed rebuild = %d, want 1", table, count)
		}
	}
}

func TestAlertMigrationFailureRedactsCurrentAndLegacyAlertSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	secret := "migration-failure-alert-secret"
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL DEFAULT '',
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL DEFAULT 0,
  sink TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL DEFAULT '',
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL DEFAULT 0,
  sink TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  worker_id TEXT NOT NULL DEFAULT '',
  last_error TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL DEFAULT ''
);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, insert := range []struct {
		query string
		args  []any
	}{
		{
			query: `INSERT INTO alert_events_legacy(id, kind, service_id, new_state, reason, dedupe_key, created_at, status)
VALUES(7, 'health_transition', 'legacy-svc', 'unhealthy', ?, ?, '2026-07-09T12:00:00Z', 'pending')`,
			args: []any{"legacy reason " + secret, "legacy-key-" + secret},
		},
		{
			query: `INSERT INTO alert_dispatches_legacy(id, event_id, sink, status, worker_id, last_error, updated_at)
VALUES(17, 7, 'discord', 'pending', ?, ?, '2026-07-09T12:00:00Z')`,
			args: []any{"worker-" + secret, "legacy dispatch " + secret},
		},
		{
			query: `INSERT INTO alert_events(id, kind, service_id, new_state, reason, dedupe_key, created_at, status)
VALUES(8, 'health_transition', 'current-svc', 'unhealthy', ?, ?, '2026-07-09T12:01:00Z', 'pending')`,
			args: []any{"current reason " + secret, "current-key-" + secret},
		},
		{
			query: `INSERT INTO alert_dispatches(id, event_id, sink, status, worker_id, last_error, updated_at)
VALUES(18, 8, 'webhook', 'pending', ?, ?, '2026-07-09T12:01:00Z')`,
			args: []any{"worker-" + secret, "current dispatch " + secret},
		},
	} {
		if _, err := db.ExecContext(ctx, insert.query, insert.args...); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := OpenWithOptions(dbPath, OpenOptions{RedactionValues: []string{secret}})
	if err != nil {
		t.Fatalf("Open failed for conflicted alert tables: %v", err)
	}
	if !strings.Contains(strings.Join(store.StartupWarnings(), "\n"), "alert state locked") {
		_ = store.Close()
		t.Fatalf("startup warnings = %#v, want locked alert state", store.StartupWarnings())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`SELECT reason || ' ' || dedupe_key FROM alert_events`,
		`SELECT reason || ' ' || dedupe_key FROM alert_events_legacy`,
		`SELECT worker_id || ' ' || last_error FROM alert_dispatches`,
		`SELECT worker_id || ' ' || last_error FROM alert_dispatches_legacy`,
	} {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			if strings.Contains(value, secret) {
				_ = rows.Close()
				t.Fatalf("query %q still contains configured secret: %q", query, value)
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAlertDispatchTimestampBackfillIgnoresNullNullableTimestamps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:null-timestamps",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true")
	}
	changed, err := store.backfillAlertDispatchTimestampNS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("timestamp backfill changed rows with NULL nullable timestamps, want no-op")
	}
}

func TestAlertMigrationReopensTerminalEventWithActiveDispatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  dedupe_key TEXT NOT NULL,
  created_at TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  updated_at TEXT NOT NULL,
  UNIQUE(event_id, sink),
  FOREIGN KEY(event_id) REFERENCES alert_events(id) ON DELETE CASCADE
);
INSERT INTO alert_events(id, kind, service_id, new_state, dedupe_key, created_at, status)
VALUES(1, 'health_transition', 'svc', 'unhealthy', 'terminal-active', '2026-07-09T12:00:00Z', 'delivered');
INSERT INTO alert_dispatches(event_id, sink, status, updated_at)
VALUES(1, 'discord', 'pending', '2026-07-09T12:00:00Z');
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if got := alertEventStatus(t, store, 1); got != AlertEventStatusPending {
		t.Fatalf("event status = %q, want pending for active dispatch", got)
	}
	deliveries, err := store.ListUndeliveredAlertDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].Event.ID != 1 || deliveries[0].Dispatch.Sink != "discord" {
		t.Fatalf("undelivered deliveries = %#v, want active discord dispatch", deliveries)
	}
}

func TestAlertDedupeHMACDiffersAcrossInstalls(t *testing.T) {
	t.Parallel()
	firstPath := filepath.Join(t.TempDir(), "dashboard.db")
	first, err := Open(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	firstHash := first.alertDedupeHash("health:svc:secret")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if reopenedHash := reopened.alertDedupeHash("health:svc:secret"); reopenedHash != firstHash {
		t.Fatalf("reopened install hash = %q, want stable %q", reopenedHash, firstHash)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if secondHash := second.alertDedupeHash("health:svc:secret"); secondHash == firstHash {
		t.Fatalf("second install hash = %q, want different keyed HMAC", secondHash)
	}
}

func TestAlertMigrationBackfillsDedupeHashBeforeRedactingLegacyDisplayKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	rawKey := "health:svc:super-secret-token-1234"
	secret := "super-secret-token-1234"
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL DEFAULT '',
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending'
);
INSERT INTO alert_events_legacy(id, kind, service_id, new_state, reason, dedupe_key, created_at, status)
VALUES(7, 'health_transition', 'svc', 'unhealthy', 'legacy reason super-secret-token-1234', ?, '2026-07-09T12:00:00Z', 'pending');
`, rawKey); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	store, err := OpenWithOptions(dbPath, OpenOptions{RedactionValues: []string{secret}})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var migratedHash, displayKey, reason string
	if err := store.db.QueryRowContext(ctx, `SELECT dedupe_hash, dedupe_key, reason FROM alert_events WHERE id=7`).Scan(&migratedHash, &displayKey, &reason); err != nil {
		t.Fatal(err)
	}
	if migratedHash == "" {
		t.Fatal("migrated dedupe_hash is empty")
	}
	if strings.Contains(displayKey, secret) || strings.Contains(reason, secret) {
		t.Fatalf("legacy display fields not redacted: dedupe=%q reason=%q", displayKey, reason)
	}
	fresh, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: rawKey,
	}, []string{"discord"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("fresh enqueue was suppressed, want new row after failed legacy event")
	}
	if fresh.DedupeHash != migratedHash {
		t.Fatalf("fresh dedupe hash = %q, want migrated raw-key hash %q", fresh.DedupeHash, migratedHash)
	}
}

func TestAlertLockedStatePreservesUnhashedLegacyDedupeKeysUntilKeyRestored(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	rawKey := "health:svc:short-recovery-secret-1234"
	secret := "short-recovery-secret-1234"
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL DEFAULT '',
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  dedupe_key TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending'
);
INSERT INTO alert_events_legacy(id, kind, service_id, new_state, reason, dedupe_key, created_at, status)
VALUES(7, 'health_transition', 'svc', 'unhealthy', 'legacy reason short-recovery-secret-1234', ?, '2026-07-09T12:00:00Z', 'pending');
`, rawKey); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	writeTestAlertDedupeKey(t, dbPath)
	key, err := readAlertDedupeKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := (&Store{alertDedupeKey: key}).alertDedupeHash(rawKey)
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	locked, err := OpenWithOptions(dbPath, OpenOptions{RedactionValues: []string{secret}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(locked.StartupWarnings(), "\n"), "alert state locked") {
		_ = locked.Close()
		t.Fatalf("startup warnings = %#v, want locked alert state", locked.StartupWarnings())
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var preservedKey, lockedReason string
	if err := db.QueryRowContext(ctx, `SELECT dedupe_key, reason FROM alert_events_legacy WHERE id=7`).Scan(&preservedKey, &lockedReason); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if preservedKey != rawKey {
		t.Fatalf("locked legacy dedupe_key = %q, want raw recovery key %q", preservedKey, rawKey)
	}
	if strings.Contains(lockedReason, secret) {
		t.Fatalf("locked legacy reason still contains configured secret: %q", lockedReason)
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenWithOptions(dbPath, OpenOptions{RedactionValues: []string{secret}})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var migratedHash, displayKey, reason string
	if err := restored.db.QueryRowContext(ctx, `SELECT dedupe_hash, dedupe_key, reason FROM alert_events WHERE id=7`).Scan(&migratedHash, &displayKey, &reason); err != nil {
		t.Fatal(err)
	}
	if migratedHash != wantHash {
		t.Fatalf("migrated hash = %q, want hash from preserved raw key %q", migratedHash, wantHash)
	}
	if strings.Contains(displayKey, secret) || strings.Contains(reason, secret) {
		t.Fatalf("restored legacy display fields not redacted: dedupe=%q reason=%q", displayKey, reason)
	}
}

func TestAlertDedupeKeyFreshInstallWritesFingerprint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key, err := readAlertDedupeKey(filepath.Join(filepath.Dir(dbPath), "alert-dedupe.key"))
	if err != nil {
		t.Fatal(err)
	}
	var fingerprint string
	if err := store.db.QueryRowContext(ctx, `SELECT value FROM storage_metadata WHERE key=?`, alertDedupeKeyFingerprintMetadata).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	if want := alertDedupeKeyFingerprint(key); fingerprint != want {
		t.Fatalf("fingerprint = %q, want %q", fingerprint, want)
	}
}

func TestAlertDedupeKeyFingerprintAcceptsCorrectKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(filepath.Dir(dbPath), "alert-dedupe.key")
	key, err := readAlertDedupeKey(keyPath)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:correct-key",
	}, []string{"discord"}, time.Hour); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedKey, err := readAlertDedupeKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reopenedKey, key) {
		t.Fatalf("reopened key changed, want original correct key")
	}
	if _, _, err := reopened.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:correct-key-2",
	}, []string{"discord"}, time.Hour); err != nil {
		t.Fatalf("enqueue with correct restored key failed: %v", err)
	}
}

func TestAlertDedupeKeyFingerprintMismatchLocksAlerting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:wrong-key",
	}, []string{"discord"}, time.Hour); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("b", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	restored, err := OpenWithOptions(dbPath, OpenOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "alert state locked: dedupe key mismatch") || !strings.Contains(logs.String(), "alerting.resetOnMissingKey") {
		t.Fatalf("logs = %q, want mismatch lock repair guidance", logs.String())
	}
	if _, _, err := restored.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:wrong-key",
	}, []string{"discord"}, time.Hour); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("enqueue err = %v, want ErrAlertStateLocked", err)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	reset, err := OpenWithOptions(dbPath, OpenOptions{ResetAlertStateOnMissingKey: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reset.Close()
	if got := countRows(t, reset, `SELECT COUNT(*) FROM storage_metadata WHERE key='`+alertDedupeResetPendingMetadata+`'`); got != 0 {
		t.Fatalf("pending reset markers = %d, want cleared after wrong-key reset", got)
	}
	if _, _, err := reset.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:wrong-key-recovered",
	}, []string{"discord"}, time.Hour); err != nil {
		t.Fatalf("enqueue after wrong-key reset failed: %v", err)
	}
}

func TestAlertDedupeKeyConcurrentResetsConverge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seed, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:concurrent-reset",
	}, []string{"discord"}, time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = store.Close()
		t.Fatal("seed event inserted = false, want true")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("b", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type openResult struct {
		store *Store
		err   error
	}
	results := make(chan openResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			opened, err := OpenWithOptions(dbPath, OpenOptions{ResetAlertStateOnMissingKey: true})
			results <- openResult{store: opened, err: err}
		}()
	}
	close(start)
	var openedStores []*Store
	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			for _, opened := range openedStores {
				_ = opened.Close()
			}
			t.Fatalf("concurrent reset open %d failed: %v", i, result.err)
		}
		openedStores = append(openedStores, result.store)
	}
	defer func() {
		for _, opened := range openedStores {
			_ = opened.Close()
		}
	}()
	key, err := readAlertDedupeKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	for i, opened := range openedStores {
		if alertDedupeKeyFingerprint(opened.alertDedupeKey) != alertDedupeKeyFingerprint(key) {
			t.Fatalf("opened store %d has key fingerprint %q, want current file fingerprint %q", i, alertDedupeKeyFingerprint(opened.alertDedupeKey), alertDedupeKeyFingerprint(key))
		}
		var markerCount int
		if err := opened.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_metadata WHERE key=?`, alertDedupeResetPendingMetadata).Scan(&markerCount); err != nil {
			t.Fatal(err)
		}
		if markerCount != 0 {
			t.Fatalf("pending reset marker count = %d, want cleared", markerCount)
		}
		var fingerprint string
		if err := opened.db.QueryRowContext(ctx, `SELECT value FROM storage_metadata WHERE key=?`, alertDedupeKeyFingerprintMetadata).Scan(&fingerprint); err != nil {
			t.Fatal(err)
		}
		if want := alertDedupeKeyFingerprint(key); fingerprint != want {
			t.Fatalf("stored fingerprint = %q, want %q", fingerprint, want)
		}
	}
	if got := alertEventStatus(t, openedStores[0], seed.ID); got != AlertEventStatusReset {
		t.Fatalf("seed event status = %q, want reset after concurrent reset", got)
	}
}

func TestAlertDedupeKeyMissingFingerprintWithHashedRowsLocksAlerting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:missing-fingerprint",
	}, []string{"discord"}, time.Hour); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := deleteStorageMetadata(ctx, store.db, alertDedupeKeyFingerprintMetadata); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("b", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	locked, err := OpenWithOptions(dbPath, OpenOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	if !strings.Contains(logs.String(), "dedupe key fingerprint missing") || !strings.Contains(logs.String(), "alerting.resetOnMissingKey") {
		t.Fatalf("logs = %q, want missing-fingerprint lock guidance", logs.String())
	}
	if _, _, err := locked.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:missing-fingerprint",
	}, []string{"discord"}, time.Hour); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("enqueue err = %v, want ErrAlertStateLocked", err)
	}
	if got := countRows(t, locked, `SELECT COUNT(*) FROM storage_metadata WHERE key='`+alertDedupeKeyFingerprintMetadata+`'`); got != 0 {
		t.Fatalf("fingerprint rows = %d, want missing fingerprint not blessed", got)
	}
}

func TestAlertDedupeKeyTruncatedResetRecovers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:truncated-key",
	}, []string{"discord"}, time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = store.Close()
		t.Fatal("inserted = false, want seed event")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("abcd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	locked, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := locked.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:truncated-key",
	}, []string{"discord"}, time.Hour); !errors.Is(err, ErrAlertStateLocked) {
		_ = locked.Close()
		t.Fatalf("enqueue err = %v, want ErrAlertStateLocked", err)
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	reset, err := OpenWithOptions(dbPath, OpenOptions{ResetAlertStateOnMissingKey: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reset.Close()
	if got := alertEventStatus(t, reset, event.ID); got != AlertEventStatusReset {
		t.Fatalf("old event status = %q, want reset after corrupt-key reset", got)
	}
	if _, err := os.Stat(keyPath + ".corrupt"); err != nil {
		t.Fatalf("corrupt key backup missing: %v", err)
	}
	if _, _, err := reset.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:truncated-key-recovered",
	}, []string{"discord"}, time.Hour); err != nil {
		t.Fatalf("enqueue after corrupt-key reset failed: %v", err)
	}
}

func TestAlertDedupeKeyMissingWithHashedAlertRowsStartsLocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:restored",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour); err != nil {
		_ = store.Close()
		t.Fatal(err)
	} else if !inserted {
		_ = store.Close()
		t.Fatal("inserted = false, want true")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	restored, err := OpenWithOptions(dbPath, OpenOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !strings.Contains(logs.String(), "alert state locked: dedupe key missing") || !strings.Contains(logs.String(), "alerting.resetOnMissingKey") {
		t.Fatalf("logs = %q, want prominent locked-state repair guidance", logs.String())
	}
	if _, _, err := restored.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:restored",
	}, []string{"discord"}, time.Hour); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("enqueue err = %v, want ErrAlertStateLocked", err)
	}
	if _, err := restored.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("claim err = %v, want ErrAlertStateLocked", err)
	}
}

func TestAlertDedupeKeyMissingResetRecoversAlertState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:reset",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = store.Close()
		t.Fatal("inserted = false, want true")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	reset, err := OpenWithOptions(dbPath, OpenOptions{ResetAlertStateOnMissingKey: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reset.Close()
	if got := alertEventStatus(t, reset, event.ID); got != AlertEventStatusReset {
		t.Fatalf("old event status = %q, want reset after missing-key reset", got)
	}
	var dispatchStatus, lastError string
	if err := reset.db.QueryRowContext(ctx, `SELECT status, last_error FROM alert_dispatches WHERE event_id=?`, event.ID).Scan(&dispatchStatus, &lastError); err != nil {
		t.Fatal(err)
	}
	if dispatchStatus != AlertDispatchStatusReset || !strings.Contains(lastError, "alert state reset") {
		t.Fatalf("old dispatch = %q/%q, want reset diagnostic", dispatchStatus, lastError)
	}
	fresh, inserted, err := reset.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:reset",
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted || fresh.ID == event.ID {
		t.Fatalf("fresh event inserted=%v event=%#v, want recovered alert state with new row", inserted, fresh)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("reset dedupe key permissions = %o, want 0600", got)
	}
}

func TestAlertDedupeKeyResetFlagIsOneShotUntilRearmed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	firstArm := "first-arm-super-secret-token"
	secondArm := "second-arm"
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seed, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:first-reset",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = store.Close()
		t.Fatal("seed event inserted = false, want true")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	reset, err := OpenWithOptions(dbPath, OpenOptions{
		ResetAlertStateOnMissingKey: true,
		ResetAlertStateToken:        firstArm,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := alertEventStatus(t, reset, seed.ID); got != AlertEventStatusReset {
		_ = reset.Close()
		t.Fatalf("seed event status = %q, want reset", got)
	}
	active, inserted, err := reset.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:second-incident",
		CreatedAt: time.Date(2026, 7, 9, 12, 1, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		_ = reset.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = reset.Close()
		t.Fatal("active event inserted = false, want true")
	}
	if err := reset.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("abcd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	locked, err := OpenWithOptions(dbPath, OpenOptions{
		Logger:                      slog.New(slog.NewTextHandler(&logs, nil)),
		ResetAlertStateOnMissingKey: true,
		ResetAlertStateToken:        firstArm,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "resetOnMissingKey was already consumed") || !strings.Contains(logs.String(), "resetToken") {
		_ = locked.Close()
		t.Fatalf("logs = %q, want consumed reset guidance", logs.String())
	}
	if strings.Contains(logs.String(), firstArm) {
		_ = locked.Close()
		t.Fatalf("logs leaked raw reset token: %q", logs.String())
	}
	if _, _, err := locked.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:locked-after-consumed-reset",
	}, []string{"discord"}, time.Hour); !errors.Is(err, ErrAlertStateLocked) {
		_ = locked.Close()
		t.Fatalf("enqueue err = %v, want ErrAlertStateLocked", err)
	} else if strings.Contains(err.Error(), firstArm) {
		_ = locked.Close()
		t.Fatalf("lock error leaked raw reset token: %v", err)
	}
	if got := alertEventStatus(t, locked, active.ID); got != AlertEventStatusPending {
		_ = locked.Close()
		t.Fatalf("active event status after stale reset flag = %q, want pending/no data loss", got)
	}
	if err := locked.Close(); err != nil {
		t.Fatal(err)
	}
	rearmed, err := OpenWithOptions(dbPath, OpenOptions{
		ResetAlertStateOnMissingKey: true,
		ResetAlertStateToken:        secondArm,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := alertEventStatus(t, rearmed, active.ID); got != AlertEventStatusReset {
		t.Fatalf("active event status after re-armed reset = %q, want reset", got)
	}
	if _, _, err := rearmed.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:after-rearm",
	}, []string{"discord"}, time.Hour); err != nil {
		t.Fatalf("enqueue after re-armed reset failed: %v", err)
	}
	afterRearm, inserted, err := rearmed.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:reuse-first-arm",
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("after-rearm event inserted = false, want true")
	}
	if got := countRows(t, rearmed, `SELECT COUNT(*) FROM alert_dedupe_reset_consumed`); got != 2 {
		t.Fatalf("consumed reset tokens = %d, want A and B retained", got)
	}
	if err := rearmed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("abcd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reuseA, err := OpenWithOptions(dbPath, OpenOptions{
		ResetAlertStateOnMissingKey: true,
		ResetAlertStateToken:        firstArm,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reuseA.Close()
	if _, _, err := reuseA.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:reuse-a-locked",
	}, []string{"discord"}, time.Hour); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("reuse A enqueue err = %v, want ErrAlertStateLocked", err)
	}
	if got := alertEventStatus(t, reuseA, afterRearm.ID); got != AlertEventStatusPending {
		t.Fatalf("event status after reused reset token = %q, want pending/no data loss", got)
	}
}

func TestAlertDedupeKeyResetPreservesDeliveredHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	deliveredEvent, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:delivered-before-reset",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = store.Close()
		t.Fatal("delivered event inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		_ = store.Close()
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	deliveredDispatch, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, "")
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	activeEvent, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:active-before-reset",
		CreatedAt: time.Date(2026, 7, 9, 12, 1, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = store.Close()
		t.Fatal("active event inserted = false, want true")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	reset, err := OpenWithOptions(dbPath, OpenOptions{ResetAlertStateOnMissingKey: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reset.Close()
	if got := alertEventStatus(t, reset, deliveredEvent.ID); got != AlertEventStatusDelivered {
		t.Fatalf("delivered history status = %q, want delivered", got)
	}
	var preserved AlertDispatch
	var deliveredAt sql.NullString
	if err := reset.db.QueryRowContext(ctx, `
SELECT id, event_id, status, attempts, last_error, delivered_at
FROM alert_dispatches
WHERE id=?
`, deliveredDispatch.ID).Scan(&preserved.ID, &preserved.EventID, &preserved.Status, &preserved.Attempts, &preserved.LastError, &deliveredAt); err != nil {
		t.Fatal(err)
	}
	if preserved.Status != AlertDispatchStatusDelivered || preserved.LastError != "" || !deliveredAt.Valid {
		t.Fatalf("delivered dispatch after reset = %#v delivered_at=%v, want verbatim delivered history", preserved, deliveredAt)
	}
	if got := alertEventStatus(t, reset, activeEvent.ID); got != AlertEventStatusReset {
		t.Fatalf("active event status = %q, want reset", got)
	}
	var activeStatus, activeError string
	if err := reset.db.QueryRowContext(ctx, `SELECT status, last_error FROM alert_dispatches WHERE event_id=?`, activeEvent.ID).Scan(&activeStatus, &activeError); err != nil {
		t.Fatal(err)
	}
	if activeStatus != AlertDispatchStatusReset || !strings.Contains(activeError, "alert state reset") {
		t.Fatalf("active dispatch after reset = %q/%q, want reset diagnostic", activeStatus, activeError)
	}
}

func TestAlertDedupeKeyMissingResetTerminalizesLegacyAlertState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  service_id TEXT NOT NULL DEFAULT '',
  new_state TEXT NOT NULL,
  dedupe_key TEXT NOT NULL,
  dedupe_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  created_at_ns INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE TABLE alert_dispatches_legacy (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id INTEGER NOT NULL,
  sink TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  last_error TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  updated_at_ns INTEGER NOT NULL DEFAULT 0
);
INSERT INTO alert_events_legacy(id, kind, service_id, new_state, dedupe_key, dedupe_hash, created_at, created_at_ns, status)
VALUES(7, 'health_transition', 'svc', 'unhealthy', 'health:svc:legacy-reset', 'restored-keyed-hash', '2026-07-09T12:00:00Z', 100, 'pending');
INSERT INTO alert_dispatches_legacy(id, event_id, sink, status, updated_at, updated_at_ns)
VALUES(8, 7, 'discord', 'pending', '2026-07-09T12:00:00Z', 100);
`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reset, err := OpenWithOptions(dbPath, OpenOptions{ResetAlertStateOnMissingKey: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reset.Close()
	if got := alertEventStatus(t, reset, 7); got != AlertEventStatusReset {
		t.Fatalf("legacy event status = %q, want reset after missing-key reset", got)
	}
	var dispatchStatus, lastError string
	if err := reset.db.QueryRowContext(ctx, `SELECT status, last_error FROM alert_dispatches WHERE id=8`).Scan(&dispatchStatus, &lastError); err != nil {
		t.Fatal(err)
	}
	if dispatchStatus != AlertDispatchStatusReset || !strings.Contains(lastError, "alert state reset") {
		t.Fatalf("legacy dispatch = %q/%q, want reset diagnostic", dispatchStatus, lastError)
	}
	if got, err := reset.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1); err != nil || len(got) != 0 {
		t.Fatalf("legacy reset claim result = %#v, err=%v; want no claimable old rows", got, err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatal(err)
	}
}

func TestAlertDedupeKeyPendingResetMarkerCompletesAfterInterruptedReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:interrupted-reset",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = store.Close()
		t.Fatal("inserted = false, want seed alert event")
	}
	if err := setStorageMetadata(ctx, store.db, alertDedupeResetPendingMetadata, "1"); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("c", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if got := alertEventStatus(t, recovered, event.ID); got != AlertEventStatusReset {
		t.Fatalf("event status = %q, want reset after completing interrupted reset", got)
	}
	var markerCount int
	if err := recovered.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM storage_metadata WHERE key=?`, alertDedupeResetPendingMetadata).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if markerCount != 0 {
		t.Fatalf("pending reset marker count = %d, want cleared", markerCount)
	}
	key, err := readAlertDedupeKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	var fingerprint string
	if err := recovered.db.QueryRowContext(ctx, `SELECT value FROM storage_metadata WHERE key=?`, alertDedupeKeyFingerprintMetadata).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	if want := alertDedupeKeyFingerprint(key); fingerprint != want {
		t.Fatalf("fingerprint after interrupted reset = %q, want %q", fingerprint, want)
	}
}

func TestAlertDedupeKeyPendingResetConsumesOriginalTokenFingerprint(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name            string
		preResetEvent   bool
		preConsumeToken bool
	}{
		{name: "state reset before token consumption", preResetEvent: true},
		{name: "token consumed before state reset", preConsumeToken: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "dashboard.db")
			originalToken := "original-reset-token"
			nextToken := "next-reset-token"
			store, err := Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
				Kind:      "health_transition",
				ServiceID: "svc",
				NewState:  "unhealthy",
				DedupeKey: "health:svc:interrupted-token",
				CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
			}, []string{"discord"}, time.Hour)
			if err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			if !inserted {
				_ = store.Close()
				t.Fatal("inserted = false, want seed alert event")
			}
			key := append([]byte(nil), store.alertDedupeKey...)
			if tc.preResetEvent {
				if _, err := store.db.ExecContext(ctx, `UPDATE alert_events SET status=? WHERE id=?`, AlertEventStatusReset, event.ID); err != nil {
					_ = store.Close()
					t.Fatal(err)
				}
			}
			if err := setStorageMetadata(ctx, store.db, alertDedupeResetPendingMetadata, alertDedupeResetTokenFingerprint(originalToken)); err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			if tc.preConsumeToken {
				tx, err := store.db.BeginTx(ctx, nil)
				if err != nil {
					_ = store.Close()
					t.Fatal(err)
				}
				if err := writeAlertDedupeResetConsumedTx(ctx, tx, originalToken, key); err != nil {
					_ = tx.Rollback()
					_ = store.Close()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					_ = store.Close()
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			recovered, err := OpenWithOptions(dbPath, OpenOptions{
				ResetAlertStateOnMissingKey: true,
				ResetAlertStateToken:        nextToken,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			if got := alertEventStatus(t, recovered, event.ID); got != AlertEventStatusReset {
				t.Fatalf("event status = %q, want reset", got)
			}
			if got := alertResetConsumedCount(t, recovered, originalToken); got != 1 {
				t.Fatalf("original reset token consumed count = %d, want 1", got)
			}
			if got := alertResetConsumedCount(t, recovered, nextToken); got != 0 {
				t.Fatalf("next reset token consumed count = %d, want 0", got)
			}
			if got := countRows(t, recovered, `SELECT COUNT(*) FROM storage_metadata WHERE key='`+alertDedupeResetPendingMetadata+`'`); got != 0 {
				t.Fatalf("pending reset markers = %d, want cleared", got)
			}
		})
	}
}

func TestAlertDedupeKeyMissingWithLegacyPlaintextAlertRowsMigrates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_events_legacy (id INTEGER PRIMARY KEY AUTOINCREMENT, dedupe_key TEXT NOT NULL DEFAULT '');
INSERT INTO alert_events_legacy(dedupe_key) VALUES('legacy-restored');
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
	var hash string
	if err := store.db.QueryRowContext(ctx, `SELECT dedupe_hash FROM alert_events WHERE dedupe_key='legacy-restored'`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Fatal("legacy plaintext row dedupe_hash is empty, want migration backfill with generated key")
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("dedupe key permissions = %o, want 0600", got)
	}
}

func TestAlertDedupeKeyMissingWithLegacyDispatchRowsMigrates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE alert_dispatches_legacy (id INTEGER PRIMARY KEY AUTOINCREMENT, event_id INTEGER NOT NULL DEFAULT 0);
INSERT INTO alert_dispatches_legacy(event_id) VALUES(1);
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
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_dispatches`); got != 0 {
		t.Fatalf("orphan legacy dispatch rows after migration = %d, want 0", got)
	}
}

func TestAlertDedupeFingerprintMismatchLocksOpenStoreWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wrongKey := []byte("01234567890123456789012345678901")
	if err := setStorageMetadata(ctx, store.db, alertDedupeKeyFingerprintMetadata, alertDedupeKeyFingerprint(wrongKey)); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:stale-open-key",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("enqueue err = %v, want ErrAlertStateLocked", err)
	}
	if !strings.Contains(err.Error(), "restart all dashboard processes") {
		t.Fatalf("enqueue err = %v, want rolling-restart guidance", err)
	}
	warnings := strings.Join(store.StartupWarnings(), "\n")
	if !strings.Contains(warnings, "alert dedupe key fingerprint changed") || !strings.Contains(warnings, "restart all dashboard processes") {
		t.Fatalf("startup warnings = %q, want latched fingerprint mismatch guidance", warnings)
	}
	if err := setStorageMetadata(ctx, store.db, alertDedupeKeyFingerprintMetadata, alertDedupeKeyFingerprint(store.alertDedupeKey)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:stale-open-key-after-fix",
		CreatedAt: time.Date(2026, 7, 9, 12, 1, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("enqueue after metadata repair err = %v, want latched ErrAlertStateLocked until restart", err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE dedupe_key='health:svc:stale-open-key'`); got != 0 {
		t.Fatalf("stale-key event rows = %d, want no mis-keyed writes", got)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE dedupe_key='health:svc:stale-open-key-after-fix'`); got != 0 {
		t.Fatalf("post-latch event rows = %d, want no writes until restart", got)
	}
}

func TestAlertDedupeKeyUnreadableFileLocksAlerting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0o000); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	store, err := OpenWithOptions(dbPath, OpenOptions{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatalf("Open returned error for unreadable alert dedupe key: %v", err)
	}
	defer store.Close()
	warnings := strings.Join(store.StartupWarnings(), "\n")
	for _, want := range []string{"alert state locked", "dedupe key unavailable", keyPath} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("startup warnings = %q, want %q", warnings, want)
		}
	}
	if !strings.Contains(logs.String(), "alert state locked") {
		t.Fatalf("logs = %q, want locked alert state", logs.String())
	}
	if _, _, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:locked",
	}, []string{"discord"}, time.Hour); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("enqueue err = %v, want ErrAlertStateLocked", err)
	}
	if _, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("claim err = %v, want ErrAlertStateLocked", err)
	}
}

func TestAlertStateLockedRejectsDispatchCompletion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:locked-completion",
	}, []string{"discord"}, time.Hour); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		_ = store.Close()
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	dispatchID := claimed[0].Dispatch.ID
	claimID := claimed[0].Dispatch.ClaimID
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keyPath, 0o000); err != nil {
		t.Fatal(err)
	}
	locked, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	if _, err := locked.RecordAlertDispatchResult(ctx, dispatchID, "worker-a", claimID, AlertDispatchStatusDelivered, ""); !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("completion err = %v, want ErrAlertStateLocked", err)
	}
}

func TestAlertDedupeKeyConcurrentCreatorsConverge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "dashboard.db")
	keyPath := filepath.Join(dir, "alert-dedupe.key")
	db, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureStorageMetadataTable(ctx, db); err != nil {
		t.Fatal(err)
	}
	const workers = 20
	start := make(chan struct{})
	type result struct {
		key []byte
		err error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			loaded, err := loadOrCreateAlertDedupeKey(ctx, db, dbPath, keyPath, false, defaultAlertDedupeResetToken, slog.Default())
			results <- result{key: loaded.key, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var first string
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		encoded := fmt.Sprintf("%x", result.key)
		if first == "" {
			first = encoded
			continue
		}
		if encoded != first {
			t.Fatalf("key = %s, want converged key %s", encoded, first)
		}
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != first {
		t.Fatalf("stored key = %q, want %q", strings.TrimSpace(string(data)), first)
	}
}

func TestAlertEventDedupeSuppressesPendingAndCooldown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	createdAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	cooldown := time.Hour
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		Target:    "docker/serenity",
		OldState:  "healthy",
		NewState:  "unhealthy",
		Reason:    "container unhealthy",
		DedupeKey: "health:svc:docker/serenity:unhealthy",
		CreatedAt: createdAt,
	}, []string{"discord"}, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true for first event")
	}
	if event.ID == 0 || event.Status != AlertEventStatusPending || !event.CreatedAt.Equal(createdAt) {
		t.Fatalf("event = %#v", event)
	}
	duplicate, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		Target:    "docker/serenity",
		OldState:  "healthy",
		NewState:  "unhealthy",
		Reason:    "duplicate reason should not replace original",
		DedupeKey: "health:svc:docker/serenity:unhealthy",
		CreatedAt: createdAt.Add(time.Minute),
	}, []string{"discord"}, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("inserted = true, want false while prior event is pending")
	}
	if duplicate.ID != event.ID || duplicate.Reason != "container unhealthy" {
		t.Fatalf("duplicate event = %#v, want original event", duplicate)
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Event.ID != event.ID || claimed[0].Dispatch.Sink != "discord" {
		t.Fatalf("claimed = %#v, want discord dispatch for event %d", claimed, event.ID)
	}
	if _, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, ""); err != nil {
		t.Fatal(err)
	}
	deliveredAt := createdAt.Add(5 * time.Minute)
	if _, err := store.db.ExecContext(ctx, `
UPDATE alert_dispatches SET delivered_at=?, delivered_at_ns=?, updated_at=?, updated_at_ns=? WHERE event_id=?
`, deliveredAt.Format(time.RFC3339), deliveredAt.UnixNano(), deliveredAt.Format(time.RFC3339), deliveredAt.UnixNano(), event.ID); err != nil {
		t.Fatal(err)
	}
	withinCooldown, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		Target:    "docker/serenity",
		OldState:  "healthy",
		NewState:  "unhealthy",
		Reason:    "still inside cooldown",
		DedupeKey: "health:svc:docker/serenity:unhealthy",
		CreatedAt: deliveredAt.Add(30 * time.Minute),
	}, []string{"discord"}, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("inserted = true, want false inside cooldown")
	}
	if withinCooldown.ID != event.ID {
		t.Fatalf("within cooldown event ID = %d, want original %d", withinCooldown.ID, event.ID)
	}
	afterCooldown, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		Target:    "docker/serenity",
		OldState:  "healthy",
		NewState:  "unhealthy",
		Reason:    "fresh incident after cooldown",
		DedupeKey: "health:svc:docker/serenity:unhealthy",
		CreatedAt: deliveredAt.Add(cooldown + time.Minute),
	}, []string{"discord"}, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true after cooldown")
	}
	if afterCooldown.ID == event.ID {
		t.Fatalf("after cooldown event reused ID %d, want fresh event", afterCooldown.ID)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE dedupe_key='health:svc:docker/serenity:unhealthy'`); got != 2 {
		t.Fatalf("alert event history rows = %d, want 2", got)
	}
}

func TestAlertDispatchRejectsSecretBearingSinkIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "dashboard.db"), OpenOptions{
		RedactionValues: []string{"embedded-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, _, err = store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:sink-identity",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"webhook-embedded-secret-a", "webhook-embedded-secret-b"}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "registered secret") {
		t.Fatalf("EnqueueAlertEvent error = %v, want secret-bearing sink rejection", err)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_dispatches`); got != 0 {
		t.Fatalf("persisted alert dispatches = %d, want 0", got)
	}
}

func TestAlertMutationsLockWhenDedupeKeySidecarChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Remove(filepath.Join(dir, "alert-dedupe.key")); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.EnqueueAlertEvent(ctx, AlertEvent{Kind: "health_transition", ServiceID: "svc", NewState: "unhealthy", DedupeKey: "sidecar-loss"}, []string{"discord"}, time.Minute)
	if !errors.Is(err, ErrAlertStateLocked) {
		t.Fatalf("EnqueueAlertEvent error = %v, want ErrAlertStateLocked", err)
	}
}

func TestAlertEnqueueRejectsTerminalEventStatusAndUnconfiguredSink(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := OpenWithOptions(filepath.Join(t.TempDir(), "dashboard.db"), OpenOptions{AlertSinkNames: []string{"discord"}, AlertSinkAllowlist: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event := AlertEvent{Kind: "health_transition", ServiceID: "svc", NewState: "unhealthy", DedupeKey: "terminal", Status: AlertEventStatusDelivered}
	if _, _, err := store.EnqueueAlertEvent(ctx, event, []string{"discord"}, time.Minute); err == nil || !strings.Contains(err.Error(), "empty or pending") {
		t.Fatalf("terminal status error = %v", err)
	}
	event.Status, event.DedupeKey = "", "unconfigured"
	if _, _, err := store.EnqueueAlertEvent(ctx, event, []string{"webhook"}, time.Minute); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured sink error = %v", err)
	}
}

func TestAlertEventPendingDedupeAddsNewlyRequestedSinks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	createdAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		Reason:    "original pending alert",
		DedupeKey: "health:svc:extra-sink",
		CreatedAt: createdAt,
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want first event inserted")
	}
	suppressed, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		Reason:    "repeat with newly configured sink",
		DedupeKey: "health:svc:extra-sink",
		CreatedAt: createdAt.Add(time.Minute),
	}, []string{"discord", "webhook"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("inserted = true, want pending dedupe suppression")
	}
	if suppressed.ID != event.ID || suppressed.Reason != "original pending alert" {
		t.Fatalf("suppressed event = %#v, want original event %#v", suppressed, event)
	}
	rows, err := store.db.QueryContext(ctx, `SELECT sink FROM alert_dispatches WHERE event_id=? ORDER BY sink`, event.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sinks []string
	for rows.Next() {
		var sink string
		if err := rows.Scan(&sink); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		sinks = append(sinks, sink)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(sinks) != 2 || sinks[0] != "discord" || sinks[1] != "webhook" {
		t.Fatalf("dispatch sinks = %#v, want original plus newly requested sink", sinks)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE dedupe_key='health:svc:extra-sink'`); got != 1 {
		t.Fatalf("event rows = %d, want original event unchanged", got)
	}
}

func TestAlertEventDedupeUsesRawHashNotRedactedDisplayKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.AddRedactionValues("secret-one", "secret-two")
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	first, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:secret-one",
		CreatedAt: base,
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("first insert suppressed")
	}
	second, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:secret-two",
		CreatedAt: base.Add(time.Second),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("second insert suppressed by redacted display key, want distinct raw hash")
	}
	if first.DedupeKey != second.DedupeKey {
		t.Fatalf("display keys = %q and %q, want same redacted display key", first.DedupeKey, second.DedupeKey)
	}
	if first.DedupeHash == second.DedupeHash {
		t.Fatalf("dedupe hashes both %q, want distinct raw-key hashes", first.DedupeHash)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE dedupe_key='health:svc:[REDACTED]'`); got != 2 {
		t.Fatalf("redacted-display event rows = %d, want 2", got)
	}
}

func TestAlertEventConcurrentEnqueueSameKeyCreatesOnePendingEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const workers = 12
	start := make(chan struct{})
	type result struct {
		event    AlertEvent
		inserted bool
		err      error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
				Kind:      "health_transition",
				ServiceID: "svc",
				Target:    "docker/serenity",
				NewState:  "unhealthy",
				Reason:    fmt.Sprintf("attempt %d", i),
				DedupeKey: "health:svc:docker/serenity:unhealthy",
				CreatedAt: time.Date(2026, 7, 9, 12, 0, i, 0, time.UTC),
			}, []string{"discord"}, time.Hour)
			results <- result{event: event, inserted: inserted, err: err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	insertedCount := 0
	eventIDs := map[int64]struct{}{}
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.inserted {
			insertedCount++
		}
		eventIDs[result.event.ID] = struct{}{}
	}
	if insertedCount != 1 {
		t.Fatalf("inserted count = %d, want 1", insertedCount)
	}
	if len(eventIDs) != 1 {
		t.Fatalf("event IDs = %#v, want all callers to see one pending event", eventIDs)
	}
	if got := countRows(t, store, `SELECT COUNT(*) FROM alert_events WHERE status='pending' AND dedupe_hash='`+store.alertDedupeHash("health:svc:docker/serenity:unhealthy")+`'`); got != 1 {
		t.Fatalf("pending dedupe rows = %d, want 1", got)
	}
}

func TestAlertDispatchesKeepEventPendingUntilAllSinksTerminalAcrossRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		Target:    "docker/serenity",
		OldState:  "healthy",
		NewState:  "unhealthy",
		Reason:    "container unhealthy",
		DedupeKey: "health:svc:docker/serenity:unhealthy",
		CreatedAt: createdAt,
	}, []string{"discord", "webhook"}, time.Hour)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if !inserted {
		_ = store.Close()
		t.Fatal("inserted = false, want true")
	}
	deliveries, err := store.ListUndeliveredAlertDeliveries(ctx, 10)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if got, want := alertDeliverySinks(deliveries), []string{"discord", "webhook"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		_ = store.Close()
		t.Fatalf("pending delivery sinks = %#v, want %#v", got, want)
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Dispatch.Sink != "discord" {
		_ = store.Close()
		t.Fatalf("claimed = %#v, want discord", claimed)
	}
	dispatch, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, "")
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if dispatch.Attempts != 1 || dispatch.LastError != "" || dispatch.DeliveredAt == nil || dispatch.Status != AlertDispatchStatusDelivered {
		_ = store.Close()
		t.Fatalf("delivered dispatch = %#v", dispatch)
	}
	deliveries, err = store.ListUndeliveredAlertDeliveries(ctx, 10)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if got, want := alertDeliverySinks(deliveries), []string{"webhook"}; len(got) != len(want) || got[0] != want[0] {
		_ = store.Close()
		t.Fatalf("pending delivery sinks after one success = %#v, want %#v", got, want)
	}
	if got := alertEventStatus(t, store, event.ID); got != AlertEventStatusPending {
		_ = store.Close()
		t.Fatalf("event status = %q, want pending until every sink is terminal", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deliveries, err = store.ListUndeliveredAlertDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := alertDeliverySinks(deliveries), []string{"webhook"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("pending delivery sinks after restart = %#v, want %#v", got, want)
	}
	claimed, err = store.ClaimPendingAlertDeliveries(ctx, "worker-b", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Dispatch.Sink != "webhook" {
		t.Fatalf("claimed after restart = %#v, want webhook", claimed)
	}
	dispatch, err = store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-b", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, "")
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Status != AlertDispatchStatusDelivered || dispatch.DeliveredAt == nil {
		t.Fatalf("second delivered dispatch = %#v", dispatch)
	}
	deliveries, err = store.ListUndeliveredAlertDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 0 {
		t.Fatalf("undelivered after both sinks terminal = %#v, want none", deliveries)
	}
	if got := alertEventStatus(t, store, event.ID); got != AlertEventStatusDelivered {
		t.Fatalf("event status = %q, want delivered after every sink is terminal", got)
	}
}

func TestAlertDispatchClaimersGetDisjointRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:unhealthy",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord", "home-assistant", "webhook-a", "webhook-b"}, time.Hour); err != nil {
		t.Fatal(err)
	} else if !inserted {
		t.Fatal("inserted = false, want true")
	}
	start := make(chan struct{})
	results := make(chan []AlertDelivery, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, worker := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			<-start
			deliveries, err := store.ClaimPendingAlertDeliveries(ctx, worker, time.Minute, 2)
			if err != nil {
				errs <- err
				return
			}
			results <- deliveries
		}(worker)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := map[int64]string{}
	total := 0
	for deliveries := range results {
		total += len(deliveries)
		for _, delivery := range deliveries {
			if previous, ok := seen[delivery.Dispatch.ID]; ok {
				t.Fatalf("dispatch %d claimed by both %s and %s", delivery.Dispatch.ID, previous, delivery.Dispatch.WorkerID)
			}
			seen[delivery.Dispatch.ID] = delivery.Dispatch.WorkerID
		}
	}
	if total != 4 {
		t.Fatalf("claimed deliveries = %d, want 4", total)
	}
}

func TestAlertDispatchBatchDeliveryKeysAreDistinct(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:delivery-keys",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord", "home-assistant", "webhook"}, time.Hour); err != nil {
		t.Fatal(err)
	} else if !inserted {
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 3 {
		t.Fatalf("claimed = %#v, want three dispatches", claimed)
	}
	keys := map[string]struct{}{}
	for _, delivery := range claimed {
		key := delivery.Dispatch.DeliveryKey()
		if key != fmt.Sprint(delivery.Dispatch.ID) {
			t.Fatalf("delivery key = %q for dispatch %#v, want stable dispatch ID", key, delivery.Dispatch)
		}
		keys[key] = struct{}{}
	}
	if len(keys) != 3 {
		t.Fatalf("delivery keys = %#v, want one distinct key per dispatch", keys)
	}
}

func TestAlertDispatchSameWorkerRepeatedClaimsUseDisjointClaimIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:unhealthy",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord", "home-assistant", "webhook-a", "webhook-b"}, time.Hour); err != nil {
		t.Fatal(err)
	} else if !inserted {
		t.Fatal("inserted = false, want true")
	}
	first, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("claim batches = %d/%d, want 2/2", len(first), len(second))
	}
	firstClaimID := first[0].Dispatch.ClaimID
	secondClaimID := second[0].Dispatch.ClaimID
	if firstClaimID == "" || secondClaimID == "" || firstClaimID == secondClaimID {
		t.Fatalf("claim IDs = %q/%q, want non-empty distinct IDs", firstClaimID, secondClaimID)
	}
	seen := map[int64]struct{}{}
	for _, delivery := range append(first, second...) {
		if _, ok := seen[delivery.Dispatch.ID]; ok {
			t.Fatalf("dispatch %d was returned by repeated claims", delivery.Dispatch.ID)
		}
		seen[delivery.Dispatch.ID] = struct{}{}
	}
}

func TestAlertDispatchExpiredLeaseIsReclaimed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:unhealthy",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	firstDeliveryKey := claimed[0].Dispatch.DeliveryKey()
	expired := time.Now().UTC().Add(-time.Minute)
	if _, err := store.db.ExecContext(ctx, `UPDATE alert_dispatches SET lease_expires_at=?, lease_expires_at_ns=? WHERE id=?`, expired.Format(time.RFC3339Nano), expired.UnixNano(), claimed[0].Dispatch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-b", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Dispatch.ID != claimed[0].Dispatch.ID || reclaimed[0].Dispatch.WorkerID != "worker-b" {
		t.Fatalf("reclaimed = %#v, want worker-b to reclaim dispatch %d", reclaimed, claimed[0].Dispatch.ID)
	}
	if reclaimed[0].Dispatch.DeliveryKey() != firstDeliveryKey {
		t.Fatalf("reclaimed delivery key = %q, want stable key %q across retries", reclaimed[0].Dispatch.DeliveryKey(), firstDeliveryKey)
	}
	if _, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, ""); !errors.Is(err, ErrAlertDispatchClaimNotHeld) {
		t.Fatalf("old worker completion err = %v, want ErrAlertDispatchClaimNotHeld", err)
	}
	if _, err := store.RecordAlertDispatchResult(ctx, reclaimed[0].Dispatch.ID, "worker-b", reclaimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, ""); err != nil {
		t.Fatal(err)
	}
	if got := alertEventStatus(t, store, event.ID); got != AlertEventStatusDelivered {
		t.Fatalf("event status = %q, want delivered", got)
	}
}

func TestAlertDispatchExpiredLeaseCompletionIsRejectedAndRemainsDue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	policy := AlertRetryPolicy{
		MaxAttempts:     2,
		InitialInterval: time.Second,
		MaxInterval:     time.Second,
	}
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:late-completion",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour); err != nil {
		t.Fatal(err)
	} else if !inserted {
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-a", time.Minute, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	expired := time.Now().UTC().Add(-time.Minute)
	if _, err := store.db.ExecContext(ctx, `UPDATE alert_dispatches SET lease_expires_at=?, lease_expires_at_ns=? WHERE id=?`, expired.Format(time.RFC3339Nano), expired.UnixNano(), claimed[0].Dispatch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordAlertDispatchResultWithRetryPolicy(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, "", policy); !errors.Is(err, ErrAlertDispatchLeaseExpired) {
		t.Fatalf("late completion err = %v, want ErrAlertDispatchLeaseExpired", err)
	}
	var attempts int
	var status, lastError string
	var deliveredAt sql.NullString
	if err := store.db.QueryRowContext(ctx, `SELECT attempts, status, last_error, delivered_at FROM alert_dispatches WHERE id=?`, claimed[0].Dispatch.ID).Scan(&attempts, &status, &lastError, &deliveredAt); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || status != AlertDispatchStatusInFlight || lastError != alertDispatchLeaseExpiredError || deliveredAt.Valid {
		t.Fatalf("dispatch after late completion = attempts %d status %q error %q delivered %v; want in-flight expired retry note", attempts, status, lastError, deliveredAt.Valid)
	}
	reclaimed, err := store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-b", time.Minute, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Dispatch.ID != claimed[0].Dispatch.ID || reclaimed[0].Dispatch.Attempts != 1 {
		t.Fatalf("reclaimed after late completion = %#v, want expired row still due with attempts=1", reclaimed)
	}
}

func TestAlertDispatchTransientFailureBackoffAndRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	policy := AlertRetryPolicy{
		MaxAttempts:     5,
		InitialInterval: time.Hour,
		MaxInterval:     3 * time.Hour,
	}
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:backoff",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour); err != nil {
		_ = store.Close()
		t.Fatal(err)
	} else if !inserted {
		_ = store.Close()
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-a", time.Minute, 1, policy)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		_ = store.Close()
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	firstFailure, err := store.RecordAlertDispatchResultWithRetryPolicy(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusPending, "temporary failure", policy)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if firstFailure.Status != AlertDispatchStatusPending || firstFailure.Attempts != 1 || firstFailure.NextAttemptAt == nil {
		_ = store.Close()
		t.Fatalf("first failure dispatch = %#v, want pending attempt with next attempt", firstFailure)
	}
	if got := firstFailure.NextAttemptAt.Sub(firstFailure.UpdatedAt); got != policy.InitialInterval {
		_ = store.Close()
		t.Fatalf("first backoff = %s, want %s", got, policy.InitialInterval)
	}
	replayedFailure, err := store.RecordAlertDispatchResultWithRetryPolicy(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusPending, "temporary failure", policy)
	if err != nil {
		_ = store.Close()
		t.Fatalf("replayed requeue completion failed: %v", err)
	}
	if replayedFailure.ID != firstFailure.ID || replayedFailure.Status != AlertDispatchStatusPending || replayedFailure.Attempts != firstFailure.Attempts || replayedFailure.NextAttemptAt == nil || !replayedFailure.NextAttemptAt.Equal(*firstFailure.NextAttemptAt) {
		_ = store.Close()
		t.Fatalf("replayed failure = %#v, want persisted first failure %#v", replayedFailure, firstFailure)
	}
	claimed, err = store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-b", time.Minute, 1, policy)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		_ = store.Close()
		t.Fatalf("claimed before next_attempt_at = %#v, want none", claimed)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	claimed, err = store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-c", time.Minute, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 0 {
		t.Fatalf("claimed before next_attempt_at after restart = %#v, want none", claimed)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE alert_dispatches SET next_attempt_at_ns=? WHERE id=?`, time.Now().UTC().Add(-time.Second).UnixNano(), firstFailure.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err = store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-d", time.Minute, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Dispatch.ID != firstFailure.ID || claimed[0].Dispatch.Attempts != 1 {
		t.Fatalf("claimed after due = %#v, want same dispatch with attempts still 1", claimed)
	}
	secondFailure, err := store.RecordAlertDispatchResultWithRetryPolicy(ctx, claimed[0].Dispatch.ID, "worker-d", claimed[0].Dispatch.ClaimID, AlertDispatchStatusPending, "temporary failure again", policy)
	if err != nil {
		t.Fatal(err)
	}
	if secondFailure.Attempts != 2 || secondFailure.NextAttemptAt == nil {
		t.Fatalf("second failure dispatch = %#v, want second pending attempt", secondFailure)
	}
	if got, want := secondFailure.NextAttemptAt.Sub(secondFailure.UpdatedAt), 2*policy.InitialInterval; got != want {
		t.Fatalf("second backoff = %s, want %s", got, want)
	}
}

func TestAlertDispatchExpiredLeaseCountsAttemptsAndDeadLettersAtMax(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	policy := AlertRetryPolicy{
		MaxAttempts:     2,
		InitialInterval: time.Second,
		MaxInterval:     time.Second,
	}
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:crash",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-a", time.Minute, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Dispatch.Attempts != 0 {
		t.Fatalf("initial claim = %#v, want one dispatch with 0 attempts", claimed)
	}
	expired := time.Now().UTC().Add(-time.Minute)
	if _, err := store.db.ExecContext(ctx, `UPDATE alert_dispatches SET lease_expires_at=?, lease_expires_at_ns=? WHERE id=?`, expired.Format(time.RFC3339Nano), expired.UnixNano(), claimed[0].Dispatch.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-b", time.Minute, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Dispatch.ID != claimed[0].Dispatch.ID || reclaimed[0].Dispatch.Attempts != 1 {
		t.Fatalf("first reclaim = %#v, want attempts incremented to 1", reclaimed)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE alert_dispatches SET lease_expires_at=?, lease_expires_at_ns=? WHERE id=?`, expired.Format(time.RFC3339Nano), expired.UnixNano(), reclaimed[0].Dispatch.ID); err != nil {
		t.Fatal(err)
	}
	finalClaim, err := store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-c", time.Minute, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalClaim) != 0 {
		t.Fatalf("claim at max attempts = %#v, want none", finalClaim)
	}
	var attempts int
	var status, lastError string
	if err := store.db.QueryRowContext(ctx, `SELECT attempts, status, last_error FROM alert_dispatches WHERE id=?`, reclaimed[0].Dispatch.ID).Scan(&attempts, &status, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || status != AlertDispatchStatusDeadLettered || lastError != alertDispatchLeaseExpiredError {
		t.Fatalf("dispatch attempts/status/error = %d/%q/%q, want 2/%q/%q", attempts, status, lastError, AlertDispatchStatusDeadLettered, alertDispatchLeaseExpiredError)
	}
	if got := alertEventStatus(t, store, event.ID); got != AlertEventStatusFailed {
		t.Fatalf("event status = %q, want failed after max-attempt timeout", got)
	}
}

func TestAlertDispatchFinalLeaseTimeoutReplacesTransientError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	policy := AlertRetryPolicy{
		MaxAttempts:     2,
		InitialInterval: time.Hour,
		MaxInterval:     time.Hour,
	}
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:timeout-replaces-error",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour); err != nil {
		t.Fatal(err)
	} else if !inserted {
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-a", time.Minute, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	failed, err := store.RecordAlertDispatchResultWithRetryPolicy(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusPending, "temporary network failure", policy)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Attempts != 1 || failed.LastError != "temporary network failure" {
		t.Fatalf("first failure = %#v, want transient error at attempt 1", failed)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE alert_dispatches SET next_attempt_at_ns=? WHERE id=?`, time.Now().UTC().Add(-time.Second).UnixNano(), failed.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-b", time.Minute, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 {
		t.Fatalf("reclaimed = %#v, want one dispatch", reclaimed)
	}
	expired := time.Now().UTC().Add(-time.Minute)
	if _, err := store.db.ExecContext(ctx, `UPDATE alert_dispatches SET lease_expires_at=?, lease_expires_at_ns=? WHERE id=?`, expired.Format(time.RFC3339Nano), expired.UnixNano(), reclaimed[0].Dispatch.ID); err != nil {
		t.Fatal(err)
	}
	finalClaim, err := store.ClaimPendingAlertDeliveriesWithRetryPolicy(ctx, "worker-c", time.Minute, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalClaim) != 0 {
		t.Fatalf("final claim = %#v, want dead-letter instead of reclaim", finalClaim)
	}
	var attempts int
	var status, lastError string
	if err := store.db.QueryRowContext(ctx, `SELECT attempts, status, last_error FROM alert_dispatches WHERE id=?`, failed.ID).Scan(&attempts, &status, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || status != AlertDispatchStatusDeadLettered || lastError != alertDispatchLeaseExpiredError {
		t.Fatalf("terminal dispatch = attempts %d status %q error %q, want timeout dead-letter", attempts, status, lastError)
	}
}

func TestAlertDispatchLeaseReclaimUsesIntegerTimestamp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:lease-order",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"expired-by-ns", "future-by-ns"}, time.Hour); err != nil {
		t.Fatal(err)
	} else if !inserted {
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed = %#v, want two dispatches", claimed)
	}
	expiredID := claimed[0].Dispatch.ID
	futureID := claimed[1].Dispatch.ID
	expiredNS := time.Now().UTC().Add(-time.Second)
	futureNS := time.Now().UTC().Add(time.Hour)
	if _, err := store.db.ExecContext(ctx, `
UPDATE alert_dispatches
SET lease_expires_at='2999-01-01T00:00:00.9Z', lease_expires_at_ns=?
WHERE id=?
`, expiredNS.UnixNano(), expiredID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE alert_dispatches
SET lease_expires_at='1970-01-01T00:00:00.1Z', lease_expires_at_ns=?
WHERE id=?
`, futureNS.UnixNano(), futureID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-b", time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 1 || reclaimed[0].Dispatch.ID != expiredID {
		t.Fatalf("reclaimed = %#v, want only dispatch %d expired by integer ns", reclaimed, expiredID)
	}
}

func TestAlertDispatchCompletedRowNotClaimedAfterRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:unhealthy",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour); err != nil {
		_ = store.Close()
		t.Fatal(err)
	} else if !inserted {
		_ = store.Close()
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		_ = store.Close()
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	if _, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, ""); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if retry, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, ""); err != nil {
		_ = store.Close()
		t.Fatalf("retry after committed completion failed: %v", err)
	} else if retry.Status != AlertDispatchStatusDelivered || retry.Attempts != 1 {
		_ = store.Close()
		t.Fatalf("retry dispatch = %#v, want delivered without duplicate attempt", retry)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reclaimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-b", time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(reclaimed) != 0 {
		t.Fatalf("claimed after completed restart = %#v, want none", reclaimed)
	}
}

func TestAlertEnqueueRetriesSQLiteBusyBeyondShortBudget(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	locker, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	type enqueueResult struct {
		event    AlertEvent
		inserted bool
		err      error
	}
	done := make(chan enqueueResult, 1)
	go func() {
		event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
			Kind:      "health_transition",
			ServiceID: "svc",
			NewState:  "unhealthy",
			DedupeKey: "health:svc:enqueue-busy",
			CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
		}, []string{"discord"}, time.Hour)
		done <- enqueueResult{event: event, inserted: inserted, err: err}
	}()
	time.Sleep(600 * time.Millisecond)
	if _, err := locker.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("enqueue after busy retry failed: %v", result.err)
		}
		if !result.inserted || result.event.ID == 0 {
			t.Fatalf("enqueue result = inserted %v event %#v, want inserted event", result.inserted, result.event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue did not complete after busy lock was released")
	}
}

func TestAlertClaimRetriesSQLiteBusyBeyondShortBudget(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:claim-busy",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour); err != nil {
		t.Fatal(err)
	} else if !inserted {
		t.Fatal("inserted = false, want true")
	}
	locker, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	type claimResult struct {
		deliveries []AlertDelivery
		err        error
	}
	done := make(chan claimResult, 1)
	go func() {
		deliveries, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
		done <- claimResult{deliveries: deliveries, err: err}
	}()
	time.Sleep(600 * time.Millisecond)
	if _, err := locker.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("claim after busy retry failed: %v", result.err)
		}
		if len(result.deliveries) != 1 || result.deliveries[0].Dispatch.Status != AlertDispatchStatusInFlight {
			t.Fatalf("claim result = %#v, want one leased dispatch", result.deliveries)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("claim did not complete after busy lock was released")
	}
}

func TestAlertDispatchCompletionRetriesSQLiteBusy(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:busy",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour); err != nil {
		t.Fatal(err)
	} else if !inserted {
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	locker, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	if _, err := locker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	if _, err := locker.ExecContext(ctx, `UPDATE alert_dispatches SET updated_at=updated_at WHERE id=?`, claimed[0].Dispatch.ID); err != nil {
		_, _ = locker.ExecContext(ctx, `ROLLBACK`)
		t.Fatal(err)
	}
	type recordResult struct {
		dispatch AlertDispatch
		err      error
	}
	done := make(chan recordResult, 1)
	go func() {
		dispatch, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDelivered, "")
		done <- recordResult{dispatch: dispatch, err: err}
	}()
	time.Sleep(600 * time.Millisecond)
	if _, err := locker.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("record after busy retry failed: %v", result.err)
		}
		if result.dispatch.Status != AlertDispatchStatusDelivered {
			t.Fatalf("dispatch status = %q, want delivered", result.dispatch.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("record did not complete after busy lock was released")
	}
}

func TestAlertPartialOutcomeSuppressesWithinCooldown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	base := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:partial",
		CreatedAt: base,
	}, []string{"discord", "webhook"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed = %#v, want two dispatches", claimed)
	}
	for _, delivery := range claimed {
		status := AlertDispatchStatusDelivered
		if delivery.Dispatch.Sink == "webhook" {
			status = AlertDispatchStatusDeadLettered
		}
		if _, err := store.RecordAlertDispatchResult(ctx, delivery.Dispatch.ID, "worker-a", delivery.Dispatch.ClaimID, status, "permanent failure"); err != nil {
			t.Fatal(err)
		}
	}
	if got := alertEventStatus(t, store, event.ID); got != AlertEventStatusPartial {
		t.Fatalf("event status = %q, want partial", got)
	}
	closedAt := base.Add(5 * time.Minute)
	if _, err := store.db.ExecContext(ctx, `UPDATE alert_dispatches SET delivered_at=COALESCE(delivered_at, ?), delivered_at_ns=COALESCE(delivered_at_ns, ?), updated_at=?, updated_at_ns=? WHERE event_id=?`, closedAt.Format(time.RFC3339Nano), closedAt.UnixNano(), closedAt.Format(time.RFC3339Nano), closedAt.UnixNano(), event.ID); err != nil {
		t.Fatal(err)
	}
	again, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		Reason:    "inside partial cooldown",
		DedupeKey: "health:svc:partial",
		CreatedAt: base.Add(10 * time.Minute),
	}, []string{"discord", "webhook"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("inserted = true, want partial outcome to suppress inside cooldown")
	}
	if again.ID != event.ID {
		t.Fatalf("suppressed event ID = %d, want original %d", again.ID, event.ID)
	}
}

func TestAlertDeadLetteredDispatchClosesSingleSinkEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		Reason:    "container unhealthy",
		DedupeKey: "health:svc:unhealthy",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].Dispatch.Sink != "discord" {
		t.Fatalf("claimed = %#v, want discord", claimed)
	}
	dispatch, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDeadLettered, "permanent failure")
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Status != AlertDispatchStatusDeadLettered || dispatch.LastError != "permanent failure" || dispatch.DeliveredAt != nil {
		t.Fatalf("dead-lettered dispatch = %#v", dispatch)
	}
	deliveries, err := store.ListUndeliveredAlertDeliveries(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 0 {
		t.Fatalf("undelivered after dead-letter = %#v, want none", deliveries)
	}
	if got := alertEventStatus(t, store, event.ID); got != AlertEventStatusFailed {
		t.Fatalf("event status = %q, want failed after all sinks dead-letter", got)
	}
	next, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		Reason:    "same incident after failed delivery",
		DedupeKey: "health:svc:unhealthy",
		CreatedAt: time.Date(2026, 7, 9, 12, 1, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want failed events not to suppress follow-up incidents")
	}
	if next.ID == event.ID {
		t.Fatalf("next event reused failed event ID %d, want fresh row", next.ID)
	}
}

func TestAlertDeadLetteredDispatchSynthesizesMissingDiagnostic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:      "health_transition",
		ServiceID: "svc",
		NewState:  "unhealthy",
		DedupeKey: "health:svc:empty-dead-letter",
		CreatedAt: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}, []string{"discord"}, time.Hour); err != nil {
		t.Fatal(err)
	} else if !inserted {
		t.Fatal("inserted = false, want true")
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-a", time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	dispatch, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-a", claimed[0].Dispatch.ClaimID, AlertDispatchStatusDeadLettered, "")
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Status != AlertDispatchStatusDeadLettered || dispatch.LastError != alertDispatchDeadNoDiagnostic {
		t.Fatalf("dead-letter dispatch = %#v, want synthesized diagnostic %q", dispatch, alertDispatchDeadNoDiagnostic)
	}
}

func TestAlertStorageRedactsSinkSecretsWhenPersisting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	token := "alert-secret-token"
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.AddRedactionValues(token)
	event, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind:       "health_transition",
		ServiceID:  "svc",
		Target:     "routes: https://user:" + token + "@example.com/app",
		Repository: "repo-" + token,
		OldState:   "healthy",
		NewState:   "unhealthy",
		Reason:     "discord webhook https://hooks.example.test/" + token + " failed with " + token,
		DedupeKey:  "health:svc:" + token,
	}, []string{"discord"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true")
	}
	if event.ID == 0 {
		t.Fatal("event ID is zero")
	}
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-"+token, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %#v, want one dispatch", claimed)
	}
	if _, err := store.RecordAlertDispatchResult(ctx, claimed[0].Dispatch.ID, "worker-"+token, claimed[0].Dispatch.ClaimID, AlertDispatchStatusDeadLettered, "POST failed with "+token); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`SELECT target FROM alert_events`,
		`SELECT repository FROM alert_events`,
		`SELECT reason FROM alert_events`,
		`SELECT dedupe_key FROM alert_events`,
		`SELECT worker_id FROM alert_dispatches`,
		`SELECT last_error FROM alert_dispatches`,
	} {
		rows, err := store.db.QueryContext(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			assertRedacted(t, value, token)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func alertDeliverySinks(deliveries []AlertDelivery) []string {
	sinks := make([]string, 0, len(deliveries))
	for _, delivery := range deliveries {
		sinks = append(sinks, delivery.Dispatch.Sink)
	}
	return sinks
}

func alertEventStatus(t *testing.T, store *Store, eventID int64) string {
	t.Helper()
	var status string
	if err := store.db.QueryRowContext(context.Background(), `SELECT status FROM alert_events WHERE id=?`, eventID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func tableHasColumn(t *testing.T, store *Store, table, column string) bool {
	t.Helper()
	rows, err := store.db.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}

func writeTestAlertDedupeKey(t *testing.T, dbPath string) {
	t.Helper()
	keyPath := filepath.Join(filepath.Dir(dbPath), "alert-dedupe.key")
	if err := os.WriteFile(keyPath, []byte(strings.Repeat("a", 64)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := readAlertDedupeKey(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", sqliteDSN(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureStorageMetadataTable(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := writeAlertDedupeKeyFingerprint(context.Background(), db, key); err != nil {
		t.Fatal(err)
	}
}

func assertRedacted(t *testing.T, value, token string) {
	t.Helper()
	if strings.Contains(value, token) {
		t.Fatalf("value contains token %q: %q", token, value)
	}
	if strings.Contains(value, "@example.com") {
		t.Fatalf("value contains URL userinfo: %q", value)
	}
}

func sqliteFilesContainToken(t *testing.T, dbPath, token string) bool {
	t.Helper()
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), token) {
			return true
		}
	}
	return false
}

type failingAlertDedupeKeyFile struct {
	writeErr error
	closeErr error
}

func (f *failingAlertDedupeKeyFile) WriteString(string) (int, error) { return 0, f.writeErr }
func (f *failingAlertDedupeKeyFile) Sync() error                     { return nil }
func (f *failingAlertDedupeKeyFile) Close() error                    { return f.closeErr }
