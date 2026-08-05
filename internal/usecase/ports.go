// Package usecase orchestrates what the plugin does: build the tree the host draws, run the actions
// the user picks, and keep the surfaces and dialogs those actions open. It talks to the host and to
// Docker only through the ports below.
package usecase

import (
	"context"
	"encoding/json"
	"io"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/infra/dockerapi"
)

// Host is the plugin→host half of the protocol.
type Host interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
	Notify(method string, params any) error
}

// Streams opens a new stream to the Docker daemon over the parent session.
//
// Every long-lived operation takes one of its own: an HTTP connection carrying a followed log or a
// hijacked exec cannot also serve requests, and the plugin declares a channel budget that assumes
// exactly this (channel.maxConcurrent: 8).
type Streams interface {
	OpenExec(ctx context.Context, parentSessionID string) (io.ReadWriteCloser, error)
}

// SettingsStore persists per-resource settings the user edits in the node panel.
//
// Keyed by container NAME rather than id: an id changes every time a container is recreated, and
// settings that vanish on recreate are settings nobody will set twice.
type SettingsStore interface {
	Load(connectionID string) (map[string]ResourceSettings, error)
	Save(connectionID string, settings map[string]ResourceSettings) error
}

// ResourceSettings is what the user can configure about one container.
type ResourceSettings struct {
	Shell      string `json:"shell,omitempty"`
	User       string `json:"user,omitempty"`
	WorkingDir string `json:"workingDir,omitempty"`
	Privileged bool   `json:"privileged,omitempty"`
	LogTail    int    `json:"logTail,omitempty"`
	LogStamps  bool   `json:"logTimestamps,omitempty"`
}

// DefaultSettings is what a container the user never configured gets.
//
// /bin/sh rather than bash: it exists in alpine, busybox and distroless-with-a-shell, and a console
// that fails to open on the most common base images would be a console nobody trusts.
func DefaultSettings() ResourceSettings {
	return ResourceSettings{Shell: "/bin/sh", LogTail: 500, LogStamps: false}
}

// dockerClientFactory builds an API client on a fresh stream. Swapped in tests.
type dockerClientFactory func(io.ReadWriteCloser) *dockerapi.Client
