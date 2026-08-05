// Command xqs-docker is the xQuakShell Docker discovery plugin.
//
// It draws a Docker subtree under every SSH connection, and gives each resource the actions,
// surfaces and forms the host lends it (ADR-014, ADR-015). Everything it does with the remote host
// travels over one declared command, `docker system dial-stdio`, and the Engine API inside it.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/infra/chanbus"
	"github.com/teoritty/xqs-plugin-docker-discovery/internal/infra/ipc"
	"github.com/teoritty/xqs-plugin-docker-discovery/internal/infra/settings"
	"github.com/teoritty/xqs-plugin-docker-discovery/internal/presentation"
	"github.com/teoritty/xqs-plugin-docker-discovery/internal/usecase"
)

func main() {
	// stdout is the wire. Every log line goes to stderr, or the first one would corrupt a frame and
	// the host would kill the process for a protocol violation it did not commit.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	client := ipc.NewClient(os.Stdin, os.Stdout)
	channels := chanbus.NewManager(client)
	client.SetBinaryHandler(channels.Deliver)
	client.SetCreditHandler(channels.Credit)

	store := settings.NewStore(settings.HostFS{Call: client.Call})
	service := usecase.NewService(hostAdapter{client}, streamAdapter{channels}, store)
	handlers := presentation.New(service)

	client.SetHandler(dispatch(handlers, channels, service))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := client.Serve(ctx); err != nil {
		slog.Error("plugin ended", "err", err)
		service.Shutdown()
		channels.CloseAll()
		os.Exit(1)
	}
	service.Shutdown()
	channels.CloseAll()
}

// dispatch routes every inbound method. One switch, so the set of things this plugin answers is
// readable in one place.
func dispatch(h *presentation.Handlers, channels *chanbus.Manager, service *usecase.Service) ipc.Handler {
	return func(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
		switch method {
		case "initialize", "activate", "deactivate":
			return json.Marshal(map[string]bool{"ok": true})
		case "ping":
			return json.Marshal(map[string]string{"pong": "ok"})
		case "shutdown":
			service.Shutdown()
			channels.CloseAll()
			return json.Marshal(map[string]bool{"ok": true})

		case "discovery.observe":
			h.Observe(ctx, params)
			return nil, nil
		case "discovery.invokeAction":
			return marshal(h.InvokeAction(ctx, params))
		case "discovery.describeNode":
			return marshal(h.DescribeNode(ctx, params))
		case "discovery.applyDetails":
			return marshal(h.ApplyDetails(ctx, params))

		case "surface.input":
			h.SurfaceInput(ctx, params)
			return nil, nil
		case "surface.resize":
			h.SurfaceResize(ctx, params)
			return nil, nil
		case "surface.closed":
			h.SurfaceClosed(ctx, params)
			return nil, nil

		case "dialog.submit":
			h.DialogSubmitted(ctx, params)
			return nil, nil
		case "dialog.cancel":
			h.DialogCancelled(ctx, params)
			return nil, nil

		case "channel.close":
			// The host closing a channel is how a remote command's exit reaches us: there is no
			// binary error frame, so a stream that ended badly says so here (ADR-011).
			channels.HostClosed(params)
			return nil, nil

		default:
			return nil, &ipc.RPCError{Code: -32601, Message: "method not found"}
		}
	}
}

func marshal(value any, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// hostAdapter narrows the IPC client to what the use case is allowed to do with it.
type hostAdapter struct{ client *ipc.Client }

func (h hostAdapter) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return h.client.Call(ctx, method, params)
}

func (h hostAdapter) Notify(method string, params any) error {
	return h.client.Notify(method, params)
}

// streamAdapter opens a Docker stream over a new exec channel.
type streamAdapter struct{ channels *chanbus.Manager }

func (s streamAdapter) OpenExec(ctx context.Context, parentSessionID string) (io.ReadWriteCloser, error) {
	return s.channels.OpenExec(ctx, parentSessionID)
}
