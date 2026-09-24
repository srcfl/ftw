//go:build !unix

package ftwcli

import "os"

func terminalWidth(*os.File) int { return 80 }
