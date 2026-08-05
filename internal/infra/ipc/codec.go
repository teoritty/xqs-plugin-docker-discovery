// Package ipc implements the length-prefixed frame layer the host and a plugin speak over
// stdin/stdout once the binary channel bus is live (ADR-011 "Frame layer").
//
// Wire format:
//
//	[4 bytes: payload length][1 byte: kind][4 bytes: channelId][payload]
//
// length and channelId are big-endian uint32; length excludes the 9 header bytes.
//
// This file is the lowest layer in the plugin and knows about bytes and frame boundaries only —
// nothing about JSON-RPC, Docker, or sessions. It imports nothing beyond the standard library.
package ipc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Kind identifies what a frame's payload contains.
type Kind byte

const (
	// KindJSONRPC carries a JSON-RPC 2.0 message. In practice the host uses channel 0 for it.
	KindJSONRPC Kind = 0x01
	// KindChannelData carries raw channel bytes: no JSON, no base64.
	KindChannelData Kind = 0x02
	// KindCredit carries a fixed 8-byte flow-control update: [4B channelId][4B credit].
	KindCredit Kind = 0x03
)

// HeaderLen is the fixed frame header size: 4 (length) + 1 (kind) + 4 (channelId).
const HeaderLen = 9

// MaxFrameLength is the largest payload this codec will read or write.
//
// It is the larger of the two documented ceilings (1 MiB for binary channel data; JSON-RPC is
// capped at 256 KiB) because this layer is deliberately kind-agnostic: it exists to stop a
// corrupted length field from driving an unbounded allocation, and the kind-specific limits belong
// to the layers that know which kind they are handling.
const MaxFrameLength = 1 << 20

// CreditPayloadLen is the fixed size of a credit frame's payload.
const CreditPayloadLen = 8

// ErrProtocolViolation is the sentinel wrapped by every framing refusal, so callers can test with
// errors.Is rather than by string.
var ErrProtocolViolation = errors.New("ipc: protocol violation")

// ProtocolViolationError reports a frame that breaks the wire contract.
//
// There is no recovery path from one: once frame boundaries cannot be trusted, every subsequent
// read is guesswork. The host kills a plugin that sends one, and a plugin that receives one exits
// rather than trying to resynchronise — this type exists so the exit can say why.
type ProtocolViolationError struct {
	Reason string
	Kind   Kind
	Length uint32
}

func (e *ProtocolViolationError) Error() string {
	return fmt.Sprintf("ipc: protocol violation: %s (kind=0x%02x length=%d)", e.Reason, byte(e.Kind), e.Length)
}

func (e *ProtocolViolationError) Unwrap() error { return ErrProtocolViolation }

// Frame is one decoded message.
type Frame struct {
	Kind      Kind
	ChannelID uint32
	// Payload is owned by the caller: ReadFrame allocates a fresh slice per frame rather than
	// reusing a buffer, because a frame's bytes routinely outlive the read loop that produced them
	// (they are handed to a channel consumer or parked in a queue).
	Payload []byte
}

// ValidKind reports whether k is one of the three kinds v1 defines. 0x04-0x0F are reserved and
// forbidden; anything else is a violation.
func ValidKind(k Kind) bool {
	switch k {
	case KindJSONRPC, KindChannelData, KindCredit:
		return true
	default:
		return false
	}
}

// ReadFrame reads exactly one frame.
//
// It returns io.EOF only when the stream ended cleanly at a frame boundary. A stream that ends
// mid-frame is io.ErrUnexpectedEOF, which is a different thing: the host closed on us while we
// were mid-message, and a caller that treated the two alike would report a normal shutdown as one.
func ReadFrame(r io.Reader) (Frame, error) {
	var header [HeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	length := binary.BigEndian.Uint32(header[0:4])
	kind := Kind(header[4])
	channelID := binary.BigEndian.Uint32(header[5:9])

	if !ValidKind(kind) {
		return Frame{}, &ProtocolViolationError{Reason: "unknown or reserved kind", Kind: kind, Length: length}
	}
	if length > MaxFrameLength {
		return Frame{}, &ProtocolViolationError{Reason: "oversize length", Kind: kind, Length: length}
	}
	if kind == KindCredit && length != CreditPayloadLen {
		return Frame{}, &ProtocolViolationError{Reason: "credit frame must carry 8 bytes", Kind: kind, Length: length}
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			return Frame{}, io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{Kind: kind, ChannelID: channelID, Payload: payload}, nil
}

// WriteFrame writes one frame.
//
// The header and payload go out in a single Write. Two writes would let a concurrent writer
// interleave between them and produce a frame that is a valid header followed by someone else's
// payload — a corruption the reader cannot detect, because both halves are individually
// well-formed.
func WriteFrame(w io.Writer, f Frame) error {
	if !ValidKind(f.Kind) {
		return &ProtocolViolationError{Reason: "unknown or reserved kind", Kind: f.Kind}
	}
	if len(f.Payload) > MaxFrameLength {
		return &ProtocolViolationError{Reason: "oversize payload", Kind: f.Kind, Length: uint32(len(f.Payload))}
	}
	if f.Kind == KindCredit && len(f.Payload) != CreditPayloadLen {
		return &ProtocolViolationError{Reason: "credit frame must carry 8 bytes", Kind: f.Kind, Length: uint32(len(f.Payload))}
	}

	buf := make([]byte, HeaderLen+len(f.Payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(f.Payload)))
	buf[4] = byte(f.Kind)
	binary.BigEndian.PutUint32(buf[5:9], f.ChannelID)
	copy(buf[HeaderLen:], f.Payload)

	_, err := w.Write(buf)
	return err
}

// EncodeCredit builds a credit frame's payload: the channel id repeated, then the count.
func EncodeCredit(channelID, credit uint32) []byte {
	payload := make([]byte, CreditPayloadLen)
	binary.BigEndian.PutUint32(payload[0:4], channelID)
	binary.BigEndian.PutUint32(payload[4:8], credit)
	return payload
}

// DecodeCredit reads a credit frame's payload.
//
// The repeated channel id must match the header's and the count must be non-zero; both are
// protocol violations rather than values to round off, because a credit frame that disagrees with
// its own header is not a frame anyone can act on.
func DecodeCredit(headerChannelID uint32, payload []byte) (credit uint32, err error) {
	if len(payload) != CreditPayloadLen {
		return 0, &ProtocolViolationError{Reason: "credit frame must carry 8 bytes", Kind: KindCredit, Length: uint32(len(payload))}
	}
	inner := binary.BigEndian.Uint32(payload[0:4])
	if inner != headerChannelID {
		return 0, &ProtocolViolationError{Reason: "credit channel id disagrees with the frame header", Kind: KindCredit}
	}
	credit = binary.BigEndian.Uint32(payload[4:8])
	if credit == 0 {
		return 0, &ProtocolViolationError{Reason: "credit must be non-zero", Kind: KindCredit}
	}
	return credit, nil
}
