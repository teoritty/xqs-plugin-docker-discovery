package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// MaxAPIVersion is the newest Engine API version this plugin knows how to ask for.
//
// The negotiated version is min(daemon, this): pinning blind breaks on an older daemon, and taking
// whatever the daemon offers breaks the day it offers something whose shapes changed.
const MaxAPIVersion = "1.45"

// Client issues Engine API requests over one stream.
//
// Requests are serialized. HTTP/1.1 keep-alive on a single connection has no request ids, so a
// second request written before the first response is read would read the wrong response — this is
// a pipelining hazard, not a throughput choice. Streaming operations (logs, events, exec) each get
// their own stream and do not queue behind this mutex.
type Client struct {
	conn    *Conn
	reader  *bufio.Reader
	mu      sync.Mutex
	version string
}

// NewClient wraps a stream. Call Negotiate before issuing requests.
func NewClient(stream io.ReadWriteCloser) *Client {
	conn := NewConn(stream)
	return &Client{conn: conn, reader: bufio.NewReader(conn), version: MaxAPIVersion}
}

// Version returns the negotiated API version, used as the path prefix.
func (c *Client) Version() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

// Close ends the underlying stream.
func (c *Client) Close() error { return c.conn.Close() }

type versionResponse struct {
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	Version       string `json:"Version"`
}

// Negotiate asks the daemon what it speaks and settles on a version both sides know.
func (c *Client) Negotiate(ctx context.Context) (string, error) {
	// Sent without a version prefix: /version is the one endpoint that must answer whatever the
	// daemon's age, and it is how we find out what prefix the others may carry.
	body, err := c.do(ctx, http.MethodGet, "/version", nil, nil)
	if err != nil {
		return "", err
	}
	var v versionResponse
	decodeErr := json.NewDecoder(body).Decode(&v)
	// Closed BEFORE the lock is taken below, not deferred: the body now HOLDS that lock (see do),
	// so deferring the close would have this function wait for a lock it is itself holding.
	_ = body.Close()
	if decodeErr != nil {
		return "", fmt.Errorf("dockerapi: decode version: %w", decodeErr)
	}
	negotiated := MaxAPIVersion
	if v.APIVersion != "" && compareVersions(v.APIVersion, MaxAPIVersion) < 0 {
		negotiated = v.APIVersion
	}
	c.mu.Lock()
	c.version = negotiated
	c.mu.Unlock()
	return v.Version, nil
}

// GetJSON issues a GET and decodes the response into out.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	body, err := c.request(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, body)
		return nil
	}
	return json.NewDecoder(body).Decode(out)
}

// GetRaw issues a GET and returns the undecoded body — for inspect, whose raw JSON the UI shows.
func (c *Client) GetRaw(ctx context.Context, path string, query url.Values) ([]byte, error) {
	body, err := c.request(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, maxInspectBytes))
}

// maxInspectBytes bounds a raw inspect payload. A container's inspect JSON is a few KiB; a cap two
// orders of magnitude above that turns a daemon returning something absurd into a truncated field
// rather than an allocation.
const maxInspectBytes = 4 << 20

// PostJSON issues a POST with an optional JSON body and decodes an optional JSON response.
func (c *Client) PostJSON(ctx context.Context, path string, query url.Values, in any, out any) error {
	var payload []byte
	if in != nil {
		var err error
		payload, err = json.Marshal(in)
		if err != nil {
			return fmt.Errorf("dockerapi: encode request: %w", err)
		}
	}
	body, err := c.request(ctx, http.MethodPost, path, query, payload)
	if err != nil {
		return err
	}
	defer body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, body)
		return nil
	}
	return json.NewDecoder(body).Decode(out)
}

