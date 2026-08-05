package usecase

import (
	"context"
	"log/slog"
	"sort"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/domain"
)

// publish sends one branch snapshot to the host.
//
// A snapshot fully replaces the branch (ADR-014): there are no deltas, so a node absent from it is
// removed. That is what makes a container disappearing from the tree require no special message.
func (s *Service) publish(ctx context.Context, conn *Connection, nodeID string, children []Node, state, errMessage string) {
	for _, child := range children {
		if child.Kind == "instance" {
			if _, dockerID, err := domain.ParseInstanceID(child.ID); err == nil {
				conn.Remember(child.ID, dockerID)
			}
		}
	}
	payload := map[string]any{
		"sessionId": conn.SessionID(),
		"nodeId":    nodeID,
		"state":     state,
		"children":  children,
	}
	if errMessage != "" {
		payload["error"] = errMessage
	}
	if state == "error" && errMessage != "" {
		// Logged at warn, because "the Docker node is empty" is otherwise the only symptom and it
		// names no cause. The message is the same one the row carries.
		slog.Warn("branch failed", "component", "docker", "node", nodeID, "reason", errMessage)
	}
	if _, err := s.host.Call(ctx, "discovery.publish", payload); err != nil {
		// A publish for a branch the user collapsed, or a session that stopped leading, is accepted
		// and dropped by the host — an ordinary race, not something to retry or report.
		slog.Debug("publish refused", "node", nodeID, "err", err)
	}
}

// refreshBranch enumerates one branch and publishes it.
//
// Only observed branches are enumerated. That restraint is the point of the level-triggered
// protocol: a plugin that listed everything would poll a server for resources nobody is looking at.
func (s *Service) refreshBranch(ctx context.Context, conn *Connection, nodeID string) {
	if !conn.Observes(nodeID) {
		return
	}
	// NO "loading" snapshot first. A publish REPLACES a branch (ADR-014), so an empty one to say
	// "working on it" deletes the branch's children — and the host then sheds those ids from the
	// observed set, so the real snapshot that follows arrives for a node nobody is watching and is
	// accepted and dropped. That is what made the Docker node expand into nothing.
	//
	// One publish per branch, carrying what is actually there. A branch is briefly absent instead of
	// briefly empty, which is the same wait without the destruction.
	client, err := conn.Control(ctx)
	if err != nil {
		s.publish(ctx, conn, nodeID, nil, "error", userMessage(err))
		return
	}

	switch nodeID {
	case "":
		s.refreshRoot(ctx, conn)
	case domain.NodeRoot:
		s.publishRootGroups(ctx, conn)
	case domain.NodeContainers:
		containers, err := client.ListContainers(ctx)
		if err != nil {
			s.publish(ctx, conn, nodeID, nil, "error", userMessage(err))
			return
		}
		s.publish(ctx, conn, nodeID, containerNodes(ctx, client, containers), "ready", "")
	case domain.NodeImages:
		images, err := client.ListImages(ctx)
		if err != nil {
			s.publish(ctx, conn, nodeID, nil, "error", userMessage(err))
			return
		}
		// Containers too, because "is anything using this image" is derived rather than reported.
		// A failure here loses the marks, not the list: an image row without a usage dot is still a
		// usable row.
		containers, err := client.ListContainers(ctx)
		if err != nil {
			slog.Debug("image usage unavailable", "err", err)
			containers = nil
		}
		s.publish(ctx, conn, nodeID, imageNodes(images, containers), "ready", "")
	case domain.NodeVolumes:
		volumes, err := client.ListVolumes(ctx)
		if err != nil {
			s.publish(ctx, conn, nodeID, nil, "error", userMessage(err))
			return
		}
		// Containers too: whether anything mounts a volume is derived, not reported. A failure here
		// loses the marks, not the list — same trade as the image branch above.
		containers, err := client.ListContainers(ctx)
		if err != nil {
			slog.Debug("volume usage unavailable", "err", err)
			containers = nil
		}
		s.publish(ctx, conn, nodeID, volumeNodes(volumes, containers), "ready", "")
	case domain.NodeNetworks:
		networks, err := client.ListNetworks(ctx)
		if err != nil {
			s.publish(ctx, conn, nodeID, nil, "error", userMessage(err))
			return
		}
		containers, err := client.ListContainers(ctx)
		if err != nil {
			slog.Debug("network usage unavailable", "err", err)
			containers = nil
		}
		s.publish(ctx, conn, nodeID, networkNodes(networks, containers), "ready", "")
	}
}

