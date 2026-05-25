// ftw-connect is the friend-side CLI that turns a magic-wormhole code
// into a live MCP endpoint Claude Code can talk to.
//
// Usage:
//
//	ftw-connect 7-crossover-clockwork
//
// On success it:
//  1. opens the wormhole tunnel via the fowl subprocess wrapper,
//  2. exposes a local TCP port forwarded to the host's MCP server,
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
	"strings"
	"syscall"

	wh "github.com/frahlg/forty-two-watts/go/internal/wormhole"
)

var Version = "dev"

const mcpName = "ftw-remote"

func main() {
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Printf("ftw-connect %s\n", Version)
		os.Exit(0)
	}
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: ftw-connect <wormhole-code>")
		os.Exit(2)
	}
	code := flag.Arg(0)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Printf("Connecting to %s...\n", code)
	client, err := wh.Connect(ctx, code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wormhole connect: %v\n", err)
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
	var b strings.Builder
	b.WriteString("You are connected to a forty-two-watts instance over the MCP server `")
	b.WriteString(mcpName)
	b.WriteString("`. The owner wants you to help with:\n\n> ")
	b.WriteString(intent)
	b.WriteString("\n\nStart with `ftw_api` GET `/api/status` to see the current state. The full HTTP API is documented in the repo at `docs/api.md` — `read_file` it if you need.\n\nEverything you do is recorded and will accompany the PR you open. When done, call `session_end` and report back to the owner.\n\nSession timeout: ")
	b.WriteString(ttl)
	b.WriteString(". If you need more time, the owner can re-pair.\n")
	return b.String()
}
