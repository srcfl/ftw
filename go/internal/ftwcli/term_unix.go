//go:build unix

package ftwcli

import (
	"os"

	"golang.org/x/sys/unix"
)

func terminalWidth(f *os.File) int {
	if ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col >= 40 {
		return int(ws.Col)
	}
	return 80
}
