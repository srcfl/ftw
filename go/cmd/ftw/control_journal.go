package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/journal"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/protocol"
)

const (
	// controlJournalEvery is how often the journal samples the box's
	// answer to "Are we in control?".
	controlJournalEvery = 5 * time.Second
	// controlJournalKeep is how far back the journal answers.
	controlJournalKeep = 366 * 24 * time.Hour
)

// newControlJournal ends the spans a previous run left open, at the last
// moment that run saw them hold, and returns the recorder for this one.
func newControlJournal(st *state.Store) *journal.Recorder {
	if n, err := st.CloseOpenControlSpans(context.Background()); err != nil {
		slog.Warn("control journal: could not close spans the last run left open", "err", err)
	} else if n > 0 {
		slog.Info("control journal: closed spans the last run left open", "spans", n)
	}
	return journal.New(st)
}

// runControlJournal samples, writes and prunes until ctx ends.
func runControlJournal(ctx context.Context, rec *journal.Recorder, sample func(time.Time) []journal.Verdict, st *state.Store) {
	t := time.NewTicker(controlJournalEvery)
	defer t.Stop()
	var prunedAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			rec.Observe(now, sample(now))
			flushCtx, cancel := context.WithTimeout(ctx, controlJournalEvery)
			rec.Flush(flushCtx, now)
			cancel()
			if now.Sub(prunedAt) < 24*time.Hour {
				continue
			}
			prunedAt = now
			if n, err := st.PruneControlSpans(ctx, now.Add(-controlJournalKeep).UnixMilli()); err != nil {
				slog.Warn("control journal: prune failed", "err", err)
			} else if n > 0 {
				slog.Info("control journal: pruned old spans", "spans", n)
			}
		}
	}
}

// stopControlJournal ends every open span now and writes them, so a clean
// stop leaves no span running through the time the box was down.
func stopControlJournal(rec *journal.Recorder) {
	now := time.Now()
	rec.CloseAll(now)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rec.Flush(ctx, now)
}

// stateJournal serves events.get from the control journal and the
// notification history.
type stateJournal struct{ st *state.Store }

func (j stateJournal) Journal(ctx context.Context, q appproto.JournalQuery) ([]appproto.JournalSpan, []appproto.JournalNotice, bool, error) {
	spans, more, err := j.st.ControlSpans(ctx, state.ControlSpanQuery{
		SinceMs: q.SinceMs, UntilMs: q.UntilMs, Driver: q.Driver, Severities: q.Severities, Limit: q.Limit,
	})
	if err != nil {
		return nil, nil, false, err
	}
	notices, noticesMore, err := j.st.NotificationsBetween(ctx, q.SinceMs, q.UntilMs, q.Limit)
	if err != nil {
		return nil, nil, false, err
	}
	out := make([]appproto.JournalSpan, 0, len(spans))
	for _, sp := range spans {
		out = append(out, appproto.JournalSpan{
			Driver: sp.Driver, Kind: sp.Kind, Status: sp.Status, Reason: sp.Reason, Severity: sp.Severity, Mode: sp.Mode,
			StartMs: sp.StartMs, EndMs: sp.EndMs, Samples: sp.Samples,
			Requested: powerRange(sp.Requested), Actual: powerRange(sp.Actual), Last: sp.Last,
		})
	}
	var ns []appproto.JournalNotice
	for _, n := range notices {
		if q.Driver != "" && n.Driver != q.Driver {
			continue
		}
		ns = append(ns, appproto.JournalNotice{TsMs: n.TsMs, Type: n.EventType, Driver: n.Driver, Status: n.Status, Priority: n.Priority})
	}
	return out, ns, more || noticesMore, nil
}

func powerRange(p state.PowerStats) *protocol.PowerRange {
	mean, ok := p.Mean()
	if !ok {
		return nil
	}
	return &protocol.PowerRange{Min: p.Min, Max: p.Max, Mean: mean}
}
