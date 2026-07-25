package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/example/gitops-dashboard/internal/core"
)

// agentReportAck is the private acknowledgement wire type for the server side
// of the agent report protocol. internal/agent defines a structurally
// identical private type of its own: internal/core is frozen for this
// protocol, so the two packages intentionally do not share one wire type.
type agentReportAck struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Code   string `json:"code"`
}

const agentReportAckType = "agent_report_ack"

const (
	agentAckStatusOK    = "ok"
	agentAckStatusError = "error"
)

const (
	agentAckCodePersisted          = "persisted"
	agentAckCodeUnauthorizedTarget = "unauthorized_target"
	agentAckCodeInvalidReport      = "invalid_report"
	agentAckCodePersistenceFailed  = "persistence_failed"
)

func newAgentReportAckOK() agentReportAck {
	return agentReportAck{Type: agentReportAckType, Status: agentAckStatusOK, Code: agentAckCodePersisted}
}

func newAgentReportAckError(code string) agentReportAck {
	return agentReportAck{Type: agentReportAckType, Status: agentAckStatusError, Code: code}
}

// Schema and semantic validation failures are distinguished so the caller can
// select the close code the spec pins to each stage (1007 vs 1008), while
// still being independently testable via errors.Is.
var (
	errAgentReportSchemaInvalid   = errors.New("agent report schema invalid")
	errAgentReportSemanticInvalid = errors.New("agent report semantic invalid")
)

// agentReportWire holds the report exactly as decoded against the strict
// schema, before semantic validation. CheckedAt stays a string here because
// "is a JSON string" is a schema property; "parses as non-zero RFC3339" is a
// semantic one.
type agentReportWire struct {
	Target     string
	CheckedAt  string
	Containers []agentReportContainerWire
}

type agentReportContainerWire struct {
	ID           string
	Name         string
	Image        string
	ImageID      string
	RepoDigests  []string
	Labels       map[string]string
	HasLabels    bool
	State        string
	Status       string
	Health       string
	RestartCount int
}

var agentReportTopFields = map[string]struct{}{
	"target": {}, "checkedAt": {}, "containers": {},
}

var agentReportContainerFields = map[string]struct{}{
	"id": {}, "name": {}, "image": {}, "imageId": {}, "repoDigests": {},
	"labels": {}, "state": {}, "status": {}, "health": {}, "restartCount": {},
}

// decodeAgentReportWire applies the wire schema contract: the top-level
// value is exactly one JSON object with exactly the required fields, every
// container entry is a non-null object with exactly its allowed fields, and
// nothing follows the top-level object. It never echoes decoded field values
// into its error text, since those values are attacker-controlled and must
// not reach logs.
func decodeAgentReportWire(data []byte) (agentReportWire, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	top, err := decodeStrictJSONObject(decoder, agentReportTopFields)
	if err != nil {
		return agentReportWire{}, fmt.Errorf("%w: top-level: %v", errAgentReportSchemaInvalid, err)
	}
	if err := ensureNoTrailingJSON(decoder); err != nil {
		return agentReportWire{}, fmt.Errorf("%w: trailing content after report", errAgentReportSchemaInvalid)
	}

	var wire agentReportWire

	targetRaw, ok := top["target"]
	if !ok || isAgentJSONNull(targetRaw) {
		return agentReportWire{}, fmt.Errorf("%w: missing or null target", errAgentReportSchemaInvalid)
	}
	if err := json.Unmarshal(targetRaw, &wire.Target); err != nil {
		return agentReportWire{}, fmt.Errorf("%w: target is not a string", errAgentReportSchemaInvalid)
	}

	checkedAtRaw, ok := top["checkedAt"]
	if !ok || isAgentJSONNull(checkedAtRaw) {
		return agentReportWire{}, fmt.Errorf("%w: missing or null checkedAt", errAgentReportSchemaInvalid)
	}
	if err := json.Unmarshal(checkedAtRaw, &wire.CheckedAt); err != nil {
		return agentReportWire{}, fmt.Errorf("%w: checkedAt is not a string", errAgentReportSchemaInvalid)
	}

	containersRaw, ok := top["containers"]
	if !ok || isAgentJSONNull(containersRaw) {
		return agentReportWire{}, fmt.Errorf("%w: missing or null containers", errAgentReportSchemaInvalid)
	}
	var containerItems []json.RawMessage
	if err := json.Unmarshal(containersRaw, &containerItems); err != nil {
		return agentReportWire{}, fmt.Errorf("%w: containers is not an array", errAgentReportSchemaInvalid)
	}

	wire.Containers = make([]agentReportContainerWire, 0, len(containerItems))
	for _, item := range containerItems {
		container, err := decodeAgentReportContainerWire(item)
		if err != nil {
			return agentReportWire{}, fmt.Errorf("%w: container: %v", errAgentReportSchemaInvalid, err)
		}
		wire.Containers = append(wire.Containers, container)
	}

	return wire, nil
}

