package nativeupdate

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// FreeBytes reports the space available to Core on the file system that
// holds the install root.
func (m Manager) FreeBytes() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.Root, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// SpaceNeeded is the free space a download of the next release needs: its
// archive and its unpacked tree, each no larger than the current release,
// and the same again so the data on that disk keeps room to write.
func (m Manager) SpaceNeeded(current string) (int64, error) {
	dir, err := m.ReleaseDir(current)
	if err != nil {
		return 0, err
	}
	var size int64
	err = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		info, err := entry.Info()
		if err == nil {
			size += info.Size()
		}
		return err
	})
	return 3 * size, err
}

// CheckSpace refuses a download that would fill the disk. State and history
// usually share it, and Core stops recording and controlling safely when
// they cannot be written.
func (m Manager) CheckSpace(current string) error {
	need, err := m.SpaceNeeded(current)
	if err != nil {
		return err
	}
	free, err := m.FreeBytes()
	if err != nil {
		return err
	}
	if free < need {
		return fmt.Errorf("not enough disk space in %s for the next release: %d MB free, %d MB needed",
			m.Root, free/1_000_000, need/1_000_000)
	}
	return nil
}

// removeLeftovers deletes what an interrupted download or unpack left: files
// in the download directory and release stages older than an hour. One
// update runs at a time, so nothing current is in either place.
func (m Manager) removeLeftovers(downloads string) {
	if entries, err := os.ReadDir(downloads); err == nil {
		for _, entry := range entries {
			_ = os.RemoveAll(filepath.Join(downloads, entry.Name()))
		}
	}
	releases := filepath.Join(m.Root, "releases")
	entries, err := os.ReadDir(releases)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".stage-") {
			continue
		}
		if info, err := entry.Info(); err == nil && m.now().Sub(info.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(releases, entry.Name()))
		}
	}
}
