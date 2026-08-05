package domain

import (
	"strings"
	"testing"
)

func TestInstanceIDRoundTrips(t *testing.T) {
	id := InstanceID(KindContainer, "abc123")
	kind, dockerID, err := ParseInstanceID(id)
	if err != nil {
		t.Fatalf("ParseInstanceID: %v", err)
	}
	if kind != KindContainer || dockerID != "abc123" {
		t.Fatalf("kind=%q id=%q", kind, dockerID)
	}
}

// The id comes back from the host inside an invokeAction, across a boundary. A validated id cannot
// carry a traversal or a query string into a request, whatever happened in between.
func TestParseInstanceIDRefusesDangerousIDs(t *testing.T) {
	for _, id := range []string{
		"docker/container/../../etc/passwd",
		"docker/container/abc?force=1",
		"docker/container/abc/json",
		"docker/container/",
		"docker/container/-leading-dash",
		"docker/hologram/abc",
		"nothing",
		"",
		"docker/container/" + strings.Repeat("x", 200),
	} {
		if _, _, err := ParseInstanceID(id); err == nil {
			t.Fatalf("%q was accepted", id)
		}
	}
}

func TestGroupForKind(t *testing.T) {
	if GroupForKind(KindVolume) != NodeVolumes || GroupForKind(KindNetwork) != NodeNetworks {
		t.Fatal("an instance must land under the group it belongs to")
	}
}

// --- status ----------------------------------------------------------------

func TestRunningContainerIsGreen(t *testing.T) {
	got := ContainerStatus(ContainerState{State: "running", StatusText: "Up 3 hours", Image: "nginx"})
	if got.Tone != ToneOK {
		t.Fatalf("tone = %q", got.Tone)
	}
	if !strings.Contains(got.Tooltip, "Up 3 hours") || !strings.Contains(got.Tooltip, "nginx") {
		t.Fatalf("tooltip = %q", got.Tooltip)
	}
}

// A paused container reports Running=true underneath. Asking "running?" first paints it green,
// which is precisely the case the user is looking for a colour to explain.
func TestPausedContainerIsNotGreen(t *testing.T) {
	got := ContainerStatus(ContainerState{State: "paused", StatusText: "Up 2 hours (Paused)"})
	if got.Tone != ToneWarn {
		t.Fatalf("tone = %q, want warn", got.Tone)
	}
}

func TestRestartingIsBusy(t *testing.T) {
	if ContainerStatus(ContainerState{State: "restarting"}).Tone != ToneBusy {
		t.Fatal("a restarting container must read as in-progress, not as healthy or failed")
	}
}

// Both are stopped; only one is a problem, and one grey dot for both hides the one worth finding.
func TestExitedSplitsOnTheExitCode(t *testing.T) {
	clean := ContainerStatus(ContainerState{State: "exited", StatusText: "Exited (0) 5 minutes ago"})
	if clean.Tone != ToneNeutral {
		t.Fatalf("clean exit tone = %q", clean.Tone)
	}
	failed := ContainerStatus(ContainerState{State: "exited", StatusText: "Exited (137) 1 minute ago", ExitCode: 137})
	if failed.Tone != ToneError {
		t.Fatalf("failed exit tone = %q", failed.Tone)
	}
	if !strings.Contains(failed.Tooltip, "137") {
		t.Fatalf("the exit code is the whole explanation and is missing: %q", failed.Tooltip)
	}
}

// Running and failing its own check is not the same as dead: red would file it next to a container
// that has stopped, which is a different thing to do about.
func TestUnhealthyIsWarnAndCarriesTheCheckOutput(t *testing.T) {
	got := ContainerStatus(ContainerState{
		State: "running", StatusText: "Up 10 minutes",
		Health: "unhealthy", HealthOutput: "curl: (7) Failed to connect\nmore detail",
	})
	if got.Tone != ToneWarn {
		t.Fatalf("tone = %q, want warn", got.Tone)
	}
	if !strings.Contains(got.Tooltip, "Failed to connect") {
		t.Fatalf("tooltip = %q", got.Tooltip)
	}
	if strings.Contains(got.Tooltip, "more detail") {
		t.Fatal("only the first line of the check output belongs in a tooltip")
	}
}

func TestHealthStartingIsBusy(t *testing.T) {
	got := ContainerStatus(ContainerState{State: "running", Health: "starting"})
	if got.Tone != ToneBusy {
		t.Fatalf("tone = %q", got.Tone)
	}
}

func TestCreatedAndDead(t *testing.T) {
	if ContainerStatus(ContainerState{State: "created"}).Tone != ToneNeutral {
		t.Fatal("created is neutral: it has not failed, it has not run")
	}
	if ContainerStatus(ContainerState{State: "dead"}).Tone != ToneError {
		t.Fatal("dead is an error")
	}
}

// A newer daemon inventing a state is not a fault, and red would be a lie about a container that
// may be perfectly fine.
func TestUnfamiliarStateIsUnknownNotError(t *testing.T) {
	got := ContainerStatus(ContainerState{State: "hibernating"})
	if got.Tone != ToneUnknown {
		t.Fatalf("tone = %q, want unknown", got.Tone)
	}
}

