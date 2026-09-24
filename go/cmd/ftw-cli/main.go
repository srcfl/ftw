// Command ftw-cli is installed on PATH as `ftw`, the operator command for a
// native install. Core itself is releases/<tag>/ftw, started by the launcher.
package main

import (
	"os"

	"github.com/srcfl/ftw/go/internal/ftwcli"
)

func main() {
	os.Exit(ftwcli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
