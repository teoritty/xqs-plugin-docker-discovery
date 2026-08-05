package usecase

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/infra/dockerapi"
)

// openLogSurface opens a log tab and pumps the container's output into it.
func (s *Service) openLogSurface(ctx context.Context, conn *Connection, containerID string) {
	name := s.containerName(ctx, conn, containerID)
	settings := conn.SettingsFor(name)

	client, err := conn.OpenStream(ctx)
	if err != nil {
		s.openMessageDialog(ctx, conn, "Logs unavailable", userMessage(err))
		return
	}

	surfaceID, err := s.openSurface(ctx, conn, "log", "Logs · "+name)
	if err != nil {
		_ = client.Close()
		return
	}

	stream, err := client.ContainerLogs(ctx, containerID, dockerapi.LogOptions{
		Follow: true, Timestamps: settings.LogStamps, Tail: settings.LogTail,
	})
	if err != nil {
		s.setSurfaceError(surfaceID, userMessage(err))
		_ = client.Close()
		return
	}

	pumpCtx, cancel := context.WithCancel(context.Background())
	s.trackSurface(surfaceID, &surface{
		cancel: cancel,
		closer: func() { _ = stream.Close() },
		client: client,
	})
	s.setSurfaceReady(surfaceID)
	go s.pumpLogs(pumpCtx, surfaceID, stream)
}

// pumpLogs demultiplexes the container's output and forwards it with its stream tag intact.
//
// The tag is why this is not an io.Copy: stdout and stderr arrive interleaved on one connection,
// each chunk headed by which it is, and that distinction is unrecoverable the moment the bytes are
// concatenated — it is exactly what the viewer colours by.
func (s *Service) pumpLogs(ctx context.Context, surfaceID string, stream io.ReadCloser) {
	defer stream.Close()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		frame, err := dockerapi.Demux(stream)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				slog.Debug("log stream ended", "err", err)
			}
			s.setSurfaceError(surfaceID, "The log stream ended.")
			return
		}
		name := "stdout"
		if frame.Stream == dockerapi.StreamStderr {
			name = "stderr"
		}
		if err := s.writeSurface(surfaceID, frame.Data, name); err != nil {
			// The tab is gone, or the host is asking us to slow down and we have nothing to slow
			// down with — either way there is nowhere to put the next frame.
			return
		}
	}
}

// openConsoleSurface opens a terminal tab attached to a shell inside the container.
func (s *Service) openConsoleSurface(ctx context.Context, conn *Connection, containerID string) {
	name := s.containerName(ctx, conn, containerID)
	settings := conn.SettingsFor(name)

	client, err := conn.OpenStream(ctx)
	if err != nil {
		s.openMessageDialog(ctx, conn, "Console unavailable", userMessage(err))
		return
	}

	shell := settings.Shell
	if strings.TrimSpace(shell) == "" {
		shell = DefaultSettings().Shell
	}
	execID, err := client.CreateExec(ctx, containerID, dockerapi.ExecConfig{
		Cmd:        []string{shell},
		User:       settings.User,
		WorkingDir: settings.WorkingDir,
		Privileged: settings.Privileged,
		// Tty true is the whole reason this goes through the Engine API: the daemon allocates the
		// pty, so the shell behaves like a shell even though the host's exec channel has none.
		Tty:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		_ = client.Close()
		s.openMessageDialog(ctx, conn, "Console unavailable", userMessage(err))
		return
	}

	stream, err := client.StartExec(ctx, execID, true)
	if err != nil {
		_ = client.Close()
		s.openMessageDialog(ctx, conn, "Console unavailable", userMessage(err))
		return
	}

	surfaceID, err := s.openSurface(ctx, conn, "terminal", "Console · "+name)
	if err != nil {
		_ = stream.Close()
		_ = client.Close()
		return
	}

	pumpCtx, cancel := context.WithCancel(context.Background())
	s.trackSurface(surfaceID, &surface{
		cancel: cancel,
		closer: func() { _ = stream.Close() },
		execID: execID,
		client: client,
	})
	s.setSurfaceReady(surfaceID)
	go s.pumpConsole(pumpCtx, surfaceID, stream)
	s.registerConsoleInput(surfaceID, stream)
}

// pumpConsole forwards the shell's output. With a TTY the stream is raw — the daemon does no
// multiplexing, because a terminal has one stream by definition.
func (s *Service) pumpConsole(ctx context.Context, surfaceID string, stream io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := stream.Read(buf)
		if n > 0 {
			if writeErr := s.writeSurface(surfaceID, buf[:n], "stdout"); writeErr != nil {
				return
			}
		}
		if err != nil {
			s.setSurfaceError(surfaceID, "The console session ended.")
			return
		}
	}
}

