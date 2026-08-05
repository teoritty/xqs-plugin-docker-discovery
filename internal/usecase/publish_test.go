package usecase

import (
	"testing"

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
