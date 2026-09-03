// Package protocol defines the wire frames of the ButterStack Connector
// protocol v0, exactly as specified in connector/PROTOCOL.md.
//
// Every frame is a single JSON object carrying a "type" discriminator. There
// is no binary frame type and no streaming: a verb that would return more than
// its max_bytes budget truncates and says so.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the protocol version this build speaks. The broker compares it
// against min_supported_version and may reject.
const Version = "0"

// Frame type discriminators.
const (
	TypeHello     = "hello"
	TypeWelcome   = "welcome"
	TypeReject    = "reject"
	TypeCommand   = "command"
	TypeResult    = "result"
	TypeHeartbeat = "heartbeat"
)

// Result statuses. "denied" is reserved for the allowlist and the argument
// constraint layer; a tool that answers with an error is "error". Keeping them
// distinct is what makes the denial drills legible in the audit log.
const (
	StatusOK      = "ok"
	StatusError   = "error"
	StatusDenied  = "denied"
	StatusTimeout = "timeout"
)

// Envelope is enough to route an inbound frame to its concrete type.
type Envelope struct {
	Type string `json:"type"`
}

// Hello is the connector's first frame. It is egress: every field here leaves
// the studio network on every connect and belongs in egress.md.
type Hello struct {
	Type          string            `json:"type"`
	ConnectorID   string            `json:"connector_id"`
	IntegrationID string            `json:"integration_id"`
	Version       string            `json:"version"`
	Protocol      string            `json:"protocol"`
	Capabilities  []string          `json:"capabilities"`
	ToolVersions  map[string]string `json:"tool_versions,omitempty"`
}

// Welcome is the broker's acceptance frame.
type Welcome struct {
	Type                string `json:"type"`
	SessionID           string `json:"session_id"`
	ServerTime          string `json:"server_time"`
	MinSupportedVersion string `json:"min_supported_version"`
	HeartbeatInterval   int    `json:"heartbeat_interval"`
}

// Reject is the broker's refusal frame. A reject is a log line on the studio
// side, never an alert: the connector backs off and retries.
type Reject struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// Command is the only frame that can cause the connector to touch a studio
// tool. Note what is absent: no host, no port, no URL, no shell string. Those
// live in connector.yml and never on the wire (survival condition 2).
type Command struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	Verb       string          `json:"verb"`
	Args       json.RawMessage `json:"args"`
	DeadlineMs int             `json:"deadline_ms"`
	MaxBytes   int             `json:"max_bytes"`
}

// Result answers exactly one Command, keyed by its id.
type Result struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	Body      any    `json:"body,omitempty"`
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"bytes"`
}

// Heartbeat is egress too (queue depth is a signal about the studio's load).
type Heartbeat struct {
	Type       string `json:"type"`
	TS         string `json:"ts"`
	QueueDepth int    `json:"queue_depth"`
}

// ErrUnknownFrame is returned for a frame type this protocol version does not
// define. The connector treats it as a no-op and logs it rather than closing:
// an older connector talking to a newer broker must degrade, not break.
var ErrUnknownFrame = errors.New("protocol: unknown frame type")

// DecodeEnvelope reads just the discriminator.
func DecodeEnvelope(b []byte) (string, error) {
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return "", fmt.Errorf("protocol: malformed frame: %w", err)
	}
	if e.Type == "" {
		return "", errors.New("protocol: frame has no type")
	}
	return e.Type, nil
}

// NewHello builds the connector's opening frame.
func NewHello(connectorID, integrationID, version string, capabilities []string, toolVersions map[string]string) Hello {
	return Hello{
		Type:          TypeHello,
		ConnectorID:   connectorID,
		IntegrationID: integrationID,
		Version:       version,
		Protocol:      Version,
		Capabilities:  capabilities,
		ToolVersions:  toolVersions,
	}
}

// Deny builds a denied result. Reason is a stable machine token, not prose:
// the drills assert on it and the audit log records it.
func Deny(id, reason string) Result {
	return Result{Type: TypeResult, ID: id, Status: StatusDenied, Reason: reason}
}

// Errorf builds an error result.
func Errorf(id, format string, a ...any) Result {
	return Result{Type: TypeResult, ID: id, Status: StatusError, Reason: fmt.Sprintf(format, a...)}
}

// OK builds a successful result.
func OK(id string, body any, bytes int, truncated bool) Result {
	return Result{Type: TypeResult, ID: id, Status: StatusOK, Body: body, Bytes: bytes, Truncated: truncated}
}
