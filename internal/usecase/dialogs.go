package usecase

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/infra/dockerapi"
)

// field and section build the declarative form the host renders (ADR-015 §2).
type field struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Description string   `json:"description,omitempty"`
	Width       string   `json:"width,omitempty"`
	Order       int      `json:"order,omitempty"`
	Options     []option `json:"options,omitempty"`
	Secret      bool     `json:"secret"`
}

type option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type section struct {
	ID     string  `json:"id"`
	Label  string  `json:"label"`
	Order  int     `json:"order,omitempty"`
	Fields []field `json:"fields"`
}

// openDialog shows a form and records what to do with its answer.
func (s *Service) openDialog(ctx context.Context, kind, title, submitLabel string, sections []section, values map[string]string, handle func(context.Context, map[string]string) error) {
	payload := map[string]any{
		"kind":     kind,
		"title":    title,
		"sections": sections,
	}
	if submitLabel != "" {
		payload["submitLabel"] = submitLabel
	}
	if values != nil {
		payload["values"] = values
	}
	raw, err := s.host.Call(ctx, "dialog.open", payload)
	if err != nil {
		return
	}
	var res struct {
		DialogID string `json:"dialogId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.DialogID == "" {
		return
	}
	if handle != nil {
		s.mu.Lock()
		s.dialogs[res.DialogID] = &pendingDialog{handle: handle}
		s.mu.Unlock()
	}
}

// DialogSubmitted handles the host's dialog.submit. Exactly one answer arrives per dialog, so the
// entry is taken rather than read.
func (s *Service) DialogSubmitted(dialogID string, values map[string]string) {
	s.mu.Lock()
	pending := s.dialogs[dialogID]
	delete(s.dialogs, dialogID)
	s.mu.Unlock()
	if pending == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*60_000_000_000)
		defer cancel()
		if err := pending.handle(ctx, values); err != nil {
			// The dialog is already closed by the time an answer arrives, so a failure cannot be
			// shown in place; it gets a message of its own rather than vanishing.
			s.openMessageDialogRaw(ctx, "Docker refused this", userMessage(err))
		}
	}()
}

// DialogCancelled handles the host's dialog.cancel: the user closed it, and the work never starts.
func (s *Service) DialogCancelled(dialogID string) {
	s.mu.Lock()
	delete(s.dialogs, dialogID)
	s.mu.Unlock()
}

// --- inspect ----------------------------------------------------------------

// openInspectDialog shows a readable summary and the raw JSON behind it.
func (s *Service) openInspectDialog(ctx context.Context, conn *Connection, kind, dockerID string) {
	client, err := conn.Control(ctx)
	if err != nil {
		s.openMessageDialog(ctx, conn, "Inspect unavailable", userMessage(err))
		return
	}
	raw, err := client.InspectRaw(ctx, kind, dockerID)
	if err != nil {
		s.openMessageDialog(ctx, conn, "Inspect unavailable", userMessage(err))
		return
	}
	pretty := prettyJSON(raw)
	summary := summarize(raw)

	s.openDialog(ctx, "detail", "Inspect · "+shortID(dockerID), "", []section{
		{ID: "summary", Label: "Summary", Order: 1, Fields: []field{
			{ID: "summary", Label: "", Type: "code", Order: 1},
		}},
		{ID: "raw", Label: "Raw JSON", Order: 2, Fields: []field{
			{ID: "raw", Label: "", Type: "code", Order: 1},
		}},
	}, map[string]string{"summary": summary, "raw": pretty}, nil)
}

// prettyJSON re-indents a payload so a code block is readable. On anything unparseable the original
// bytes are shown: a raw view that hides what it could not format would be the one view that lies.
func prettyJSON(raw []byte) string {
	var buf strings.Builder
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	encoder := json.NewEncoder(&buf)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return string(raw)
	}
	return buf.String()
}

// summarize pulls the handful of fields a person actually looks for out of an inspect payload.
func summarize(raw []byte) string {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	var out []string
	add := func(label string, value any) {
		if value == nil {
			return
		}
		text := strings.TrimSpace(toText(value))
		if text != "" && text != "<nil>" {
			out = append(out, label+": "+text)
		}
	}
	add("Name", doc["Name"])
	if config, ok := doc["Config"].(map[string]any); ok {
		add("Image", config["Image"])
		add("Command", config["Cmd"])
		add("Working dir", config["WorkingDir"])
	}
	if state, ok := doc["State"].(map[string]any); ok {
		add("State", state["Status"])
		add("Started", state["StartedAt"])
		add("Finished", state["FinishedAt"])
		add("Exit code", state["ExitCode"])
	}
	add("Driver", doc["Driver"])
	add("Mountpoint", doc["Mountpoint"])
	add("Scope", doc["Scope"])
	add("Created", doc["Created"])
	add("Size", doc["Size"])
	return strings.Join(out, "\n")
}

func toText(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case float64:
		return strconv.FormatFloat(value, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			parts = append(parts, toText(item))
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

// --- create -----------------------------------------------------------------

func (s *Service) openCreateVolumeDialog(ctx context.Context, conn *Connection) {
	s.openDialog(ctx, "form", "Create volume", "Create", []section{
		{ID: "main", Label: "Volume", Order: 1, Fields: []field{
			{ID: "name", Label: "Name", Type: "text", Required: true, Order: 1, Width: "half"},
			{ID: "driver", Label: "Driver", Type: "text", Placeholder: "local", Order: 2, Width: "half"},
			{ID: "driverOpts", Label: "Driver options", Type: "keyValue", Order: 3},
			{ID: "labels", Label: "Labels", Type: "keyValue", Order: 4},
		}},
	}, nil, func(ctx context.Context, values map[string]string) error {
		client, err := conn.Control(ctx)
		if err != nil {
			return err
		}
		if err := client.CreateVolume(ctx, dockerapi.CreateVolumeRequest{
			Name:       values["name"],
			Driver:     values["driver"],
			DriverOpts: parsePairs(values["driverOpts"]),
			Labels:     parsePairs(values["labels"]),
		}); err != nil {
			return err
		}
		s.refreshAll(ctx, conn)
		return nil
	})
}

func (s *Service) openCreateNetworkDialog(ctx context.Context, conn *Connection) {
	s.openDialog(ctx, "form", "Create network", "Create", []section{
		{ID: "main", Label: "Network", Order: 1, Fields: []field{
			{ID: "name", Label: "Name", Type: "text", Required: true, Order: 1, Width: "half"},
			{ID: "driver", Label: "Driver", Type: "select", Order: 2, Width: "half", Options: []option{
				{Value: "bridge", Label: "bridge"},
				{Value: "overlay", Label: "overlay"},
				{Value: "macvlan", Label: "macvlan"},
				{Value: "ipvlan", Label: "ipvlan"},
			}},
		}},
		{ID: "ipam", Label: "Addressing", Order: 2, Fields: []field{
			{ID: "subnet", Label: "Subnet", Type: "text", Placeholder: "172.20.0.0/16", Order: 1, Width: "half"},
			{ID: "gateway", Label: "Gateway", Type: "text", Placeholder: "172.20.0.1", Order: 2, Width: "half"},
			{ID: "ipRange", Label: "IP range", Type: "text", Order: 3, Width: "half"},
			{ID: "auxAddresses", Label: "Auxiliary addresses", Type: "keyValue", Order: 4},
		}},
		{ID: "options", Label: "Options", Order: 3, Fields: []field{
			{ID: "internal", Label: "Internal", Type: "checkbox", Order: 1,
				Description: "No outbound access from this network."},
			{ID: "attachable", Label: "Attachable", Type: "checkbox", Order: 2},
			{ID: "ipv6", Label: "Enable IPv6", Type: "checkbox", Order: 3},
			{ID: "driverOpts", Label: "Driver options", Type: "keyValue", Order: 4},
			{ID: "labels", Label: "Labels", Type: "keyValue", Order: 5},
		}},
	}, map[string]string{"driver": "bridge"}, func(ctx context.Context, values map[string]string) error {
		client, err := conn.Control(ctx)
		if err != nil {
			return err
		}
		req := dockerapi.CreateNetworkRequest{
			Name:       values["name"],
			Driver:     values["driver"],
			Internal:   values["internal"] == "true",
			Attachable: values["attachable"] == "true",
			EnableIPv6: values["ipv6"] == "true",
			Options:    parsePairs(values["driverOpts"]),
			Labels:     parsePairs(values["labels"]),
		}
		// The IPAM block is sent only when the user filled something in: an empty one makes the
		// daemon skip its own defaults and hand out a network with no addresses.
		if values["subnet"] != "" || values["gateway"] != "" || values["ipRange"] != "" {
			req.IPAM = &struct {
				Driver string                 `json:"Driver,omitempty"`
				Config []dockerapi.IPAMConfig `json:"Config,omitempty"`
			}{
				Config: []dockerapi.IPAMConfig{{
					Subnet:     values["subnet"],
					Gateway:    values["gateway"],
					IPRange:    values["ipRange"],
					AuxAddress: parsePairs(values["auxAddresses"]),
				}},
			}
		}
		if err := client.CreateNetwork(ctx, req); err != nil {
			return err
		}
		s.refreshAll(ctx, conn)
		return nil
	})
}

// --- remove -----------------------------------------------------------------

func (s *Service) openRemoveContainerDialog(ctx context.Context, conn *Connection, containerID string) {
	name := s.containerName(ctx, conn, containerID)
	s.openDialog(ctx, "form", "Remove "+name, "Remove", []section{
		{ID: "options", Label: "Remove options", Order: 1, Fields: []field{
			{ID: "force", Label: "Force", Type: "checkbox", Order: 1,
				Description: "Remove it even if it is running."},
			{ID: "volumes", Label: "Remove anonymous volumes", Type: "checkbox", Order: 2,
				Description: "Named volumes are never touched."},
		}},
	}, nil, func(ctx context.Context, values map[string]string) error {
		client, err := conn.Control(ctx)
		if err != nil {
			return err
		}
		if err := client.RemoveContainer(ctx, containerID, values["force"] == "true", values["volumes"] == "true"); err != nil {
			return err
		}
		s.refreshAll(ctx, conn)
		return nil
	})
}

func (s *Service) openRemoveImageDialog(ctx context.Context, conn *Connection, imageID string) {
	s.openDialog(ctx, "form", "Remove image", "Remove", []section{
		{ID: "options", Label: "Remove options", Order: 1, Fields: []field{
			{ID: "force", Label: "Force", Type: "checkbox", Order: 1,
				Description: "Remove it even if containers reference it."},
			{ID: "noprune", Label: "Keep untagged parents", Type: "checkbox", Order: 2},
		}},
	}, nil, func(ctx context.Context, values map[string]string) error {
		client, err := conn.Control(ctx)
		if err != nil {
			return err
		}
		if err := client.RemoveImage(ctx, imageID, values["force"] == "true", values["noprune"] == "true"); err != nil {
			return err
		}
		s.refreshAll(ctx, conn)
		return nil
	})
}

func (s *Service) openRemoveVolumeDialog(ctx context.Context, conn *Connection, name string) {
	s.openDialog(ctx, "form", "Remove volume "+name, "Remove", []section{
		{ID: "options", Label: "Remove options", Order: 1, Fields: []field{
			{ID: "force", Label: "Force", Type: "checkbox", Order: 1},
		}},
	}, nil, func(ctx context.Context, values map[string]string) error {
		client, err := conn.Control(ctx)
		if err != nil {
			return err
		}
		if err := client.RemoveVolume(ctx, name, values["force"] == "true"); err != nil {
			return err
		}
		s.refreshAll(ctx, conn)
		return nil
	})
}

// --- messages ---------------------------------------------------------------

// openMessageDialog reports a failure the user needs to see.
func (s *Service) openMessageDialog(ctx context.Context, _ *Connection, title, message string) {
	s.openMessageDialogRaw(ctx, title, message)
}

func (s *Service) openMessageDialogRaw(ctx context.Context, title, message string) {
	s.openDialog(ctx, "detail", title, "", []section{
		{ID: "message", Label: "", Fields: []field{{ID: "message", Label: "", Type: "code"}}},
	}, map[string]string{"message": message}, nil)
}

// parsePairs decodes a keyValue field. An unparseable value yields nothing rather than an error:
// the host already validated it, so anything else here is a shape nobody can act on.
func parsePairs(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
