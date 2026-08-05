package chanbus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// RPC is what the manager needs from the JSON-RPC client.
type RPC interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Notify(method string, params any) error
	WriteChannel(channelID uint32, data []byte) error
	GrantCredit(channelID, credit uint32) error
}

// execTemplate is the index of `docker system dial-stdio` in the manifest's execCommands list.
//
// The host matches a channel.open against the declared templates by index; there is exactly one,
// and this constant is the only place that fact is written down.
const execTemplate = 0

// Manager opens exec channels and routes inbound frames to them.
//
// It owns the id→channel map because the frame reader has one callback for every channel and needs
// somewhere to look up which one a frame belongs to. Nothing else about a channel lives here.
type Manager struct {
	rpc RPC

	mu       sync.RWMutex
	channels map[uint32]*Channel
}

// NewManager creates a channel manager.
func NewManager(rpc RPC) *Manager {
	return &Manager{rpc: rpc, channels: make(map[uint32]*Channel)}
}

type openRequest struct {
	Purpose         string `json:"purpose"`
	ParentSessionID string `json:"parentSessionId"`
	Hint            string `json:"hint"`
}

type execHint struct {
	Template int               `json:"template"`
	Params   map[string]string `json:"params,omitempty"`
}

// OpenExec opens a channel running the plugin's one declared command over the parent session.
//
// The returned stream is the daemon socket: `docker system dial-stdio` connects its own stdio to
// it, and the host relays that stdio onto the channel.
func (m *Manager) OpenExec(ctx context.Context, parentSessionID string) (*Channel, error) {
	hint, err := json.Marshal(execHint{Template: execTemplate})
	if err != nil {
		return nil, err
	}
	raw, err := m.rpc.Call(ctx, "channel.open", openRequest{
		Purpose:         "exec",
		ParentSessionID: parentSessionID,
		Hint:            string(hint),
	})
	if err != nil {
		return nil, fmt.Errorf("chanbus: open exec channel: %w", err)
	}
	var res struct {
		ChannelID uint32 `json:"channelId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("chanbus: decode channel id: %w", err)
	}

	// The host grants four frames for an exec channel (ADR-011). Assuming it rather than waiting
	// for a credit frame is what lets the first request go out immediately; the host corrects us
	// with credit updates from then on.
	ch := New(res.ChannelID, m, initialCredit)
	m.mu.Lock()
	m.channels[res.ChannelID] = ch
	m.mu.Unlock()
	return ch, nil
}

// Deliver routes an inbound data frame. Wired to the client's binary handler.
func (m *Manager) Deliver(channelID uint32, payload []byte) {
	if ch := m.lookup(channelID); ch != nil {
		ch.Deliver(payload)
	}
}

// Credit routes an inbound credit frame. Wired to the client's credit handler.
func (m *Manager) Credit(channelID uint32, credit uint32) {
	if ch := m.lookup(channelID); ch != nil {
		ch.AddCredit(credit)
	}
}

// HostClosed handles the host's channel.close notification: the remote end ended, for a reason the
// user may need (a command that exited non-zero arrives this way, not as a data frame).
func (m *Manager) HostClosed(params json.RawMessage) {
	var note struct {
		ChannelID uint32 `json:"channelId"`
		Reason    string `json:"reason"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(params, &note); err != nil {
		return
	}
	m.mu.Lock()
	ch := m.channels[note.ChannelID]
	delete(m.channels, note.ChannelID)
	m.mu.Unlock()
	if ch == nil {
		return
	}
	reason := note.Message
	if reason == "" {
		reason = note.Reason
	}
	ch.CloseRemote(reason)
}

// CloseAll ends every open channel. Called on shutdown so no stream outlives the process.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	channels := make([]*Channel, 0, len(m.channels))
	for _, ch := range m.channels {
		channels = append(channels, ch)
	}
	m.channels = make(map[uint32]*Channel)
	m.mu.Unlock()
	for _, ch := range channels {
		_ = ch.Close()
	}
}

func (m *Manager) lookup(channelID uint32) *Channel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.channels[channelID]
}

// --- Transport, for the channels this manager owns -------------------------

// WriteChannel sends data for a channel.
func (m *Manager) WriteChannel(channelID uint32, data []byte) error {
	return m.rpc.WriteChannel(channelID, data)
}

// GrantCredit returns credit for a channel.
func (m *Manager) GrantCredit(channelID, credit uint32) error {
	return m.rpc.GrantCredit(channelID, credit)
}

// CloseChannel tells the host a channel is finished and forgets it.
func (m *Manager) CloseChannel(channelID uint32, reason string) {
	m.mu.Lock()
	delete(m.channels, channelID)
	m.mu.Unlock()
	_ = m.rpc.Notify("channel.close", map[string]any{
		"channelId": channelID,
		"reason":    reason,
	})
}

var _ Transport = (*Manager)(nil)
