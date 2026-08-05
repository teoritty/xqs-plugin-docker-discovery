# xQuakShell Docker Discovery plugin — design

Host contracts this depends on: [ADR-014](../../../xQuakShell/docs/adr/014-discovery-subtrees.md)
(discovery subtrees), [ADR-015](../../../xQuakShell/docs/adr/015-plugin-ui-surfaces.md) (plugin UI
surfaces), [ADR-011](../../../xQuakShell/docs/adr/011-binary-channel-bus.md) (binary channel bus),
`docs/plugin-api.md`, `docs/plugin-manifest.md`.

## Goal

Under any SSH connection in the tree, show the remote host's Docker resources and let the user work
with them without leaving xQuakShell: a `Docker` subtree with `Containers`, `Images`, `Volumes` and
`Networks`, a colour-coded status dot on each container, actions on every item, container logs and
an interactive console as their own tabs, inspect and create forms as modal dialogs, and per-node
settings in the sidebar panel.

## Transport

**One `exec` channel per Docker connection, running `docker system dial-stdio`, carrying HTTP/1.1
to the Docker Engine API.**

The alternative — a `docker` CLI invocation per operation, each its own argv template — was
rejected for three reasons that are not stylistic:

1. **No PTY.** The host's exec channel does not request one (`channel_exec_backend.go` never calls
   `RequestPty`), so `docker exec -it` fails outright. Over the Engine API the container still gets
   a real TTY (`Tty: true` on exec create, resized through `/exec/{id}/resize`) because the PTY is
   allocated by the daemon, not by the SSH channel.
2. **Parsing.** `docker ps --format` output is a presentation format that changes between versions;
   `/containers/json` is a typed contract.
3. **Streaming.** `/events`, `/logs?follow=1` and a hijacked exec stream are single long-lived
   connections. The CLI equivalent is a process per stream, each holding a channel.

The cost is honest and stated in the README: one argv template means the host's exec allowlist
cannot restrict *which* Docker operations the plugin performs. It can restrict that the plugin runs
`docker system dial-stdio` and nothing else — and anything that can reach the Docker socket can
reach all of it anyway, whichever verb it went through.

**Channel budget** (`channel.maxConcurrent: 8`, declared explicitly — the default of 4 would fail
the fifth `channel.open` with `ErrRateLimited`, which reads like a throughput problem and is not
one):

| Channel | Lifetime |
|---|---|
| control | one per connection, HTTP keep-alive, short requests serialized |
| `/events` | one per connection, long-lived |
| logs | one per open log tab |
| exec | one per open console tab |

**API version**: `GET /version` once per connection, then `min(daemon.ApiVersion, 1.45)` as the
path prefix. Pinning blind would break on old daemons; taking whatever the daemon offers would
break on new ones.

**Privileges**: plain `docker`, no sudo. A user not in the `docker` group gets a `Docker` node in
the error state saying so, which is a truthful answer; putting `sudo` in an argv template would be
a privilege escalation the host can no longer inspect.

## The tree

```
<ssh connection>
└── Docker                      group, icon docker
    ├── Containers (12)         group, icon containers
    │   └── nginx  ●            instance, status dot
    ├── Images (34)             group
    ├── Volumes (7)             group
    └── Networks (4)            group
```

Counts are in the group label because a group's own row is the only place a total can appear
without expanding it.

**Status dot** (`Status{Tone, Color, Tooltip}`, filled only by the plugin — ADR-014):

| Container state | Tone | Tooltip carries |
|---|---|---|
| running, health healthy or absent | `ok` | uptime, image, ports |
| running, health starting | `busy` | as above + "health check starting" |
| running, health unhealthy | `warn` | as above + last health output line |
| paused | `warn` | uptime before pause |
| restarting, removing | `busy` | — |
| created | `neutral` | image |
| exited, code 0 | `neutral` | finished-at, exit code |
| exited, code ≠ 0 | `error` | finished-at, exit code, last log line |
| dead | `error` | — |

## Refresh

One `/events` subscription per connection. An event names the object it touched, so only the
affected branch is republished, debounced 300 ms; a reconciling full re-list runs every 60 s so a
dropped event cannot leave the tree wrong indefinitely. Only branches the host says are expanded
are enumerated — `discovery.observe` carries the full expanded set, which is exactly the load the
level-triggered protocol exists to avoid.

## Actions

| Node | Actions |
|---|---|
| Container | Start, Stop, Restart, Pause, Resume, Kill, Remove (dialog: force / remove volumes), Logs, Console, Inspect |
| Image | Remove (dialog: force / no-prune), Inspect |
| Volumes group | Create (dialog) |
| Volume | Remove (dialog: force), Inspect |
| Networks group | Create (dialog) |
| Network | Remove, Inspect |

