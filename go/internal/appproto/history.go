package appproto

import (
	"context"
	"fmt"
	"math"
)

// History: the box's side of the tile protocol.
//
// The geometry here — resolutions, tile spans, strides, ids, the etag — is
// the same arithmetic as the app's src/lib/protocol/history.ts, because the
// two ends must compute identical tiles from identical inputs or the etag
// comparison silently degrades into "always refetch". Spans and retention
// come from contract/registry.yaml through contract_gen.go; nothing here is
// hand-written twice.
//
// Power series are derived from the energy ledger: a bucket's mean power is
// its energy divided by its width. That is an average, and it is presented as
// one — the chunk carries its step, and the app labels it. Sub-bucket wiggles
// are gone, which is the correct price for history that survives a restart.

// HistorySample is one stored bucket of one series, in mean watts.
type HistorySample struct {
	// StartMs is the bucket start, wall clock.
	StartMs int64
	// W is the mean power over the bucket, site-signed.
	W float64
}

// HistoryProvider answers windows of stored series.
//
// An interface so appproto never touches SQL, and so tests can serve
// synthetic ledgers. A nil provider means the box keeps no history and says
// so — the state this package shipped in first.
type HistoryProvider interface {
	// Series returns the buckets of `name` overlapping [fromMs, toMs), at
	// the stored bucket width `stepMs`. Missing buckets are simply absent;
	// the tile builder turns absence into MISSING samples. An unknown name
	// returns an empty slice — a series that does not exist has no data,
	// which is already a thing the wire can say.
	Series(ctx context.Context, name string, stepMs, fromMs, toMs int64) ([]HistorySample, error)
}

// resolutionStepMs is the stored bucket width per resolution. This is the
// meaning of the resolution's name; the registry carries spans and retention.
var resolutionStepMs = map[Resolution]int64{
	ResN5m: 300_000,
	ResN1h: 3_600_000,
}

// resolutionOrder is fine to coarse: the order the box clamps along.
var resolutionOrder = []Resolution{ResN5m, ResN1h}

// defaultMaxPoints matches the app's DEFAULT_MAX_POINTS.
const defaultMaxPoints = 2000

// missingSample marks a hole on the wire. Distinct from zero, which is a
// real reading. Same value as the app's MISSING_SAMPLE.
const missingSample = math.MinInt32

func retentionMsOf(res Resolution) int64 {
	return int64(ResolutionRetentionDays[res]) * 86_400_000
}

func pointsPerTile(res Resolution) int64 {
	return ResolutionTileSpanMs[res] / resolutionStepMs[res]
}

func tileStartFor(res Resolution, atMs int64) int64 {
	span := ResolutionTileSpanMs[res]
	return (atMs / span) * span
}

// tileIDFor matches the app's tileId(): res/stride/tileIndex.
func tileIDFor(res Resolution, stride, startMs int64) string {
	return fmt.Sprintf("%s/%d/%d", res, stride, startMs/ResolutionTileSpanMs[res])
}

// etagOf is FNV-1a over the packed block, eight hex digits — the same hash
// the app computes, because the whole point of an etag is that both ends
// agree on it.
func etagOf(data []byte) string {
	h := uint32(0x811c9dc5)
	for _, b := range data {
		h ^= uint32(b)
		h *= 0x01000193
	}
	return fmt.Sprintf("%08x", h)
}

type plannedTile struct {
	id      string
	startMs int64
	points  int64
}

type historyPlan struct {
	res    Resolution
	stride int64
	stepMs int64
	tiles  []plannedTile
}

// planHistoryQuery is the app's planQuery(), ported term for term. Both ends
// run it on the same inputs and must land on the same tiles.
func planHistoryQuery(res Resolution, fromMs, toMs int64, maxPoints int) historyPlan {
	pointCap := int64(maxPoints)
	if pointCap < 1 {
		pointCap = 1
	}

	// Transfer is whole tiles, so the count that must fit under the cap is
	// the tile-aligned span, not the requested one.
	alignedSpan := func(candidate Resolution) int64 {
		span := ResolutionTileSpanMs[candidate]
		first := tileStartFor(candidate, fromMs)
		last := tileStartFor(candidate, max(fromMs, toMs-1))
		return last + span - first
	}

	rank := func(r Resolution) int {
		for i, candidate := range resolutionOrder {
			if candidate == r {
				return i
			}
		}
		return 0
	}

	// Clamp to a coarser store before aggregating: coarser stored data is
	// real data, while an aggregate of finer data is an average of it.
	chosen := resolutionOrder[len(resolutionOrder)-1]
	for _, candidate := range resolutionOrder {
		if rank(candidate) < rank(res) {
			continue
		}
		chosen = candidate
		if alignedSpan(candidate)/resolutionStepMs[candidate] <= pointCap {
			break
		}
	}

	// The stride must divide the tile's point count evenly, or a tile stops
	// being a whole number of points and every boundary needs special
	// handling. Constraining it to a divisor keeps tiles independent.
	base := resolutionStepMs[chosen]
	total := alignedSpan(chosen)
	perTile := pointsPerTile(chosen)
	stride := perTile
	for k := int64(1); k <= perTile; k++ {
		if perTile%k != 0 {
			continue
		}
		if total/(base*k) <= pointCap {
			stride = k
			break
		}
	}

	span := ResolutionTileSpanMs[chosen]
	first := tileStartFor(chosen, fromMs)
	last := tileStartFor(chosen, max(fromMs, toMs-1))

	var tiles []plannedTile
	for start := first; start <= last; start += span {
		tiles = append(tiles, plannedTile{
			id:      tileIDFor(chosen, stride, start),
			startMs: start,
			points:  perTile / stride,
		})
	}

	return historyPlan{res: chosen, stride: stride, stepMs: base * stride, tiles: tiles}
}

