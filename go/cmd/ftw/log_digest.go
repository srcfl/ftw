package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const (
	// logDigestEvery is how often warning counts reach the store. A crash
	// loses at most this much of the digest.
	logDigestEvery = time.Minute
	// logDigestKeep covers the support file's week with a day to spare.
	logDigestKeep = 8 * 24 * time.Hour
)

// runLogDigest writes the log ring's warning and error counts to the store
// until ctx ends, then writes once more.
func runLogDigest(ctx context.Context, ring *telemetry.LogRing, st *state.Store) {
	t := time.NewTicker(logDigestEvery)
	defer t.Stop()
	var prunedAt time.Time
	for {
		select {
		case <-ctx.Done():
			flushLogDigest(context.Background(), ring, st, time.Now())
			return
		case now := <-t.C:
			flushLogDigest(ctx, ring, st, now)
			if now.Sub(prunedAt) < 24*time.Hour {
				continue
			}
			prunedAt = now
			if _, err := st.PruneLogDigest(ctx, now.Add(-logDigestKeep).UnixMilli()); err != nil {
				slog.Warn("log digest: prune failed", "err", err)
			}
		}
	}
}

func flushLogDigest(ctx context.Context, ring *telemetry.LogRing, st *state.Store, now time.Time) {
	entries := ring.TakeDigest(now)
	if len(entries) == 0 {
		return
	}
	rows := make([]state.LogDigest, len(entries))
	for i, e := range entries {
		rows[i] = state.LogDigest{
			HourMs: e.Hour.UnixMilli(), Level: e.Level, Driver: e.Driver, Pattern: e.Pattern,
			Msg: e.Msg, Attrs: e.Attrs, Count: int64(e.Count),
			FirstMs: e.First.UnixMilli(), LastMs: e.Last.UnixMilli(),
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := st.AddLogDigest(ctx, rows); err != nil {
		slog.Warn("log digest: write failed", "rows", len(rows), "err", err)
	}
}
