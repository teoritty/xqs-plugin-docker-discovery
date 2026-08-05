package dockerapi

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
)

// readHijackResponse reads the status line and headers of an upgrade response, leaving everything
// after them in the buffered reader.
//
// http.ReadResponse cannot be used here: it would try to interpret what follows as a body, and what
// follows is not a body — it is the process's stdio. Reading only the head is the whole point.
func readHijackResponse(r *bufio.Reader, req *http.Request) (int, error) {
	resp, err := http.ReadResponse(r, req)
	if err != nil {
		return 0, fmt.Errorf("dockerapi: read exec response: %w", err)
	}
	// A 101 has no body by definition, so nothing has been consumed past the headers. Any other
	// status is a refusal that DOES have a body, and it must be drained before the caller decides
	// what to do, or the next read would find the error text where the stream should be.
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	}
	return resp.StatusCode, nil
}

// hijackedStream is the connection after the upgrade, plus whatever the header reader had already
// pulled off the wire.
//
// That buffered remainder matters: bufio reads ahead, so the first bytes of the container's output
// are routinely sitting in it by the time the headers are parsed. Reading straight from the socket
// would silently drop them — the first line of a shell prompt, most often.
type hijackedStream struct {
	conn     io.ReadWriteCloser
	buffered *bufio.Reader
}

func (s *hijackedStream) Read(p []byte) (int, error) {
	if s.buffered != nil && s.buffered.Buffered() > 0 {
		return s.buffered.Read(p)
	}
	return s.conn.Read(p)
}

func (s *hijackedStream) Write(p []byte) (int, error) { return s.conn.Write(p) }

func (s *hijackedStream) Close() error { return s.conn.Close() }
