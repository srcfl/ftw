# `ftw-pair` — handing over a 42W instance temporarily

`ftw-pair` lets you grant time-bound MCP access to a running
forty-two-watts instance over an encrypted relay tunnel. The recipient's
Claude Code gets a curated tool surface for driver development, model
tuning, or live debugging.

## When to use it

- A friend with deeper experience offers to help you write a driver.
- You're away from the instance but want to iterate against it from
  Claude Code on a laptop somewhere else.
- An incident needs eyes-on access from someone other than the operator.

The trust level is "ssh-equivalent for the session duration". Only
pair with people you'd already give shell to.

## On the host

Open the dashboard at `http://<pi>:8080`, scroll to the **Pair session**
card, fill in an optional intent and pick a TTL, then click **Start pair
session**. The card flips to active mode within a few seconds showing a
**6-word token** (e.g. `garage-coffee-river-bicycle-window-cat`) with a
**Copy** button. Copy the token and send it to the friend over any channel
— Signal, SMS, Slack DM.

To end the session early, click **Abort** on the card.

Or, if you prefer the terminal:

```bash
forty-two-watts pair --intent "help me write a goodwe XS driver" --ttl 4h
```

The CLI prints the token directly:

```
PAIR CODE: garage-coffee-river-bicycle-window-cat
TTL: 4h0m0s — sidecar will exit at expiry
```

To abort from the terminal:

```bash
forty-two-watts pair --abort
```

## On the friend

One-time install (Mac or Linux):

```bash
curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/scripts/install-ftw-connect.sh | bash
```

The curl installer detects your OS and CPU architecture, downloads the
right binary from the latest GitHub release, and installs it to
`/usr/local/bin` (or `~/.local/bin` as a fallback when `/usr/local/bin`
requires elevated permissions).

Developer / Go toolchain alternative:

```bash
go install github.com/frahlg/forty-two-watts/go/cmd/ftw-connect@latest
```

Per session:

```bash
ftw-connect garage-coffee-river-bicycle-window-cat
```

That:

1. Connects to the Sourceful relay with the given token.
2. Registers an MCP server named `ftw-remote` with Claude Code.
3. Copies a context-priming prompt to the clipboard.

Open Claude Code, paste the prompt, work. When done, **the friend
opens the PR from their own machine** — they clone the 42W repo
locally (Claude does this for them via its own `Bash` tool), apply
the changes they wrote on the owner's instance, and run `gh pr
create` with the `pair-session.md` template.

The owner doesn't touch git or GitHub. They share the token,
let the friend work, and get a PR link back.

## Relay

The transport uses a Sourceful-operated relay server at
`pair-relay.sourceful.energy:7777`. Both the host (Pi) and the friend
connect outbound to the relay; the relay matches them by their shared
6-word token and splices the TCP streams bidirectionally.

```
       pair-relay.sourceful.energy:7777
                      |
          +-----------+-----------+
          |                       |
       outbound                outbound
          |                       |
    OWNER's Pi              FRIEND's Mac
  (ftw-pair sidecar)       (ftw-connect)
```

All traffic is end-to-end encrypted with **ChaCha20-Poly1305 AEAD**
keyed from the token via HKDF-SHA256. The relay sees only ciphertext and
token-match strings; it cannot read or modify the MCP payload.

**Token format:** 6 random words from the BIP39 English wordlist
(2048 words, ~66 bits of entropy). Example:
`garage-coffee-river-bicycle-window-cat`.

The relay address can be overridden with the `-relay-addr` flag or the
`FTW_PAIR_RELAY` environment variable — useful for self-hosted relays or
local testing.

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
| Trigger pair session | Click **Start pair session** in the dashboard (or `forty-two-watts pair --intent "..."`) | — |
| Share token | Copy from the dashboard card, send via Signal/SMS/Slack | Receive |
| Connect | — | `ftw-connect <token>` |
| Develop the driver / debug | — | Drives Claude Code through the tunnel |
| Open the PR | — | Clones repo locally, `gh pr create` from own machine |
| Review the PR | Reviews via GitHub web UI | — |

The owner stays out of git entirely. They don't need a GitHub account,
don't need `gh` installed, don't need to know what a fork is. Their
only job is starting the pair session and (optionally) reviewing the
PR the friend opens.

## Architecture in one paragraph

`forty-two-watts pair` spawns the `ftw-pair` sidecar. The sidecar runs
the 17-tool MCP server on `localhost:9999`, then connects to the
Sourceful relay (`pair-relay.sourceful.energy:7777`) using a randomly
generated 6-word token. The relay holds the connection until the
friend connects with the same token, then splices the two TCP streams.
All bytes are ChaCha20-Poly1305 encrypted end-to-end using HKDF-derived
keys from the shared token — the relay sees only ciphertext. The friend
runs `ftw-connect <token>` which opens a local port on their machine;
their Claude Code talks to that port as an ordinary HTTP MCP server.

## When the work is done — driver persistence

When the friend calls `deploy_driver` during a session, the Lua file is
written to `/app/data/drivers/` (the persistent volume) rather than the
image-bundled `/app/drivers/`. This means:

- **The driver survives `docker compose pull` / image updates.** The
  `/app/data/` volume bind-mount (`./data:/app/data` in
  docker-compose.yml) persists across every image upgrade. Drivers added
  during pair sessions stay loaded without any extra action from the
  owner.

- **User drivers shadow bundled drivers by name.** If a file named
  `ferroamp.lua` exists in `/app/data/drivers/`, it takes precedence
  over the same-named file in `/app/drivers/`. This lets an operator
  test a patched version of a bundled driver without waiting for an
  upstream release.

The PR that the friend opens after the session is for sharing the driver
with the broader user base so it ships in future image builds. The
owner's running instance already has the local copy and keeps it
regardless of whether the PR is merged.

## Limits

- One session at a time.
- 4 h default TTL, configurable, hard kill at expiry.
- Relay: `pair-relay.sourceful.energy:7777` (Sourceful-operated). Override
  with `-relay-addr` flag or `FTW_PAIR_RELAY` env var for self-hosted relays.
- No per-call approval. Pairing = full trust for the session.

## Troubleshooting

**`relay connect: dial relay pair-relay.sourceful.energy:7777: ...`** —
the relay is not yet deployed in this environment, or network access is
blocked. Use `-relay-addr` to point at a local relay for testing.

**`claude mcp add failed`** — the Claude Code CLI isn't on PATH or
isn't logged in. `ftw-connect` prints the manual command to run.

**Card doesn't appear on the dashboard** — the sidecar posts to
`/api/pair/status` every 5 s; check the main service is reachable on
`localhost:8080` (the sidecar default `-api` flag).

## Self-hosting the relay

The `ftw-pair-relay` binary is published as a release asset alongside
`ftw-connect`. To run your own relay:

```bash
# Download from the latest release
curl -fsSL .../ftw-pair-relay-linux-amd64 -o ftw-pair-relay
chmod +x ftw-pair-relay

# Run (plain TCP; add -tls-cert / -tls-key for TLS)
./ftw-pair-relay -addr :7777

# Point host + friend at your relay
FTW_PAIR_RELAY=myrelay.example.com:7777 forty-two-watts pair --intent "..."
FTW_PAIR_RELAY=myrelay.example.com:7777 ftw-connect <token>
```