// refreshRoot publishes the Docker node itself under the connection.
//
// Reaching the daemon is what decides whether it appears at all: a host without Docker gets no
// Docker node, and a host with Docker the user cannot reach gets one that says so. Both beat a
// group that expands into an error.
func (s *Service) refreshRoot(ctx context.Context, conn *Connection) {
	client, err := conn.Control(ctx)
	if err != nil {
		s.publish(ctx, conn, "", []Node{{
			ID: domain.NodeRoot, Kind: "group", Label: "Docker", IconID: domain.IconDocker,
			Status: &Status{Tone: string(domain.ToneError), Tooltip: userMessage(err)},
		}}, "ready", "")
		return
	}
	if _, err := client.Negotiate(ctx); err != nil {
		s.publish(ctx, conn, "", []Node{{
			ID: domain.NodeRoot, Kind: "group", Label: "Docker", IconID: domain.IconDocker,
			Status: &Status{Tone: string(domain.ToneError), Tooltip: userMessage(err)},
		}}, "ready", "")
		return
	}
	s.publish(ctx, conn, "", []Node{{
		ID: domain.NodeRoot, Kind: "group", Label: "Docker", IconID: domain.IconDocker,
	}}, "ready", "")
}

// publishRootGroups fills the Docker node with its four groups, counting each so the label can say
// how much is inside without the user expanding it.
func (s *Service) publishRootGroups(ctx context.Context, conn *Connection) {
	client, err := conn.Control(ctx)
	if err != nil {
		s.publish(ctx, conn, domain.NodeRoot, nil, "error", userMessage(err))
		return
	}
	counts := map[string]int{}
	if containers, err := client.ListContainers(ctx); err == nil {
		counts[domain.NodeContainers] = len(containers)
	}
	if images, err := client.ListImages(ctx); err == nil {
		counts[domain.NodeImages] = len(images)
	}
	if volumes, err := client.ListVolumes(ctx); err == nil {
		counts[domain.NodeVolumes] = len(volumes)
	}
	if networks, err := client.ListNetworks(ctx); err == nil {
		counts[domain.NodeNetworks] = len(networks)
	}
	s.publish(ctx, conn, domain.NodeRoot, buildRoot(counts), "ready", "")
}

// refreshAll re-enumerates every branch the user has open, parents first.
//
// The order is not cosmetic. A parent's snapshot replaces its children, so publishing a parent
// AFTER a child briefly removes the child and makes the host shed it from the observed set — and
// ObservedNodes is backed by a map, whose iteration order Go randomises, so getting this wrong
// fails intermittently and looks like a daemon problem.
func (s *Service) refreshAll(ctx context.Context, conn *Connection) {
	for _, nodeID := range orderedBranches(conn.ObservedNodes()) {
		if conn.Gone() {
			return
		}
		s.refreshBranch(ctx, conn, nodeID)
	}
}

// orderedBranches sorts observed nodes so a parent always precedes its children: the connection
// root, then the Docker node, then the four groups, then anything else.
func orderedBranches(nodeIDs []string) []string {
	rank := func(id string) int {
		switch id {
		case "":
			return 0
		case domain.NodeRoot:
			return 1
		case domain.NodeContainers, domain.NodeImages, domain.NodeVolumes, domain.NodeNetworks:
			return 2
		default:
			return 3
		}
	}
	out := append([]string(nil), nodeIDs...)
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}
