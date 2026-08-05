package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// pipePair wires a client to a fake host: what the client writes, the host reads, and vice versa.
type pipePair struct {
	client   *Client
	hostIn   *io.PipeReader // what the client wrote
	hostOut  *io.PipeWriter // what the host sends
	serveErr chan error
}

func newPipePair(t *testing.T) *pipePair {
	t.Helper()
	clientReads, hostWrites := io.Pipe()
	hostReads, clientWrites := io.Pipe()
	p := &pipePair{
		client:   NewClient(clientReads, clientWrites),
		hostIn:   hostReads,
		hostOut:  hostWrites,
		serveErr: make(chan error, 1),
	}
	t.Cleanup(func() {
		_ = hostWrites.Close()
		_ = clientWrites.Close()
	})
	return p
}

func (p *pipePair) serve(ctx context.Context) {
	go func() { p.serveErr <- p.client.Serve(ctx) }()
}

// hostRead reads one frame the client sent.
func (p *pipePair) hostRead(t *testing.T) Frame {
	t.Helper()
	type result struct {
		f   Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := ReadFrame(p.hostIn)
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("host read: %v", r.err)
		}
		return r.f
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a frame from the client")
		return Frame{}
	}
}

func (p *pipePair) hostWrite(t *testing.T, f Frame) {
	t.Helper()
	if err := WriteFrame(p.hostOut, f); err != nil {
		t.Fatalf("host write: %v", err)
	}
}

func TestCallReceivesItsResponse(t *testing.T) {
	p := newPipePair(t)
	p.serve(context.Background())

	done := make(chan json.RawMessage, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := p.client.Call(context.Background(), "surface.open", map[string]string{"kind": "log"})
		if err != nil {
			errCh <- err
			return
		}
		done <- res
	}()

	req := p.hostRead(t)
	if req.Kind != KindJSONRPC || req.ChannelID != ControlChannel {
		t.Fatalf("request went out as %+v", req)
	}
	msg, err := DecodeMessage(req.Payload)
	if err != nil || msg.Method != "surface.open" || !msg.IsRequest() {
		t.Fatalf("decoded %+v (%v)", msg, err)
	}

	reply, _ := EncodeMessage(Message{ID: msg.ID, Result: json.RawMessage(`{"surfaceId":"srf-1"}`)})
	p.hostWrite(t, Frame{Kind: KindJSONRPC, Payload: reply})

	select {
	case res := <-done:
		if string(res) != `{"surfaceId":"srf-1"}` {
			t.Fatalf("result = %s", res)
		}
	case err := <-errCh:
		t.Fatalf("Call: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Call never returned")
	}
}

// An error response must surface as an *RPCError so callers can tell a denial from a transport
// failure without parsing a message.
func TestCallSurfacesHostErrors(t *testing.T) {
	p := newPipePair(t)
	p.serve(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := p.client.Call(context.Background(), "surface.open", nil)
		errCh <- err
	}()

	req := p.hostRead(t)
	msg, _ := DecodeMessage(req.Payload)
	reply, _ := EncodeMessage(Message{ID: msg.ID, Error: &RPCError{Code: CodeCapabilityDenied, Message: "denied"}})
	p.hostWrite(t, Frame{Kind: KindJSONRPC, Payload: reply})

	select {
	case err := <-errCh:
		if !IsCapabilityDenied(err) {
			t.Fatalf("got %v, want a capability denial", err)
		}
		if IsRateLimited(err) {
			t.Fatal("a denial must not read as rate limiting: one is permanent, the other is not")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call never returned")
	}
}

// A cancelled call stops waiting, but the request has already left. The pending entry must go, or
// every abandoned call leaks one.
func TestCancelledCallDoesNotLeakItsPendingEntry(t *testing.T) {
	p := newPipePair(t)
	p.serve(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := p.client.Call(ctx, "slow.method", nil)
		errCh <- err
	}()
	p.hostRead(t)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled Call never returned")
	}

	p.client.mu.Lock()
	pending := len(p.client.pending)
	p.client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending entries = %d, want 0", pending)
	}
}

