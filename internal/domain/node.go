// Package domain holds the plugin's own vocabulary: what a node id means, what a container's state
// says about its status dot, and which actions exist. It imports nothing but the standard library.
package domain

import (
	"errors"
	"regexp"
	"strings"
)

// Node ids the plugin publishes. The four groups are fixed; instances are prefixed by their group
// so an id says what it is without a lookup.
const (
	NodeRoot       = "docker"
	NodeContainers = "docker/containers"
	NodeImages     = "docker/images"
	NodeVolumes    = "docker/volumes"
	NodeNetworks   = "docker/networks"
)

// Icon ids, matching contributions.discoveryIcons in the manifest.
const (
	IconDocker     = "docker"
	IconContainers = "containers"
	IconImages     = "images"
	IconVolumes    = "volumes"
	IconNetworks   = "networks"
)

// Kind is what an instance node represents.
type Kind string

const (
	KindContainer Kind = "container"
	KindImage     Kind = "image"
	KindVolume    Kind = "volume"
	KindNetwork   Kind = "network"
)

// ErrInvalidNodeID reports an id that does not name anything this plugin publishes.
var ErrInvalidNodeID = errors.New("docker: unknown node")

// dockerIDPattern is what a Docker object id or name may look like before it is put in a URL path.
//
// It is checked even though every id the plugin uses came from the daemon a moment earlier: the id
// arrives back from the host inside an invokeAction, and between publishing it and acting on it
// there is a boundary.
//
// The colon is here because Docker mints image ids as "sha256:<hex>" — the first version of this
// pattern was written from what a container id looks like, and silently refused every image, which
// is how "Inspect does nothing" reached a user. What the pattern must NOT do is carry the safety
// argument on its own: the characters that matter for a URL path are escaped at the point the path
// is built (dockerapi.pathSegment), so widening the charset here cannot reopen a traversal. The
// pattern's job is to reject an id that is not an id at all; the escaping's job is to make any id
// safe to put in a path.
var dockerIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,190}$`)

// ValidDockerID reports whether s is safe to place in an Engine API path.
func ValidDockerID(s string) bool { return dockerIDPattern.MatchString(s) }

// InstanceID builds the node id for one resource.
func InstanceID(kind Kind, dockerID string) string {
	return "docker/" + string(kind) + "/" + dockerID
}

// ParseInstanceID splits an instance node id back into its kind and Docker id.
//
// The Docker id is validated here rather than at the call site, so there is one place that decides
// what may reach a URL and no path that forgot to ask.
func ParseInstanceID(nodeID string) (Kind, string, error) {
	rest, ok := strings.CutPrefix(nodeID, "docker/")
	if !ok {
		return "", "", ErrInvalidNodeID
	}
	kindPart, dockerID, ok := strings.Cut(rest, "/")
	if !ok || dockerID == "" {
		return "", "", ErrInvalidNodeID
	}
	kind := Kind(kindPart)
	switch kind {
	case KindContainer, KindImage, KindVolume, KindNetwork:
	default:
		return "", "", ErrInvalidNodeID
	}
	if !ValidDockerID(dockerID) {
		return "", "", ErrInvalidNodeID
	}
	return kind, dockerID, nil
}

// NodeClass says what a node id names, so a caller can branch without parsing twice.
type NodeClass int

const (
	// ClassUnknown is an id this plugin never published.
	ClassUnknown NodeClass = iota
	// ClassConnectionRoot is the connection itself: the empty id.
	ClassConnectionRoot
	// ClassDockerRoot is the Docker node.
	ClassDockerRoot
	// ClassGroup is one of the four resource groups.
	ClassGroup
	// ClassInstance is a single container, image, volume or network.
	ClassInstance
)

// ClassOf classifies a node id.
//
// It exists because the details panel is asked about whatever the user selected, and that is
// routinely a group — asking Resolve about one produced "unknown node" and an error toast, when the
// honest answer is that a group has details of its own.
func ClassOf(nodeID string) NodeClass {
	switch nodeID {
	case "":
		return ClassConnectionRoot
	case NodeRoot:
		return ClassDockerRoot
	case NodeContainers, NodeImages, NodeVolumes, NodeNetworks:
		return ClassGroup
	}
	if _, _, err := ParseInstanceID(nodeID); err == nil {
		return ClassInstance
	}
	return ClassUnknown
}

// GroupLabel names a group for display.
func GroupLabel(nodeID string) string {
	switch nodeID {
	case NodeContainers:
		return "Containers"
	case NodeImages:
		return "Images"
	case NodeVolumes:
		return "Volumes"
	case NodeNetworks:
		return "Networks"
	case NodeRoot:
		return "Docker"
	default:
		return ""
	}
}

// GroupForKind returns the group node an instance of this kind belongs under.
func GroupForKind(kind Kind) string {
	switch kind {
	case KindContainer:
		return NodeContainers
	case KindImage:
		return NodeImages
	case KindVolume:
		return NodeVolumes
	case KindNetwork:
		return NodeNetworks
	default:
		return NodeRoot
	}
}
