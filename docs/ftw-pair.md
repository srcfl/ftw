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
session**. The card flips to active mode within a few seconds showing the
friend's **URL** (built from a 6-word token, e.g.
`garage-coffee-river-bicycle-window-cat`) and a prominent **4-digit code**,
with **Copy URL + code** / **Copy code** buttons. Send both to the friend
over any channel — Signal, SMS, Slack DM. They open the URL and type the
code to activate the session; you don't approve anything yourself.

To end the session early, click **Abort** on the card.

Or, if you prefer the terminal:

```bash
forty-two-watts pair --intent "help me write a goodwe XS driver" --ttl 4h
```

The CLI prints the token, URL and code directly:

```
PAIR CODE: garage-coffee-river-bicycle-window-cat
PAIR URL:  https://relay.fortytwowatts.com/h/garage-coffee-river-bicycle-window-cat
APPROVAL CODE (share with the friend): 4827
TTL: 4h0m0s — sidecar will exit at expiry
```

To abort from the terminal:

```bash
forty-two-watts pair --abort
```

## On the friend

**No install.** The host sends the friend a URL of the form:

```
https://relay.fortytwowatts.com/h/garage-coffee-river-bicycle-window-cat
```

...along with the **4-digit code** the dashboard showed. The friend opens
the URL in any modern browser, types the code on the landing page, and
clicks **Activate**. The host doesn't click anything — entering the code
*is* the approval. (The URL and the code together start the session, so the
host shares both; the grant minted below, not the URL, is what guards the
session afterward.)

On the correct code the relay mints a one-time **session grant** and the
page reveals two ready-to-paste blocks:

1. For Claude Code (or any MCP-aware agent) — note the Bearer header
   carrying the grant:
   ```bash
   claude mcp add ftw-friend --transport http \
     https://relay.fortytwowatts.com/h/<token>/mcp \
     --header "Authorization: Bearer <grant>"
   ```
2. For the browser dashboard (the grant rides in an HttpOnly cookie the
   same page sets):
   ```
   https://relay.fortytwowatts.com/h/<token>/web/
   ```

The grant — not the URL — is the access secret from here on, so a URL that
leaks after activation is useless without it. Both blocks are live for the
rest of the TTL (default 4 h). The host can revoke from the dashboard at any
time.

When the work is done, **the friend opens the PR from their own
machine** — they clone the 42W repo locally (the agent does this for
them via its own shell), apply the changes they wrote on the owner's
instance, and run `gh pr create` with the `pair-session.md` template.

The owner doesn't touch git or GitHub. They share the URL + code, let the
friend work, and get a PR link back.

## Relay

The transport uses the Sourceful-operated HTTPS relay at
`relay.fortytwowatts.com`. The host (Pi) opens a long-poll
connection outbound; the relay enqueues friend traffic for the host
to pick up. Friend connects with normal HTTPS — browser, `curl`, or
Claude Code's HTTP MCP transport.

```
       relay.fortytwowatts.com (HTTPS, terminated by CF + ftw-relay)
                      |
          +-----------+-----------+
          |                       |
     outbound long-poll      HTTPS request
          |                       |
    OWNER's Pi              FRIEND's browser /
  (ftw-pair sidecar)        Claude Code
```

The relay terminates TLS and sees plaintext MCP + dashboard traffic.
This is a deliberate trade — the operator runs the relay (or trusts
the operator who does), and end-to-end encryption was protecting
against a threat the help-a-friend flow doesn't actually face. See
`docs/goals/relay-as-tunnel.md` for the security model.

**Token format:** 6 short words. Example:
`garage-coffee-river-bicycle-window-cat`. The token is a routing
key, not an access secret — the 4-digit voice-channel approval is
the actual access gate.

The relay base URL can be overridden with the `-relay` flag or the
`FTW_PAIR_RELAY` environment variable — useful for self-hosted
relays or local development against `http://localhost:7378`.

## What the friend gets

A 17-tool HTTP surface (REST; MCP is also served at `/mcp` for
agents that prefer it). All tool calls run on the **owner's** machine
through the encrypted relay tunnel.

