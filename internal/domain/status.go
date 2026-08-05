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
