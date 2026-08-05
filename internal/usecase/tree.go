package usecase

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/domain"
	"github.com/teoritty/xqs-plugin-docker-discovery/internal/infra/dockerapi"
)

// Node is one row the host draws (ADR-014). The JSON tags are the wire contract.
type Node struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	Label           string   `json:"label"`
	IconID          string   `json:"iconId,omitempty"`
	Order           int      `json:"order,omitempty"`
	Status          *Status  `json:"status,omitempty"`
	Actions         []Action `json:"actions,omitempty"`
	DefaultActionID string   `json:"defaultActionId,omitempty"`
}

// Status is the node's dot.
type Status struct {
	Tone    string `json:"tone"`
	Tooltip string `json:"tooltip,omitempty"`
}

// Action is one menu entry.
type Action struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Danger  bool   `json:"danger,omitempty"`
	Confirm string `json:"confirm,omitempty"`
	Multi   bool   `json:"multi,omitempty"`
	// Delete tells the host which action its Delete key means. The host still knows nothing about
	// what the action does — it relays the same id the menu would have (ADR-014 "Actions").
	Delete bool `json:"delete,omitempty"`
}

// buildRoot returns the four groups under the Docker node.
//
// Counts live in the label because a group's own row is the only place a total can appear without
// expanding it — and the number is the reason a user expands one or does not.
func buildRoot(counts map[string]int) []Node {
	return []Node{
		{ID: domain.NodeContainers, Kind: "group", Label: withCount("Containers", counts[domain.NodeContainers]), IconID: domain.IconContainers, Order: 1},
		{ID: domain.NodeImages, Kind: "group", Label: withCount("Images", counts[domain.NodeImages]), IconID: domain.IconImages, Order: 2},
		{ID: domain.NodeVolumes, Kind: "group", Label: withCount("Volumes", counts[domain.NodeVolumes]), IconID: domain.IconVolumes, Order: 3,
			Actions: []Action{{ID: ActionVolumeCreate, Label: "Create volume…"}}},
		{ID: domain.NodeNetworks, Kind: "group", Label: withCount("Networks", counts[domain.NodeNetworks]), IconID: domain.IconNetworks, Order: 4,
			Actions: []Action{{ID: ActionNetworkCreate, Label: "Create network…"}}},
	}
}

func withCount(label string, n int) string {
	if n <= 0 {
		return label
	}
	return fmt.Sprintf("%s (%d)", label, n)
}

// containerNodes turns the container list into rows, newest-looking first by name so the tree does
// not reshuffle between refreshes.
func containerNodes(ctx context.Context, client *dockerapi.Client, containers []dockerapi.Container) []Node {
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name() < containers[j].Name() })
	nodes := make([]Node, 0, len(containers))
	for _, container := range containers {
		state := domain.ContainerState{
			State:      container.State,
			StatusText: container.Status,
			Image:      container.Image,
			Ports:      formatPorts(container.Ports),
		}
		// The list endpoint does not carry health or the exit code, and those decide two of the six
		// tones. One inspect per container is the price of a dot that tells the truth; it is only
		// paid for containers whose state can hide something (running with a check, or exited).
		if needsDetail(container.State) {
			if detail, err := client.InspectContainer(ctx, container.ID); err == nil {
				state.ExitCode = detail.State.ExitCode
				if detail.State.Health != nil {
					state.Health = detail.State.Health.Status
					if n := len(detail.State.Health.Log); n > 0 {
						state.HealthOutput = detail.State.Health.Log[n-1].Output
					}
				}
			}
		}
		status := domain.ContainerStatus(state)
		nodes = append(nodes, Node{
			ID:              domain.InstanceID(domain.KindContainer, container.ID),
			Kind:            "instance",
			Label:           container.Name(),
			Status:          &Status{Tone: string(status.Tone), Tooltip: status.Tooltip},
			Actions:         containerActions(container.State),
			DefaultActionID: ActionContainerLogs,
		})
	}
	return nodes
}

// needsDetail reports whether a container's tone depends on something the list omits.
func needsDetail(state string) bool {
	switch strings.ToLower(state) {
	case "running", "exited":
		return true
	default:
		return false
	}
}

func formatPorts(ports []dockerapi.Port) string {
	if len(ports) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(ports))
	var parts []string
	for _, p := range ports {
		var text string
		if p.PublicPort > 0 {
			text = fmt.Sprintf("%d→%d/%s", p.PublicPort, p.PrivatePort, p.Type)
		} else {
			text = fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
		}
		if _, dup := seen[text]; dup {
			// Docker lists one entry per bound address, so a port published on both IPv4 and IPv6
			// appears twice. The user cares that it is published, not how many stacks it reached.
			continue
		}
		seen[text] = struct{}{}
		parts = append(parts, text)
	}
	return strings.Join(parts, ", ")
}

