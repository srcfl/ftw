//go:build windows

package main

import "os/exec"

// setPairSysProcAttr is a no-op on Windows: SysProcAttr.Setpgid does not
// exist in the Windows syscall package. The ftw-pair sidecar is a
// Linux-only binary (uses journalctl, /proc/net/arp, etc.) and is not
// shipped in the Windows release archive, so this path is never exercised
// in practice. The stub exists only so that the ftw Windows
// .exe compiles cleanly from a single module.
//
// If you ever invoke `ftw pair` on Windows you will get a
// "ftw-pair binary not found" error from runPair before this function
// is ever called.
func setPairSysProcAttr(_ *exec.Cmd) {}
