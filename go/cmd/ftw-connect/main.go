// ftw-connect is the friend-side CLI that turns a 6-word relay token into
// a live local HTTP endpoint an AI agent (Claude Code, Codex, Gemini, etc.)
// can drive over Bash + curl.
//
// Usage:
//
//	ftw-connect [flags] <token>
//
// Example:
//
//	ftw-connect garage-coffee-river-bicycle-window-cat
//
// On success it:
//  1. connects to the Sourceful relay with the given token,
//  2. exposes a local HTTP port forwarded to the host's sidecar through
//     the end-to-end-encrypted relay tunnel,
//  3. prints the local URL and copies a ready-to-paste agent prompt to
//     the clipboard,
//  4. blocks until the tunnel closes (TTL, owner abort, ^C).
//
// We deliberately do NOT touch the friend's agent CLI config (no
// `claude mcp add`, no Codex config writes, nothing). The agent is told
// in its prompt to talk to the local URL with plain HTTP — that keeps
// ftw-connect tool-agnostic and leaves zero traces on the friend's disk.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/frahlg/forty-two-watts/go/internal/subetha"
)

var Version = "dev"

func main() {
	version := flag.Bool("version", false, "print version and exit")
	relayAddr := flag.String("relay-addr", "", "Relay server address (overrides FTW_PAIR_RELAY env var and default subetha.fortytwowatts.com:7777)")
	flag.Parse()

	if *version {
		fmt.Printf("ftw-connect %s\n", Version)
		os.Exit(0)
	}
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: ftw-connect [flags] <token>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "flags:")
		flag.PrintDefaults()
		os.Exit(2)
	}
	code := flag.Arg(0)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	effectiveRelay := subetha.RelayAddr(*relayAddr)
	fmt.Printf("Connecting to relay (%s)...\n", effectiveRelay)

	client, err := subetha.Connect(ctx, code, subetha.WithRelayAddr(*relayAddr))
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay connect: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	baseURL := "http://" + client.LocalAddr
	fmt.Printf("Tunnel ready: %s\n", baseURL)

	prompt := buildPrompt(baseURL)
	clipboardOK := copyClipboard(prompt) == nil

	// Always print the prompt to stderr inside fenced markers so the user can
	// scroll up and copy it manually if the clipboard didn't take.
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "──────────────────── BEGIN AGENT PROMPT ────────────────────")
	fmt.Fprintln(os.Stderr, prompt)
	fmt.Fprintln(os.Stderr, "───────────────────── END AGENT PROMPT ─────────────────────")
	fmt.Fprintln(os.Stderr, "")
	if clipboardOK {
		fmt.Println("Prompt above is also copied to your clipboard — paste it into Claude Code / Codex / your agent of choice.")
	} else {
		fmt.Println("Clipboard copy failed — copy the prompt above manually.")
	}

	fmt.Println("Tunnel open. Ctrl-C to disconnect.")
	<-ctx.Done()
}

func buildPrompt(baseURL string) string {
	return `You are connected to a live forty-two-watts (42W) home-energy system via a local HTTP API.

LOCAL API: ` + baseURL + `

You're helping the owner remotely. The owner is *not* expected to know git or GitHub — **you** open the PR at the end, from your own machine, not theirs. The owner's role is to share their site with you and accept your help; you handle the development.

## How to call the API

Discover the catalog:
  curl ` + baseURL + `/tools

Call a tool (JSON args in, JSON result out):
  curl -X POST ` + baseURL + `/tools/<name> \
    -H 'Content-Type: application/json' \
    -d '<json args>'

Status codes: 200 = ok, 400 = bad request, 404 = unknown tool, 502 = tool errored (body has ` + "`{\"error\":...}`" + `).

## First, orient yourself

Run these in order on your first turn:

1. ` + "`curl " + baseURL + `/tools` + "`" + ` — list available tools and their schemas (17 tools).
2. ` + "`curl -X POST " + baseURL + `/tools/ftw_api -H 'Content-Type: application/json' -d '{"method":"GET","path":"/api/pair/status"}'` + "`" + ` — reads the owner's stated intent for this session and the time remaining.
3. ` + "`curl -X POST " + baseURL + `/tools/ftw_api -d '{"method":"GET","path":"/api/status"}'` + "`" + ` — shows the running state of the instance (drivers, mode, grid/PV/battery readings).

Tell the owner in chat what you found so they can confirm the plan before you start making changes.

## What the tools can do (all run on the owner's machine through the tunnel)

- ` + "`ftw_api`" + ` — proxy to the running 42W HTTP API (see docs/api.md)
- ` + "`read_file`" + ` / ` + "`write_file`" + ` / ` + "`list_directory`" + ` — scoped to the owner's repo, state dir, and /tmp
- ` + "`run_command`" + ` — shell on the owner's machine, same scope, 30s default timeout
- ` + "`restart_main_service`" + ` / ` + "`tail_service_logs`" + ` — restart the owner's service, read recent logs
- ` + "`network_scan`" + ` / ` + "`http_probe`" + ` / ` + "`modbus_probe`" + ` / ` + "`modbus_write`" + ` / ` + "`mqtt_observe`" + ` / ` + "`pcap_capture`" + ` — LAN-level introspection from the owner's machine
- ` + "`deploy_driver`" + ` — write a Lua driver, update config.yaml, wait for reload, verify it ticks against the owner's hardware
- ` + "`session_log`" + ` / ` + "`session_remaining`" + ` / ` + "`session_end`" + ` — session controls

You also have your *own* local tools (your editor, file system, shell) — that's how you'll prepare and submit the PR.

## When the work is done — opening the PR

The driver source lives on the owner's machine after you write it there. To turn that into a PR from your own machine:

1. **Snapshot the final state.** Call ` + "`read_file`" + ` on every file you modified on the owner's machine, so you have the canonical text in this conversation. Also call ` + "`session_log`" + ` once to get the audit-log markdown.
2. **Clone the repo locally** if you haven't already: ` + "`git clone https://github.com/frahlg/forty-two-watts.git /tmp/ftw-work`" + ` (use your *local* shell, not the ` + "`run_command`" + ` tool).
3. **Apply the changes** to that local clone — drop the driver file into ` + "`drivers/`" + `, edit ` + "`config.yaml`" + ` to add the driver entry, etc. Match what's on the owner's machine.
4. **Open the PR** with ` + "`gh pr create`" + ` against ` + "`master`" + `, picking the ` + "`pair-session.md`" + ` template and pasting the session-log markdown into the *Pair-session report* section. Use a ` + "`feat(driver): ...`" + ` style title.
5. **Tell the owner** in chat: link to the PR, what was changed, what you'd like them to test, anything unexpected they should know.
6. **Call ` + "`session_end`" + `** to close the tunnel. The owner's sidecar exits.

## Boundaries

- Trust level is "ssh-equivalent for the duration of this session". Be respectful of the owner's site.
- Modbus writes and ` + "`deploy_driver`" + ` calls touch real hardware. Confirm with the owner in chat before doing anything that could move energy.
- Everything you do is recorded; the owner sees the audit log in the PR you open.
`
}
