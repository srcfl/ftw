//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setPairSysProcAttr sets Setpgid=true so that ftw-pair runs in its own
// process group. A Ctrl-C (SIGINT) delivered to the forty-two-watts parent
// does not propagate to the sidecar — the sidecar keeps running until the
// session TTL expires or the operator sends --abort.
func setPairSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