// A late response for an abandoned call must not park the reader goroutine, which serves every
// other channel.
func TestLateResponseDoesNotBlockTheReader(t *testing.T) {
	p := newPipePair(t)
	p.serve(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, _ = p.client.Call(ctx, "slow.method", nil) }()
	req := p.hostRead(t)
	msg, _ := DecodeMessage(req.Payload)
	cancel()
	time.Sleep(20 * time.Millisecond)

	reply, _ := EncodeMessage(Message{ID: msg.ID, Result: json.RawMessage(`{}`)})
	p.hostWrite(t, Frame{Kind: KindJSONRPC, Payload: reply})

	// The reader must still be alive: a notification sent afterwards has to be delivered.
	delivered := make(chan string, 1)
	p.client.SetHandler(func(_ context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
		delivered <- method
		return nil, nil
	})
	note, _ := EncodeMessage(Message{Method: "surface.closed"})
	p.hostWrite(t, Frame{Kind: KindJSONRPC, Payload: note})

	select {
	case method := <-delivered:
		if method != "surface.closed" {
			t.Fatalf("method = %q", method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the reader was blocked by a response nobody was waiting for")
	}
}

func TestInboundRequestIsAnswered(t *testing.T) {
	p := newPipePair(t)
	p.client.SetHandler(func(_ context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
		if method != "discovery.describeNode" {
			return nil, errors.New("unexpected method")
		}
		return json.RawMessage(`{"editable":true}`), nil
	})
	p.serve(context.Background())

	req, _ := EncodeMessage(Message{ID: json.RawMessage(`"h1"`), Method: "discovery.describeNode"})
	p.hostWrite(t, Frame{Kind: KindJSONRPC, Payload: req})

	reply := p.hostRead(t)
	msg, err := DecodeMessage(reply.Payload)
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if string(msg.Result) != `{"editable":true}` {
		t.Fatalf("result = %s, error = %v", msg.Result, msg.Error)
	}
}

// A handler that calls back into the host is the normal case — publishing a snapshot in response
// to observe, for instance. Serving inbound messages on the reader goroutine would deadlock it
// against the response it is waiting for.
func TestInboundHandlerMayCallBackIntoTheHost(t *testing.T) {
	p := newPipePair(t)
	p.client.SetHandler(func(ctx context.Context, method string, _ json.RawMessage) (json.RawMessage, error) {
		if method == "discovery.observe" {
			if _, err := p.client.Call(ctx, "discovery.publish", nil); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	p.serve(context.Background())

	note, _ := EncodeMessage(Message{Method: "discovery.observe"})
	p.hostWrite(t, Frame{Kind: KindJSONRPC, Payload: note})

	nested := p.hostRead(t)
	msg, _ := DecodeMessage(nested.Payload)
	if msg.Method != "discovery.publish" {
		t.Fatalf("the handler's own call never reached the host: %+v", msg)
	}
	reply, _ := EncodeMessage(Message{ID: msg.ID, Result: json.RawMessage(`{"ok":true}`)})
	p.hostWrite(t, Frame{Kind: KindJSONRPC, Payload: reply})
}

func TestInboundNotificationGetsNoReply(t *testing.T) {
	p := newPipePair(t)
	served := make(chan struct{})
	p.client.SetHandler(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		close(served)
		return json.RawMessage(`{"ignored":true}`), nil
	})
	p.serve(context.Background())

	note, _ := EncodeMessage(Message{Method: "surface.input"})
	p.hostWrite(t, Frame{Kind: KindJSONRPC, Payload: note})
	<-served

	// Nothing should come back. A frame arriving here would be a reply to a notification.
	got := make(chan struct{}, 1)
	go func() {
		if _, err := ReadFrame(p.hostIn); err == nil {
			got <- struct{}{}
		}
	}()
	select {
	case <-got:
		t.Fatal("a notification was answered")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestBinaryAndCreditFramesReachTheirHandlers(t *testing.T) {
	p := newPipePair(t)
	var mu sync.Mutex
	var data []byte
	var credit uint32
	gotData := make(chan struct{})
	gotCredit := make(chan struct{})
	p.client.SetBinaryHandler(func(_ uint32, payload []byte) {
		mu.Lock()
		data = payload
		mu.Unlock()
		close(gotData)
	})
	p.client.SetCreditHandler(func(_ uint32, c uint32) {
		mu.Lock()
		credit = c
		mu.Unlock()
		close(gotCredit)
	})
	p.serve(context.Background())

	p.hostWrite(t, Frame{Kind: KindChannelData, ChannelID: 5, Payload: []byte("bytes")})
	p.hostWrite(t, Frame{Kind: KindCredit, ChannelID: 5, Payload: EncodeCredit(5, 4)})

	<-gotData
	<-gotCredit
	mu.Lock()
	defer mu.Unlock()
	if string(data) != "bytes" || credit != 4 {
		t.Fatalf("data=%q credit=%d", data, credit)
	}
}

// Once frame boundaries cannot be trusted, continuing would turn a detectable fault into silent
// corruption.
func TestServeEndsOnAProtocolViolation(t *testing.T) {
	p := newPipePair(t)
	p.serve(context.Background())

	bad := make([]byte, HeaderLen)
	bad[4] = 0x09 // reserved kind
	if _, err := p.hostOut.Write(bad); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case err := <-p.serveErr:
		if !errors.Is(err, ErrProtocolViolation) {
			t.Fatalf("Serve returned %v, want a protocol violation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve kept reading after a protocol violation")
	}
}

func TestCallsFailOnceTheConnectionIsClosed(t *testing.T) {
	p := newPipePair(t)
	p.serve(context.Background())
	_ = p.hostOut.Close()

	select {
	case <-p.client.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done was never closed")
	}

	if _, err := p.client.Call(context.Background(), "any", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

func TestEncodeMessageRefusesAnOversizePayload(t *testing.T) {
	big := make([]byte, MaxJSONRPCFrame)
	for i := range big {
		big[i] = 'x'
	}
	params, _ := json.Marshal(string(big))
	if _, err := EncodeMessage(Message{Method: "x", Params: params}); err == nil {
		t.Fatal("expected a message over the JSON-RPC limit to be refused before it is sent")
	}
}
