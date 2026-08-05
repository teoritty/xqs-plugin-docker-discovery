// Package dockerapi speaks the Docker Engine API over a host channel.
//
// The channel carries whatever `docker system dial-stdio` is connected to — the daemon socket — so
// what travels on it is ordinary HTTP/1.1. This package writes requests and reads responses with
// the standard library's own parser rather than a hand-rolled one, and keeps the ability to take
// the connection over afterwards, which is what an attached exec stream requires.
package dockerapi

import (
	"errors"
	"io"
	"net"
	"time"
)

// Conn adapts a byte stream to net.Conn so http.ReadResponse and Request.Write can be used on it.
//
// The addresses are placeholders: there is no TCP endpoint here, and nothing in the stack asks for
// one except error formatting. Deadlines are unsupported for the same reason — the stream's
// lifetime is the channel's, and it is ended by closing it, not by a timer inside it.
type Conn struct {
	rw io.ReadWriteCloser
}

// NewConn wraps a stream.
func NewConn(rw io.ReadWriteCloser) *Conn { return &Conn{rw: rw} }

func (c *Conn) Read(p []byte) (int, error)  { return c.rw.Read(p) }
func (c *Conn) Write(p []byte) (int, error) { return c.rw.Write(p) }
func (c *Conn) Close() error                { return c.rw.Close() }

func (c *Conn) LocalAddr() net.Addr  { return channelAddr{} }
func (c *Conn) RemoteAddr() net.Addr { return channelAddr{} }

// errDeadlineUnsupported is returned rather than silently ignored: a caller that set a deadline and
// got no error would believe it had a timeout it does not have.
var errDeadlineUnsupported = errors.New("dockerapi: deadlines are not supported on a channel stream")

func (c *Conn) SetDeadline(time.Time) error      { return errDeadlineUnsupported }
func (c *Conn) SetReadDeadline(time.Time) error  { return errDeadlineUnsupported }
func (c *Conn) SetWriteDeadline(time.Time) error { return errDeadlineUnsupported }

type channelAddr struct{}

func (channelAddr) Network() string { return "channel" }
func (channelAddr) String() string  { return "docker-dial-stdio" }

var _ net.Conn = (*Conn)(nil)
