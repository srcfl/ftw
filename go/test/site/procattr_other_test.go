//go:build unix && !linux

package site

import "syscall"

// childProcAttr puts a child in its own process group. Without Pdeathsig a
// child outlives a test binary that dies; stop it by its run directory.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