func decodeAgentReportContainerWire(raw json.RawMessage) (agentReportContainerWire, error) {
	if isAgentJSONNull(raw) {
		return agentReportContainerWire{}, errors.New("container entry is null")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	fields, err := decodeStrictJSONObject(decoder, agentReportContainerFields)
	if err != nil {
		return agentReportContainerWire{}, err
	}

	var container agentReportContainerWire
	stringField := func(name string, dest *string) error {
		fieldRaw, ok := fields[name]
		if !ok || isAgentJSONNull(fieldRaw) {
			return fmt.Errorf("missing or null %q", name)
		}
		if err := json.Unmarshal(fieldRaw, dest); err != nil {
			return fmt.Errorf("%q is not a string", name)
		}
		return nil
	}
	for _, field := range []struct {
		name string
		dest *string
	}{
		{"id", &container.ID},
		{"name", &container.Name},
		{"image", &container.Image},
		{"imageId", &container.ImageID},
		{"state", &container.State},
		{"status", &container.Status},
		{"health", &container.Health},
	} {
		if err := stringField(field.name, field.dest); err != nil {
			return agentReportContainerWire{}, err
		}
	}

	repoDigestsRaw, ok := fields["repoDigests"]
	if !ok || isAgentJSONNull(repoDigestsRaw) {
		return agentReportContainerWire{}, errors.New("missing or null repoDigests")
	}
	var repoDigests []string
	if err := json.Unmarshal(repoDigestsRaw, &repoDigests); err != nil {
		return agentReportContainerWire{}, errors.New("repoDigests is not an array of strings")
	}
	if repoDigests == nil {
		repoDigests = []string{}
	}
	container.RepoDigests = repoDigests

	restartCountRaw, ok := fields["restartCount"]
	if !ok || isAgentJSONNull(restartCountRaw) {
		return agentReportContainerWire{}, errors.New("missing or null restartCount")
	}
	if err := json.Unmarshal(restartCountRaw, &container.RestartCount); err != nil {
		return agentReportContainerWire{}, errors.New("restartCount is not an integer")
	}

	if labelsRaw, ok := fields["labels"]; ok {
		if isAgentJSONNull(labelsRaw) {
			return agentReportContainerWire{}, errors.New("labels is null")
		}
		labels, err := decodeAgentReportLabelsWire(labelsRaw)
		if err != nil {
			return agentReportContainerWire{}, err
		}
		container.Labels = labels
		container.HasLabels = true
	}

	return container, nil
}

func decodeAgentReportLabelsWire(raw json.RawMessage) (map[string]string, error) {
	var rawLabels map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawLabels); err != nil {
		return nil, errors.New("labels is not a JSON object")
	}
	labels := make(map[string]string, len(rawLabels))
	for key, value := range rawLabels {
		if isAgentJSONNull(value) {
			return nil, fmt.Errorf("label %q value is null", key)
		}
		var strValue string
		if err := json.Unmarshal(value, &strValue); err != nil {
			return nil, fmt.Errorf("label %q value is not a string", key)
		}
		labels[key] = strValue
	}
	return labels, nil
}

// decodeStrictJSONObject decodes exactly one JSON object from decoder,
// rejecting any key not present in allowed. It rejects non-object JSON
// values (including null, which decodes to a nil map) up front.
func decodeStrictJSONObject(decoder *json.Decoder, allowed map[string]struct{}) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errors.New("value is not a JSON object")
	}
	for key := range raw {
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("unknown field %q", key)
		}
	}
	return raw, nil
}

