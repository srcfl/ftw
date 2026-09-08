package backup

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// Windows can reject FlushFileBuffers on a directory opened for reading.
// Archive and extracted file contents are still flushed before publication.
// Directory flush remains best effort here, as in config and state.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("backup: %s is not a directory", dir)
	}
	err = f.Sync()
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		return nil
	}
	return err
}
