package usecase

import (
	"context"
	"strconv"
	"strings"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/domain"
)

// NodeDetails is the panel the host draws beside the tree (ADR-015 §3).
type NodeDetails struct {
	Sections []section         `json:"sections"`
	Values   map[string]string `json:"values"`
	Editable bool              `json:"editable"`
}

// DescribeNode answers discovery.describeNode.
//
// A container gets an editable panel — the console and log settings the user configures once
// instead of answering a dialog every time — and everything else gets a read-only summary, because
// its properties are facts about a remote object rather than preferences anybody owns.
func (s *Service) DescribeNode(ctx context.Context, sessionID, nodeID string) (NodeDetails, error) {
	conn := s.connection(sessionID)
	kind, dockerID, err := conn.Resolve(nodeID)
	if err != nil {
		return NodeDetails{}, err
	}
	if kind != domain.KindContainer {
		return s.readOnlyDetails(ctx, conn, string(kind), dockerID)
	}

	name := s.containerName(ctx, conn, dockerID)
	settings := conn.SettingsFor(name)
	return NodeDetails{
		Editable: true,
		Values: map[string]string{
			"shell":      settings.Shell,
			"user":       settings.User,
			"workingDir": settings.WorkingDir,
			"privileged": strconv.FormatBool(settings.Privileged),
			"logTail":    strconv.Itoa(settings.LogTail),
			"logStamps":  strconv.FormatBool(settings.LogStamps),
		},
		Sections: []section{
			{ID: "console", Label: "Console", Order: 1, Fields: []field{
				{ID: "shell", Label: "Shell", Type: "text", Order: 1, Width: "half",
					Placeholder: DefaultSettings().Shell,
					Description: "Run when you open a console for this container."},
				{ID: "user", Label: "User", Type: "text", Order: 2, Width: "half",
					Placeholder: "root"},
				{ID: "workingDir", Label: "Working directory", Type: "text", Order: 3, Width: "half"},
				{ID: "privileged", Label: "Privileged", Type: "checkbox", Order: 4, Width: "half"},
			}},
			{ID: "logs", Label: "Logs", Order: 2, Fields: []field{
				{ID: "logTail", Label: "Lines to load", Type: "number", Order: 1, Width: "half"},
				{ID: "logStamps", Label: "Timestamps", Type: "checkbox", Order: 2, Width: "half"},
			}},
		},
	}, nil
}

// readOnlyDetails renders an image, volume or network's inspect as a panel nobody can edit.
func (s *Service) readOnlyDetails(ctx context.Context, conn *Connection, kind, dockerID string) (NodeDetails, error) {
	client, err := conn.Control(ctx)
	if err != nil {
		return NodeDetails{}, err
	}
	raw, err := client.InspectRaw(ctx, kind, dockerID)
	if err != nil {
		return NodeDetails{}, err
	}
	return NodeDetails{
		Editable: false,
		Values:   map[string]string{"summary": summarize(raw)},
		Sections: []section{
			{ID: "summary", Label: strings.ToUpper(kind[:1]) + kind[1:], Fields: []field{
				{ID: "summary", Label: "", Type: "code"},
			}},
		},
	}, nil
}

// ApplyDetails answers discovery.applyDetails: the user saved the panel.
//
// The host stores nothing (ADR-015 §3), so this is where the values become durable. They are keyed
// by container NAME: an id changes on every recreate, and settings that disappear when a container
// is recreated are settings nobody will set twice.
func (s *Service) ApplyDetails(ctx context.Context, sessionID, nodeID string, values map[string]string) error {
	conn := s.connection(sessionID)
	kind, dockerID, err := conn.Resolve(nodeID)
	if err != nil {
		return err
	}
	if kind != domain.KindContainer {
		return nil
	}
	name := s.containerName(ctx, conn, dockerID)
	current := conn.SettingsFor(name)

	updated := ResourceSettings{
		Shell:      strings.TrimSpace(values["shell"]),
		User:       strings.TrimSpace(values["user"]),
		WorkingDir: strings.TrimSpace(values["workingDir"]),
		Privileged: values["privileged"] == "true",
		LogTail:    parseTail(values["logTail"], current.LogTail),
		LogStamps:  values["logStamps"] == "true",
	}
	if updated.Shell == "" {
		updated.Shell = DefaultSettings().Shell
	}
	all := conn.PutSettings(name, updated)
	return s.settings.Save(sessionID, all)
}

// parseTail keeps a sane line count. A negative or unparseable value falls back rather than being
// sent to Docker, which would answer with a refusal about a field the user did not know they broke.
func parseTail(raw string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 {
		if fallback > 0 {
			return fallback
		}
		return DefaultSettings().LogTail
	}
	if n > 100000 {
		return 100000
	}
	return n
}
