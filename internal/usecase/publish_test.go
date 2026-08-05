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
