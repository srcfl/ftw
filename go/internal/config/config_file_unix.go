//go:build !windows

package config

import "os"

func createConfigTemp(path string, mode os.FileMode) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
}

func replaceConfigTemp(tmp, path string) error {
	return os.Rename(tmp, path)
}

// syncDir fsyncs a directory so a completed rename survives power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
