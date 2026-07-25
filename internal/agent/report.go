package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/example/gitops-dashboard/internal/agentprotocol"
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
// packages intentionally do not share one wire type. The type/status/code
// values themselves are the shared external contract, defined once in
// internal/agentprotocol.
type agentReportAck struct {
	Type   string
	Status string
	Code   string
}

var agentReportAckFields = map[string]struct{}{
	"type": {}, "status": {}, "code": {},
}

var agentAckErrorCodes = map[string]struct{}{
	agentprotocol.AckCodeUnauthorizedTarget: {},
	agentprotocol.AckCodeInvalidReport:      {},
	agentprotocol.AckCodePersistenceFailed:  {},
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
	raw, err := agentprotocol.DecodeStrictJSONObject(decoder, agentReportAckFields)
	if err != nil {
		return agentReportAck{}, fmt.Errorf("acknowledgement is not a JSON object: %w", err)
	}
	if err := agentprotocol.EnsureNoTrailingJSON(decoder); err != nil {
		return agentReportAck{}, errors.New("trailing content after acknowledgement")
	}

	var ack agentReportAck
	if err := agentprotocol.DecodeStringFields(raw, []agentprotocol.StringField{
		{Name: "type", Dest: &ack.Type},
		{Name: "status", Dest: &ack.Status},
		{Name: "code", Dest: &ack.Code},
	}); err != nil {
		return agentReportAck{}, err
	}
	if ack.Type != agentprotocol.AckType {
		return agentReportAck{}, errors.New("unexpected acknowledgement type")
	}
	return ack, nil
}
