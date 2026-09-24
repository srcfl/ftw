// Command ftwupdate is installed on PATH as `ftw`. It only runs updates.
// Starting Core is the long-running ftw binary, not this command.
package main

import (
	"fmt"
	"os"

	"github.com/srcfl/ftw/go/internal/updatecli"
)

func main() {
	if err := updatecli.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ftw:", err)
		os.Exit(1)
	}
}
