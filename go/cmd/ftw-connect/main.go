// ftw-connect is the friend-side CLI that turns a 6-word relay token into
// a live MCP endpoint Claude Code can talk to.
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
//  2. exposes a local TCP port forwarded to the host's MCP server
//     through the end-to-end encrypted relay tunnel,
//  3. registers that port with Claude Code (`claude mcp add ...`),
//  4. copies a context prompt to the clipboard,
//  5. blocks until the tunnel closes (TTL, owner abort, ^C).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/frahlg/forty-two-watts/go/internal/subetha"
)

var Version = "dev"

const mcpName = "ftw-remote"

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

	mcpURL := "http://" + client.LocalAddr + "/mcp"
	fmt.Printf("Tunnel ready: %s\n", mcpURL)

	// Best-effort registration with Claude Code.
	if err := exec.Command("claude", "mcp", "add", mcpName, mcpURL, "--transport", "http").Run(); err != nil {
		fmt.Fprintf(os.Stderr, "claude mcp add failed: %v — register manually with:\n  claude mcp add %s %s\n", err, mcpName, mcpURL)
	} else {
		fmt.Printf("Registered MCP server '%s' with Claude Code.\n", mcpName)
	}
	defer func() {
		_ = exec.Command("claude", "mcp", "remove", mcpName).Run()
	}()

	prompt := buildPrompt("(see owner's message)", "(see session_remaining)")
	if err := copyClipboard(prompt); err != nil {
		fmt.Fprintf(os.Stderr, "clipboard copy failed: %v — paste manually:\n\n%s\n", err, prompt)
	} else {
		fmt.Printf("Context prompt copied to clipboard. Paste it into Claude Code now.\n")
	}

	fmt.Println("Tunnel open. Ctrl-C to disconnect.")
	<-ctx.Done()
}

func buildPrompt(intent, ttl string) string {
	// Intent and ttl are placeholders — the prompt instructs Claude to fetch
	// them via ftw_api on its first turn.
	_ = intent
	_ = ttl
	return `You are connected to a live forty-two-watts (42W) instance over the MCP server ` + "`" + mcpName + "`" + `.

You're helping the owner remotely. The owner is *not* expected to know git or GitHub — **you** open the PR at the end, from your own machine, not theirs. The owner's role here is to share their site with you and accept your help; you handle the development.

## First, orient yourself

Run these in order on your first turn:

1. ` + "`ftw_api`" + ` with ` + "`method: GET, path: /api/pair/status`" + ` — reads the owner's stated intent for this session and the time remaining.
2. ` + "`ftw_api`" + ` with ` + "`method: GET, path: /api/status`" + ` — shows the running state of the instance (drivers, mode, grid/PV/battery readings).
3. ` + "`read_file`" + ` at ` + "`/app/docs/api.md`" + ` (or wherever the repo is mounted — try ` + "`list_directory`" + ` from ` + "`/app`" + ` first) if you need a catalog of HTTP endpoints.

Tell the owner in chat what you found so they can confirm the plan before you start making changes.

## Available MCP tools (17 — these run *on the owner's machine* through the tunnel)

- ` + "`ftw_api(method, path, body?)`" + ` — proxy to the running 42W HTTP API (see docs/api.md)
- ` + "`read_file`" + ` / ` + "`write_file`" + ` / ` + "`list_directory`" + ` — scoped to the owner's repo, state dir, and /tmp
- ` + "`run_command(cmd, workdir)`" + ` — shell on the owner's machine, same scope, 30s default timeout
- ` + "`restart_main_service`" + ` / ` + "`tail_service_logs`" + ` — restart the owner's service, read recent logs
- ` + "`network_scan`" + ` / ` + "`http_probe`" + ` / ` + "`modbus_probe`" + ` / ` + "`modbus_write`" + ` / ` + "`mqtt_observe`" + ` / ` + "`pcap_capture`" + ` — LAN-level introspection from the owner's machine
- ` + "`deploy_driver(name, lua_source, config)`" + ` — write a Lua driver, update config.yaml, wait for reload, verify it ticks against the owner's hardware
- ` + "`session_log`" + ` / ` + "`session_remaining`" + ` / ` + "`session_end`" + ` — session controls

You also have your *own* local tools (Read/Write/Edit/Bash on your local filesystem) — those are how you'll prepare and submit the PR.

## When the work is done — opening the PR

The driver source lives on the owner's machine after you ` + "`write_file`" + ` it there. To turn that into a PR from your own machine:

1. **Snapshot the final state.** Call ` + "`read_file`" + ` on every file you modified on the owner's machine, so you have the canonical text in this conversation. Also call ` + "`session_log`" + ` once to get the audit-log markdown.
2. **Clone the repo locally** if you haven't already: ` + "`git clone https://github.com/frahlg/forty-two-watts.git /tmp/ftw-work`" + ` (use your local Bash tool, not ` + "`run_command`" + `).
3. **Apply the changes** to that local clone using your local Write tool — drop the driver file into ` + "`drivers/`" + `, edit ` + "`config.yaml`" + ` to add the driver entry, etc. Match what's on the owner's machine.
4. **Open the PR** with ` + "`gh pr create`" + ` against ` + "`master`" + `, picking the ` + "`pair-session.md`" + ` template and pasting the session-log markdown into the *Pair-session report* section. Use a ` + "`feat(driver): ...`" + ` style title.
5. **Tell the owner** in chat: link to the PR, what was changed, what you'd like them to test, anything unexpected they should know.
6. **Call ` + "`session_end`" + `** to close the tunnel. The owner's sidecar exits.

## Boundaries

- Trust level is "ssh-equivalent for the duration of this session". Be respectful of the owner's site.
- Modbus writes and ` + "`deploy_driver`" + ` calls touch real hardware. Confirm with the owner in chat before doing anything that could move energy.
- Everything you do is recorded; the owner sees the audit log in the PR you open.
`
}
