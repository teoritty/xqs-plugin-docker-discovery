package usecase

import "strings"

// Action ids. They are the plugin's own vocabulary — the host relays them without understanding
// them (ADR-014) — and they live here so the menu and the dispatcher cannot drift apart.
const (
	ActionContainerStart   = "container.start"
	ActionContainerStop    = "container.stop"
	ActionContainerRestart = "container.restart"
	ActionContainerPause   = "container.pause"
	ActionContainerResume  = "container.resume"
	ActionContainerKill    = "container.kill"
	ActionContainerRemove  = "container.remove"
	ActionContainerLogs    = "container.logs"
	ActionContainerConsole = "container.console"
	ActionContainerInspect = "container.inspect"

	ActionImageRemove  = "image.remove"
	ActionImageInspect = "image.inspect"

	ActionVolumeCreate  = "volume.create"
	ActionVolumeRemove  = "volume.remove"
	ActionVolumeInspect = "volume.inspect"

	ActionNetworkCreate  = "network.create"
	ActionNetworkRemove  = "network.remove"
	ActionNetworkInspect = "network.inspect"
)

// containerActions returns the menu for one container, offering only what its state allows.
//
// Offering everything and failing afterwards would be simpler and worse: a menu that lists "Start"
// on a running container teaches the user that the menu does not mean anything, and the failure
// arrives after the click rather than before it.
func containerActions(state string) []Action {
	running := strings.EqualFold(state, "running")
	paused := strings.EqualFold(state, "paused")
	stopped := !running && !paused

	actions := make([]Action, 0, 10)
	if stopped {
		actions = append(actions, Action{ID: ActionContainerStart, Label: "Start", Multi: true})
	}
	if running {
		actions = append(actions,
			Action{ID: ActionContainerStop, Label: "Stop", Multi: true},
			Action{ID: ActionContainerPause, Label: "Pause", Multi: true},
		)
	}
	if paused {
		actions = append(actions, Action{ID: ActionContainerResume, Label: "Resume", Multi: true})
	}
	if running || paused {
		actions = append(actions,
			Action{ID: ActionContainerRestart, Label: "Restart", Multi: true},
			// Kill is destructive but needs no confirm dialog of its own: it is already marked
			// danger, and the host asks before running a danger action on a selection.
			Action{ID: ActionContainerKill, Label: "Kill", Danger: true, Multi: true,
				Confirm: "Send SIGKILL to the selected containers?"},
		)
	}
	actions = append(actions,
		Action{ID: ActionContainerLogs, Label: "Logs"},
		Action{ID: ActionContainerConsole, Label: "Console"},
		Action{ID: ActionContainerInspect, Label: "Inspect"},
		// Remove opens a dialog rather than carrying a confirm string: the question is not "are you
		// sure" but "with its volumes?", and only a form can ask that.
		Action{ID: ActionContainerRemove, Label: "Remove…", Danger: true, Multi: true},
	)
	return actions
}

func imageActions() []Action {
	return []Action{
		{ID: ActionImageInspect, Label: "Inspect"},
		{ID: ActionImageRemove, Label: "Remove…", Danger: true, Multi: true},
	}
}

func volumeActions() []Action {
	return []Action{
		{ID: ActionVolumeInspect, Label: "Inspect"},
		{ID: ActionVolumeRemove, Label: "Remove…", Danger: true, Multi: true},
	}
}

// networkActions omits removal for the three networks Docker creates and depends on. Removing
// `bridge` is not a thing a user meant to do, and the daemon would refuse it anyway — better to
// not offer it than to offer it and explain afterwards.
func networkActions(name string) []Action {
	actions := []Action{{ID: ActionNetworkInspect, Label: "Inspect"}}
	switch name {
	case "bridge", "host", "none":
		return actions
	}
	return append(actions, Action{ID: ActionNetworkRemove, Label: "Remove", Danger: true, Multi: true,
		Confirm: "Remove the selected networks?"})
}
