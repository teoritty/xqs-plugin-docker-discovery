package ipc

import (
	"encoding/json"
	"fmt"
)

// MaxJSONRPCFrame is the ceiling on a JSON-RPC payload. Lower than the codec's general frame limit
// because the host enforces this one specifically (docs/plugin-api.md "Limits"), and a message
// refused there after we spent the memory building it is worse than one refused here.
const MaxJSONRPCFrame = 256 << 10

// ControlChannel is the channel id reserved for JSON-RPC. Every other id belongs to a binary
// channel the host allocated.
const ControlChannel uint32 = 0

// Message is a JSON-RPC 2.0 envelope in either direction.
//
// One type for requests, responses and notifications, because the wire has one shape and the
// difference is which fields are present: a request has an id and a method, a response has an id
// and a result or an error, a notification has a method and no id. Three types would mean three
// decodes to find out which one arrived.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// IsRequest reports whether the message expects a response.
func (m Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification reports whether the message is one-way.
func (m Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// IsResponse reports whether the message answers a request we sent.
func (m Message) IsResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// RPCError is the JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message)
}

// Host error codes worth naming, from docs/plugin-api.md. The rest are handled generically.
const (
	// CodeCapabilityDenied means the manifest does not grant this, the session is not ours, or an
	// exec request did not match a declared template. Not retryable: the answer will not change.
	CodeCapabilityDenied = -32001
	// CodeRateLimited means slow down — a full consumer, or a limit like one dialog per plugin.
	CodeRateLimited = -32003
)

// IsCapabilityDenied reports whether err is the host refusing on capability grounds. Callers use it
// to stop rather than retry: a denial is a statement about the manifest, not about the moment.
func IsCapabilityDenied(err error) bool {
	rpcErr, ok := err.(*RPCError)
	return ok && rpcErr.Code == CodeCapabilityDenied
}

// IsRateLimited reports whether err is the host asking us to slow down.
func IsRateLimited(err error) bool {
	rpcErr, ok := err.(*RPCError)
	return ok && rpcErr.Code == CodeRateLimited
}

// EncodeMessage marshals a message into a JSON-RPC frame payload, refusing one too large for the
// host to accept.
func EncodeMessage(m Message) ([]byte, error) {
	m.JSONRPC = "2.0"
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("ipc: encode message: %w", err)
	}
	if len(payload) > MaxJSONRPCFrame {
		return nil, fmt.Errorf("ipc: message of %d bytes exceeds the %d byte JSON-RPC limit", len(payload), MaxJSONRPCFrame)
	}
	return payload, nil
}

// DecodeMessage parses a JSON-RPC frame payload.
func DecodeMessage(payload []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(payload, &m); err != nil {
		return Message{}, fmt.Errorf("ipc: decode message: %w", err)
	}
	return m, nil
}
