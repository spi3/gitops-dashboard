package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Timeouts and limits for the collect-before-dial report protocol. These are
// vars, not consts, so tests can substitute short values deterministically
// instead of waiting on the production ten-second bounds.
var (
	agentDialHandshakeTimeout       = 10 * time.Second
	agentReportWriteTimeout         = 10 * time.Second
	agentAckReadTimeout             = 10 * time.Second
	agentAckReadLimit         int64 = 4 * 1024
)

// Classification sentinels for sendOnce failures. Categories stay
// independently testable with errors.Is regardless of the underlying cause.
var (
	errAgentCollectionFailure      = errors.New("collection_failure")
	errAgentServerRejection        = errors.New("server_rejection")
	errAgentConnectionFailure      = errors.New("connection_failure")
	errAgentAcknowledgementTimeout = errors.New("acknowledgement_timeout")
	errAgentProtocolFailure        = errors.New("protocol_failure")
)

var (
	errBinaryAcknowledgement         = errors.New("acknowledgement was a binary message")
	errInvalidAcknowledgementPairing = errors.New("acknowledgement status/code pairing is invalid")
)

func classifyAgentError(sentinel, cause error) error {
	return fmt.Errorf("%w: %v", sentinel, cause)
}

// classifyAgentErrorCode wraps only an already-allowlisted acknowledgement
// code, never the raw acknowledgement body, so a typed error acknowledgement
// exposes exactly its code and nothing else.
func classifyAgentErrorCode(sentinel error, code string) error {
	return fmt.Errorf("%w: %s", sentinel, code)
}

func isAgentReadTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// agentReportAck is the private acknowledgement wire type for the agent side
// of the protocol. internal/app defines a structurally identical private
// type of its own; internal/core is frozen for this protocol, so the two
// packages intentionally do not share one wire type.
type agentReportAck struct {
	Type   string
	Status string
	Code   string
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

var agentAckErrorCodes = map[string]struct{}{
	agentAckCodeUnauthorizedTarget: {},
	agentAckCodeInvalidReport:      {},
	agentAckCodePersistenceFailed:  {},
}

func isAgentAckErrorCode(code string) bool {
	_, ok := agentAckErrorCodes[code]
	return ok
}

// decodeAgentReportAck applies a strict schema to the acknowledgement: it
// must be exactly one JSON object with exactly the three required string
// fields, nothing else, and nothing trailing.
func decodeAgentReportAck(data []byte) (agentReportAck, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return agentReportAck{}, fmt.Errorf("acknowledgement is not a JSON object: %w", err)
	}
	if raw == nil {
		return agentReportAck{}, errors.New("acknowledgement is not a JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return agentReportAck{}, errors.New("trailing content after acknowledgement")
	}
	for key := range raw {
		switch key {
		case "type", "status", "code":
		default:
			return agentReportAck{}, fmt.Errorf("unknown acknowledgement field %q", key)
		}
	}

	var ack agentReportAck
	for _, field := range []struct {
		name string
		dest *string
	}{
		{"type", &ack.Type},
		{"status", &ack.Status},
		{"code", &ack.Code},
	} {
		fieldRaw, ok := raw[field.name]
		if !ok || isAgentAckJSONNull(fieldRaw) {
			return agentReportAck{}, fmt.Errorf("missing or null %q", field.name)
		}
		if err := json.Unmarshal(fieldRaw, field.dest); err != nil {
			return agentReportAck{}, fmt.Errorf("%q is not a string", field.name)
		}
	}
	if ack.Type != agentReportAckType {
		return agentReportAck{}, errors.New("unexpected acknowledgement type")
	}
	return ack, nil
}

func isAgentAckJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
