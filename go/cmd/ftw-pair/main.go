// ftw-pair is the host-side sidecar that exposes a forty-two-watts
// instance as an MCP server over a magic-wormhole tunnel.
//
// Spawned by `forty-two-watts pair`. Talks to the running main
// service via http://localhost:8080. Exposes MCP on :9999, forwarded
// through wormhole to the friend's laptop.
//
// Lifecycle: TTL-bound (default 4h). Hard kill at expiry. One active
// session per host.
package main

import (
	"flag"
	"fmt"
	"os"
)

var Version = "dev"

func main() {
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Printf("ftw-pair %s\n", Version)
		os.Exit(0)
	}
	// real entry point implemented in later tasks
	fmt.Fprintln(os.Stderr, "ftw-pair: not yet implemented")
	os.Exit(1)
}
