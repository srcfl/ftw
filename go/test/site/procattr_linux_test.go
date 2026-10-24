package site

import "syscall"

// childProcAttr puts a child in its own process group and kills it if the
// test binary dies, as when go test's -timeout fires.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
