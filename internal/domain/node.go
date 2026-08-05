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
// there is a boundary. A validated id cannot carry a path traversal or a query string into a
// request, whatever happened in between.
var dockerIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

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
