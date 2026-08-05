package ipc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := Frame{Kind: KindChannelData, ChannelID: 7, Payload: []byte("hello")}
	if err := WriteFrame(&buf, want); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got.Kind != want.Kind || got.ChannelID != want.ChannelID || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestHeaderIsBigEndianAndNineBytes(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{Kind: KindJSONRPC, ChannelID: 0x01020304, Payload: []byte("ab")}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	raw := buf.Bytes()
	if len(raw) != HeaderLen+2 {
		t.Fatalf("frame length = %d, want %d", len(raw), HeaderLen+2)
	}
	if binary.BigEndian.Uint32(raw[0:4]) != 2 {
		t.Fatalf("length field = %v", raw[0:4])
	}
	if raw[4] != byte(KindJSONRPC) {
		t.Fatalf("kind byte = 0x%02x", raw[4])
	}
	if binary.BigEndian.Uint32(raw[5:9]) != 0x01020304 {
		t.Fatalf("channel id field = %v", raw[5:9])
	}
}

// The header and payload must leave in one Write. Two writes let a concurrent writer interleave
// between them and produce a frame that is a valid header followed by someone else's payload —
// corruption the reader cannot detect, because both halves are individually well-formed.
func TestWriteFrameEmitsASingleWrite(t *testing.T) {
	counter := &countingWriter{}
	if err := WriteFrame(counter, Frame{Kind: KindChannelData, ChannelID: 1, Payload: bytes.Repeat([]byte("x"), 4096)}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if counter.writes != 1 {
		t.Fatalf("writes = %d, want 1", counter.writes)
	}
}

type countingWriter struct{ writes int }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	return len(p), nil
}

func TestReadFrameRejectsReservedKind(t *testing.T) {
	for _, kind := range []byte{0x00, 0x04, 0x0f, 0xff} {
		var buf bytes.Buffer
		header := make([]byte, HeaderLen)
		header[4] = kind
		buf.Write(header)
		_, err := ReadFrame(&buf)
		if !errors.Is(err, ErrProtocolViolation) {
			t.Fatalf("kind 0x%02x: got %v, want a protocol violation", kind, err)
		}
	}
}

func TestReadFrameRejectsOversizeLength(t *testing.T) {
	var buf bytes.Buffer
	header := make([]byte, HeaderLen)
	binary.BigEndian.PutUint32(header[0:4], MaxFrameLength+1)
	header[4] = byte(KindChannelData)
	buf.Write(header)
	if _, err := ReadFrame(&buf); !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("got %v, want a protocol violation", err)
	}
}

// An oversize length must be refused BEFORE the payload is allocated: the whole point of the
// ceiling is that a corrupted length field cannot drive an allocation.
func TestReadFrameDoesNotAllocateOnOversizeLength(t *testing.T) {
	header := make([]byte, HeaderLen)
	binary.BigEndian.PutUint32(header[0:4], 0xFFFFFFFF)
	header[4] = byte(KindChannelData)
	// The reader supplies the header and then blocks forever. If ReadFrame tried to read the
	// declared payload it would hang here instead of returning.
	r := io.MultiReader(bytes.NewReader(header), neverReader{})
	done := make(chan error, 1)
	go func() {
		_, err := ReadFrame(r)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrProtocolViolation) {
			t.Fatalf("got %v, want a protocol violation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReadFrame tried to read a payload for an oversize length")
	}
}

type neverReader struct{}

func (neverReader) Read([]byte) (int, error) { select {} }

// A clean end at a frame boundary is EOF; an end mid-frame is ErrUnexpectedEOF. A caller that
// treated the two alike would report a truncated stream as a normal shutdown.
func TestReadFrameDistinguishesCleanEndFromTruncation(t *testing.T) {
	if _, err := ReadFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty stream: got %v, want io.EOF", err)
	}

	var buf bytes.Buffer
	_ = WriteFrame(&buf, Frame{Kind: KindChannelData, ChannelID: 1, Payload: []byte("abcdef")})
	truncated := buf.Bytes()[:HeaderLen+2]
	if _, err := ReadFrame(bytes.NewReader(truncated)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated payload: got %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestZeroLengthFrameIsValid(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, Frame{Kind: KindChannelData, ChannelID: 3}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if len(got.Payload) != 0 || got.ChannelID != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestPayloadsAreNotShared(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, Frame{Kind: KindChannelData, ChannelID: 1, Payload: []byte("first")})
	_ = WriteFrame(&buf, Frame{Kind: KindChannelData, ChannelID: 1, Payload: []byte("secnd")})
	a, _ := ReadFrame(&buf)
	b, _ := ReadFrame(&buf)
	if string(a.Payload) != "first" || string(b.Payload) != "secnd" {
		t.Fatalf("a=%q b=%q", a.Payload, b.Payload)
	}
	// A frame's bytes routinely outlive the read loop, so a reused buffer would corrupt whatever
	// still holds the earlier frame.
	if &a.Payload[0] == &b.Payload[0] {
		t.Fatal("consecutive frames share a payload buffer")
	}
}

// --- credit ---------------------------------------------------------------

func TestCreditRoundTrip(t *testing.T) {
	payload := EncodeCredit(9, 4)
	got, err := DecodeCredit(9, payload)
	if err != nil {
		t.Fatalf("DecodeCredit: %v", err)
	}
	if got != 4 {
		t.Fatalf("credit = %d, want 4", got)
	}
}

// A credit frame that disagrees with its own header is not a frame anyone can act on.
func TestDecodeCreditRejectsMismatchedChannel(t *testing.T) {
	if _, err := DecodeCredit(9, EncodeCredit(10, 4)); !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("got %v, want a protocol violation", err)
	}
}

func TestDecodeCreditRejectsZero(t *testing.T) {
	if _, err := DecodeCredit(9, EncodeCredit(9, 0)); !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("got %v, want a protocol violation", err)
	}
}

func TestCreditFrameLengthIsEnforcedOnBothSides(t *testing.T) {
	var buf bytes.Buffer
	err := WriteFrame(&buf, Frame{Kind: KindCredit, ChannelID: 1, Payload: []byte("short")})
	if !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("write: got %v, want a protocol violation", err)
	}

	header := make([]byte, HeaderLen)
	binary.BigEndian.PutUint32(header[0:4], 4)
	header[4] = byte(KindCredit)
	if _, err := ReadFrame(bytes.NewReader(header)); !errors.Is(err, ErrProtocolViolation) {
		t.Fatalf("read: got %v, want a protocol violation", err)
	}
}

func TestProtocolViolationMessageNamesTheProblem(t *testing.T) {
	err := &ProtocolViolationError{Reason: "oversize length", Kind: KindChannelData, Length: 99}
	if !strings.Contains(err.Error(), "oversize length") {
		t.Fatalf("message = %q", err.Error())
	}
}