// Delete issues a DELETE.
func (c *Client) Delete(ctx context.Context, path string, query url.Values) error {
	body, err := c.request(ctx, http.MethodDelete, path, query, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	_, _ = io.Copy(io.Discard, body)
	return nil
}

// Stream issues a request and hands back the response body without reading it, for endpoints that
// never end on their own: /events and /containers/{id}/logs?follow.
//
// The caller owns the body and the whole stream: this client cannot serve another request until it
// is done, which is why a streaming operation gets a stream of its own.
func (c *Client) Stream(ctx context.Context, method, path string, query url.Values) (io.ReadCloser, error) {
	return c.request(ctx, method, path, query, nil)
}

// request builds the versioned path and performs one exchange.
func (c *Client) request(ctx context.Context, method, path string, query url.Values, body []byte) (io.ReadCloser, error) {
	return c.do(ctx, method, "/v"+c.Version()+path, query, body)
}

// do performs one HTTP exchange on the stream.
//
// The lock is held until the RESPONSE BODY IS CLOSED, not until the headers are read. On a
// keep-alive connection there are no request ids: the next request's response is whatever bytes
// come next, so releasing the lock with a body still unread lets a second caller write its request
// and then read the first caller's body as its own answer. Both sides parse fine and the data is
// simply wrong, which is the worst shape a bug can take.
//
// The unlock therefore rides on the body: every caller already closes it, and a caller that forgets
// deadlocks itself immediately rather than corrupting someone else's read.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte) (io.ReadCloser, error) {
	c.mu.Lock()
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			c.mu.Unlock()
		}
	}

	req, err := buildRequest(ctx, method, path, query, body)
	if err != nil {
		unlock()
		return nil, err
	}
	if err := req.Write(c.conn); err != nil {
		unlock()
		return nil, fmt.Errorf("dockerapi: send request: %w", err)
	}
	resp, err := http.ReadResponse(c.reader, req)
	if err != nil {
		unlock()
		return nil, fmt.Errorf("dockerapi: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		apiErr := decodeAPIError(resp.StatusCode, resp.Body)
		_ = resp.Body.Close()
		unlock()
		return nil, apiErr
	}
	return &lockedBody{ReadCloser: resp.Body, release: unlock}, nil
}

// lockedBody releases the client's request lock when the body is closed.
type lockedBody struct {
	io.ReadCloser
	release func()
}

func (b *lockedBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// buildRequest assembles a request against the daemon's virtual host.
//
// The Host header is a placeholder because there is no host: the socket is the endpoint. Docker
// itself uses the same convention, and HTTP/1.1 requires the header to be present regardless.
func buildRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	target := "http://docker" + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("dockerapi: build request: %w", err)
	}
	req.Host = "docker"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(body))
	}
	return req, nil
}

// APIError is a refusal from the daemon.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("docker returned status %d", e.StatusCode)
	}
	return e.Message
}

// decodeAPIError turns the daemon's error body into something a user can read.
//
// Docker's message is used as-is when there is one: it is written for a person ("No such container:
// web") and is more useful than anything this layer could synthesize. What is dropped is the
// status-code noise around it.
func decodeAPIError(status int, body io.Reader) error {
	var payload struct {
		Message string `json:"message"`
	}
	raw, _ := io.ReadAll(io.LimitReader(body, 64<<10))
	if err := json.Unmarshal(raw, &payload); err == nil && payload.Message != "" {
		return &APIError{StatusCode: status, Message: strings.TrimSpace(payload.Message)}
	}
	return &APIError{StatusCode: status}
}

// IsNotFound reports whether err is the daemon saying the object is gone — an ordinary race when
// the tree is a moment behind reality, not a failure worth alarming the user about.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if !errorsAs(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusNotFound
}

// compareVersions orders dotted numeric versions. Returns -1, 0 or 1.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var an, bn int
		if i < len(as) {
			an = atoi(as[i])
		}
		if i < len(bs) {
			bn = atoi(bs[i])
		}
		if an != bn {
			if an < bn {
				return -1
			}
			return 1
		}
	}
	return 0
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}
