package storage

import (
	"context"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/core"
	"github.com/example/gitops-dashboard/internal/routetarget"
)

func (store *Store) UpsertAgent(ctx context.Context, message core.AgentMessage) error {
	message = core.FilterAgentMessageDockerLabels(message)
	staleAfter := ""
	if !message.StaleAfter.IsZero() {
		staleAfter = message.StaleAfter.UTC().Format(time.RFC3339)
	}
	_, err := store.db.ExecContext(ctx, `
	INSERT INTO agents(target, last_seen_at, stale_after, status_json)
VALUES(?, ?, ?, ?)
ON CONFLICT(target) DO UPDATE SET last_seen_at=excluded.last_seen_at, stale_after=excluded.stale_after, status_json=excluded.status_json
`, message.Target, time.Now().UTC().Format(time.RFC3339), staleAfter, toJSON(message.Containers))
	return err
}

// UpsertAgentReport commits the server receipt and all derived service rows as
// one observation, so a summary cannot expose fresh agent state with old tiles.
func (store *Store) UpsertAgentReport(ctx context.Context, message core.AgentMessage, statuses []core.StatusResult, receivedAt time.Time) error {
	message = core.FilterAgentMessageDockerLabels(message)
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	staleAfter := ""
	if !message.StaleAfter.IsZero() {
		staleAfter = message.StaleAfter.UTC().Format(time.RFC3339)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agents(target,last_seen_at,stale_after,status_json) VALUES(?,?,?,?) ON CONFLICT(target) DO UPDATE SET last_seen_at=excluded.last_seen_at, stale_after=excluded.stale_after, status_json=excluded.status_json`, message.Target, receivedAt.UTC().Format(time.RFC3339), staleAfter, toJSON(message.Containers)); err != nil {
		return err
	}
	affectedServices := make([]string, 0, len(statuses))
	for _, status := range statuses {
		status.ServiceID = strings.TrimSpace(status.ServiceID)
		status.Target = canonicalStatusTarget(status.Target)
		if status.ObservedImages == nil {
			status.ObservedImages = []core.ObservedImage{}
		}
		exactOverride, parentOverride, err := monitorOverrideState(ctx, store, tx, status.ServiceID, status.Target)
		if err != nil {
			return err
		}
		if parentOverride && routetarget.IsChildTarget(status.Target) {
			continue
		}
		if exactOverride {
			status.Health = core.HealthNotApplicable
			status.Message = "not applicable"
			status.ObservedImages = []core.ObservedImage{}
			status.ExpiresAt = time.Time{}
		}
		expiresAt := ""
		if !status.ExpiresAt.IsZero() {
			expiresAt = status.ExpiresAt.UTC().Format(time.RFC3339)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO status_results(service_id,target,health,message,checked_at,expires_at,observed_images_json) VALUES(?,?,?,?,?,?,?) ON CONFLICT(service_id,target) DO UPDATE SET health=excluded.health,message=excluded.message,checked_at=excluded.checked_at,expires_at=excluded.expires_at,observed_images_json=excluded.observed_images_json`, status.ServiceID, status.Target, string(status.Health), store.redact(status.Message), status.CheckedAt.UTC().Format(time.RFC3339), expiresAt, toJSON(status.ObservedImages)); err != nil {
			return err
		}
		if status.Health != core.HealthNotApplicable {
			if _, err := tx.ExecContext(ctx, `INSERT INTO status_history(service_id,target,health,message,checked_at) VALUES(?,?,?,?,?)`, status.ServiceID, status.Target, string(status.Health), store.redact(status.Message), status.CheckedAt.UTC().Format(time.RFC3339)); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `DELETE FROM status_history WHERE service_id=? AND target=?`, status.ServiceID, status.Target); err != nil {
			return err
		}
		affectedServices = append(affectedServices, status.ServiceID)
	}
	if err := store.commitAndInvalidateSummary(tx); err != nil {
		return err
	}
	// Alert production is advisory and deliberately runs only after the core
	// receipt and projection transaction commits.
	store.observeHealthAlerts(ctx, dedupeStrings(affectedServices), receivedAt.UTC())
	return nil
}

func (store *Store) Agents(ctx context.Context) ([]core.AgentInfo, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT rowid, target, last_seen_at, stale_after, status_json FROM agents ORDER BY target
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
		if err := rows.Scan(&rowID, &agent.Target, &agent.LastSeenAt, &agent.StaleAfter, &statusJSON); err != nil {
			return nil, err
		}
		if err := store.fromPersistedJSON(statusJSON, &agent.Containers, "agents", "status_json", rowID, agent.Target); err != nil {
			return nil, err
		}
		if agent.Containers == nil {
			agent.Containers = []core.ContainerStatus{}
		}
		if agent.StaleAfter == "" {
			if seen, err := time.Parse(time.RFC3339, agent.LastSeenAt); err == nil {
				if ttl := store.statusTTL(agent.Target); ttl > 0 {
					agent.StaleAfter = seen.Add(ttl).Format(time.RFC3339)
				}
			}
		}
		agents = append(agents, agent)
	}
	return agents, rows.Err()
}
