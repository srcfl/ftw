package state

import (
	"log/slog"
	"path/filepath"
	"time"
)

// Compaction thresholds. SQLite never returns freed pages to the filesystem
// on its own — after a large prune the file stays at its high-water mark and
// every freed page sits on the freelist. A one-time VACUUM at the next boot
// reclaims that space. Vars (not consts) so tests can lower them.
var (
	// compactMinFreelistBytes: don't bother reclaiming less than this — a
	// VACUUM rewrites the whole file, which is real SD-card wear.
	compactMinFreelistBytes int64 = 64 << 20
	// compactMinFreelistShare: freelist must also be a meaningful share of
	// the file, so a large-but-mostly-live DB isn't rewritten for a sliver.
	compactMinFreelistShare = 0.20
)

// CompactIfBloated runs VACUUM on state.db when a large share of the file is
// freelist pages (typically the boot after the first history prune of a DB
// that grew unpruned). Boot is the only safe window: VACUUM takes an
// exclusive lock for up to minutes on a large file, which would starve the
// control loop's writers at runtime.
//
// Best-effort by design — a failed or skipped VACUUM is logged and ignored
// (VACUUM is transactional; an abort leaves the DB exactly as it was).
func (s *Store) CompactIfBloated() {
	var pageCount, freelist, pageSize int64
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return
	}
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&freelist); err != nil {
		return
	}
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return
	}
	freeBytes := freelist * pageSize
	if freeBytes < compactMinFreelistBytes ||
		float64(freelist) < compactMinFreelistShare*float64(pageCount) {
		return
	}

	// VACUUM rebuilds the live data into a temp file before swapping it in,
	// so it needs roughly the live size free on the same filesystem. Filling
	// the SD card to the brim helps nobody — skip and warn instead.
	liveBytes := (pageCount - freelist) * pageSize
	if avail, err := diskAvail(filepath.Dir(s.mainDBPath)); err == nil && avail < liveBytes+(64<<20) {
		slog.Warn("state: state.db is bloated but disk is too full to VACUUM",
			"reclaimable_mb", freeBytes>>20, "avail_mb", avail>>20, "needed_mb", (liveBytes+(64<<20))>>20)
		return
	}

	t0 := time.Now()
	slog.Info("state: compacting state.db (one-time VACUUM after prune)",
		"file_mb", (pageCount*pageSize)>>20, "reclaimable_mb", freeBytes>>20)
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		slog.Warn("state: VACUUM failed (DB left unchanged)", "err", err)
		return
	}
	// Under WAL the rewritten pages sit in the -wal file until a checkpoint;
	// truncate-checkpoint now so the main file actually shrinks on disk and
	// the (VACUUM-sized) WAL is released immediately.
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		slog.Warn("state: post-VACUUM checkpoint failed", "err", err)
	}
	slog.Info("state: compaction complete",
		"reclaimed_mb", freeBytes>>20, "elapsed", time.Since(t0).Round(time.Millisecond))
}
