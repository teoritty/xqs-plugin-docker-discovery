package dockerapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// nopStream is an empty stream, for the one test that needs a Conn and no traffic.
type nopStream struct{}

func (nopStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopStream) Write(p []byte) (int, error) { return len(p), nil }
func (nopStream) Close() error                { return nil }

// fakeDaemon is a Docker daemon at the far end of a stream: it reads requests and replies with
// whatever the test queued.
type fakeDaemon struct {
	client net.Conn
	server net.Conn
	mu     sync.Mutex
	got    []string
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	d := &fakeDaemon{client: clientSide, server: serverSide}
	t.Cleanup(func() {
		_ = clientSide.Close()
		_ = serverSide.Close()
	})
	return d
}

// reply serves one request and answers with the given raw HTTP response.
func (d *fakeDaemon) reply(t *testing.T, response string) {
	t.Helper()
	go func() {
		buf := make([]byte, 4096)
		n, err := d.server.Read(buf)
		if err != nil {
			return
		}
		d.mu.Lock()
		d.got = append(d.got, string(buf[:n]))
		d.mu.Unlock()
		_, _ = d.server.Write([]byte(response))
	}()
}

func (d *fakeDaemon) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.got...)
}

func body(payload string) string {
	return "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " +
		itoa(len(payload)) + "\r\n\r\n" + payload
}

func TestNegotiateTakesTheLowerVersion(t *testing.T) {
	d := newFakeDaemon(t)
	c := NewClient(d.client)
	d.reply(t, body(`{"ApiVersion":"1.41","Version":"20.10.7"}`))

	version, err := c.Negotiate(context.Background())
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if version != "20.10.7" {
		t.Fatalf("daemon version = %q", version)
	}
	if c.Version() != "1.41" {
		t.Fatalf("negotiated = %q, want the daemon's older 1.41", c.Version())
	}
}

// A daemon newer than we know must not drag us past what we were written against.
func TestNegotiateCapsAtWhatWeKnow(t *testing.T) {
	d := newFakeDaemon(t)
	c := NewClient(d.client)
	d.reply(t, body(`{"ApiVersion":"1.99","Version":"99.0"}`))
	if _, err := c.Negotiate(context.Background()); err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if c.Version() != MaxAPIVersion {
		t.Fatalf("negotiated = %q, want %q", c.Version(), MaxAPIVersion)
	}
}

// /version must be asked without a version prefix: it is how the prefix is discovered.
func TestNegotiateAsksWithoutAVersionPrefix(t *testing.T) {
	d := newFakeDaemon(t)
	c := NewClient(d.client)
	d.reply(t, body(`{"ApiVersion":"1.41"}`))
	if _, err := c.Negotiate(context.Background()); err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	req := d.requests()[0]
	if !strings.HasPrefix(req, "GET /version ") {
		t.Fatalf("request line = %q", strings.SplitN(req, "\r\n", 2)[0])
	}
}

func TestRequestsCarryTheNegotiatedPrefix(t *testing.T) {
	d := newFakeDaemon(t)
	c := NewClient(d.client)
	d.reply(t, body(`{"ApiVersion":"1.41"}`))
	_, _ = c.Negotiate(context.Background())

	d.reply(t, body(`[]`))
	if _, err := c.ListContainers(context.Background()); err != nil {
		t.Fatalf("ListContainers: %v", err)
	}
	req := d.requests()[1]
	if !strings.Contains(req, "/v1.41/containers/json") {
		t.Fatalf("request = %q", strings.SplitN(req, "\r\n", 2)[0])
	}
	// all=1 is not optional: a tree that hid stopped containers would answer "where did it go?"
	// with silence.
	if !strings.Contains(req, "all=1") {
		t.Fatalf("containers were listed without all=1: %q", strings.SplitN(req, "\r\n", 2)[0])
	}
}

// Docker's own message is written for a person; keeping it beats anything this layer could invent.
func TestAPIErrorKeepsTheDaemonsMessage(t *testing.T) {
	d := newFakeDaemon(t)
	c := NewClient(d.client)
	payload := `{"message":"No such container: web"}`
	d.reply(t, "HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: "+
		itoa(len(payload))+"\r\n\r\n"+payload)

	err := c.StartContainer(context.Background(), "web")
	if err == nil {
		t.Fatal("expected an error")
	}
	if err.Error() != "No such container: web" {
		t.Fatalf("message = %q", err.Error())
	}
	if !IsNotFound(err) {
		t.Fatal("a 404 must be recognisable: the tree being a moment behind reality is ordinary")
	}
}

func TestAPIErrorWithoutAMessageStillSaysSomething(t *testing.T) {
	d := newFakeDaemon(t)
	c := NewClient(d.client)
	d.reply(t, "HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\n\r\n")
	err := c.StartContainer(context.Background(), "web")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
}

