package dockerapi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
)

// Stream identifiers in Docker's multiplexed frame header.
const (
	StreamStdin  byte = 0
	StreamStdout byte = 1
	StreamStderr byte = 2
)

// demuxHeaderLen is Docker's frame header: [stream][0][0][0][4-byte big-endian length].
const demuxHeaderLen = 8

// maxDemuxFrame bounds one demultiplexed frame. Docker writes in 32 KiB chunks; a ceiling well
// above that turns a corrupted length into a refused frame rather than an allocation.
const maxDemuxFrame = 16 << 20

// ErrDemuxProtocol reports a malformed multiplexed stream.
var ErrDemuxProtocol = errors.New("dockerapi: malformed multiplexed stream")

// DemuxFrame is one chunk of a container's output, tagged with which stream produced it.
type DemuxFrame struct {
	Stream byte
	Data   []byte
}

// Demux reads Docker's multiplexed stream format.
//
// Without a TTY the daemon interleaves stdout and stderr on one connection, each chunk prefixed by
// a header saying which it is. That tag is unrecoverable once the bytes are concatenated, and it is
// exactly what the log viewer colours by — so it is read here and carried, rather than being
// flattened and guessed at later.
func Demux(r io.Reader) (DemuxFrame, error) {
	var header [demuxHeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return DemuxFrame{}, err
	}
	stream := header[0]
	if stream > StreamStderr {
		return DemuxFrame{}, fmt.Errorf("%w: unknown stream %d", ErrDemuxProtocol, stream)
	}
	length := binary.BigEndian.Uint32(header[4:8])
	if length > maxDemuxFrame {
		return DemuxFrame{}, fmt.Errorf("%w: frame of %d bytes", ErrDemuxProtocol, length)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		if errors.Is(err, io.EOF) {
			return DemuxFrame{}, io.ErrUnexpectedEOF
		}
		return DemuxFrame{}, err
	}
	return DemuxFrame{Stream: stream, Data: data}, nil
}

// --- exec ------------------------------------------------------------------

// ExecConfig is what an interactive console asks the daemon for.
type ExecConfig struct {
	Cmd          []string `json:"Cmd"`
	User         string   `json:"User,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
	Privileged   bool     `json:"Privileged,omitempty"`
	Tty          bool     `json:"Tty"`
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
}

// CreateExec asks the daemon to prepare an exec instance and returns its id.
func (c *Client) CreateExec(ctx context.Context, containerID string, cfg ExecConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	if err := c.PostJSON(ctx, "/containers/"+pathSegment(containerID)+"/exec", nil, cfg, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("dockerapi: the daemon returned no exec id")
	}
	return out.ID, nil
}

// StartExec attaches to an exec instance and hands back the raw stream.
//
// This is the hijack: after the response headers the connection stops being HTTP and becomes the
// process's stdio. That is why the client cannot be reused afterwards and why an exec gets a
// channel of its own — there is no way back to request/response on this stream.
func (c *Client) StartExec(ctx context.Context, execID string, tty bool) (io.ReadWriteCloser, error) {
	body := map[string]any{"Detach": false, "Tty": tty}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := buildRequest(ctx, "POST", "/v"+c.Version()+"/exec/"+pathSegment(execID)+"/start", nil, payload)
	if err != nil {
		return nil, err
	}
	// Docker answers an upgrade request with 101 and then speaks raw bytes. The headers below are
	// what makes it choose that path rather than a normal chunked response.
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := req.Write(c.conn); err != nil {
		return nil, fmt.Errorf("dockerapi: send exec start: %w", err)
	}
	resp, err := readHijackResponse(c.reader, req)
	if err != nil {
		return nil, err
	}
	if resp >= 400 {
		return nil, &APIError{StatusCode: resp}
	}
	// Whatever the reader already buffered belongs to the stream now, so it is handed over with it.
	return &hijackedStream{conn: c.conn, buffered: c.reader}, nil
}

// ResizeExec tells the daemon the terminal changed size, so the process inside sees a real SIGWINCH.
func (c *Client) ResizeExec(ctx context.Context, execID string, cols, rows int) error {
	query := url.Values{
		"w": []string{itoa(cols)},
		"h": []string{itoa(rows)},
	}
	return c.PostJSON(ctx, "/exec/"+pathSegment(execID)+"/resize", query, nil, nil)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
