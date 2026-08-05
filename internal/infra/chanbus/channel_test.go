package chanbus

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type fakeTransport struct {
	mu       sync.Mutex
	writes   [][]byte
	grants   []uint32
	closes   []string
	writeErr error
}

func (f *fakeTransport) WriteChannel(_ uint32, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, append([]byte(nil), data...))
	return nil
}

func (f *fakeTransport) GrantCredit(_ uint32, credit uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants = append(f.grants, credit)
	return nil
}

func (f *fakeTransport) CloseChannel(_ uint32, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, reason)
}

func (f *fakeTransport) written() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.writes...)
}

func (f *fakeTransport) granted() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.grants...)
}

func TestReadReturnsDeliveredBytes(t *testing.T) {
	tr := &fakeTransport{}
	ch := New(1, tr, 4)
	ch.Deliver([]byte("hello"))

	buf := make([]byte, 16)
	n, err := ch.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("read %q", buf[:n])
	}
}

// A reader with a small buffer must drain a frame across several calls rather than losing its tail.
func TestReadSplitsAFrameAcrossCalls(t *testing.T) {
	ch := New(1, &fakeTransport{}, 4)
	ch.Deliver([]byte("abcdef"))

	var got []byte
	buf := make([]byte, 2)
	for i := 0; i < 3; i++ {
		n, err := ch.Read(buf)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	if string(got) != "abcdef" {
		t.Fatalf("got %q", got)
	}
}

// Credit is returned when a frame has been CONSUMED, not when it arrived. Otherwise the window
// measures delivery and a slow consumer fills memory while the host is told everything is fine.
func TestCreditIsGrantedOnConsumptionNotArrival(t *testing.T) {
	tr := &fakeTransport{}
	ch := New(1, tr, 4)
	ch.Deliver([]byte("one"))
	ch.Deliver([]byte("two"))

	if len(tr.granted()) != 0 {
		t.Fatal("credit was granted before anything was read")
	}

	buf := make([]byte, 16)
	if _, err := ch.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	// One frame consumed: still under the batch threshold of half the window.
	if len(tr.granted()) != 0 {
		t.Fatalf("granted too early: %v", tr.granted())
	}
	if _, err := ch.Read(buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	grants := tr.granted()
	if len(grants) != 1 || grants[0] != 2 {
		t.Fatalf("grants = %v, want one grant of 2", grants)
	}
}

// A partly-read frame has not been consumed, so its credit must not come back yet.
func TestCreditIsNotGrantedForAPartlyReadFrame(t *testing.T) {
	tr := &fakeTransport{}
	ch := New(1, tr, 4)
	ch.Deliver([]byte("abcdef"))
	ch.Deliver([]byte("ghijkl"))

	buf := make([]byte, 2)
	for i := 0; i < 4; i++ { // drains the first frame and starts the second
		if _, err := ch.Read(buf); err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	for _, g := range tr.granted() {
		if g > 1 {
			t.Fatalf("credit for an unfinished frame was returned: %v", tr.granted())
		}
	}
}

func TestWriteSpendsCredit(t *testing.T) {
	tr := &fakeTransport{}
	ch := New(1, tr, 2)
	if _, err := ch.Write([]byte("a")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := ch.Write([]byte("b")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(tr.written()) != 2 {
		t.Fatalf("writes = %v", tr.written())
	}
}

// With the window shut the writer waits rather than overrunning it — a plugin writing past its
// credit is a protocol violation the host kills the process for.
func TestWriteBlocksUntilCreditArrives(t *testing.T) {
	tr := &fakeTransport{}
	ch := New(1, tr, 1)
	if _, err := ch.Write([]byte("first")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := ch.Write([]byte("second"))
		done <- err
	}()

	select {
	case <-done:
		t.Fatal("the second write went out with no credit for it")
	case <-time.After(100 * time.Millisecond):
	}

	ch.AddCredit(1)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Write after credit: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a credit grant did not wake the writer")
	}
	if len(tr.written()) != 2 {
		t.Fatalf("writes = %v", tr.written())
	}
}

// The layers above deal in HTTP bodies, not frames. Splitting here keeps one transport detail out
// of all of them.
func TestWriteSplitsOversizeBuffers(t *testing.T) {
	tr := &fakeTransport{}
	ch := New(1, tr, 100)
	payload := bytes.Repeat([]byte("x"), MaxChannelFrame+10)
	n, err := ch.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("wrote %d of %d", n, len(payload))
	}
	writes := tr.written()
	if len(writes) != 2 || len(writes[0]) != MaxChannelFrame || len(writes[1]) != 10 {
		t.Fatalf("writes split as %d frames of %d and %d", len(writes), len(writes[0]), len(writes[1]))
	}
}

// A reader parked on an empty channel must wake when the host closes it, or every consumer of a
// finished stream hangs forever.
func TestCloseRemoteWakesABlockedReader(t *testing.T) {
	ch := New(1, &fakeTransport{}, 4)
	done := make(chan error, 1)
	go func() {
		_, err := ch.Read(make([]byte, 4))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	ch.CloseRemote("the container stopped")

	select {
	case err := <-done:
		if !errors.Is(err, ErrChannelClosed) {
			t.Fatalf("got %v, want ErrChannelClosed", err)
		}
		if err.Error() == ErrChannelClosed.Error() {
			t.Fatal("the reason the host gave was dropped; it is the only explanation the user gets")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked reader was not woken by the close")
	}
}

func TestCloseRemoteWakesABlockedWriter(t *testing.T) {
	ch := New(1, &fakeTransport{}, 0)
	done := make(chan error, 1)
	go func() {
		_, err := ch.Write([]byte("x"))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	ch.CloseRemote("")

	select {
	case err := <-done:
		if !errors.Is(err, ErrChannelClosed) {
			t.Fatalf("got %v, want ErrChannelClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a writer waiting on credit was not woken by the close")
	}
}

// Bytes already delivered are still readable after a close: the stream ended, but what arrived
// before it ended is the tail of a log the user still wants.
func TestReadDrainsBufferedBytesAfterClose(t *testing.T) {
	ch := New(1, &fakeTransport{}, 4)
	ch.Deliver([]byte("tail"))
	ch.CloseRemote("")

	buf := make([]byte, 16)
	n, err := ch.Read(buf)
	if err != nil {
		t.Fatalf("Read after close: %v", err)
	}
	if string(buf[:n]) != "tail" {
		t.Fatalf("read %q", buf[:n])
	}
	if _, err := ch.Read(buf); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("second read: got %v, want ErrChannelClosed", err)
	}
}

// Both sides may close without coordinating (ADR-011), so a second close must be a no-op rather
// than a second notification.
func TestCloseIsIdempotent(t *testing.T) {
	tr := &fakeTransport{}
	ch := New(1, tr, 4)
	if err := ch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.closes) != 1 {
		t.Fatalf("the host was told %d times, want 1", len(tr.closes))
	}
}

// A channel the plugin closed must not tell the host again when the host closes it back.
func TestCloseAfterRemoteCloseDoesNotNotify(t *testing.T) {
	tr := &fakeTransport{}
	ch := New(1, tr, 4)
	ch.CloseRemote("host said so")
	_ = ch.Close()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.closes) != 0 {
		t.Fatalf("the host was notified about a close it initiated: %v", tr.closes)
	}
}

func TestDeliverAfterCloseIsDropped(t *testing.T) {
	ch := New(1, &fakeTransport{}, 4)
	ch.CloseRemote("")
	ch.Deliver([]byte("late"))
	if _, err := ch.Read(make([]byte, 4)); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("got %v, want ErrChannelClosed", err)
	}
}

func TestReadOnAnEmptyClosedChannelIsEOFWhenNoReasonWasGiven(t *testing.T) {
	ch := New(1, &fakeTransport{}, 4)
	ch.Close()
	_, err := ch.Read(make([]byte, 4))
	if !errors.Is(err, ErrChannelClosed) && !errors.Is(err, io.EOF) {
		t.Fatalf("got %v", err)
	}
}

func TestConcurrentReadersAndWritersAreRaceFree(t *testing.T) {
	tr := &fakeTransport{}
	ch := New(1, tr, 1000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = ch.Write([]byte("x"))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ch.Deliver([]byte("y"))
				ch.AddCredit(1)
			}
		}()
	}
	go func() {
		buf := make([]byte, 8)
		for {
			if _, err := ch.Read(buf); err != nil {
				return
			}
		}
	}()
	wg.Wait()
	ch.Close()
}
