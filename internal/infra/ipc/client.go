package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// ErrClosed reports that the connection to the host is gone. Every pending call fails with it, and
// so does every later one: a plugin whose host has closed stdio has nothing left to do.
var ErrClosed = errors.New("ipc: connection closed")

// Handler serves one inbound host->plugin message. A request returns a result to send back; a
// notification returns nil and its result is discarded.
type Handler func(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error)

// BinaryHandler receives one channel data frame.
type BinaryHandler func(channelID uint32, payload []byte)

// CreditHandler receives one flow-control update.
type CreditHandler func(channelID uint32, credit uint32)

// Client multiplexes the single stdio pipe pair: JSON-RPC in both directions on channel 0, binary
// data and credit on the channels the host allocates.
//
// One reader goroutine owns the input side and one mutex serializes writes. That split is the
// whole concurrency design: frames must not interleave on the wire (see WriteFrame), and a second
// reader would race for the next frame with no way to put one back.
type Client struct {
	out io.Writer
	in  io.Reader

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan Message
	closed  bool
	closeMu sync.Once
	done    chan struct{}

	handler Handler
	binary  BinaryHandler
	credit  CreditHandler
}

// NewClient creates a client over the given streams, usually os.Stdin and os.Stdout.
func NewClient(in io.Reader, out io.Writer) *Client {
	return &Client{
		in:      in,
		out:     out,
		pending: make(map[string]chan Message),
		done:    make(chan struct{}),
	}
}

// SetHandler registers the handler for inbound host->plugin RPC. Must be called before Serve.
func (c *Client) SetHandler(h Handler) { c.handler = h }

// SetBinaryHandler registers the sink for channel data frames.
func (c *Client) SetBinaryHandler(h BinaryHandler) { c.binary = h }

// SetCreditHandler registers the sink for flow-control updates.
func (c *Client) SetCreditHandler(h CreditHandler) { c.credit = h }

// Done is closed when the connection ends, for whatever reason.
func (c *Client) Done() <-chan struct{} { return c.done }

// Serve reads frames until the stream ends or breaks. It returns the reason.
//
// A protocol violation ends the connection rather than being skipped: once frame boundaries cannot
// be trusted, every later read is guesswork, and continuing would turn a detectable fault into
// silent corruption.
func (c *Client) Serve(ctx context.Context) error {
	defer c.close()
	for {
		frame, err := ReadFrame(c.in)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch frame.Kind {
		case KindJSONRPC:
			c.dispatchJSONRPC(ctx, frame.Payload)
		case KindChannelData:
			if c.binary != nil {
				c.binary(frame.ChannelID, frame.Payload)
			}
		case KindCredit:
			credit, err := DecodeCredit(frame.ChannelID, frame.Payload)
			if err != nil {
				return err
			}
			if c.credit != nil {
				c.credit(frame.ChannelID, credit)
			}
		}
	}
}

// Call sends a request and waits for its response.
//
// The context bounds the wait, not the send: a cancelled call stops waiting but the request has
// already left, so the pending entry is removed rather than left to leak. The host answers on its
// own schedule and its late response is then dropped, which is correct — nobody is listening.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}

	id, ch, err := c.registerPending()
	if err != nil {
		return nil, err
	}
	defer c.forgetPending(id)

	payload, err := EncodeMessage(Message{ID: json.RawMessage(strconv.Quote(id)), Method: method, Params: raw})
	if err != nil {
		return nil, err
	}
	if err := c.writeFrame(Frame{Kind: KindJSONRPC, ChannelID: ControlChannel, Payload: payload}); err != nil {
		return nil, err
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, ErrClosed
	case msg := <-ch:
		if msg.Error != nil {
			return nil, msg.Error
		}
		return msg.Result, nil
	}
}

// Notify sends a one-way message.
func (c *Client) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	payload, err := EncodeMessage(Message{Method: method, Params: raw})
	if err != nil {
		return err
	}
	return c.writeFrame(Frame{Kind: KindJSONRPC, ChannelID: ControlChannel, Payload: payload})
}

// WriteChannel sends binary data on an open channel.
func (c *Client) WriteChannel(channelID uint32, data []byte) error {
	return c.writeFrame(Frame{Kind: KindChannelData, ChannelID: channelID, Payload: data})
}

// GrantCredit tells the host it may send us more frames on a channel.
func (c *Client) GrantCredit(channelID, credit uint32) error {
	return c.writeFrame(Frame{Kind: KindCredit, ChannelID: channelID, Payload: EncodeCredit(channelID, credit)})
}

func (c *Client) writeFrame(f Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	return WriteFrame(c.out, f)
}

func (c *Client) registerPending() (string, chan Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return "", nil, ErrClosed
	}
	c.nextID++
	id := "d" + strconv.FormatUint(c.nextID, 10)
	// Buffered, so a response that arrives after the caller gave up does not block the reader
	// goroutine — which serves every other channel and must never be parked by one abandoned call.
	ch := make(chan Message, 1)
	c.pending[id] = ch
	return id, ch, nil
}

func (c *Client) forgetPending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// dispatchJSONRPC routes one control-plane message: a response resumes its caller, a request or
// notification goes to the handler.
func (c *Client) dispatchJSONRPC(ctx context.Context, payload []byte) {
	msg, err := DecodeMessage(payload)
	if err != nil {
		// A malformed control message is not a framing violation: the frame arrived intact and only
		// its contents are wrong. Dropping it keeps a stray message from taking the process down.
		return
	}

	if msg.IsResponse() {
		c.resume(msg)
		return
	}
	if c.handler == nil {
		return
	}
	// Handled on its own goroutine so a handler that calls back into the host — which every
	// interesting one does — cannot deadlock against the reader that would deliver its answer.
	go c.serveInbound(ctx, msg)
}

func (c *Client) serveInbound(ctx context.Context, msg Message) {
	result, err := c.handler(ctx, msg.Method, msg.Params)
	if msg.IsNotification() {
		return
	}
	reply := Message{ID: msg.ID}
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			reply.Error = rpcErr
		} else {
			reply.Error = &RPCError{Code: -32603, Message: err.Error()}
		}
	} else {
		reply.Result = result
	}
	payload, encErr := EncodeMessage(reply)
	if encErr != nil {
		payload, _ = EncodeMessage(Message{ID: msg.ID, Error: &RPCError{Code: -32603, Message: "response too large"}})
	}
	_ = c.writeFrame(Frame{Kind: KindJSONRPC, ChannelID: ControlChannel, Payload: payload})
}

func (c *Client) resume(msg Message) {
	var id string
	if err := json.Unmarshal(msg.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	c.mu.Unlock()
	if ch == nil {
		// The caller gave up. Its entry is already gone and the buffered send would have nobody to
		// receive it.
		return
	}
	select {
	case ch <- msg:
	default:
	}
}

func (c *Client) close() {
	c.closeMu.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		close(c.done)
	})
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("ipc: encode params: %w", err)
	}
	return encoded, nil
}
