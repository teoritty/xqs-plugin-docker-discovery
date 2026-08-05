// Package presentation decodes what the host sends and hands it to the use case.
//
// It is the plugin's mirror of the core's own presentation layer: every inbound method is decoded
// in exactly one place, and no business rule lives here.
package presentation

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/usecase"
)

// Service is what the handlers call. An interface so this package can be tested without a daemon.
type Service interface {
	Observe(ctx context.Context, sessionID string, nodeIDs []string)
	InvokeAction(sessionID string, nodeIDs []string, actionID string)
	DescribeNode(ctx context.Context, sessionID, nodeID string) (usecase.NodeDetails, error)
	ApplyDetails(ctx context.Context, sessionID, nodeID string, values map[string]string) error
	SurfaceInput(surfaceID string, data []byte)
	SurfaceResize(surfaceID string, cols, rows int)
	SurfaceClosed(surfaceID string)
	DialogSubmitted(dialogID string, values map[string]string)
	DialogCancelled(dialogID string)
	Shutdown()
}

// Handlers routes host→plugin methods.
type Handlers struct {
	service Service
}

// New creates the handler set.
func New(service Service) *Handlers { return &Handlers{service: service} }

type observeParams struct {
	SessionID string   `json:"sessionId"`
	NodeIDs   []string `json:"nodeIds"`
}

type invokeParams struct {
	SessionID string   `json:"sessionId"`
	NodeIDs   []string `json:"nodeIds"`
	ActionID  string   `json:"actionId"`
}

type nodeParams struct {
	SessionID string            `json:"sessionId"`
	NodeID    string            `json:"nodeId"`
	Values    map[string]string `json:"values"`
}

type surfaceParams struct {
	SurfaceID  string `json:"surfaceId"`
	DataBase64 string `json:"dataBase64"`
	Cols       int    `json:"cols"`
	Rows       int    `json:"rows"`
}

type dialogParams struct {
	DialogID string            `json:"dialogId"`
	Values   map[string]string `json:"values"`
}

// Observe handles discovery.observe.
func (h *Handlers) Observe(ctx context.Context, raw json.RawMessage) {
	var req observeParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	h.service.Observe(ctx, req.SessionID, req.NodeIDs)
}

// InvokeAction handles discovery.invokeAction. It acknowledges immediately; the work runs behind it
// (ADR-014: the ack must arrive inside five seconds whatever the action costs).
func (h *Handlers) InvokeAction(_ context.Context, raw json.RawMessage) (any, error) {
	var req invokeParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	h.service.InvokeAction(req.SessionID, req.NodeIDs, req.ActionID)
	return map[string]bool{"ok": true}, nil
}

// DescribeNode handles discovery.describeNode.
func (h *Handlers) DescribeNode(ctx context.Context, raw json.RawMessage) (any, error) {
	var req nodeParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return h.service.DescribeNode(ctx, req.SessionID, req.NodeID)
}

// ApplyDetails handles discovery.applyDetails.
func (h *Handlers) ApplyDetails(ctx context.Context, raw json.RawMessage) (any, error) {
	var req nodeParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if err := h.service.ApplyDetails(ctx, req.SessionID, req.NodeID, req.Values); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

// SurfaceInput handles surface.input.
func (h *Handlers) SurfaceInput(_ context.Context, raw json.RawMessage) {
	var req surfaceParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.DataBase64)
	if err != nil {
		return
	}
	h.service.SurfaceInput(req.SurfaceID, data)
}

// SurfaceResize handles surface.resize.
func (h *Handlers) SurfaceResize(_ context.Context, raw json.RawMessage) {
	var req surfaceParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	if req.Cols <= 0 || req.Rows <= 0 {
		return
	}
	h.service.SurfaceResize(req.SurfaceID, req.Cols, req.Rows)
}

// SurfaceClosed handles surface.closed.
func (h *Handlers) SurfaceClosed(_ context.Context, raw json.RawMessage) {
	var req surfaceParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	h.service.SurfaceClosed(req.SurfaceID)
}

// DialogSubmitted handles dialog.submit.
func (h *Handlers) DialogSubmitted(_ context.Context, raw json.RawMessage) {
	var req dialogParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	h.service.DialogSubmitted(req.DialogID, req.Values)
}

// DialogCancelled handles dialog.cancel.
func (h *Handlers) DialogCancelled(_ context.Context, raw json.RawMessage) {
	var req dialogParams
	if err := json.Unmarshal(raw, &req); err != nil {
		return
	}
	h.service.DialogCancelled(req.DialogID)
}