`multi: true` on start/stop/restart/kill/pause/resume/remove. An action acknowledges within the 5 s
budget, moves its nodes to `busy` itself, and reports the outcome by republishing — partial success
is some nodes `ok` and others `error`, with no reporting protocol of its own (ADR-014).

**Create forms** carry the full portainer field set: volumes get name, driver, driver options
(`keyValue`), labels (`keyValue`); networks get name, driver, subnet, gateway, IP range, aux
addresses, internal, attachable, IPv6, driver options, labels.

**Inspect** is a `detail` dialog: a readable summary plus the raw JSON in a `code` field.

## Logs and console

Both are ADR-015 surfaces bound to the SSH session the subtree rides.

- **Logs** → `kind: "log"`. `/containers/{id}/logs?follow&tail&timestamps&stdout&stderr`,
  demultiplexed by the 8-byte Docker stream header so stdout and stderr reach the viewer tagged and
  the viewer can colour them. Defaults come from the node's settings panel.
- **Console** → `kind: "terminal"`. `POST /containers/{id}/exec` with `Tty: true`, then
  `POST /exec/{id}/start` hijacked; `surface.resize` becomes `POST /exec/{id}/resize`. Shell, user
  and working directory come from the node's settings panel, so the user configures a container
  once rather than answering a dialog every time.

## Node settings

`discovery.describeNode` returns, per node kind:

- **Container**: editable console settings (shell, user, working directory, privileged) and log
  settings (tail, timestamps, follow), plus a read-only summary.
- **Image / Volume / Network**: read-only summary (`editable: false`).

`applyDetails` persists to `${pluginData}/settings/<connectionId>.json`, **keyed by container name,
not id**: an id changes on every `docker run --rm`-style recreate, and settings that vanish when a
container is recreated are settings nobody will bother to set.

## Layers

```
cmd/xqs-docker/          main.go, wiring.go — composition root
cmd/xqs-docker-pack/     .xqsp packing, SHA256SUMS, Ed25519 signing
internal/domain/         container.go image.go volume.go network.go
                         node_id.go status_tone.go actions.go errors.go ports.go
internal/usecase/        observe.go publish.go events_watch.go
                         tree_containers.go tree_images.go tree_volumes.go tree_networks.go
                         action_container.go action_image.go action_volume.go action_network.go
                         logs_surface.go exec_surface.go
                         dialog_inspect.go dialog_create_volume.go dialog_create_network.go
                         dialog_remove.go node_details.go settings.go
internal/infra/ipc/      codec.go client.go dispatch.go rpc.go — the 9-byte frame layer
internal/infra/chanbus/  open.go credit.go stream.go — channel bus with flow control
internal/infra/dockerapi/ transport.go http_codec.go version.go hijack.go stream_demux.go
                          containers.go images.go volumes.go networks.go events.go exec.go
internal/infra/settings/ store.go — ${pluginData} persistence
internal/infra/logger/   secure_logger.go
internal/presentation/   handler_observe.go handler_invoke.go handler_describe.go
                         handler_surface.go handler_dialog.go dto.go
ui/icons/                docker.svg containers.svg images.svg volumes.svg networks.svg
```

Same layering rules as the host: `domain` is stdlib only; `usecase` imports `domain` and stdlib;
`infra` may import third parties; `presentation` translates. ≤ 350 lines per file.

## Safety

- **Never trust a `nodeId` from `invokeAction`.** Ids are resolved against the snapshot this
  session last published, and the resolved Docker id is re-validated against
  `^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$` before it reaches a URL path. An id that resolves to nothing
  is refused, not guessed.
- **Concurrency**: one state object per connection under its own mutex; single-flight per branch
  refresh; a bounded worker pool for actions; every goroutine bound to the session's context; no
  lock held across an RPC or a channel write.
- **Errors**: Docker's JSON error becomes a short user message. Nothing carries a host path, a
  socket path or a stack.
- **Logs**: no container environment, no labels, no command lines — a plugin log is not a place to
  copy someone's secrets to.

## Build

Windows/Linux/macOS (amd64 + arm64 for macOS), `CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`.
`.xqsp` with `SHA256SUMS` and an Ed25519 dev signature, produced by `cmd/xqs-docker-pack`. GitHub
Actions builds all four and attaches them to a release.

## Out of scope

`docker compose`, image pull/build/push, swarm, prune, registry auth, container create/run. The
plugin manages what exists; creating containers is a different product surface and would need its
own form the size of everything here.
