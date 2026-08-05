# xqs-plugin-docker-discovery

Docker management inside [xQuakShell](https://github.com/teoritty/xQuakShell). Expand any SSH
connection in the tree and its Docker resources are there: containers with a live status dot,
images, volumes and networks, each with the actions you would otherwise open a shell for.

## What it does

- **Tree** — `Docker → Containers / Images / Volumes / Networks`, refreshed from the Docker event
  stream rather than by polling, so a container that stops is grey before you notice it stopped.
- **Status** — a colour-coded dot per container: running, unhealthy, paused, restarting, exited
  clean, exited failing. Hovering says what and since when.
- **Actions** — start, stop, restart, pause, resume, kill, remove (with the force and
  remove-volumes options), on one container or on a selection.
- **Logs** — a tab of its own, following the container, with search, stdout/stderr colouring and
  save-to-file.
- **Console** — a tab of its own with a real TTY inside the container, resized with the window.
- **Inspect** — a readable summary plus the raw JSON, in a dialog.
- **Create** — volumes and networks, with the full option set.
- **Per-container settings** — shell, user and working directory for the console; tail and
  timestamps for the logs. Configure a container once instead of answering a dialog every time.

## Requirements

- xQuakShell with plugin API 1.0 and the `ui` capability (ADR-015).
- Docker CLI **18.09 or newer** on the remote host — the plugin reaches the daemon through
  `docker system dial-stdio`.
- The SSH user must be able to reach the Docker socket, i.e. be in the `docker` group. The plugin
  does not use `sudo`: putting it in the manifest's exec allowlist would be a privilege escalation
  the host could no longer inspect. Without access, the `Docker` node says so instead of failing
  silently.

## Permissions it asks for

| Capability | Why |
|---|---|
| `discovery` | Draw the subtree under SSH connections |
| `channel` / `exec` | Run `docker system dial-stdio` over the connection you authenticated — **requires your explicit consent at install** |
| `ui` | Its own tabs, dialogs and node panel |
| `filesystem` | Per-container settings under the plugin's own data directory |

**On the exec grant.** The plugin declares exactly one command, `docker system dial-stdio`, and
speaks the Docker Engine API over it. The host's allowlist can therefore guarantee the plugin runs
nothing but that command — and cannot restrict which Docker operations follow, because those travel
inside the stream. That is stated plainly rather than hidden: anything able to reach the Docker
socket can do anything Docker can do, whichever verb it went through. The alternative, one argv
template per operation, was rejected because the host's exec channel allocates no PTY, which makes
an interactive `docker exec` impossible — see
[the design doc](docs/specs/2026-08-05-docker-discovery-design.md).

## Building

```sh
make build          # host platform
make build-all      # windows/linux/darwin, amd64 + arm64 for darwin
make pack           # .xqsp bundles with SHA256SUMS
make test           # unit tests
```

`CGO_ENABLED=0` throughout, `-trimpath -ldflags="-s -w"`.

## Not in scope

`docker compose`, image pull/build/push, swarm, prune, registry authentication, and creating
containers. This plugin manages what exists.

## License

See [LICENSE](LICENSE).