func TestTooltipDropsEmptyParts(t *testing.T) {
	got := ContainerStatus(ContainerState{State: "running", StatusText: "Up"})
	if strings.Contains(got.Tooltip, "\n\n") || strings.HasSuffix(got.Tooltip, "\n") {
		t.Fatalf("tooltip has blank lines: %q", got.Tooltip)
	}
}

// Docker mints image ids as "sha256:<hex>". The first version of this pattern was written from what
// a container id looks like and silently refused every image, which reached a user as "Inspect does
// nothing".
func TestImageIDsAreAcceptedWholeSHA(t *testing.T) {
	id := "sha256:802c91d5298192c0f3a08101aeb5f9ade2992e22c9e27fa8b88eab82602550d0"
	node := InstanceID(KindImage, id)
	kind, dockerID, err := ParseInstanceID(node)
	if err != nil {
		t.Fatalf("ParseInstanceID(%q): %v", node, err)
	}
	if kind != KindImage || dockerID != id {
		t.Fatalf("kind=%q id=%q", kind, dockerID)
	}
}

// Widening the charset must not widen what can reach a path. These stay refused.
func TestWiderCharsetStillRefusesPathTricks(t *testing.T) {
	for _, id := range []string{
		"docker/image/../../etc/passwd",
		"docker/image/sha256:abc/../..",
		"docker/image/:leading-colon",
		"docker/image/sha256:abc?force=1",
	} {
		if _, _, err := ParseInstanceID(id); err == nil {
			t.Fatalf("%q was accepted", id)
		}
	}
}

// Selecting a group asks the host for its details. Classifying one as unknown produced an error
// toast for a user who had done nothing wrong.
func TestClassOfDistinguishesGroupsFromInstances(t *testing.T) {
	cases := map[string]NodeClass{
		"":                    ClassConnectionRoot,
		NodeRoot:              ClassDockerRoot,
		NodeContainers:        ClassGroup,
		NodeImages:            ClassGroup,
		NodeVolumes:           ClassGroup,
		NodeNetworks:          ClassGroup,
		"docker/image/abc123": ClassInstance,
		"docker/nonsense":     ClassUnknown,
		"something-else":      ClassUnknown,
	}
	for id, want := range cases {
		if got := ClassOf(id); got != want {
			t.Fatalf("ClassOf(%q) = %v, want %v", id, got, want)
		}
	}
}

// A volume nothing mounts is amber, not grey. That is the one way this differs from an image: an
// unused image is a cache, an unused volume is data nothing can reach any more, and it is the case
// a person scans the list for.
func TestVolumeStatusMarksWhatNothingMounts(t *testing.T) {
	used := VolumeStatus(VolumeState{Driver: "local", Mountpoint: "/var/lib/docker/volumes/db", Users: 2})
	if used.Tone != ToneOK {
		t.Fatalf("a mounted volume should read as in use: %+v", used)
	}
	if !strings.Contains(used.Tooltip, "2 containers") || !strings.Contains(used.Tooltip, "local") {
		t.Fatalf("tooltip = %q", used.Tooltip)
	}

	idle := VolumeStatus(VolumeState{Driver: "local", Users: 0})
	if idle.Tone != ToneWarn {
		t.Fatalf("an unmounted volume is the one worth pointing at: %+v", idle)
	}

	// Singular and plural, because "Used by 1 containers" is the kind of detail that makes a tooltip
	// look machine-written.
	if one := VolumeStatus(VolumeState{Users: 1}); !strings.Contains(one.Tooltip, "1 container\n") &&
		!strings.HasSuffix(one.Tooltip, "1 container") {
		t.Fatalf("tooltip = %q", one.Tooltip)
	}
}

// Docker's own three networks are neutral whatever is attached: the user did not create them and
// cannot remove them, so colouring them by usage puts attention where no decision exists.
func TestNetworkStatusLeavesTheBuiltInsAlone(t *testing.T) {
	for _, name := range []string{"bridge", "host", "none"} {
		got := NetworkStatus(NetworkState{Name: name, Driver: name, Scope: "local", Users: 7})
		if got.Tone != ToneNeutral {
			t.Fatalf("%s should stay neutral, got %+v", name, got)
		}
		if !strings.Contains(got.Tooltip, "Built-in") {
			t.Fatalf("%s tooltip = %q", name, got.Tooltip)
		}
	}
}

func TestNetworkStatusMarksUserNetworksByAttachment(t *testing.T) {
	attached := NetworkStatus(NetworkState{Name: "app-net", Driver: "bridge", Scope: "local", Users: 3})
	if attached.Tone != ToneOK || !strings.Contains(attached.Tooltip, "3 containers") {
		t.Fatalf("an attached network should read as in use: %+v", attached)
	}

	empty := NetworkStatus(NetworkState{Name: "app-net", Driver: "bridge", Scope: "local"})
	if empty.Tone != ToneWarn {
		t.Fatalf("an empty user network is a removal candidate: %+v", empty)
	}

	// Internal is a property of the network, not of its use, so it appears either way.
	internal := NetworkStatus(NetworkState{Name: "app-net", Driver: "bridge", Internal: true, Users: 1})
	if !strings.Contains(internal.Tooltip, "internal") {
		t.Fatalf("tooltip = %q", internal.Tooltip)
	}
}
