package usecase

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/domain"
)

// debounceWindow collapses a burst of events into one refresh.
//
// `docker compose up` on a ten-service stack emits dozens of events in a moment; refreshing per
// event would enumerate the daemon dozens of times to draw the same tree once.
const debounceWindow = 300 * time.Millisecond

// reconcileInterval re-lists everything periodically, whatever the event stream said.
//
// The stream is the fast path, not the source of truth: a dropped connection or an event we do not
// know how to interpret would otherwise leave the tree wrong until the user collapsed and reopened
// a branch. A minute of staleness is the worst case rather than the normal one.
const reconcileInterval = 60 * time.Second

// watcher follows the daemon's event stream for one connection.
type watcher struct {
	once   sync.Once
	cancel context.CancelFunc
}

// ensureEventWatcher starts the event watcher for a connection, once.
func (s *Service) ensureEventWatcher(conn *Connection) {
	s.mu.Lock()
	w, ok := s.watchers[conn.SessionID()]
	if !ok {
		w = &watcher{}
		s.watchers[conn.SessionID()] = w
	}
	s.mu.Unlock()

	w.once.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		w.cancel = cancel
		go s.watchEvents(ctx, conn)
		go s.reconcile(ctx, conn)
	})
}

// watchEvents follows /events and refreshes the branches an event touched.
func (s *Service) watchEvents(ctx context.Context, conn *Connection) {
	for ctx.Err() == nil {
		if err := s.followEvents(ctx, conn); err != nil && ctx.Err() == nil {
			// A dropped stream is expected — the SSH session bounces, the daemon restarts — and
			// reconnecting after a pause beats spinning on a host that is not answering.
			slog.Debug("event stream ended", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
}

func (s *Service) followEvents(ctx context.Context, conn *Connection) error {
	client, err := conn.OpenStream(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	stream, err := client.Events(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	pending := make(map[string]struct{})
	var mu sync.Mutex
	var timer *time.Timer

	flush := func() {
		mu.Lock()
		branches := make([]string, 0, len(pending))
		for id := range pending {
			branches = append(branches, id)
		}
		pending = make(map[string]struct{})
		mu.Unlock()

		refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, branch := range branches {
			s.refreshBranch(refreshCtx, conn, branch)
		}
		// The group labels carry counts, so a container appearing or leaving changes the row above
		// it too.
		s.refreshBranch(refreshCtx, conn, domain.NodeRoot)
	}

	decoder := json.NewDecoder(stream)
	for {
		var event struct {
			Type   string `json:"Type"`
			Action string `json:"Action"`
		}
		if err := decoder.Decode(&event); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		branch := branchForEventType(event.Type)
		if branch == "" {
			continue
		}
		mu.Lock()
		pending[branch] = struct{}{}
		if timer == nil {
			timer = time.AfterFunc(debounceWindow, func() {
				mu.Lock()
				timer = nil
				mu.Unlock()
				flush()
			})
		}
		mu.Unlock()
	}
}

// branchForEventType maps a Docker event to the branch it changes. An event about something the
// tree does not draw is ignored rather than triggering a refresh of everything.
func branchForEventType(eventType string) string {
	switch eventType {
	case "container":
		return domain.NodeContainers
	case "image":
		return domain.NodeImages
	case "volume":
		return domain.NodeVolumes
	case "network":
		return domain.NodeNetworks
	default:
		return ""
	}
}

// reconcile re-lists every open branch on a timer, so a lost event cannot leave the tree wrong
// indefinitely.
func (s *Service) reconcile(ctx context.Context, conn *Connection) {
	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.refreshAll(refreshCtx, conn)
			cancel()
		}
	}
}

// stopWatchers ends every watcher. Called on shutdown so no goroutine outlives the process.
func (s *Service) stopWatchers() {
	s.mu.Lock()
	watchers := make([]*watcher, 0, len(s.watchers))
	for _, w := range s.watchers {
		watchers = append(watchers, w)
	}
	s.watchers = make(map[string]*watcher)
	s.mu.Unlock()
	for _, w := range watchers {
		if w.cancel != nil {
			w.cancel()
		}
	}
}
