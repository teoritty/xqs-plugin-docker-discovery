package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/domain"
)

// recordingHost captures every plugin→host call and answers dialog.open with an id.
type recordingHost struct {
	mu    sync.Mutex
	calls []hostCall
}

type hostCall struct {
	method string
	params map[string]any
}

func (h *recordingHost) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)

	h.mu.Lock()
	h.calls = append(h.calls, hostCall{method: method, params: decoded})
	n := len(h.calls)
	h.mu.Unlock()

	if method == "dialog.open" {
		return json.RawMessage(`{"dialogId":"d` + string(rune('0'+n%10)) + `"}`), nil
	}
	return json.RawMessage(`{}`), nil
}

func (h *recordingHost) Notify(string, any) error { return nil }

func (h *recordingHost) of(method string) []hostCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []hostCall
	for _, c := range h.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// newTestService wires a Service against a host that records, and a connection that has already
// published the given instance ids — which is what Resolve requires.
func newTestService(t *testing.T, kind domain.Kind, dockerIDs ...string) (*Service, *Connection, *recordingHost) {
	t.Helper()
	host := &recordingHost{}
	svc := NewService(host, nil, nil)
	conn := svc.connection("session-1")
	for _, id := range dockerIDs {
		conn.Remember(domain.InstanceID(kind, id), id)
	}
	return svc, conn, host
}

func nodeIDsFor(kind domain.Kind, dockerIDs ...string) []string {
	out := make([]string, 0, len(dockerIDs))
	for _, id := range dockerIDs {
		out = append(out, domain.InstanceID(kind, id))
	}
	return out
}

// The host allows a plugin ONE open dialog at a time. A remove that asked per node therefore got the
// first dialog through and had every one after it refused with a rate-limit error that openDialog
// dropped on the floor — so selecting five images and removing them removed one, silently. One
// dialog for the whole selection is the fix, and this is the property that must not regress.
func TestBulkRemoveAsksOnce(t *testing.T) {
	svc, conn, host := newTestService(t, domain.KindImage, "sha256:aaa", "sha256:bbb", "sha256:ccc")

	svc.runAction(context.Background(), conn,
		nodeIDsFor(domain.KindImage, "sha256:aaa", "sha256:bbb", "sha256:ccc"), ActionImageRemove)

	opened := host.of("dialog.open")
	if len(opened) != 1 {
		t.Fatalf("a three-image removal must ask exactly once, got %d dialogs", len(opened))
	}
	if title, _ := opened[0].params["title"].(string); title != "Remove 3 images" {
		t.Fatalf("the dialog must name the whole selection, got %q", title)
	}
}

// One image keeps the singular wording: a count is only informative when there is a choice about it.
func TestSingleRemoveKeepsItsTitle(t *testing.T) {
	svc, conn, host := newTestService(t, domain.KindImage, "sha256:aaa")

	svc.runAction(context.Background(), conn, nodeIDsFor(domain.KindImage, "sha256:aaa"), ActionImageRemove)

	opened := host.of("dialog.open")
	if len(opened) != 1 {
		t.Fatalf("got %d dialogs", len(opened))
	}
	if title, _ := opened[0].params["title"].(string); title != "Remove image" {
		t.Fatalf("title = %q", title)
	}
}

// A node this session never published is refused, and refusing it must not take the rest with it.
func TestUnresolvableNodesAreDroppedNotFatal(t *testing.T) {
	svc, conn, host := newTestService(t, domain.KindImage, "sha256:aaa")

	svc.runAction(context.Background(), conn,
		[]string{domain.InstanceID(domain.KindImage, "sha256:aaa"), "docker/image/sha256:neverpublished"},
		ActionImageRemove)

	opened := host.of("dialog.open")
	if len(opened) != 1 {
		t.Fatalf("got %d dialogs", len(opened))
	}
	// One target survived, so the title is the singular one.
	if title, _ := opened[0].params["title"].(string); title != "Remove image" {
		t.Fatalf("only the resolvable node should have been carried through, title = %q", title)
	}
}

