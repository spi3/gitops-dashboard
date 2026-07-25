package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// deliverAndCloseAlertEventAt claims, delivers, and backdates the delivered
// timestamp of every pending dispatch for eventID, so a subsequent
// CooldownKey-based enqueue attempt can be tested at a controlled offset
// from closure. Mirrors the manual delivered_at backdating already used by
// TestAlertEventDedupeSuppressesPendingAndCooldown.
func deliverAndCloseAlertEventAt(t *testing.T, store *Store, eventID int64, sink string, closedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	claimed, err := store.ClaimPendingAlertDeliveries(ctx, "worker-"+sink, time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	var dispatchID int64
	var claimID string
	found := false
	for _, delivery := range claimed {
		if delivery.Event.ID == eventID && delivery.Dispatch.Sink == sink {
			dispatchID, claimID = delivery.Dispatch.ID, delivery.Dispatch.ClaimID
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no pending %s dispatch claimed for event %d", sink, eventID)
	}
	if _, err := store.RecordAlertDispatchResult(ctx, dispatchID, "worker-"+sink, claimID, AlertDispatchStatusDelivered, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE alert_dispatches SET delivered_at=?, delivered_at_ns=?, updated_at=?, updated_at_ns=? WHERE event_id=?
`, closedAt.Format(time.RFC3339Nano), closedAt.UnixNano(), closedAt.Format(time.RFC3339Nano), closedAt.UnixNano(), eventID); err != nil {
		t.Fatal(err)
	}
}

// TestCooldownKeyIsolatesCrossAgentIdentities covers the CooldownKey defect
// this task fixes: two configured agents' agent_offline events share kind,
// empty service_id, old_state, and new_state, differing only by Agent. A
// cooldown started by one agent's delivered event must never suppress the
// other agent's otherwise-independent occurrence.
func TestCooldownKeyIsolatesCrossAgentIdentities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cooldown := time.Hour
	createdAt := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)

	alpha, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind: "agent_offline", Agent: "alpha", OldState: "online", NewState: "offline",
		Reason:      "agent report is stale",
		DedupeKey:   "agent:alpha:agent_offline:2026-07-24T00:00:00Z",
		CooldownKey: "agent:alpha:agent_offline",
		CreatedAt:   createdAt,
	}, []string{"discord"}, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true for alpha's first agent_offline event")
	}
	deliverAndCloseAlertEventAt(t, store, alpha.ID, "discord", createdAt)

	// Beta goes offline a minute later, well inside alpha's cooldown window.
	beta, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind: "agent_offline", Agent: "beta", OldState: "online", NewState: "offline",
		Reason:      "agent report is stale",
		DedupeKey:   "agent:beta:agent_offline:2026-07-24T00:01:00Z",
		CooldownKey: "agent:beta:agent_offline",
		CreatedAt:   createdAt.Add(time.Minute),
	}, []string{"discord"}, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true: beta's agent_offline must not be cross-suppressed by alpha's cooldown")
	}
	if beta.ID == alpha.ID {
		t.Fatalf("beta reused alpha's event id %d, want a distinct event", alpha.ID)
	}

	// Sanity: alpha's OWN repeat occurrence inside the same window is still
	// correctly suppressed by its own cooldown (the fix must not turn the
	// cooldown into a no-op).
	alphaRepeat, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind: "agent_offline", Agent: "alpha", OldState: "online", NewState: "offline",
		Reason:      "agent report is stale",
		DedupeKey:   "agent:alpha:agent_offline:2026-07-24T00:02:00Z",
		CooldownKey: "agent:alpha:agent_offline",
		CreatedAt:   createdAt.Add(2 * time.Minute),
	}, []string{"discord"}, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	if inserted {
		t.Fatal("inserted = true, want false: alpha's own repeat occurrence is still inside its own cooldown")
	}
	if alphaRepeat.ID != alpha.ID {
		t.Fatalf("alpha repeat event id = %d, want original %d", alphaRepeat.ID, alpha.ID)
	}
}

// TestCooldownKeyIsolatesCrossRepositoryIdentities is the scan_failed/
// scan_recovered counterpart: two configured repositories' scan_failed
// events share kind, empty service_id, old_state, and new_state, differing
// only by Repository.
func TestCooldownKeyIsolatesCrossRepositoryIdentities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cooldown := time.Hour
	createdAt := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)

	repoA, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind: "scan_failed", Repository: "repo-a", OldState: "ok", NewState: "error",
		Reason:      "repository scan failed",
		DedupeKey:   "repository:repo-a:scan_failed:1",
		CooldownKey: "repository:repo-a:scan_failed",
		CreatedAt:   createdAt,
	}, []string{"discord"}, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true for repo-a's first scan_failed event")
	}
	deliverAndCloseAlertEventAt(t, store, repoA.ID, "discord", createdAt)

	repoB, inserted, err := store.EnqueueAlertEvent(ctx, AlertEvent{
		Kind: "scan_failed", Repository: "repo-b", OldState: "ok", NewState: "error",
		Reason:      "repository scan failed",
		DedupeKey:   "repository:repo-b:scan_failed:7",
		CooldownKey: "repository:repo-b:scan_failed",
		CreatedAt:   createdAt.Add(time.Minute),
	}, []string{"discord"}, cooldown)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("inserted = false, want true: repo-b's scan_failed must not be cross-suppressed by repo-a's cooldown")
	}
	if repoB.ID == repoA.ID {
		t.Fatalf("repo-b reused repo-a's event id %d, want a distinct event", repoA.ID)
	}
}
