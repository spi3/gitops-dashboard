package app

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

const (
	actionStatusRunning = "running"
	actionStatusOK      = "ok"
	actionStatusError   = "error"
	maxStoredActions    = 100
)

type dashboardAction struct {
	ID         string     `json:"id"`
	Action     string     `json:"action"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Error      string     `json:"error,omitempty"`
}

func (app *App) startAction(actionName string, run func(context.Context) error) dashboardAction {
	app.actionsMu.Lock()
	if app.actions == nil {
		app.actions = map[string]dashboardAction{}
	}
	if app.activeActions == nil {
		app.activeActions = map[string]string{}
	}
	if activeID := app.activeActions[actionName]; activeID != "" {
		action := app.actions[activeID]
		app.actionsMu.Unlock()
		return action
	}

	app.nextActionID++
	id := strconv.FormatInt(app.nextActionID, 10)
	action := dashboardAction{
		ID:        id,
		Action:    actionName,
		Status:    actionStatusRunning,
		StartedAt: time.Now().UTC(),
	}
	app.actions[id] = action
	app.activeActions[actionName] = id
	ctx := app.actionCtx
	if ctx == nil {
		ctx = context.Background()
	}
	app.actionsWG.Add(1)
	app.actionsMu.Unlock()

	go func() {
		defer app.actionsWG.Done()
		err := run(ctx)
		app.finishAction(id, actionName, err)
	}()
	return action
}

func (app *App) finishAction(id, actionName string, err error) {
	app.actionsMu.Lock()
	defer app.actionsMu.Unlock()
	action, ok := app.actions[id]
	if !ok {
		return
	}
	finishedAt := time.Now().UTC()
	action.FinishedAt = &finishedAt
	if err != nil {
		action.Status = actionStatusError
		action.Error = err.Error()
	} else {
		action.Status = actionStatusOK
	}
	app.actions[id] = action
	if app.activeActions[actionName] == id {
		delete(app.activeActions, actionName)
	}
	app.pruneActionsLocked()
}

func (app *App) pruneActionsLocked() {
	if len(app.actions) <= maxStoredActions {
		return
	}
	for len(app.actions) > maxStoredActions {
		var oldestID string
		var oldestStarted time.Time
		for id, action := range app.actions {
			if action.Status == actionStatusRunning {
				continue
			}
			if oldestID == "" || action.StartedAt.Before(oldestStarted) {
				oldestID = id
				oldestStarted = action.StartedAt
			}
		}
		if oldestID == "" {
			return
		}
		delete(app.actions, oldestID)
	}
}

func (app *App) actionStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app.actionsMu.Lock()
	action, ok := app.actions[id]
	app.actionsMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	app.writeJSON(w, action)
}
