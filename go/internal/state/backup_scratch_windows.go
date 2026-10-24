//go:build windows

package state

import (
	"errors"
	"os"
)

// Windows has no flock. A cleanup there cannot tell a stopped backup from a
// running one, so it removes nothing.
func lockBackupScratchDir(string, bool) (*os.File, error) {
	return nil, errors.ErrUnsupported
}
