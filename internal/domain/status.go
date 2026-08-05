package domain

import (
	"fmt"
	"strings"
)

// Tone is the host's status vocabulary (ADR-014). The host paints the dot; the plugin decides which
// of the six a container is in.
type Tone string

const (
	ToneOK      Tone = "ok"
	ToneWarn    Tone = "warn"
	ToneError   Tone = "error"
	ToneBusy    Tone = "busy"
	ToneNeutral Tone = "neutral"
	ToneUnknown Tone = "unknown"
)

// Status is what a node's dot shows.
type Status struct {
	Tone    Tone
	Tooltip string
}

// ContainerState is everything the tone depends on, gathered from the container list and its
// inspect. A struct rather than a long parameter list because the mapping below is the one place
// that reads all of it, and a caller assembling it can be checked at compile time.
type ContainerState struct {
	State        string
	StatusText   string
	Image        string
	Health       string
	HealthOutput string
	ExitCode     int
	Ports        string
}

// ContainerStatus maps a container's state to its dot and tooltip.
//
// The order matters. `paused` and `restarting` are reported by Docker as separate states, but a
// paused container's State is also "paused" while Running stays true — so the specific cases are
// asked first and "running" is the fallback, not the lead. Getting that backwards paints a paused
// container green, which is precisely the case where the user is looking for a colour.
func ContainerStatus(s ContainerState) Status {
	switch strings.ToLower(s.State) {
	case "paused":
		return Status{Tone: ToneWarn, Tooltip: describe("Paused", s)}
	case "restarting":
		return Status{Tone: ToneBusy, Tooltip: describe("Restarting", s)}
	case "removing":
		return Status{Tone: ToneBusy, Tooltip: describe("Removing", s)}
	case "created":
		return Status{Tone: ToneNeutral, Tooltip: describe("Created, never started", s)}
	case "dead":
		return Status{Tone: ToneError, Tooltip: describe("Dead", s)}
	case "exited":
		return exitedStatus(s)
	case "running":
		return runningStatus(s)
	default:
		// An unfamiliar state is unknown rather than an error: a newer daemon inventing one is not
		// a fault, and painting it red would be a lie about a container that may be fine.
		return Status{Tone: ToneUnknown, Tooltip: describe(titleCase(s.State), s)}
	}
}

// runningStatus splits a running container by its health check, which is the difference between
// "up" and "up and working".
func runningStatus(s ContainerState) Status {
	switch strings.ToLower(s.Health) {
	case "starting":
		return Status{Tone: ToneBusy, Tooltip: describe(s.StatusText+" · health check starting", s)}
	case "unhealthy":
		// Warn, not error: the container is running and serving, and its own check says something
		// is wrong. Red would put it next to a container that has died, which is a different thing
		// to do about.
		tooltip := describe(s.StatusText+" · unhealthy", s)
		if s.HealthOutput != "" {
			tooltip += "\n" + firstLine(s.HealthOutput)
		}
		return Status{Tone: ToneWarn, Tooltip: tooltip}
	default:
		return Status{Tone: ToneOK, Tooltip: describe(s.StatusText, s)}
	}
}

// exitedStatus distinguishes a container that finished from one that failed. Both are stopped; only
// one is a problem, and a single grey dot for both hides the one the user came to find.
func exitedStatus(s ContainerState) Status {
	if s.ExitCode == 0 {
		return Status{Tone: ToneNeutral, Tooltip: describe(s.StatusText+" · exit 0", s)}
	}
	return Status{Tone: ToneError, Tooltip: describe(fmt.Sprintf("%s · exit %d", s.StatusText, s.ExitCode), s)}
}

// VolumeState is what a volume's dot depends on.
type VolumeState struct {
	Driver     string
	Mountpoint string
	// Users is how many containers mount it. The daemon reports it nowhere on the volume list, so
	// the caller derives it from the container list.
	Users int
}

// VolumeStatus marks a volume by what mounts it.
//
// Unused is `warn`, not `neutral`, and that is the one place this differs from an image: an unused
// image is a cache and costs nothing to keep, whereas an unused volume is data nothing can reach
// any more — Docker itself calls those dangling, and it is the case a person scans this list for.
func VolumeStatus(s VolumeState) Status {
	where := joinLines(s.Driver, s.Mountpoint)
	if s.Users > 0 {
		return Status{Tone: ToneOK, Tooltip: joinLines(countUsers(s.Users), where)}
	}
	return Status{Tone: ToneWarn, Tooltip: joinLines("Dangling: no container mounts it", where)}
}

// NetworkState is what a network's dot depends on.
type NetworkState struct {
	Name     string
	Driver   string
	Scope    string
	Internal bool
	// Users is how many containers are attached.
	Users int
}

// NetworkStatus marks a network by what is attached to it.
//
// Docker's own three are neutral whatever is attached: they are not something a user created and
// not something they can remove, so colouring them by usage would put attention on the three rows
// where no decision exists.
func NetworkStatus(s NetworkState) Status {
	detail := joinLines(s.Driver, s.Scope)
	if s.Internal {
		detail = joinLines(detail, "internal: no outbound access")
	}
	switch s.Name {
	case "bridge", "host", "none":
		return Status{Tone: ToneNeutral, Tooltip: joinLines("Built-in", detail)}
	}
	if s.Users > 0 {
		return Status{Tone: ToneOK, Tooltip: joinLines(countUsers(s.Users), detail)}
	}
	return Status{Tone: ToneWarn, Tooltip: joinLines("Unused: no containers attached", detail)}
}

func countUsers(n int) string {
	if n == 1 {
		return "Used by 1 container"
	}
	return fmt.Sprintf("Used by %d containers", n)
}

// joinLines assembles a tooltip out of parts, dropping the empty ones rather than rendering them as
// blank lines.
func joinLines(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

// describe assembles the tooltip: what the container is doing, then what it is and where it is
// reachable. Empty parts are dropped rather than rendered as blanks.
func describe(headline string, s ContainerState) string {
	parts := []string{strings.TrimSpace(headline)}
	if s.Image != "" {
		parts = append(parts, "image "+s.Image)
	}
	if s.Ports != "" {
		parts = append(parts, s.Ports)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n")
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s)
}

func titleCase(s string) string {
	if s == "" {
		return "Unknown state"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