// --- host calls ------------------------------------------------------------

func (s *Service) openSurface(ctx context.Context, conn *Connection, kind, title string) (string, error) {
	raw, err := s.host.Call(ctx, "surface.open", map[string]any{
		"parentSessionId": conn.SessionID(),
		"kind":            kind,
		"title":           title,
	})
	if err != nil {
		return "", err
	}
	var res struct {
		SurfaceID string `json:"surfaceId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	return res.SurfaceID, nil
}

func (s *Service) writeSurface(surfaceID string, data []byte, stream string) error {
	ctx, cancel := context.WithTimeout(context.Background(), surfaceWriteTimeout)
	defer cancel()
	_, err := s.host.Call(ctx, "surface.write", map[string]any{
		"surfaceId":  surfaceID,
		"dataBase64": base64.StdEncoding.EncodeToString(data),
		"stream":     stream,
	})
	return err
}

func (s *Service) setSurfaceReady(surfaceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), surfaceWriteTimeout)
	defer cancel()
	_, _ = s.host.Call(ctx, "surface.updateState", map[string]any{"surfaceId": surfaceID, "state": "ready"})
}

func (s *Service) setSurfaceError(surfaceID, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), surfaceWriteTimeout)
	defer cancel()
	_, _ = s.host.Call(ctx, "surface.updateState", map[string]any{
		"surfaceId": surfaceID, "state": "error", "error": message,
	})
}

// surfaceWriteTimeout bounds one host call. The host's own RPC budget is five seconds; going past
// it would leave a pump waiting on a response that has already been abandoned.
const surfaceWriteTimeout = 5_000_000_000 // 5s, in nanoseconds

// --- inbound notifications --------------------------------------------------

// SurfaceInput forwards the user's keystrokes into the console's stdin.
func (s *Service) SurfaceInput(surfaceID string, data []byte) {
	s.mu.Lock()
	writer := s.consoleInput[surfaceID]
	s.mu.Unlock()
	if writer == nil {
		return
	}
	if _, err := writer.Write(data); err != nil {
		slog.Debug("console input dropped", "err", err)
	}
}

// SurfaceResize tells the daemon the terminal changed size, so the shell inside sees SIGWINCH and
// redraws — without it, everything full-screen is wrong after the first window change.
func (s *Service) SurfaceResize(surfaceID string, cols, rows int) {
	s.mu.Lock()
	sur := s.surfaces[surfaceID]
	s.mu.Unlock()
	if sur == nil || sur.execID == "" || sur.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), surfaceWriteTimeout)
	defer cancel()
	_ = sur.client.ResizeExec(ctx, sur.execID, cols, rows)
}

// SurfaceClosed stops the work behind a tab the user closed.
func (s *Service) SurfaceClosed(surfaceID string) {
	s.mu.Lock()
	sur := s.surfaces[surfaceID]
	delete(s.surfaces, surfaceID)
	delete(s.consoleInput, surfaceID)
	s.mu.Unlock()
	if sur != nil {
		sur.stop()
	}
}

func (s *Service) trackSurface(surfaceID string, sur *surface) {
	s.mu.Lock()
	s.surfaces[surfaceID] = sur
	s.mu.Unlock()
}

func (s *Service) registerConsoleInput(surfaceID string, w io.Writer) {
	s.mu.Lock()
	s.consoleInput[surfaceID] = w
	s.mu.Unlock()
}

// containerName resolves a container's display name, falling back to its short id.
func (s *Service) containerName(ctx context.Context, conn *Connection, containerID string) string {
	client, err := conn.Control(ctx)
	if err != nil {
		return shortID(containerID)
	}
	detail, err := client.InspectContainer(ctx, containerID)
	if err != nil {
		return shortID(containerID)
	}
	return strings.TrimPrefix(detail.Name, "/")
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
