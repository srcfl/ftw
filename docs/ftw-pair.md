# `ftw-pair` — handing over a 42W instance temporarily

`ftw-pair` lets you grant time-bound MCP access to a running
forty-two-watts instance over a magic-wormhole tunnel. The recipient's
Claude Code gets a curated tool surface for driver development, model
tuning, or live debugging.

## When to use it

- A friend with deeper experience offers to help you write a driver.
- You're away from the instance but want to iterate against it from
  Claude Code on a laptop somewhere else.
- An incident needs eyes-on access from someone other than the operator.

The trust level is "ssh-equivalent for the session duration". Only
pair with people you'd already give shell to.

## Prerequisites

Both sides need `fowl` (Forward Over Wormhole Locally) on `PATH`. It
provides the actual wormhole-based TCP tunnel that `ftw-pair` wraps:

```bash
uv tool install fowl
```

`fowl` is the canonical Python implementation of magic-wormhole TCP
forwarding (Dilation protocol). The Pi already has Python; `uv` is
how Sourceful tools install Python utilities.

## On the host

```bash
forty-two-watts pair --intent "help me write a goodwe XS driver" --ttl 4h
```

You'll see something like:

```
PAIR CODE: 7-crossover-clockwork
TTL: 4h0m0s — sidecar will exit at expiry
```

Send the code to the friend over any channel — Signal, SMS, Slack DM.

To end the session early:

```bash
forty-two-watts pair --abort
```

…or click **Abort** on the pair-session card in the web UI.

## On the friend

One-time install:

```bash
uv tool install fowl                              # the wormhole transport
go install github.com/frahlg/forty-two-watts/go/cmd/ftw-connect@latest
```

Per session:

```bash
ftw-connect 7-crossover-clockwork
```

That:

1. Opens the wormhole tunnel via `fowl`.
2. Registers an MCP server named `ftw-remote` with Claude Code.
3. Copies a context-priming prompt to the clipboard.

Open Claude Code, paste the prompt, work. When done, **the friend
opens the PR from their own machine** — they clone the 42W repo
locally (Claude does this for them via its own `Bash` tool), apply
the changes they wrote on the owner's instance, and run `gh pr
create` with the `pair-session.md` template.

The owner doesn't touch git or GitHub. They share the wormhole code,
let the friend work, and get a PR link back.

## What the friend gets

A 17-tool MCP surface:

- `ftw_api(method, path, body)` — full 42W HTTP API
- `read_file` / `write_file` / `list_directory` — repo, state dir, /tmp
- `run_command` — shell, same scope
- `restart_main_service` / `tail_service_logs` — systemd, journalctl
- `network_scan` / `http_probe` / `modbus_probe` / `modbus_write` / `mqtt_observe` / `pcap_capture` — LAN-level introspection
- `deploy_driver(name, lua, config)` — write Lua + edit config + reload + verify
- `session_log` / `session_remaining` / `session_end`

## What gets recorded

Every tool call lands in an audit log. `session_log()` renders the log
as markdown for the PR template. The friend pastes it into the PR body
under the *Pair-session report* section; reviewers use it to confirm
what changed on the owner's instance.

## Who does what (owner vs. friend)

| Step | Owner | Friend |
|---|---|---|
| Trigger pair session | `forty-two-watts pair --intent "..."` | — |
| Share wormhole code | Send via Signal/SMS/Slack | Receive |
| Connect | — | `ftw-connect <code>` |
| Develop the driver / debug | — | Drives Claude Code through the tunnel |
| Open the PR | — | Clones repo locally, `gh pr create` from own machine |
| Review the PR | Reviews via GitHub web UI | — |

The owner stays out of git entirely. They don't need a GitHub account,
don't need `gh` installed, don't need to know what a fork is. Their
only job is starting the pair session and (optionally) reviewing the
PR the friend opens.

## Architecture in one paragraph

`forty-two-watts pair` spawns the `ftw-pair` sidecar. The sidecar runs
the 17-tool MCP server on `localhost:9999`, then spawns a `fowld`
subprocess that opens a wormhole tunnel exposing that local port over
the internet. The friend runs `ftw-connect <code>` which spawns another
`fowld` that joins the same tunnel and exposes the other end on the
friend's localhost. Their Claude Code talks to that local port as an
ordinary HTTP MCP server. End-to-end encryption (SPAKE2 key from the
one-time code) protects all traffic; the public wormhole rendezvous
server only ferries the PAKE handshake.

## Limits

- One session at a time.
- 4 h default TTL, configurable, hard kill at expiry.
- Wormhole rendezvous is the upstream public relay
  (`relay.magic-wormhole.io`). Override at sidecar level if you run
  your own.
- No per-call approval. Pairing = full trust for the session.

## Troubleshooting

**`fowld not found on PATH`** — install with `uv tool install fowl`.

**`claude mcp add failed`** — the Claude Code CLI isn't on PATH or
isn't logged in. `ftw-connect` prints the manual command to run.

**Card doesn't appear on the dashboard** — the sidecar posts to
`/api/pair/status` every 5 s; check the main service is reachable on
`localhost:8080` (the sidecar default `-api` flag).
