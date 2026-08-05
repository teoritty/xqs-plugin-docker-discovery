package usecase

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/domain"
	"github.com/teoritty/xqs-plugin-docker-discovery/internal/infra/dockerapi"
)

// Connection is everything the plugin knows about one SSH session it draws under.
//
// One object per session, with one mutex. Every goroutine the plugin runs — the event watcher, an
// action, a log pump — reaches state through here, and keeping that in one place is what makes the
// locking reviewable: no field is guarded by a different lock than its neighbour.
type Connection struct {
	sessionID string
	streams   Streams
	newClient dockerClientFactory

	mu sync.Mutex
	// control is the client short requests share. Streaming operations never use it.
	control *dockerapi.Client
	// observed is the host's full set of expanded nodes, replaced wholesale on every observe.
	observed map[string]struct{}
	// resolved maps a published node id to the Docker id behind it. An action names a node; this
	// is what turns that back into something addressable, and an id absent from it is refused
	// rather than guessed at.
	resolved map[string]string
	settings map[string]ResourceSettings
	closed   bool
}

// ErrNoDocker reports that the daemon could not be reached on this connection.
var ErrNoDocker = errors.New("docker is not reachable on this host")

// ErrSessionGone reports that the host no longer holds the session this connection rides.
//
// Terminal for the connection, not a transient failure: the session id was minted for an SSH tab
// that has closed, and no amount of retrying makes the host bind it again. Everything for that
// session stops when this appears — without it the plugin reconnects forever against an id the host
// has forgotten, which is exactly the flood of denied channel.open a live run produced.
var ErrSessionGone = errors.New("the session this subtree rides has closed")

// NewConnection creates the per-session state.
func NewConnection(sessionID string, streams Streams, newClient dockerClientFactory) *Connection {
	return &Connection{
		sessionID: sessionID,
		streams:   streams,
		newClient: newClient,
		observed:  make(map[string]struct{}),
		resolved:  make(map[string]string),
		settings:  make(map[string]ResourceSettings),
	}
}

// SessionID is the host's address for this connection.
func (c *Connection) SessionID() string { return c.sessionID }

// Control returns the shared request client, opening it on first use.
//
// Opened lazily because a connection the user never expands should not spawn a process on their
// server: the plugin is started for every SSH session, and most of them are not about Docker.
func (c *Connection) Control(ctx context.Context) (*dockerapi.Client, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrNoDocker
	}
	if c.control != nil {
		client := c.control
		c.mu.Unlock()
		return client, nil
	}
	c.mu.Unlock()

	// Dialing outside the lock: it spawns a remote process and waits for a handshake, and holding
	// the connection's mutex across that would stall every other goroutine that touches this
	// session.
	client, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = client.Close()
		return nil, ErrNoDocker
	}
	if c.control != nil {
		// Another goroutine won the race. Theirs is as good as ours, and two control clients would
		// mean two daemon connections with no owner.
		_ = client.Close()
		return c.control, nil
	}
	c.control = client
	return client, nil
}

// OpenStream returns a client on a stream of its own, for a followed log, the event feed or an exec.
func (c *Connection) OpenStream(ctx context.Context) (*dockerapi.Client, error) {
	return c.dial(ctx)
}

func (c *Connection) dial(ctx context.Context) (*dockerapi.Client, error) {
	stream, err := c.streams.OpenExec(ctx, c.sessionID)
	if err != nil {
		if isSessionGone(err) {
			c.markGone()
			return nil, ErrSessionGone
		}
		return nil, err
	}
	client := c.newClient(stream)
	if _, err := client.Negotiate(ctx); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// isSessionGone reports whether the host refused because it does not hold this session.
//
// Matched on the host's wording as well as its error code: the code says "denied", and only the
// message distinguishes "you may not do this at all" from "not for this session, which has closed".
// Treating the first as terminal too would be wrong — a missing capability is a reason to stop
// asking, but it is not this connection's problem to report.
func isSessionGone(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "session not bound") || strings.Contains(text, "session not found")
}

// markGone closes the connection for good.
func (c *Connection) markGone() {
	c.mu.Lock()
	c.closed = true
	client := c.control
	c.control = nil
	c.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

// Gone reports whether this connection's session has closed.
func (c *Connection) Gone() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// SetObserved replaces the expanded-node set. Level-triggered: what arrives is the whole truth
// (ADR-014), so it replaces rather than merges.
func (c *Connection) SetObserved(nodeIDs []string) {
	next := make(map[string]struct{}, len(nodeIDs))
	for _, id := range nodeIDs {
		next[id] = struct{}{}
	}
	c.mu.Lock()
	c.observed = next
	c.mu.Unlock()
}

// Observes reports whether the user has this branch open. Enumerating anything else is the load the
// level-triggered protocol exists to avoid.
func (c *Connection) Observes(nodeID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.observed[nodeID]
	return ok
}

// ObservedNodes returns the current set, for a full refresh.
func (c *Connection) ObservedNodes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.observed))
	for id := range c.observed {
		out = append(out, id)
	}
	return out
}

// Remember records what a published node id resolves to.
func (c *Connection) Remember(nodeID, dockerID string) {
	c.mu.Lock()
	c.resolved[nodeID] = dockerID
	c.mu.Unlock()
}

// Resolve turns a node id from an action back into a Docker id.
//
// Two checks, both required. The id must be one this session actually published — otherwise the
// action path would address anything the caller names — and it must still look like a Docker id,
// because it has crossed a process boundary since we minted it.
func (c *Connection) Resolve(nodeID string) (domain.Kind, string, error) {
	kind, dockerID, err := domain.ParseInstanceID(nodeID)
	if err != nil {
		return "", "", err
	}
	c.mu.Lock()
	remembered, known := c.resolved[nodeID]
	c.mu.Unlock()
	if !known {
		return "", "", domain.ErrInvalidNodeID
	}
	if remembered != dockerID || !domain.ValidDockerID(remembered) {
		return "", "", domain.ErrInvalidNodeID
	}
	return kind, dockerID, nil
}

// LoadSettings installs the persisted settings for this connection.
func (c *Connection) LoadSettings(settings map[string]ResourceSettings) {
	c.mu.Lock()
	c.settings = settings
	c.mu.Unlock()
}

// SettingsFor returns a container's settings, falling back to the defaults.
func (c *Connection) SettingsFor(name string) ResourceSettings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.settings[name]; ok {
		return s
	}
	return DefaultSettings()
}

// PutSettings records a container's settings and returns the whole set, for persisting.
func (c *Connection) PutSettings(name string, s ResourceSettings) map[string]ResourceSettings {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings[name] = s
	out := make(map[string]ResourceSettings, len(c.settings))
	for k, v := range c.settings {
		out[k] = v
	}
	return out
}

// Close ends the control client. Streams opened for logs and consoles are owned by whoever opened
// them and are closed with their surface.
func (c *Connection) Close() {
	c.mu.Lock()
	client := c.control
	c.control = nil
	c.closed = true
	c.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}
