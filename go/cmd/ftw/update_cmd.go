package main

import (
	"fmt"
	"os"

	"github.com/srcfl/ftw/go/internal/updatecli"
)

func runUpdate(args []string) {
	if err := updatecli.Run(args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ftw:", err)
		os.Exit(1)
	}
}