// Nothing resolvable is nothing to ask about — an empty dialog would be a question with no subject.
func TestNothingResolvableAsksNothing(t *testing.T) {
	svc, conn, host := newTestService(t, domain.KindImage)

	svc.runAction(context.Background(), conn, []string{"docker/image/sha256:ghost"}, ActionImageRemove)

	if opened := host.of("dialog.open"); len(opened) != 0 {
		t.Fatalf("expected no dialog, got %d", len(opened))
	}
}

// Failures over a selection are one message, for the same reason the question is: nine refusals out
// of ten would otherwise be nine dialog.open calls, of which the host accepts exactly one.
func TestBatchFailuresAreOneDialog(t *testing.T) {
	svc, conn, host := newTestService(t, domain.KindVolume)

	svc.reportBatch(context.Background(), conn, []failure{
		{label: "data", err: errors.New("volume is in use")},
		{label: "cache", err: errors.New("volume is in use")},
	})

	opened := host.of("dialog.open")
	if len(opened) != 1 {
		t.Fatalf("expected one dialog for the batch, got %d", len(opened))
	}
	values, _ := opened[0].params["values"].(map[string]any)
	message, _ := values["message"].(string)
	if !strings.Contains(message, "data") || !strings.Contains(message, "cache") {
		t.Fatalf("both refusals must be named, got %q", message)
	}
	if lines := strings.Count(message, "\n") + 1; lines != 2 {
		t.Fatalf("expected one line per refusal, got %d in %q", lines, message)
	}
}

// Nothing to report shows nothing. A modal that says "0 failures" is a modal in the way.
func TestBatchWithNoFailuresIsSilent(t *testing.T) {
	svc, conn, host := newTestService(t, domain.KindVolume)

	svc.reportBatch(context.Background(), conn, nil)
	svc.reportBatch(context.Background(), conn, []failure{{label: "gone", err: nil}})

	if opened := host.of("dialog.open"); len(opened) != 0 {
		t.Fatalf("expected silence, got %d dialogs", len(opened))
	}
}

// Every remove action carries the delete role; nothing else does. The host binds the key by this
// value alone (ADR-014 "Actions"), so a missing one is a shortcut that does nothing and a stray one
// is a shortcut that does the wrong thing.
//
// Exactly one per node is also the host's rule, not a preference: it refuses a node carrying two
// actions of one role, which would take the whole branch down with it.
func TestOnlyRemovesCarryTheDeleteRole(t *testing.T) {
	marked := func(actions []Action) []string {
		var out []string
		for _, a := range actions {
			if a.Role == RoleDelete {
				out = append(out, a.ID)
			}
		}
		return out
	}
	cases := map[string][]string{
		"running container": marked(containerActions("running")),
		"stopped container": marked(containerActions("exited")),
		"image":             marked(imageActions()),
		"volume":            marked(volumeActions()),
		"user network":      marked(networkActions("my-net")),
	}
	want := map[string]string{
		"running container": ActionContainerRemove,
		"stopped container": ActionContainerRemove,
		"image":             ActionImageRemove,
		"volume":            ActionVolumeRemove,
		"user network":      ActionNetworkRemove,
	}
	for name, got := range cases {
		if len(got) != 1 || got[0] != want[name] {
			t.Fatalf("%s: marked %v, want exactly [%s]", name, got, want[name])
		}
	}
	// Docker's own three networks offer no removal at all, so the key must find nothing there.
	for _, builtin := range []string{"bridge", "host", "none"} {
		if got := marked(networkActions(builtin)); len(got) != 0 {
			t.Fatalf("%s network must not answer the Delete key, got %v", builtin, got)
		}
	}
	// Nothing carries a role the host has never heard of. An unknown one is not ignored — it makes
	// the host refuse the entire snapshot, so the whole tree would go with it.
	every := [][]Action{
		containerActions("running"), containerActions("exited"), containerActions("paused"),
		imageActions(), volumeActions(), networkActions("my-net"), networkActions("bridge"),
	}
	for _, actions := range every {
		for _, a := range actions {
			if a.Role != "" && a.Role != RoleDelete {
				t.Fatalf("action %q carries role %q, which this host contract does not define", a.ID, a.Role)
			}
		}
	}
}
