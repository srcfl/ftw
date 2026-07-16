//go:build unix

package state

import "syscall"

// diskAvail returns the bytes available to unprivileged users on the
// filesystem containing dir.
func diskAvail(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