// imageNodes turns the image list into rows, marking which ones something is actually using.
//
// Usage is what a person opens this list to decide: an image nothing references is a candidate for
// removal, and one a container depends on is not. The daemon does not report it, so it is derived
// here by matching every container's ImageID — and a container's `Image` field is whatever it was
// started with (a tag, which may since have moved), so the id is what must be compared.
func imageNodes(images []dockerapi.Image, containers []dockerapi.Container) []Node {
	usedBy := make(map[string]int, len(images))
	for _, container := range containers {
		if container.ImageID != "" {
			usedBy[container.ImageID]++
		}
	}

	nodes := make([]Node, 0, len(images))
	for _, image := range images {
		nodes = append(nodes, Node{
			ID:      domain.InstanceID(domain.KindImage, image.ID),
			Kind:    "instance",
			Label:   imageLabel(image),
			Status:  imageStatus(image, usedBy[image.ID]),
			Actions: imageActions(),
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Label < nodes[j].Label })
	return nodes
}

// imageStatus marks an image by what depends on it.
//
// In use is `ok`; unused is `neutral`, not `warn` — an unused image is not a problem, it is a
// choice, and painting every one of them amber would make the colour meaningless on a host that
// keeps a build cache. Dangling (untagged AND unused) is the one worth pointing at, because it is
// the case with no way back: nothing references it and no name can reach it.
func imageStatus(image dockerapi.Image, users int) *Status {
	size := humanBytes(image.Size)
	switch {
	case users > 0:
		containers := "container"
		if users > 1 {
			containers = "containers"
		}
		return &Status{Tone: string(domain.ToneOK),
			Tooltip: fmt.Sprintf("Used by %d %s\n%s", users, containers, size)}
	case isDangling(image):
		return &Status{Tone: string(domain.ToneWarn),
			Tooltip: "Dangling: untagged and unused\n" + size}
	default:
		return &Status{Tone: string(domain.ToneNeutral),
			Tooltip: "Not used by any container\n" + size}
	}
}

// isDangling reports an image no tag can reach.
func isDangling(image dockerapi.Image) bool {
	for _, tag := range image.RepoTags {
		if tag != "" && tag != "<none>:<none>" {
			return false
		}
	}
	return true
}

// imageLabel prefers a tag: an id is what the daemon calls it, a tag is what the user does.
func imageLabel(image dockerapi.Image) string {
	for _, tag := range image.RepoTags {
		if tag != "" && tag != "<none>:<none>" {
			return tag
		}
	}
	id := strings.TrimPrefix(image.ID, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return "<untagged> " + id
}

// volumeNodes turns the volume list into rows, marking which ones something still mounts.
//
// Same derivation as images and for the same reason: "is anything using this" is the question the
// list is opened to answer, the daemon reports it on neither endpoint, and the container list
// already fetched for the counts carries it. A volume is matched by NAME, which is also its id.
func volumeNodes(volumes []dockerapi.Volume, containers []dockerapi.Container) []Node {
	usedBy := make(map[string]int, len(volumes))
	for _, container := range containers {
		for _, mount := range container.Mounts {
			// Only named volumes: a bind mount has no Name and names nothing on this list.
			if mount.Name != "" {
				usedBy[mount.Name]++
			}
		}
	}

	nodes := make([]Node, 0, len(volumes))
	for _, volume := range volumes {
		status := domain.VolumeStatus(domain.VolumeState{
			Driver:     volume.Driver,
			Mountpoint: volume.Mountpoint,
			Users:      usedBy[volume.Name],
		})
		nodes = append(nodes, Node{
			ID:      domain.InstanceID(domain.KindVolume, volume.Name),
			Kind:    "instance",
			Label:   volume.Name,
			Status:  &Status{Tone: string(status.Tone), Tooltip: status.Tooltip},
			Actions: volumeActions(),
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Label < nodes[j].Label })
	return nodes
}

// networkNodes turns the network list into rows, marking which ones have containers on them.
//
// A container's NetworkSettings.Networks is keyed by network NAME, so that is what the count is
// built on. The driver moves into the tooltip: it used to be glued onto the label behind two
// spaces, which made every row read as a name with something stuck to it.
func networkNodes(networks []dockerapi.Network, containers []dockerapi.Container) []Node {
	usedBy := make(map[string]int, len(networks))
	for _, container := range containers {
		for name := range container.NetworkSettings.Networks {
			usedBy[name]++
		}
	}

	nodes := make([]Node, 0, len(networks))
	for _, network := range networks {
		status := domain.NetworkStatus(domain.NetworkState{
			Name:     network.Name,
			Driver:   network.Driver,
			Scope:    network.Scope,
			Internal: network.Internal,
			Users:    usedBy[network.Name],
		})
		nodes = append(nodes, Node{
			ID:      domain.InstanceID(domain.KindNetwork, network.ID),
			Kind:    "instance",
			Label:   network.Name,
			Status:  &Status{Tone: string(status.Tone), Tooltip: status.Tooltip},
			Actions: networkActions(network.Name),
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Label < nodes[j].Label })
	return nodes
}
