package storage

import (
	"context"
	"time"

	"github.com/example/gitops-dashboard/internal/core"
)

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
