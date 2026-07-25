package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func assertLatestTerminalScan(t *testing.T, latest map[string]LatestTerminalScan, repo string, wantID int64, wantStatus string) {
	t.Helper()
	got, ok := latest[repo]
	if !ok {
		t.Fatalf("%s missing from latest terminal scans, want id=%d status=%s", repo, wantID, wantStatus)
	}
	if got.ID != wantID || got.Status != wantStatus {
		t.Fatalf("%s latest terminal = %+v, want id=%d status=%s", repo, got, wantID, wantStatus)
	}
}

// TestLatestTerminalScansByRepositoryIgnoresRunningInEveryPosition covers the
// three required running-row positions: a trailing running scan after a
// terminal row, a running scan interleaved between an ok row and a later
// error row, and the symmetric error-then-running-then-ok case. In every
// case the still-running row must never be selected, whether it trails the
// latest terminal row or sits, still unfinished, between two terminal rows.
func TestLatestTerminalScansByRepositoryIgnoresRunningInEveryPosition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// ok -> running: a trailing, still-running scan must not shadow the
	// prior ok terminal row.
	okID, err := store.StartScan(ctx, "repo-ok-running")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, okID, "repo-ok-running", "aaa", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartScan(ctx, "repo-ok-running"); err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestTerminalScansByRepository(ctx, []string{"repo-ok-running"})
	if err != nil {
		t.Fatal(err)
	}
	assertLatestTerminalScan(t, latest, "repo-ok-running", okID, "ok")

	// ok -> running -> error: while the second scan is still running, the ok
	// row remains the latest terminal row; once it finishes as error, the
	// error row becomes latest terminal.
	okID2, err := store.StartScan(ctx, "repo-ok-running-error")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, okID2, "repo-ok-running-error", "aaa", nil, nil); err != nil {
		t.Fatal(err)
	}
	runningID2, err := store.StartScan(ctx, "repo-ok-running-error")
	if err != nil {
		t.Fatal(err)
	}
	midFlight, err := store.LatestTerminalScansByRepository(ctx, []string{"repo-ok-running-error"})
	if err != nil {
		t.Fatal(err)
	}
	assertLatestTerminalScan(t, midFlight, "repo-ok-running-error", okID2, "ok")
	if err := store.FinishScan(ctx, runningID2, "repo-ok-running-error", "bbb", nil, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	after, err := store.LatestTerminalScansByRepository(ctx, []string{"repo-ok-running-error"})
	if err != nil {
		t.Fatal(err)
	}
	assertLatestTerminalScan(t, after, "repo-ok-running-error", runningID2, "error")

	// error -> running -> ok: symmetric.
	errID, err := store.StartScan(ctx, "repo-error-running-ok")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, errID, "repo-error-running-ok", "aaa", nil, errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	runningID3, err := store.StartScan(ctx, "repo-error-running-ok")
	if err != nil {
		t.Fatal(err)
	}
	midFlight3, err := store.LatestTerminalScansByRepository(ctx, []string{"repo-error-running-ok"})
	if err != nil {
		t.Fatal(err)
	}
	assertLatestTerminalScan(t, midFlight3, "repo-error-running-ok", errID, "error")
	if err := store.FinishScan(ctx, runningID3, "repo-error-running-ok", "bbb", nil, nil); err != nil {
		t.Fatal(err)
	}
	after3, err := store.LatestTerminalScansByRepository(ctx, []string{"repo-error-running-ok"})
	if err != nil {
		t.Fatal(err)
	}
	assertLatestTerminalScan(t, after3, "repo-error-running-ok", runningID3, "ok")
}

func TestLatestTerminalScansByRepositoryHasNoTerminalScanWhenOnlyRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.StartScan(ctx, "repo-only-running"); err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestTerminalScansByRepository(ctx, []string{"repo-only-running", "repo-never-scanned"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := latest["repo-only-running"]; ok {
		t.Fatalf("repo-only-running present = %+v, want absent (no terminal scan means unknown)", latest["repo-only-running"])
	}
	if _, ok := latest["repo-never-scanned"]; ok {
		t.Fatal("repo-never-scanned present, want absent")
	}
}

func TestLatestTerminalScansByRepositoryUsesGreatestIDNotStartedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	firstID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, firstID, "repo", "aaa", nil, nil); err != nil {
		t.Fatal(err)
	}
	secondID, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishScan(ctx, secondID, "repo", "bbb", nil, errors.New("clone failed")); err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestTerminalScansByRepository(ctx, []string{"repo"})
	if err != nil {
		t.Fatal(err)
	}
	assertLatestTerminalScan(t, latest, "repo", secondID, "error")
}

func TestLatestTerminalScansByRepositoryRedactsErrorEvenWhenSecretRegisteredAfterWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	secret := "ghp_super_secret_token"
	id, err := store.StartScan(ctx, "repo")
	if err != nil {
		t.Fatal(err)
	}
	// Persisted before the secret is registered for redaction: the raw
	// column value is unredacted plaintext at this point.
	if err := store.FinishScan(ctx, id, "repo", "deadbeef", nil, fmt.Errorf("clone failed: token %s rejected", secret)); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, store, fmt.Sprintf(`SELECT COUNT(*) FROM scans WHERE id=%d AND error LIKE '%%%s%%'`, id, secret)); got != 1 {
		t.Fatalf("precondition failed: expected the persisted row to contain the raw secret before registration, found %d matching rows", got)
	}

	store.AddRedactionValues(secret)

	latest, err := store.LatestTerminalScansByRepository(ctx, []string{"repo"})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := latest["repo"]
	if !ok {
		t.Fatal("repo missing from latest terminal scans")
	}
	if got.Status != "error" {
		t.Fatalf("status = %q, want error", got.Status)
	}
	if got.Error == "" || got.Error == fmt.Sprintf("clone failed: token %s rejected", secret) {
		t.Fatalf("error = %q, want redacted even though the secret was registered after the row was written", got.Error)
	}
}
