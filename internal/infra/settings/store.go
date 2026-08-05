// Package settings persists per-container preferences in the plugin's own data directory.
//
// The host stores none of this (ADR-015 §3): it cannot name a discovered resource stably across
// restarts, and a plugin's opinion about a remote object is not core state. So the plugin keeps it,
// through the sandboxed fs.* RPC it already has permission for.
package settings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/usecase"
)

// FS is the host's sandboxed filesystem RPC.
type FS interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error
}

// Store keeps one settings file per connection.
type Store struct {
	fs FS

	mu    sync.Mutex
	cache map[string]map[string]usecase.ResourceSettings
}

// NewStore creates the settings store.
func NewStore(fs FS) *Store {
	return &Store{fs: fs, cache: make(map[string]map[string]usecase.ResourceSettings)}
}

// Load reads a connection's settings, answering from cache after the first read.
//
// A missing or unreadable file yields defaults rather than an error: settings are a convenience,
// and refusing to draw a tree because a preferences file is corrupt would be the wrong trade.
func (s *Store) Load(connectionID string) (map[string]usecase.ResourceSettings, error) {
	s.mu.Lock()
	if cached, ok := s.cache[connectionID]; ok {
		out := copySettings(cached)
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	raw, err := s.fs.ReadFile(context.Background(), pathFor(connectionID))
	settings := make(map[string]usecase.ResourceSettings)
	if err == nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, &settings); err != nil {
			settings = make(map[string]usecase.ResourceSettings)
		}
	}

	s.mu.Lock()
	s.cache[connectionID] = settings
	out := copySettings(settings)
	s.mu.Unlock()
	return out, nil
}

// Save writes a connection's settings.
func (s *Store) Save(connectionID string, settings map[string]usecase.ResourceSettings) error {
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("settings: encode: %w", err)
	}
	s.mu.Lock()
	s.cache[connectionID] = copySettings(settings)
	s.mu.Unlock()
	return s.fs.WriteFile(context.Background(), pathFor(connectionID), raw)
}

// pathFor builds the file name for a connection.
//
// RELATIVE, deliberately. ${pluginData} is a manifest-pattern placeholder — the host expands it
// when it resolves the capability's roots, not when it resolves a request path. A request path that
// contains it is taken literally, which is how this plugin first shipped: every read asked for a
// file named "${pluginData}" inside the plugin's data directory. A relative path is joined to that
// directory, which is what was meant.
//
// The connection id is base64url-encoded rather than interpolated. It is host-generated today, but
// it reaches this function as a string that becomes a path, and encoding it means no value it could
// ever hold produces a traversal — the guarantee does not depend on what the host promises.
func pathFor(connectionID string) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(connectionID))
	return "settings/" + encoded + ".json"
}

func copySettings(in map[string]usecase.ResourceSettings) map[string]usecase.ResourceSettings {
	out := make(map[string]usecase.ResourceSettings, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// HostFS implements FS over the plugin's JSON-RPC client.
type HostFS struct {
	Call func(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// ReadFile reads a file through fs.read.
func (h HostFS) ReadFile(ctx context.Context, path string) ([]byte, error) {
	raw, err := h.Call(ctx, "fs.read", map[string]any{"path": path})
	if err != nil {
		return nil, err
	}
	var res struct {
		ContentBase64 string `json:"contentBase64"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.ContentBase64)
}

// WriteFile writes a file through fs.write, creating the directory Docker settings live in.
func (h HostFS) WriteFile(ctx context.Context, path string, data []byte) error {
	if len(data) > maxSettingsBytes {
		// The host caps a write at 256 KiB per call. A settings file that large is not settings;
		// refusing here says so rather than failing halfway through a chunked write.
		return fmt.Errorf("settings: file of %d bytes is too large", len(data))
	}
	_, err := h.Call(ctx, "fs.write", map[string]any{
		"path":          path,
		"contentBase64": base64.StdEncoding.EncodeToString(data),
	})
	if err != nil && strings.Contains(err.Error(), "capability denied") {
		return fmt.Errorf("settings: the plugin may not write to its data directory")
	}
	return err
}

const maxSettingsBytes = 256 << 10
