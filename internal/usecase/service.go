package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/domain"
	"github.com/teoritty/xqs-plugin-docker-discovery/internal/infra/dockerapi"
)

// Service is the plugin's whole behaviour: it answers the host's three discovery verbs, runs
// actions, and owns the surfaces and dialogs those actions produce.
type Service struct {
	host     Host
	streams  Streams
	settings SettingsStore
	newDock  dockerClientFactory

	mu          sync.Mutex
	connections map[string]*Connection
	// surfaces maps a host surface id to the stream feeding it, so closing a tab closes the stream
	// rather than leaving a log pump reading into nothing.
	surfaces map[string]*surface
	// dialogs maps an open dialog to what should happen when it is answered. The host guarantees
	// exactly one answer per dialog, so an entry is removed when it arrives.
	dialogs map[string]*pendingDialog
	// consoleInput is where a console surface's keystrokes go: the exec stream's write half.
	consoleInput map[string]io.Writer
	// watchers is one event follower per connection.
	watchers map[string]*watcher
}

// surface is one open tab and the work behind it.
type surface struct {
	cancel context.CancelFunc
	closer func()
	// execID is set for a console, so a resize can reach the right exec instance.
	execID string
	// client owns the stream this tab reads from. For a console it is the HIJACKED one: after the
	// upgrade that connection is the shell's stdio and can never carry a request again.
	client *dockerapi.Client
	// conn is where a resize goes instead. Sending it down the hijacked connection would type an
	// HTTP request into the user's shell — the resize must ride the ordinary request client.
	conn *Connection
}

// pendingDialog is what to do with a dialog's answer.
type pendingDialog struct {
	handle func(ctx context.Context, values map[string]string) error
}

// NewService creates the plugin's service.
func NewService(host Host, streams Streams, settings SettingsStore) *Service {
	return &Service{
		host:         host,
		streams:      streams,
		settings:     settings,
		newDock:      dockerapi.NewClient,
		connections:  make(map[string]*Connection),
		surfaces:     make(map[string]*surface),
		dialogs:      make(map[string]*pendingDialog),
		consoleInput: make(map[string]io.Writer),
		watchers:     make(map[string]*watcher),
	}
}

// Observe handles discovery.observe: the full set of expanded nodes, resent on every change and on
// every plugin restart.
func (s *Service) Observe(ctx context.Context, sessionID string, nodeIDs []string) {
	conn := s.connection(sessionID)
	conn.SetObserved(nodeIDs)

	if settings, err := s.settings.Load(sessionID); err == nil {
		conn.LoadSettings(settings)
	}

	// On its own goroutine: observe is a notification delivered on the read loop, and every publish
	// it triggers is a request whose response only that loop can deliver.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.refreshAll(ctx, conn)
		s.ensureEventWatcher(conn)
	}()
}

// InvokeAction handles discovery.invokeAction.
//
// It acknowledges by returning, and does the work on a goroutine: the host expects an ack within
// five seconds whatever the action costs, and stopping fifty containers costs more than that
// (ADR-014).
func (s *Service) InvokeAction(sessionID string, nodeIDs []string, actionID string) {
	conn := s.connection(sessionID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		s.runAction(ctx, conn, nodeIDs, actionID)
	}()
}

// target is one resolved node an action is aimed at.
type target struct {
	kind     domain.Kind
	dockerID string
}

// failure is one target the daemon refused, and which one it was.
type failure struct {
	label string
	err   error
}

