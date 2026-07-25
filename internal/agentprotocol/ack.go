// Package agentprotocol holds the agent report acknowledgement vocabulary
// and the strict wire-decode helpers shared by the server side
// (internal/app) and the agent side (internal/agent) of the agent report
// protocol. internal/core is frozen for this protocol, so the per-side wire
// structs stay separate; only the external protocol contract (the type,
// status, and code values) and the generic decode idiom live here.
package agentprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// AckType is the acknowledgement message's wire type value.
const AckType = "agent_report_ack"

// Acknowledgement status values.
const (
	AckStatusOK    = "ok"
	AckStatusError = "error"
)

// Acknowledgement code values.
const (
	AckCodePersisted          = "persisted"
	AckCodeUnauthorizedTarget = "unauthorized_target"
	AckCodeInvalidReport      = "invalid_report"
	AckCodePersistenceFailed  = "persistence_failed"
)

// IsJSONNull reports whether raw is the JSON literal null.
func IsJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// DecodeStrictJSONObject decodes exactly one JSON object from decoder,
// rejecting any key not present in allowed. It rejects non-object JSON
// values (including null, which decodes to a nil map) up front.
func DecodeStrictJSONObject(decoder *json.Decoder, allowed map[string]struct{}) (map[string]json.RawMessage, error) {
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

// EnsureNoTrailingJSON reports an error if decoder has any further JSON
// content after the value already decoded from it.
func EnsureNoTrailingJSON(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON content")
		}
		return err
	}
	return nil
}

// StringField pairs a wire field name with the string it decodes into.
type StringField struct {
	Name string
	Dest *string
}

// DecodeStringFields decodes each field's raw JSON value from fields into
// its destination, in order, rejecting a missing, null, or non-string value.
func DecodeStringFields(fields map[string]json.RawMessage, targets []StringField) error {
	for _, field := range targets {
		raw, ok := fields[field.Name]
		if !ok || IsJSONNull(raw) {
			return fmt.Errorf("missing or null %q", field.Name)
		}
		if err := json.Unmarshal(raw, field.Dest); err != nil {
			return fmt.Errorf("%q is not a string", field.Name)
		}
	}
	return nil
}
