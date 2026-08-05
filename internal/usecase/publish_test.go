package usecase

import (
	"strings"
	"testing"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/infra/dockerapi"

	"github.com/teoritty/xqs-plugin-docker-discovery/internal/domain"
)

// A parent's snapshot replaces its children, so publishing a parent after a child briefly removes
// the child and the host sheds it from the observed set. ObservedNodes is backed by a map, whose
// iteration order Go randomises, so an unordered refresh fails intermittently and reads like a
// daemon problem.
func TestBranchesRefreshParentsFirst(t *testing.T) {
	got := orderedBranches([]string{
		domain.NodeContainers,
		domain.NodeRoot,
		"",
		domain.NodeVolumes,
	})
	if got[0] != "" {
		t.Fatalf("the connection root must come first, got %q", got[0])
	}
	if got[1] != domain.NodeRoot {
		t.Fatalf("the docker node must precede its groups, got %q", got[1])
	}
	for _, id := range got[2:] {
		if id != domain.NodeContainers && id != domain.NodeVolumes {
			t.Fatalf("unexpected branch %q in the tail", id)
		}
	}
}

// Ordering must be stable among peers: two groups have no relationship, and reshuffling them
// between refreshes would repaint rows for no reason.
func TestBranchOrderIsStableAmongPeers(t *testing.T) {
	in := []string{domain.NodeNetworks, domain.NodeImages, domain.NodeContainers}
	first := orderedBranches(in)
	for i := 0; i < 20; i++ {
		next := orderedBranches(in)
		for j := range first {
			if first[j] != next[j] {
				t.Fatalf("peer order changed between calls: %v vs %v", first, next)
			}
		}
	}
}

func TestUnknownBranchesSortLast(t *testing.T) {
	got := orderedBranches([]string{"docker/container/abc", ""})
	if got[0] != "" || got[1] != "docker/container/abc" {
		t.Fatalf("got %v", got)
	}
}

// Usage is what a person opens the image list to decide, and the daemon does not report it. A
// container's Image field is whatever it was started with — a tag, which may since have moved — so
// the id is what must be matched.
func TestImageUsageIsMatchedOnTheImageID(t *testing.T) {
	images := []dockerapi.Image{
		{ID: "sha256:used", RepoTags: []string{"nginx:1.25"}, Size: 1024 * 1024},
		{ID: "sha256:idle", RepoTags: []string{"redis:7"}, Size: 2048},
		{ID: "sha256:dangling", RepoTags: []string{"<none>:<none>"}, Size: 512},
	}
	containers := []dockerapi.Container{
		// Image names a tag that no longer points here; only ImageID is authoritative.
		{ID: "c1", ImageID: "sha256:used", Image: "nginx:latest"},
		{ID: "c2", ImageID: "sha256:used", Image: "nginx:latest"},
	}
	byLabel := map[string]*Status{}
	for _, node := range imageNodes(images, containers) {
		byLabel[node.Label] = node.Status
	}

	if byLabel["nginx:1.25"].Tone != "ok" {
		t.Fatalf("an in-use image should read as in use: %+v", byLabel["nginx:1.25"])
	}
	if !strings.Contains(byLabel["nginx:1.25"].Tooltip, "2 containers") {
		t.Fatalf("tooltip = %q", byLabel["nginx:1.25"].Tooltip)
	}
	// Unused is neutral, not amber: it is a choice, not a problem, and amber on every cached image
	// would make the colour mean nothing.
	if byLabel["redis:7"].Tone != "neutral" {
		t.Fatalf("an unused image should be neutral: %+v", byLabel["redis:7"])
	}
	dangling := byLabel["<untagged> sha256:dangl"]
	if dangling == nil {
		for label, st := range byLabel {
			if strings.HasPrefix(label, "<untagged>") {
				dangling = st
			}
		}
	}
	if dangling == nil || dangling.Tone != "warn" {
		t.Fatalf("a dangling image is the one worth pointing at: %+v", dangling)
	}
}

// Volume usage is derived from the containers, because the daemon reports it on neither endpoint.
// A bind mount carries no Name and must not be counted: it names nothing on this list, and counting
// it would paint an unrelated volume green.
func TestVolumeUsageComesFromNamedMountsOnly(t *testing.T) {
	volumes := []dockerapi.Volume{
		{Name: "db-data", Driver: "local", Mountpoint: "/var/lib/docker/volumes/db-data"},
		{Name: "orphan", Driver: "local"},
	}
	containers := []dockerapi.Container{
		{ID: "c1", Mounts: []dockerapi.Mount{{Type: "volume", Name: "db-data"}}},
		{ID: "c2", Mounts: []dockerapi.Mount{
			{Type: "volume", Name: "db-data"},
			{Type: "bind", Name: ""},
		}},
	}
	byLabel := map[string]*Status{}
	for _, node := range volumeNodes(volumes, containers) {
		byLabel[node.Label] = node.Status
	}

	if byLabel["db-data"] == nil || byLabel["db-data"].Tone != "ok" {
		t.Fatalf("a mounted volume should read as in use: %+v", byLabel["db-data"])
	}
	if !strings.Contains(byLabel["db-data"].Tooltip, "2 containers") {
		t.Fatalf("tooltip = %q", byLabel["db-data"].Tooltip)
	}
	if byLabel["orphan"] == nil || byLabel["orphan"].Tone != "warn" {
		t.Fatalf("an unmounted volume is the one worth pointing at: %+v", byLabel["orphan"])
	}
}

// A container's NetworkSettings.Networks is keyed by network NAME, so that is what the count must be
// built on — matching against the id would count nothing and paint every network amber.
func TestNetworkUsageIsMatchedOnTheNetworkName(t *testing.T) {
	networks := []dockerapi.Network{
		{ID: "netid-app", Name: "app-net", Driver: "bridge", Scope: "local"},
		{ID: "netid-idle", Name: "idle-net", Driver: "bridge", Scope: "local"},
		{ID: "netid-bridge", Name: "bridge", Driver: "bridge", Scope: "local"},
	}
	containers := []dockerapi.Container{
		{ID: "c1", NetworkSettings: dockerapi.NetworkSettings{
			Networks: map[string]struct{}{"app-net": {}, "bridge": {}},
		}},
	}
	byLabel := map[string]*Status{}
	for _, node := range networkNodes(networks, containers) {
		byLabel[node.Label] = node.Status
	}

	if byLabel["app-net"] == nil || byLabel["app-net"].Tone != "ok" {
		t.Fatalf("an attached network should read as in use: %+v", byLabel["app-net"])
	}
	if byLabel["idle-net"] == nil || byLabel["idle-net"].Tone != "warn" {
		t.Fatalf("an empty user network is a removal candidate: %+v", byLabel["idle-net"])
	}
	// bridge has a container on it and stays neutral: it is not the user's to remove.
	if byLabel["bridge"] == nil || byLabel["bridge"].Tone != "neutral" {
		t.Fatalf("a built-in network stays neutral: %+v", byLabel["bridge"])
	}
}

// The driver used to be glued onto the label behind two spaces, which made every row read as a name
// with something stuck to it. It belongs in the tooltip, which now exists.
func TestNetworkLabelIsJustTheName(t *testing.T) {
	nodes := networkNodes([]dockerapi.Network{{ID: "n1", Name: "app-net", Driver: "bridge"}}, nil)
	if nodes[0].Label != "app-net" {
		t.Fatalf("label = %q", nodes[0].Label)
	}
	if !strings.Contains(nodes[0].Status.Tooltip, "bridge") {
		t.Fatalf("the driver must still be reachable: %q", nodes[0].Status.Tooltip)
	}
}