// runAction runs one action over a whole selection.
//
// The selection is ONE operation, not a loop of them. The host allows a plugin one open dialog at a
// time (a second modal would stack over the first and be answered blind), so a per-node loop that
// opens a dialog each time gets exactly one through and is refused for every node after it — with
// nothing on screen to say so. That is what made removing five images remove one.
//
// So everything that needs an answer asks once for the whole set, and everything that fails reports
// once for the whole set.
func (s *Service) runAction(ctx context.Context, conn *Connection, nodeIDs []string, actionID string) {
	switch actionID {
	case ActionVolumeCreate:
		s.openCreateVolumeDialog(ctx, conn)
		return
	case ActionNetworkCreate:
		s.openCreateNetworkDialog(ctx, conn)
		return
	}

	targets := make([]target, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		kind, dockerID, err := conn.Resolve(nodeID)
		if err != nil {
			// A node this session never published, or an id that no longer looks like one. Refused
			// rather than passed along: the tree the user acted on is not the tree that exists.
			slog.Warn("action refused for an unresolvable node", "action", actionID)
			continue
		}
		targets = append(targets, target{kind: kind, dockerID: dockerID})
	}
	if len(targets) == 0 {
		return
	}

	// The dialog paths return here: the work happens when the dialog is answered, and so does the
	// refresh. Refreshing now would only redraw the tree the user is still looking at.
	switch actionID {
	case ActionContainerRemove:
		s.openRemoveContainerDialog(ctx, conn, targets)
		return
	case ActionImageRemove:
		s.openRemoveImageDialog(ctx, conn, targets)
		return
	case ActionVolumeRemove:
		s.openRemoveVolumeDialog(ctx, conn, targets)
		return
	}

	// Single-node actions. They are published without Multi, so the host never sends more than one
	// node for them; the first target is the selection.
	switch actionID {
	case ActionContainerLogs:
		s.openLogSurface(ctx, conn, targets[0].dockerID)
		return
	case ActionContainerConsole:
		s.openConsoleSurface(ctx, conn, targets[0].dockerID)
		return
	case ActionContainerInspect, ActionImageInspect, ActionVolumeInspect, ActionNetworkInspect:
		s.openInspectDialog(ctx, conn, string(targets[0].kind), targets[0].dockerID)
		return
	}

	s.reportBatch(ctx, conn, s.runOverTargets(ctx, conn, targets, actionID))
	s.refreshAll(ctx, conn)
}

// runOverTargets applies an immediate action to every target and returns what was refused.
//
// It collects rather than reports, so the caller can say everything that went wrong in one place —
// see reportBatch.
func (s *Service) runOverTargets(ctx context.Context, conn *Connection, targets []target, actionID string) []failure {
	client, err := conn.Control(ctx)
	if err != nil {
		// One failure for the whole batch: the daemon is unreachable, so naming each target would
		// repeat the same sentence once per selected node.
		return []failure{{err: err}}
	}
	var failures []failure
	for _, t := range targets {
		var actErr error
		switch actionID {
		case ActionContainerStart:
			actErr = client.StartContainer(ctx, t.dockerID)
		case ActionContainerStop:
			actErr = client.StopContainer(ctx, t.dockerID)
		case ActionContainerRestart:
			actErr = client.RestartContainer(ctx, t.dockerID)
		case ActionContainerKill:
			actErr = client.KillContainer(ctx, t.dockerID)
		case ActionContainerPause:
			actErr = client.PauseContainer(ctx, t.dockerID)
		case ActionContainerResume:
			actErr = client.UnpauseContainer(ctx, t.dockerID)
		case ActionNetworkRemove:
			actErr = client.RemoveNetwork(ctx, t.dockerID)
		default:
			// An action id this build does not know. Nothing to do and nothing to report — the menu
			// it came from was drawn by a version of this plugin, so this is a bug, not a refusal.
			return nil
		}
		if actErr != nil {
			failures = append(failures, failure{label: s.targetLabel(ctx, conn, t), err: actErr})
		}
	}
	return failures
}

