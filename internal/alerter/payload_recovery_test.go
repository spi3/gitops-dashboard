package alerter

import (
	"testing"

	"github.com/example/gitops-dashboard/internal/storage"
)

// TestEventPayloadRendersAlertEvaluatorKindsAsExpected is the direct
// regression test for survey 2026-07-25 IR-1: before T-065, the
// alert_evaluator emitted agent_offline/agent_recovered/scan_failed/
// scan_recovered, none of which end in ".recovery", so IsRecovery() was
// always false for a real agent/scan recovery and every recovery rendered
// as a fresh incident. The evaluator now emits the dot-convention kinds
// below (internal/app/alert_evaluator.go); this proves IsRecovery() and
// Summary() classify each one correctly.
func TestEventPayloadRendersAlertEvaluatorKindsAsExpected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind         string
		oldState     string
		newState     string
		wantRecovery bool
		wantSummary  string
	}{
		{kind: "agent.offline", oldState: "online", newState: "offline", wantRecovery: false, wantSummary: "serenity: online -> offline"},
		{kind: "agent.recovery", oldState: "offline", newState: "online", wantRecovery: true, wantSummary: "serenity recovered (offline -> online)"},
		{kind: "scan.failure", oldState: "ok", newState: "error", wantRecovery: false, wantSummary: "infra: ok -> error"},
		{kind: "scan.recovery", oldState: "error", newState: "ok", wantRecovery: true, wantSummary: "infra recovered (error -> ok)"},
	}
	for _, testCase := range cases {
		t.Run(testCase.kind, func(t *testing.T) {
			event := storage.AlertEvent{Kind: testCase.kind, OldState: testCase.oldState, NewState: testCase.newState}
			if testCase.kind[:5] == "agent" {
				event.Agent = "serenity"
			} else {
				event.Repository = "infra"
			}
			payload := newEventPayload(event)
			if got := payload.IsRecovery(); got != testCase.wantRecovery {
				t.Fatalf("IsRecovery() for kind %q = %v, want %v", testCase.kind, got, testCase.wantRecovery)
			}
			if got := payload.Summary(); got != testCase.wantSummary {
				t.Fatalf("Summary() for kind %q = %q, want %q", testCase.kind, got, testCase.wantSummary)
			}
		})
	}
}
