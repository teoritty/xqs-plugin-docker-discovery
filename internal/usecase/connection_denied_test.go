package usecase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// deniedStreams refuses every open the way the host does, and counts how often it was asked.
//
// The wrapping matters as much as the sentinel: the real adapter wraps, and a test that handed back
// a bare ErrHostDenied would pass against an implementation that compares with == and still hang in
// production.
type deniedStreams struct{ opens atomic.Int32 }

func (d *deniedStreams) OpenExec(context.Context, string) (io.ReadWriteCloser, error) {
	d.opens.Add(1)
	return nil, fmt.Errorf("chanbus: open exec channel: %w", ErrHostDenied)
}

func TestDialMarksTheConnectionGoneWhenTheHostRefuses(t *testing.T) {
	streams := &deniedStreams{}
	conn := NewConnection("session-1", streams, nil)

	_, err := conn.Control(context.Background())
	if !errors.Is(err, ErrSessionGone) {
		t.Fatalf("Control() error = %v, want ErrSessionGone; a refusal the plugin cannot recognise is a retry loop", err)
	}
	if !conn.Gone() {
		t.Fatal("a refused connection must be marked gone, or every later caller dials again")
	}
	if _, err := conn.Control(context.Background()); !errors.Is(err, ErrNoDocker) {
		t.Fatalf("second Control() error = %v, want ErrNoDocker from the closed connection", err)
	}
	if got := streams.opens.Load(); got != 1 {
		t.Fatalf("OpenExec called %d times, want 1; a connection already known gone must not dial again", got)
	}
}

// TestWatcherStopsOnARefusal is the regression guard proper: it asserts the loop ENDS.
//
// Before the fix the refusal was invisible to isSessionGone, so watchEvents took it for an ordinary
// stream drop, slept five seconds and tried again — for as long as the application ran. That is what
// pinned the plugin process in memory and filled the host's audit log; a test that only checked the
// error value would not have caught it, because the error value was right and the loop was wrong.
func TestWatcherStopsOnARefusal(t *testing.T) {
	streams := &deniedStreams{}
	svc := NewService(&recordingHost{}, streams, nil)
	conn := svc.connection("session-1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.watchEvents(context.Background(), conn)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Two seconds is chosen against the retry pause it must not take: watchEvents sleeps five
		// seconds between attempts, so anything that is still running here is still retrying.
		t.Fatal("watchEvents did not stop after the host refused; it is retrying a call that will never be allowed")
	}

	if got := streams.opens.Load(); got != 1 {
		t.Fatalf("OpenExec called %d times, want 1; the watcher must not retry a refusal", got)
	}
	if !conn.Gone() {
		t.Fatal("the watcher must leave the connection marked gone")
	}
}
