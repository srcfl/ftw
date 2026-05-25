# `ftw-pair` — MCP bridge to a live 42W instance via magic-wormhole — design
Date: 2026-05-25 Status: draft, awaiting user review
## Why
Driver development, model tuning, and field debugging on forty-two-watts all benefit massively from Claude Code running with hands-on access to the live instance and its surrounding LAN — real Modbus, real MQTT, real telemetry, real drivers reloading against real hardware. Today that only works if you physically are at the site, or if you've set up SSH/Tailscale/portforwarding ahead of time.

Two scenarios this enables:

- **Owner with Claude Code somewhere else** — sitting at a laptop, wants to iterate on a driver against the Pi at home without first wiring up a permanent remote-access path.
  
- **Friend with Claude Code** — owner asks a more experienced user ("can you help me write a driver for my Goodwe?") and wants to hand over time-bound access to their site, without setting up accounts, VPNs, or open ports.
  

The mechanism: an MCP server that wraps the entire 42W surface — HTTP API, filesystem, shell, network introspection, service control — and exposes it over a magic-wormhole tunnel paired by a short human-shareable one-time code, time-bound by default 4 h.

This is not driver-specific infrastructure. The MCP server is for _all of 42W_. Driver development is the first use case; battery-model debugging, MPC tuning, custom report generation, and incident triage are equally valid.
## Architecture
```
   OWNER'S PI                                FRIEND'S LAPTOP
 ┌──────────────────┐                       ┌──────────────────┐
 │ forty-two-watts  │ ←─ HTTP localhost ──  │   ftw-connect    │
 │  (main service)  │                       │  (local bridge)  │
 └──────────────────┘                       └────────┬─────────┘
          ↑                                          │ stdio / HTTP
          │ ftw_api proxies                          │
          │                                          ↓ HTTP/SSE MCP
 ┌──────────────────┐    wormhole tunnel     ┌──────────────────┐
 │     ftw-pair     │ ←═════════════════════→│   Claude Code    │
 │  (MCP sidecar)   │     (TCP forward)      │   (MCP client)   │
 └──────────────────┘                        └──────────────────┘
   bind :9999
   wormhole-william
```

Two new binaries:

- `go/cmd/ftw-pair/` — host-side sidecar. Embeds `psanford/wormhole-william` (pure-Go magic-wormhole). Spawned by `forty-two-watts pair`, a small subcommand on the main binary (same shape as the updater bridge). Talks to the running 42W service via `http://localhost:8080`. Exposes MCP server on `localhost:9999`, then opens a wormhole-forwarded TCP tunnel of that port to the remote side.
  
- `go/cmd/ftw-connect/` — friend-side CLI. Distributed via `go install github.com/sourceful-labs/forty-two-watts/go/cmd/ftw-connect@latest` in v1 (brew tap deferred). Takes a wormhole code, opens the tunnel, TCP-forwards `127.0.0.1:9999` (local) through the wormhole to the host-side `ftw-pair` MCP server, registers that local endpoint with Claude Code via `claude mcp add` (transport: HTTP/SSE, the MCP server itself runs on the host), and copies a context-priming prompt to clipboard.
  

Separation rationale: pairing is opt-in, on-demand, and sometimes-buggy (it has a third-party network dependency on the wormhole rendezvous server and unknown LAN code paths). The main 42W service controls physical energy flow and must keep ticking. Sidecar isolation means a runaway MCP handler, a wormhole library panic, or a recursive tool call cannot stall the control loop.
## MCP tool surface
17 tools. The 42W-API surface is collapsed into a single generic proxy because individually wrapping ~40 endpoints would be noise — `docs/api.md` already documents them, Claude reads it, and any new endpoint is automatically reachable without code changes here.