func TestContainerNameStripsTheLeadingSlash(t *testing.T) {
	c := Container{ID: "abcdef1234567890", Names: []string{"/web"}}
	if c.Name() != "web" {
		t.Fatalf("name = %q", c.Name())
	}
	// A container with no name falls back to the short id rather than an empty row.
	unnamed := Container{ID: "abcdef1234567890"}
	if unnamed.Name() != "abcdef123456" {
		t.Fatalf("fallback name = %q", unnamed.Name())
	}
}

// --- demux -----------------------------------------------------------------

func demuxFrame(stream byte, payload string) []byte {
	out := make([]byte, demuxHeaderLen+len(payload))
	out[0] = stream
	binary.BigEndian.PutUint32(out[4:8], uint32(len(payload)))
	copy(out[demuxHeaderLen:], payload)
	return out
}

// The stream tag is unrecoverable once bytes are concatenated, and it is what the log viewer
// colours by.
func TestDemuxSeparatesStdoutFromStderr(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(demuxFrame(StreamStdout, "out\n"))
	buf.Write(demuxFrame(StreamStderr, "err\n"))

	first, err := Demux(&buf)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if first.Stream != StreamStdout || string(first.Data) != "out\n" {
		t.Fatalf("first = %+v", first)
	}
	second, err := Demux(&buf)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if second.Stream != StreamStderr || string(second.Data) != "err\n" {
		t.Fatalf("second = %+v", second)
	}
}

func TestDemuxRejectsAnUnknownStream(t *testing.T) {
	frame := demuxFrame(StreamStdout, "x")
	frame[0] = 9
	if _, err := Demux(bytes.NewReader(frame)); !errors.Is(err, ErrDemuxProtocol) {
		t.Fatalf("got %v, want ErrDemuxProtocol", err)
	}
}

func TestDemuxRejectsAnOversizeFrame(t *testing.T) {
	header := make([]byte, demuxHeaderLen)
	binary.BigEndian.PutUint32(header[4:8], maxDemuxFrame+1)
	if _, err := Demux(bytes.NewReader(header)); !errors.Is(err, ErrDemuxProtocol) {
		t.Fatalf("got %v, want ErrDemuxProtocol", err)
	}
}

func TestDemuxReportsTruncationDistinctly(t *testing.T) {
	frame := demuxFrame(StreamStdout, "abcdef")
	if _, err := Demux(bytes.NewReader(frame[:demuxHeaderLen+2])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %v, want io.ErrUnexpectedEOF", err)
	}
	if _, err := Demux(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("clean end: got %v, want io.EOF", err)
	}
}

// --- hijack ----------------------------------------------------------------

// bufio reads ahead, so the first bytes of a shell prompt are routinely already buffered by the
// time the headers are parsed. Reading straight from the socket would drop them.
func TestHijackedStreamReturnsBufferedBytesFirst(t *testing.T) {
	d := newFakeDaemon(t)
	c := NewClient(d.client)
	d.reply(t, body(`{"ApiVersion":"`+MaxAPIVersion+`"}`))
	_, _ = c.Negotiate(context.Background())

	go func() {
		buf := make([]byte, 4096)
		_, _ = d.server.Read(buf)
		_, _ = d.server.Write([]byte("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\nroot@host:/# "))
	}()

	stream, err := c.StartExec(context.Background(), "exec-1", true)
	if err != nil {
		t.Fatalf("StartExec: %v", err)
	}
	buf := make([]byte, 32)
	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "root@host:/# " {
		t.Fatalf("the prompt already on the wire was lost: %q", buf[:n])
	}
}

// A refused exec must not leave its error text where the stream should be.
func TestStartExecReportsARefusal(t *testing.T) {
	d := newFakeDaemon(t)
	c := NewClient(d.client)
	d.reply(t, body(`{"ApiVersion":"`+MaxAPIVersion+`"}`))
	_, _ = c.Negotiate(context.Background())

	payload := `{"message":"Container is not running"}`
	d.reply(t, "HTTP/1.1 409 Conflict\r\nContent-Length: "+itoa(len(payload))+"\r\n\r\n"+payload)

	if _, err := c.StartExec(context.Background(), "exec-1", true); err == nil {
		t.Fatal("expected a refused exec to be reported")
	}
}

func TestDeadlinesAreRefusedRatherThanIgnored(t *testing.T) {
	conn := NewConn(nopStream{})
	// A caller that set a deadline and got no error would believe it had a timeout it does not have.
	if err := conn.SetDeadline(time.Now()); err == nil {
		t.Fatal("SetDeadline must report that it does nothing")
	}
}