func ensureNoTrailingJSON(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON content")
		}
		return err
	}
	return nil
}

func isAgentJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// Semantic bounds, all pinned by the spec (not configurable).
const (
	agentReportMaxTargetBytes = 255
	agentReportMaxContainers  = 10000
	agentReportMaxIDBytes     = 256
	agentReportMaxScalarBytes = 4096
	agentReportMaxDigests     = 256
	agentReportMaxLabels      = 32
)

var agentReportValidHealth = map[string]struct{}{
	"healthy": {}, "unhealthy": {}, "starting": {}, "none": {},
}

// validateAgentReportSemantics applies the semantic contract to an
// already-schema-valid wire report, producing the core.AgentMessage used for
// authorization and persistence.
func validateAgentReportSemantics(wire agentReportWire) (core.AgentMessage, error) {
	if strings.TrimSpace(wire.Target) == "" {
		return core.AgentMessage{}, semanticErr("target is empty after trimming")
	}
	if len(wire.Target) > agentReportMaxTargetBytes {
		return core.AgentMessage{}, semanticErr("target exceeds maximum length")
	}
	checkedAt, err := time.Parse(time.RFC3339, wire.CheckedAt)
	if err != nil || checkedAt.IsZero() {
		return core.AgentMessage{}, semanticErr("checkedAt is not a non-zero RFC3339 timestamp")
	}
	if len(wire.Containers) > agentReportMaxContainers {
		return core.AgentMessage{}, semanticErr("containers exceeds maximum count")
	}

	seenIDs := make(map[string]struct{}, len(wire.Containers))
	containers := make([]core.ContainerStatus, 0, len(wire.Containers))
	for _, item := range wire.Containers {
		if err := validateAgentReportContainerSemantics(item, seenIDs); err != nil {
			return core.AgentMessage{}, err
		}
		containers = append(containers, core.ContainerStatus{
			ID:           item.ID,
			Name:         item.Name,
			Image:        item.Image,
			ImageID:      item.ImageID,
			RepoDigests:  item.RepoDigests,
			Labels:       item.Labels,
			State:        item.State,
			Status:       item.Status,
			Health:       item.Health,
			RestartCount: item.RestartCount,
		})
	}

	return core.AgentMessage{Target: wire.Target, CheckedAt: checkedAt.UTC(), Containers: containers}, nil
}

func validateAgentReportContainerSemantics(item agentReportContainerWire, seenIDs map[string]struct{}) error {
	if len(item.ID) > agentReportMaxIDBytes {
		return semanticErr("container id exceeds maximum length")
	}
	if strings.TrimSpace(item.ID) == "" {
		return semanticErr("container id is empty after trimming")
	}
	if _, dup := seenIDs[item.ID]; dup {
		return semanticErr("duplicate container id")
	}
	seenIDs[item.ID] = struct{}{}

	for _, scalar := range []string{item.Name, item.Image, item.ImageID, item.State, item.Status, item.Health} {
		if len(scalar) > agentReportMaxScalarBytes {
			return semanticErr("container scalar field exceeds maximum length")
		}
	}
	if item.RestartCount < 0 {
		return semanticErr("restartCount is negative")
	}
	if _, ok := agentReportValidHealth[item.Health]; !ok {
		return semanticErr("health is not a recognized value")
	}
	if len(item.RepoDigests) > agentReportMaxDigests {
		return semanticErr("repoDigests exceeds maximum count")
	}
	for _, digest := range item.RepoDigests {
		if len(digest) > agentReportMaxScalarBytes {
			return semanticErr("repoDigests entry exceeds maximum length")
		}
	}
	if item.HasLabels {
		if len(item.Labels) > agentReportMaxLabels {
			return semanticErr("labels exceeds maximum count")
		}
		for key, value := range item.Labels {
			if len(key) > agentReportMaxScalarBytes || len(value) > agentReportMaxScalarBytes {
				return semanticErr("label key or value exceeds maximum length")
			}
		}
	}
	return nil
}

func semanticErr(reason string) error {
	return fmt.Errorf("%w: %s", errAgentReportSemanticInvalid, reason)
}