| Tool | Purpose |
|---|---|
| `ftw_api(method, path, body?)` | Proxy to localhost:8080 — covers all of `docs/api.md` |
| `read_file(path)` | Repo + state-dir scoped |
| `write_file(path, content)` | Same scope |
| `list_directory(path)` | Same scope |
| `run_command(cmd, workdir?)` | Shell, scoped to repo / state / `/tmp` |
| `restart_main_service()` | `systemctl restart forty-two-watts` |
| `tail_service_logs(since?)` | journalctl stream |
| `network_scan()` | ARP sweep — IP + MAC + vendor |
| `modbus_probe(host, port, unit_id, register_range)` | Raw register read |
| `modbus_write(host, port, unit_id, register, value)` | Register exploration writes |
| `mqtt_observe(broker, topic_glob, duration_s)` | Subscribe + dump traffic |
| `http_probe(url)` | Generic GET to LAN devices |
| `pcap_capture(interface, bpf_filter, duration_s)` | Raw pcap blob for protocol analysis |
| `deploy_driver(name, lua_source, config)` | Multi-step: write file + edit config + wait for reload + report status |
| `session_log()` | Full action log as markdown — for PR test report |
| `session_remaining()` | Seconds until timeout |
| `session_end()` | Voluntary early termination |

**Path scoping** — `read_file` / `write_file` / `list_directory` / `run_command(workdir)` accept paths under three roots: the 42W repo checkout on the Pi, the configured `state.dir`, and `/tmp`. Everything else returns an error. The scope is enforced by the sidecar, not by filesystem permissions.

**Structured wrappers vs raw shell** — `deploy_driver` is the one multi-step wrapper because it encodes 42W-specific knowledge: write the `.lua` file, add or update a `config.yaml` entry under `drivers:`, wait for the configreload watcher to pick it up, then return the driver's new health (`tick_count`, `last_error`) so Claude knows whether the deploy actually worked. Other operations are clean combinations of `ftw_api` + `write_file` + `restart_main_service`.
## Session lifecycle
- **Default TTL: 4 h.** Override at pair start: `ftw pair --ttl 2h`.
  
- **One active session at a time.** Concurrent `ftw pair` returns a diagnostic with the running session's ID and the command to abort it.
  
- **Hard deadline.** At expiry the sidecar kills the MCP server and tears down the wormhole tunnel without grace. An in-flight tool call on the friend side will receive a transport error. We accept this — cleaner than negotiating a grace window, and the friend can ask the owner to re-pair.
  
- **Owner-side abort.** Either `ftw pair --abort` from a second terminal, or a button in the 42W web UI.
  
- **In-UI session visibility.** When a session is active, a new card in the web UI shows:
  
  - Owner's stated intent (set via `ftw pair --intent "..."`)
    
  - Time remaining
    
  - Tool-call counter
    
  - Last few tool names (live)
    
  - Abort button
    
  
  Live updates ride the existing SSE stream that already feeds the rest of the dashboard.
  
## Friend-side onboarding
One-time install (v1, brew tap deferred):

```
go install github.com/sourceful-labs/forty-two-watts/go/cmd/ftw-connect@latest
```

Per-session, three steps after receiving a code:

```
$ ftw-connect 7-crossover-clockwork
Connected. MCP server registered as 'ftw-remote'.
Context prompt copied to clipboard.

→ Open Claude Code and paste the prompt.
```

What gets copied to clipboard:

> You are connected to a forty-two-watts instance over an MCP server named `ftw-remote`. The owner wants you to help with:
> 
> > **[owner's intent string]**
> 
> Start by running `ftw_api` with `GET /api/status` to see the current state of the instance. The full HTTP API is documented in the repo at `docs/api.md`. Use `read_file` to read it if you need.
> 
> Everything you do is recorded and will accompany the PR you open at the end. When you're done, call `session_end()` and report what you did to the owner.
> 
> Session timeout is `<remaining>`. If you need more time, the owner can re-pair.

`ftw-connect` exits when the session terminates (timeout, owner abort, or `session_end()` from the friend side), and on exit removes the MCP entry via `claude mcp remove ftw-remote` to avoid stale config.
## Test report / PR flow
`session_log()` returns a markdown document with:

- Session metadata: start/end timestamps, owner's intent, friend's reported identity (if `--as <handle>` was passed to `ftw-connect`), TTL, exit reason.
  
- Chronological tool-call log: each entry shows the tool name, key arguments (paths, hosts, driver names — never raw Lua bodies inline, but referenced by ID), and a one-line outcome (`ok`, `error: ...`, number of bytes returned, etc.).
  
- Telemetry snapshot before/after: `/api/status`, relevant battery / MPC / PV-model state, captured at session start and at `session_end`.
  
- File diff: unified diff of every file written during the session, inline.
  
- Driver deploy events: for each `deploy_driver` call, the full Lua source plus a snippet of the resulting telemetry proving the driver ticked OK.
  

The friend pastes this directly into the body of the PR they open from their own clone of the 42W repo. The PR template (a new `.github/PULL_REQUEST_TEMPLATE/pair-session.md`) will have a "Pair-session report" section calling out where to paste it.
## Security model — explicitly thin
- **Auth.** Wormhole's PAKE handshake from the one-time code is the only authentication. It's eavesdrop-resistant (SPAKE2) and the code burns on first connection. This is equivalent in strength to exchanging a fresh shared session key out-of-band.
  
- **No per-call authorization.** Once paired, every tool is available. The owner has decided to grant access; we don't second-guess them on individual calls.
  
- **Time-bound.** Default 4 h, hard kill at expiry.
  
- **Audit.** `session_log()` is the only audit surface. Stored in the state DB so it survives the sidecar exiting.
  
- **Owner-side kill switch.** `ftw pair --abort` and the UI button.
  
- **No protection against a malicious paired friend.** Out of scope by design. The mental model is "you are trusting this person at SSH level for four hours". If you wouldn't `ssh -A` them in, don't pair with them.
  
- **No protection of the owner's wider network.** The friend can issue Modbus writes, MQTT publishes, and `http_probe` calls against any reachable host on the LAN. Again — same trust level as physical presence.
  
## Phasing
**MVP — this spec:**

- `go/cmd/ftw-pair/` sidecar with the 14-tool surface above
  
- `go/cmd/ftw-connect/` friend CLI
  
- `forty-two-watts pair` subcommand on the main binary
  
- Session-card in the web UI
  
- PR template for pair-session reports
  

**Deferred to follow-up specs:**

- Per-call approval prompts (a "review every write" mode)
  
- Multiple concurrent sessions
  
- Signed session logs (so PR reviewers can verify the report wasn't hand-edited)
  
- Pre-built skill prompts shipping with `ftw-connect` for common scenarios — driver authoring vs model tuning vs incident triage
  
- Brew tap publication (initial release can be `go install`-only)
  
## Out of scope
- Reusing the wormhole tunnel for non-MCP purposes (file transfer outside the tool surface, video streaming, etc.)
  
- Owner-to-owner pairing for clustering (Nova federation already handles that)
  
- Per-tool argument filtering — if a tool exists, the friend can call it with any arguments allowed by the schema
  
- Auditable retention of session logs beyond the running state DB (no cold-storage / S3 mirror in v1)
  
- A bidirectional channel where the owner can inject tool calls into the friend's Claude session
  
## Risks & open questions
- **Wormhole rendezvous server availability.** Default rendezvous is `wss://relay.magic-wormhole.io:4000`, run by the upstream project. An outage breaks pairing. Mitigation: document that the owner can point at their own relay via `ftw pair --rendezvous <ws-url>`; we don't run one ourselves in v1.
  
- **MCP protocol stability.** Claude Code's MCP HTTP transport is still evolving. We pin a working server-side library version and call out the dependency in CI.
  
- **Path-scope enforcement bugs.** A bug that lets `..`-traversal escape the scope is effectively a "remote read/write anywhere" hole during a session. Mitigation: enforce by resolving to canonical paths via `filepath.EvalSymlinks` + a prefix check, with focused unit tests.
  
- **Race between** `deploy_driver` **and the configreload watcher.** The watcher debounces 500 ms; we need to make sure `deploy_driver` waits for the reload to actually settle (read back driver health) before returning, otherwise Claude will see a phantom "ok" and the next test will fail confusingly. Spec'd to wait + verify; flagged here as the most likely source of subtle bugs.
  
- **What "intent" should look like.** The owner is asked to provide a free-form `--intent` string at pair time, threaded into the friend-side context prompt. We're betting that's enough scaffolding; if it turns out friend-side Claude sessions consistently drift, v2 grows a richer pre-session brief format.
