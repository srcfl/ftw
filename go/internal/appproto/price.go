package appproto

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Prices: the box's side of the price window.
//
// The app has no HTTP origin — it reaches the box only over the session — so
// prices cross the wire like everything else. Two things happen here and
// nowhere else. Money becomes integer minor units, rounded once: the store
// keeps floats, the wire carries öre and cents, and rounding a second time
// somewhere else is how two screens start disagreeing about what 18.7 öre is.
// And an answer that does not cover the window asked for says so — whether it
// starts after the beginning, stops short of the end, or has a hole in the
// middle — because the app draws what it is given and would otherwise draw the
// gap as a market where prices simply stopped.

// maxPriceWindowMs bounds how much of the store one query may read.
//
// The prices table is never pruned — Prune ages the history tiers, not this —
// so it grows by about a hundred rows a day for the life of the box. A query
// with no upper bound is therefore a scan over years of rows, and on a Zap the
// memory that costs is memory local control needs. Eight days is more than
// anything asks for: a week of history and tomorrow's rates.
//
// This needs nobody to be malicious. The app derives its window from the
// phone's clock, so a device whose clock is wrong asks from 1970.
const maxPriceWindowMs = 8 * 24 * 60 * 60 * 1000

// priceFrom projects stored prices onto the wire.
//
// fromMs and toMs are the window the caller asked for, and are what staleness
// is judged against: tomorrow's rates arrive in the afternoon, so a window
// asked for at breakfast genuinely ends early.
//
// Staleness covers the whole window, both edges and the middle — see below.
//
// vatPercent is the rate the box applies to the totals. It becomes integer
// permille here, once, by the same rule as the money: every rate in use has at
// most one decimal in percent, so 25.5 % crosses as 255 and nothing is lost.
func priceFrom(zone, currency string, vatPercent float64, points []PricePoint, fromMs, toMs int64) Price {
	out := Price{
		Zone:        zone,
		Currency:    currency,
		Slots:       make([]PriceSlot, 0, len(points)),
		VATPermille: int64(math.Round(vatPercent * 10)),
	}

	for _, p := range points {
		out.Slots = append(out.Slots, PriceSlot{
			StartMs:    p.StartMs,
			DurationMs: int64(p.LenMin) * int64(time.Minute/time.Millisecond),
			SpotMinor:  int64(math.Round(p.SpotMinor)),
			TotalMinor: int64(math.Round(p.TotalMinor)),
		})
	}

	// An answer that spans the window from end to end with no hole in it is
	// complete; anything else says so. An empty store is the shortest answer
	// there is.
	//
	// All three gaps are the same gap. One failed midday fetch leaves a store
	// holding 00:00-06:00 and 12:00-24:00; a store that first heard from the
	// market at breakfast holds 06:00-24:00 of the day the app asked for from
	// midnight. A judgement that watches only the tail calls both a covered
	// day — so the app draws hours as a market that said nothing, which is
	// "never fake live" with prices in it.
	//
	// The head is covered by any slot starting at or before fromMs — it does
	// not have to start exactly on it. The reader promises the slot already
	// running at fromMs, and that slot is the price right now.
	out.Stale = true
	if n := len(out.Slots); n > 0 {
		first, last := out.Slots[0], out.Slots[n-1]
		out.Stale = first.StartMs > fromMs || last.StartMs+last.DurationMs < toMs
		for i := 1; i < n && !out.Stale; i++ {
			prev := out.Slots[i-1]
			out.Stale = prev.StartMs+prev.DurationMs < out.Slots[i].StartMs
		}
	}

	return out
}

// --------------------------------------------------------------------------
// Serving a query
// --------------------------------------------------------------------------

func (h *Handler) onPriceGet(ctx context.Context, env Envelope) error {
	if env.ID == nil {
		// A response nobody can route is a response nobody asked for.
		return nil
	}
	if h.cfg.Prices == nil {
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnavailable,
			Retryable: ErrorRetryable[ErrUnavailable],
			Args:      map[string]any{"subsystem": "prices"},
		})
	}

	var q PriceGet
	if err := Unmarshal(env.B, &q); err != nil {
		h.log.Warn("undecodable price.get dropped", "err", err)
		return nil
	}
	if q.ToMs <= q.FromMs {
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnknownOp,
			Retryable: false,
			Args:      map[string]any{"t": MsgPriceGet},
		})
	}

	// Clamped at the far end, never at the near one: the caller anchored the
	// window on FromMs and the slots either side of it are the ones being
	// planned around. priceFrom below still judges against the original
	// q.ToMs, so a clamped answer comes back short and marked stale rather
	// than claiming to cover a window it never read.
	toMs := q.ToMs
	if q.FromMs < toMs-maxPriceWindowMs {
		toMs = q.FromMs + maxPriceWindowMs
	}

	points, err := h.cfg.Prices.Slots(ctx, q.FromMs, toMs)
	if err != nil {
		// Scoped to the request. A window the box could not read is one chart
		// the user can narrow, not a reason to make the whole app look broken.
		h.log.Warn("price window unreadable", "err", err)
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnavailable,
			Retryable: ErrorRetryable[ErrUnavailable],
			Args:      map[string]any{"subsystem": "prices"},
		})
	}

	return h.sendPrice(env.ID, priceFrom(
		h.cfg.Prices.Zone(),
		h.cfg.Prices.Currency(),
		h.cfg.Prices.VATPercent(),
		points,
		q.FromMs,
		q.ToMs,
	))
}

// sendPrice puts a price window on the bulk lane, dropping the far end until
// it fits.
//
// A slot encodes to about sixty bytes and the largest bulk bucket is 16 kB, so
// roughly 270 slots is the wall. Two days of quarter-hour prices — 192 slots,
// the widest window the day-ahead market actually publishes — lands near 11 kB
// and fits; a week of them does not. The plan hit this exact wall and used to
// fail the encode, which killed the session and reached the user as "the box
// hangs up whenever I open the plan", so the rule here is the plan's rule:
// never error over size.
//
// What is dropped is the far end, and dropping it sets Stale. The near slots
// are the ones a person is planning around, and Stale already means "our
// numbers stop here" — the same sentence the box sends every morning before
// tomorrow's rates land. The app therefore needs no separate case for this,
// and can ask for the remainder as a second window if it wants it.
func (h *Handler) sendPrice(id *uint32, body Price) error {
	for {
		env, err := newEnvelope(MsgPrice, id, body)
		if err != nil {
			return err
		}
		raw, err := EncodeEnvelope(env)
		if err != nil {
			return err
		}
		if bucket := bulkBucketFor(len(raw)); bucket != 0 {
			return h.send(Frame{Lane: LaneBulk, Bucket: bucket, Envelope: env})
		}
		if len(body.Slots) <= 1 {
			return fmt.Errorf("appproto: a single price slot does not fit any bulk bucket")
		}
		body.Slots = body.Slots[:len(body.Slots)*3/4]
		body.Stale = true
	}
}
