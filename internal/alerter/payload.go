package alerter

import (
	"fmt"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/storage"
)

// EventPayload is the documented shape available to a generic webhook sink's
// body template (and marshaled directly as the JSON body for the Discord and
// Home Assistant sinks). It exposes every alert_events column relevant to
// delivery; fields that a producer left empty are simply empty strings.
type EventPayload struct {
	EventID    int64     `json:"eventId"`
	Kind       string    `json:"kind"`
	ServiceID  string    `json:"serviceId,omitempty"`
	Target     string    `json:"target,omitempty"`
	Repository string    `json:"repository,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	OldState   string    `json:"oldState,omitempty"`
	NewState   string    `json:"newState,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	OccurredAt time.Time `json:"occurredAt"`
}

func newEventPayload(event storage.AlertEvent) EventPayload {
	return EventPayload{
		EventID:    event.ID,
		Kind:       event.Kind,
		ServiceID:  event.ServiceID,
		Target:     event.Target,
		Repository: event.Repository,
		Agent:      event.Agent,
		OldState:   event.OldState,
		NewState:   event.NewState,
		Reason:     event.Reason,
		OccurredAt: event.CreatedAt,
	}
}

// Subject picks the most specific identifier available for the event, in
// the order a human is most likely to recognize it.
func (p EventPayload) Subject() string {
	switch {
	case p.ServiceID != "":
		return p.ServiceID
	case p.Target != "":
		return p.Target
	case p.Repository != "":
		return p.Repository
	case p.Agent != "":
		return p.Agent
	default:
		return "gitops-dashboard"
	}
}

// IsRecovery reports whether the event represents a return to a healthy
// state rather than a new incident. Producers signal this by suffixing the
// event kind with ".recovery" -- the convention established by the
// health-transition producer (T-022: "health.transition" / "health.recovery")
// that any future producer (agent-disconnect, scan-failure; T-064) is
// expected to follow (e.g. "agent.recovery", "scan.recovery").
func (p EventPayload) IsRecovery() bool {
	return strings.HasSuffix(p.Kind, ".recovery")
}

// Summary renders a single human-readable line describing the event, used by
// the Discord sink and available for reference by generic webhook templates.
func (p EventPayload) Summary() string {
	subject := p.Subject()
	switch {
	case p.IsRecovery() && p.OldState != "":
		return fmt.Sprintf("%s recovered (%s -> %s)", subject, p.OldState, p.NewState)
	case p.IsRecovery():
		return fmt.Sprintf("%s recovered", subject)
	case p.OldState != "" && p.NewState != "":
		return fmt.Sprintf("%s: %s -> %s", subject, p.OldState, p.NewState)
	case p.NewState != "":
		return fmt.Sprintf("%s: %s", subject, p.NewState)
	default:
		return fmt.Sprintf("%s: %s", subject, p.Kind)
	}
}
