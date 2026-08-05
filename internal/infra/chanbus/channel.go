// Package chanbus turns the host's binary channel bus into an io.ReadWriteCloser.
//
// Everything above this package — the Docker Engine API client, the log pump, the exec relay —
// wants a stream. What the host offers is `channel.open` over JSON-RPC, then kind=0x02 frames in
// both directions with credit-based flow control (ADR-011). This package is the whole of that
// translation and nothing else.
package chanbus

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrChannelClosed is returned by Read and Write once a channel has ended.
var ErrChannelClosed = errors.New("chanbus: channel closed")

// Transport is what a channel needs from the JSON-RPC client: send bytes, grant credit, and close.
// An interface rather than the concrete client so a channel can be tested without a wire.
type Transport interface {
	WriteChannel(channelID uint32, data []byte) error
	GrantCredit(channelID, credit uint32) error
	CloseChannel(channelID uint32, reason string)
}

// initialCredit is what the host grants an exec channel, and therefore what we grant back for the
// inbound direction (ADR-011 "Flow control"). Four frames in flight is enough to keep a stream
// moving and small enough that a stalled consumer parks the producer rather than the host's memory.
const initialCredit = 4

// Channel is one open binary channel, presented as a stream.
//
// Reads are fed by the frame reader through deliver; writes go out as kind=0x02 frames. The credit
// window is honoured in both directions: we do not write past what the host granted, and we grant
// the host more only once a frame has actually been consumed — which is what makes the backpressure
// real rather than a number that always says yes.
type Channel struct {
	id        uint32
	transport Transport

	mu       sync.Mutex
	cond     *sync.Cond
	inbound  [][]byte
	pending  []byte
	credit   uint32
	closed   bool
	closeErr error
	// consumed counts frames the reader has finished with but whose credit has not yet been
	// returned, so credit is granted in batches instead of one RPC per frame.
	consumed uint32
}

// New creates a channel over an already-opened channel id.
func New(id uint32, transport Transport, grantedCredit uint32) *Channel {
	c := &Channel{id: id, transport: transport, credit: grantedCredit}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// ID returns the host-allocated channel id.
func (c *Channel) ID() uint32 { return c.id }

// Deliver hands the channel one inbound frame. Called by the frame reader, never by a consumer.
func (c *Channel) Deliver(payload []byte) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.inbound = append(c.inbound, payload)
	c.mu.Unlock()
	c.cond.Broadcast()
}

// AddCredit records a credit grant from the host and wakes any writer waiting on the window.
func (c *Channel) AddCredit(credit uint32) {
	c.mu.Lock()
	c.credit += credit
	c.mu.Unlock()
	c.cond.Broadcast()
}

// CloseRemote marks the channel closed because the host said so. Blocked readers and writers wake
// and fail; the transport is not told, since it is the one that told us.
func (c *Channel) CloseRemote(reason string) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	if reason != "" {
		c.closeErr = fmt.Errorf("%w: %s", ErrChannelClosed, reason)
	} else {
		c.closeErr = ErrChannelClosed
	}
	c.mu.Unlock()
	c.cond.Broadcast()
}

// Read returns bytes from the inbound queue, blocking until some arrive.
//
// Credit for a frame is returned when the frame has been fully handed to the caller — not when it
// arrived. That is the difference between flow control that measures consumption and flow control
// that measures delivery, and only the first one stops a slow consumer from filling memory.
func (c *Channel) Read(p []byte) (int, error) {
	c.mu.Lock()
	for len(c.pending) == 0 && len(c.inbound) == 0 && !c.closed {
		c.cond.Wait()
	}
	if len(c.pending) == 0 && len(c.inbound) == 0 {
		err := c.closeErr
		c.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	if len(c.pending) == 0 {
		c.pending = c.inbound[0]
		c.inbound = c.inbound[1:]
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]

	var grant uint32
	if len(c.pending) == 0 {
		c.consumed++
		// Granted in batches: one RPC per frame would put a round trip in front of every 32 KiB of
		// a log stream. Half the window is the point where the writer would otherwise stall.
		if c.consumed >= initialCredit/2 {
			grant = c.consumed
			c.consumed = 0
		}
	}
	c.mu.Unlock()

	if grant > 0 {
		// Outside the lock: it is an RPC, and holding the channel's mutex across one would block
		// every reader and writer on the host's scheduling.
		_ = c.transport.GrantCredit(c.id, grant)
	}
	return n, nil
}

// Write sends bytes as channel frames, waiting for credit when the window is shut.
//
// A caller's buffer is split to the frame ceiling rather than refused: the layers above deal in
// HTTP bodies and log lines, and making each of them know the frame size would spread one transport
// detail across the whole plugin.
func (c *Channel) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		chunk := p[written:]
		if len(chunk) > MaxChannelFrame {
			chunk = chunk[:MaxChannelFrame]
		}
		if err := c.awaitCredit(); err != nil {
			return written, err
		}
		if err := c.transport.WriteChannel(c.id, chunk); err != nil {
			return written, err
		}
		written += len(chunk)
	}
	return written, nil
}

// MaxChannelFrame is the largest binary frame the host accepts (ADR-011).
const MaxChannelFrame = 1 << 20

// awaitCredit blocks until the window allows one more frame, then spends it.
func (c *Channel) awaitCredit() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.credit == 0 && !c.closed {
		c.cond.Wait()
	}
	if c.closed {
		if c.closeErr != nil {
			return c.closeErr
		}
		return ErrChannelClosed
	}
	c.credit--
	return nil
}

// Close ends the channel and tells the host.
//
// Idempotent, because the host may have closed it first and both sides must be able to call this
// without coordinating (ADR-011: close is idempotent regardless of which side initiates).
func (c *Channel) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.closeErr = ErrChannelClosed
	c.mu.Unlock()
	c.cond.Broadcast()
	c.transport.CloseChannel(c.id, "done")
	return nil
}
