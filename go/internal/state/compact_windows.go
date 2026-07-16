//go:build windows

package state

import "errors"

// diskAvail is unsupported on Windows; the caller treats an error as
// "unknown" and proceeds (VACUUM aborts transactionally if disk fills).
func diskAvail(dir string) (int64, error) {
	return 0, errors.New("diskAvail: unsupported on windows")
}
