package backup

import (
	"io/fs"
	"strings"

	"github.com/srcfl/ftw/go/internal/state"
)

// RemoveStaleScratch removes what stopped backups left in outputDir and in
// /dev/shm. Core calls it at start, before it accepts a backup request.
func RemoveStaleScratch(outputDir string) {
	state.RemoveStaleBackupScratchInShm()
	state.RemoveStaleBackupScratch(outputDir, isScratch)
}

// isScratch matches the stage and verification folders and the unpublished
// archive that Create and verification keep beside the backups.
func isScratch(entry fs.DirEntry) bool {
	name := entry.Name()
	if entry.IsDir() {
		return state.IsTempName(name, ".ftw-backup-stage-") || state.IsTempName(name, ".ftw-backup-verify-")
	}
	return entry.Type().IsRegular() && strings.HasPrefix(name, ".ftw-full-backup-") && strings.HasSuffix(name, ".ftwbak.tmp")
}
