//go:build unix

package state

import (
	"os"
	"syscall"
)

// lockBackupScratchDir locks dir itself, so no lock file is left behind. A
// backup waits for a shared lock while a cleanup holds it; a cleanup does not
// wait for a backup.
func lockBackupScratchDir(dir string, exclusive bool) (*os.File, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX | syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, &os.PathError{Op: "flock", Path: dir, Err: err}
	}
	return f, nil
}