```bash
curl <local-url>/tools                                  # list tools + schemas
curl -X POST <local-url>/tools/<name> -d '<json args>'  # invoke a tool
```

The 17 tools:

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
| Share URL + code | Copy URL + 4-digit code from the dashboard card, send via Signal/SMS/Slack | Receive |
| Connect | — | Open the URL in a browser |
| Activate | Shares the code with the URL — no dashboard click needed | Types the code on the landing page → gets the `claude mcp add` command |
| Develop the driver / debug | — | Drives their agent (Claude Code, Codex, …) through the relay-tunneled MCP / dashboard |
| Open the PR | — | Clones repo locally, `gh pr create` from own machine |
| Review the PR | Reviews via GitHub web UI | — |

The owner stays out of git entirely. They don't need a GitHub account,
don't need `gh` installed, don't need to know what a fork is. Their
only job is starting the pair session and (optionally) reviewing the
PR the friend opens.

## Architecture in one paragraph

`forty-two-watts pair` spawns the `ftw-pair` sidecar. The sidecar runs
a 17-tool HTTP surface on `localhost:9999` (REST at `/tools/<name>` +
MCP at `/mcp`, sharing the same Tool[] and audit log), then registers
a 6-word token with the HTTPS relay at `relay.fortytwowatts.com` and
starts a long-poll loop. When a friend opens the URL, the landing page
asks for the 4-digit code the host shared; the friend types it and the
relay mints a one-time session grant. From then on the friend's requests
carry that grant (Bearer header for MCP, HttpOnly cookie for the
dashboard) and the relay forwards them to the host: `/h/<token>/mcp` lands
on the local MCP server, `/h/<token>/web/...` proxies to the dashboard at
`localhost:8080`. The friend's machine never installs anything —
their agent uses Claude Code's `--transport http` directly against
the relay URL.

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
- Relay: `relay.fortytwowatts.com` (Sourceful-operated). Override
  with `-relay` flag or `FTW_PAIR_RELAY` env var for self-hosted relays
  or local development.
- No per-call approval after the initial connect-approval. Pairing =
  full trust for the session duration.

## Troubleshooting

**`register with relay: ...`** — the relay isn't reachable from the
host. Check the host can `curl https://relay.fortytwowatts.com/healthz`.
Use `-relay http://localhost:7378` to point at a local relay for testing.

**Friend gets `425 Too Early`** — the session hasn't been activated yet.
The friend must open the URL and type the 4-digit code the host shared;
that mints the grant. Until then, MCP and web paths return 425. (A `401`
instead means the request reached an active session without the grant —
re-copy the `claude mcp add` line, including its `Authorization: Bearer`
header.)

**Agent can't reach the dashboard** — confirm the host's `ftw-pair`
process is still running and the long-poll loop is healthy. `curl
https://relay.fortytwowatts.com/h/<token>/web/api/status` from any
shell to test directly; you should get a JSON payload from the
dashboard.

**Card doesn't appear on the dashboard** — the sidecar posts to
`/api/pair/status` every 5 s; check the main service is reachable on
`localhost:8080` (the sidecar default `-api` flag).

## Self-hosting the relay

The `ftw-relay` binary is published as a release asset for
`linux/amd64` + `linux/arm64`. Full operator runbook (Cloudflare
Origin Certificate, DNS, systemd hardening) is in
`docs/relay-deploy.md`.

Quick local-development version:

```bash
# Build from source
cd go && go build -o ../bin/ftw-relay ./cmd/ftw-relay

# Run on plain HTTP (no TLS — fine for localhost only)
./bin/ftw-relay -addr :7378

# Point the host at it
FTW_PAIR_RELAY=http://localhost:7378 forty-two-watts pair --intent "..."

# Friend just opens the URL the host prints:
#   http://localhost:7378/h/<token>
```

For production use add `-cert /path/to/cert.pem -key /path/to/key.pem`;
the binary then serves HTTPS directly. Most deployments instead front
it with Cloudflare proxy + a Cloudflare Origin Certificate — see
`docs/relay-deploy.md` for the full setup.