// --------------------------------------------------------------------------
// Serving a query
// --------------------------------------------------------------------------

func (h *Handler) onHistQuery(ctx context.Context, env Envelope) error {
	if env.ID == nil {
		// A response nobody can route is a response nobody asked for.
		return nil
	}
	if h.cfg.History == nil {
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnavailable,
			Retryable: ErrorRetryable[ErrUnavailable],
			Args:      map[string]any{"subsystem": "history"},
		})
	}

	var q HistQuery
	if err := Unmarshal(env.B, &q); err != nil {
		h.log.Warn("undecodable hist.query dropped", "err", err)
		return nil
	}
	if _, ok := ResolutionTileSpanMs[q.Res]; !ok || len(q.Series) == 0 || q.ToMs <= q.FromMs {
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnknownOp,
			Retryable: false,
			Args:      map[string]any{"t": MsgHistQuery},
		})
	}

	maxPoints := defaultMaxPoints
	if q.MaxPoints != nil && *q.MaxPoints > 0 {
		maxPoints = int(*q.MaxPoints)
	}

	have := make(map[string]string, len(q.Have))
	for _, ref := range q.Have {
		have[ref.TileID] = ref.Etag
	}

	plan := planHistoryQuery(q.Res, q.FromMs, q.ToMs, maxPoints)
	nowMs := h.cfg.Clock.Now().UnixMilli()

	// Ranges the retention window has already eaten are named, not served as
	// silence — the app says "evicted" instead of drawing a suspicious hole.
	var gaps []HistGap
	if retainedFrom := nowMs - retentionMsOf(plan.res); plan.tiles[0].startMs < retainedFrom {
		gaps = append(gaps, HistGap{
			FromMs: plan.tiles[0].startMs,
			ToMs:   min(retainedFrom, q.ToMs),
			Reason: GapEvicted,
		})
	}

	for _, tile := range plan.tiles {
		chunk, err := h.buildTile(ctx, q.Series, plan, tile, nowMs)
		if err != nil {
			return h.sendError(env.ID, ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"subsystem": "history"},
			})
		}
		// A closed tile the app already holds, byte for byte, is not resent.
		// Partial tiles always go: their content moves with the clock, which
		// is exactly why the app never caches them.
		if !chunk.Partial && have[chunk.TileID] == chunk.Etag {
			continue
		}
		if err := h.sendBulk(MsgHistChunk, env.ID, chunk); err != nil {
			return err
		}
	}

	if gaps == nil {
		// An absent list and an empty list must be the same statement.
		gaps = []HistGap{}
	}
	return h.sendBulk(MsgHistEnd, env.ID, HistEnd{ResActual: plan.res, Gaps: gaps})
}

// buildTile reads the provider and packs one tile: int32 LE columns, one
// block per series, MISSING where the store has no bucket.
func (h *Handler) buildTile(
	ctx context.Context,
	series []string,
	plan historyPlan,
	tile plannedTile,
	nowMs int64,
) (HistChunk, error) {
	stepMs := resolutionStepMs[plan.res]
	tileEnd := tile.startMs + ResolutionTileSpanMs[plan.res]
	points := tile.points

	data := make([]byte, int64(len(series))*points*4)
	for s, name := range series {
		samples, err := h.cfg.History.Series(ctx, name, stepMs, tile.startMs, tileEnd)
		if err != nil {
			return HistChunk{}, err
		}

		// Stored buckets fold into strided points by plain mean: the ledger's
		// buckets are equal width, so each carries equal weight.
		sums := make([]float64, points)
		counts := make([]int64, points)
		for _, sample := range samples {
			if sample.StartMs < tile.startMs || sample.StartMs >= tileEnd {
				continue
			}
			p := (sample.StartMs - tile.startMs) / (stepMs * plan.stride)
			if p < 0 || p >= points {
				continue
			}
			sums[p] += sample.W
			counts[p]++
		}

		base := int64(s) * points * 4
		for p := int64(0); p < points; p++ {
			v := int32(missingSample)
			if counts[p] > 0 {
				v = clampSample(sums[p] / float64(counts[p]))
			}
			off := base + p*4
			u := uint32(v)
			data[off] = byte(u)
			data[off+1] = byte(u >> 8)
			data[off+2] = byte(u >> 16)
			data[off+3] = byte(u >> 24)
		}
	}

	return HistChunk{
		TileID:  tile.id,
		Etag:    etagOf(data),
		Res:     plan.res,
		StartMs: tile.startMs,
		StepMs:  plan.stepMs,
		Series:  series,
		Data:    data,
		Partial: tileEnd > nowMs,
	}, nil
}

func clampSample(v float64) int32 {
	r := math.Round(v)
	// One step inside the limits: MinInt32 itself means "missing", and a
	// real reading must never be mistaken for a hole.
	if r >= math.MaxInt32 {
		return math.MaxInt32
	}
	if r <= missingSample+1 {
		return missingSample + 1
	}
	return int32(r)
}
