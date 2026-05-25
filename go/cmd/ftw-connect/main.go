// ftw-connect is the friend-side CLI that accepts a magic-wormhole
// code, opens the tunnel to a paired forty-two-watts instance, and
// registers the resulting MCP endpoint with Claude Code.
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
		fmt.Printf("ftw-connect %s\n", Version)
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "ftw-connect: not yet implemented")
	os.Exit(1)
}