// reportBatch surfaces everything one operation refused, in one dialog.
//
// A 404 is swallowed: the object was already gone, which is what the user asked for when they
// removed it and is an ordinary race when the tree is a moment behind. Anything else is shown,
// because an action that silently did nothing is worse than one that says why.
//
// One dialog for the batch is not only tidier — it is the only shape that works. Nine failures out
// of ten selected containers would otherwise be nine dialog.open calls, of which the host accepts
// the first and refuses the rest, and the user would be told about one failure and left to guess at
// the other eight.
func (s *Service) reportBatch(ctx context.Context, conn *Connection, failures []failure) {
	lines := make([]string, 0, len(failures))
	for _, f := range failures {
		if f.err == nil || dockerapi.IsNotFound(f.err) {
			continue
		}
		if f.label == "" {
			lines = append(lines, userMessage(f.err))
			continue
		}
		lines = append(lines, f.label+" — "+userMessage(f.err))
	}
	if len(lines) == 0 {
		return
	}
	s.openMessageDialog(ctx, conn, "Docker refused this", strings.Join(lines, "\n"))
}

// targetLabel names a target in a failure list. A container gets its name — the id it was addressed
// by is not what the user selected — and everything else gets the id it is known by, which for a
// volume IS its name.
func (s *Service) targetLabel(ctx context.Context, conn *Connection, t target) string {
	if t.kind == domain.KindContainer {
		return s.containerName(ctx, conn, t.dockerID)
	}
	return shortID(strings.TrimPrefix(t.dockerID, "sha256:"))
}

// forget drops everything belonging to a session the host no longer holds: its state, its event
// watcher and any tab still feeding from it.
//
// Without this the plugin keeps one Connection per session it has ever seen, each with a watcher
// retrying against an id that will never be bound again. A reconnect creates a new session, so the
// leak grows one dead watcher per reconnect.
func (s *Service) forget(sessionID string) {
	s.mu.Lock()
	conn := s.connections[sessionID]
	delete(s.connections, sessionID)
	w := s.watchers[sessionID]
	delete(s.watchers, sessionID)
	s.mu.Unlock()

	if w != nil && w.cancel != nil {
		w.cancel()
	}
	if conn != nil {
		conn.Close()
	}
}

// connection returns the per-session state, creating it on first sight.
func (s *Service) connection(sessionID string) *Connection {
	s.mu.Lock()
	defer s.mu.Unlock()
	if conn, ok := s.connections[sessionID]; ok {
		return conn
	}
	conn := NewConnection(sessionID, s.streams, s.newDock)
	s.connections[sessionID] = conn
	return conn
}

// Shutdown closes every stream the plugin holds.
func (s *Service) Shutdown() {
	s.mu.Lock()
	connections := make([]*Connection, 0, len(s.connections))
	for _, conn := range s.connections {
		connections = append(connections, conn)
	}
	surfaces := make([]*surface, 0, len(s.surfaces))
	for _, sur := range s.surfaces {
		surfaces = append(surfaces, sur)
	}
	s.connections = make(map[string]*Connection)
	s.surfaces = make(map[string]*surface)
	s.mu.Unlock()

	s.stopWatchers()
	for _, sur := range surfaces {
		sur.stop()
	}
	for _, conn := range connections {
		conn.Close()
	}
}

func (sur *surface) stop() {
	if sur.cancel != nil {
		sur.cancel()
	}
	if sur.closer != nil {
		sur.closer()
	}
	if sur.client != nil {
		_ = sur.client.Close()
	}
}

// ipcIsDenied reports whether the host refused on capability grounds — the answer will not change
// on a retry, and it names something the user can act on (an install without the grant).
func ipcIsDenied(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "denied")
}

// userMessage turns an error into something worth showing.
//
// Docker's own text is kept — it is written for a person — and everything else is replaced, because
// a transport failure's real message names a channel id and a socket, which tells the user nothing
// and tells anyone reading over their shoulder more than it should.
func userMessage(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *dockerapi.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Error()
	}
	if errors.Is(err, ErrNoDocker) {
		return "Docker is not reachable on this host. Check that the docker CLI is installed and that your user can reach the daemon socket."
	}
	text := err.Error()
	if strings.Contains(text, "capability denied") {
		return "The plugin is not permitted to run commands over this connection. Re-install it and grant the exec permission."
	}
	return "Could not reach Docker on this host."
}

// decodeParams is the one place inbound host params are decoded, so a malformed payload fails in a
// single recognisable way.
func decodeParams(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
