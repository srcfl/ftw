package cloudwire

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/srcfl/ftw/go/protocol"
)

// Tiles: the last 48 hours of the box's 5-minute history, so a phone can draw
// it while the box cannot answer (docs/protocol-cloud.md, tiles).
//
// A tiles body is the app protocol's hist.chunk with the common header. It
// comes in three shapes, all at 5 minutes and stride 1:
//
//   - closed: partial false, startMs the tile's start, every point. The same
//     tile id, etag and bytes as the box serves over Noise for TileSeries.
//   - whole: partial true, startMs the tile's start, every point, missing
//     from the first bucket that has not closed.
//   - append: partial true, startMs a later bucket, only the buckets that
//     closed since the last piece of that tile.
//
// A whole tile and an append are pieces of the trailing tile. Lay puts the
// pieces a receiver holds back together.

// MsgTilesGet asks the service for the tiles a phone lacks, as TilesGet. The
// service answers with the box's tiles envelopes, unchanged, then MsgTilesEnd.
const MsgTilesGet = "tiles.get"

// MsgTilesEnd closes the service's answer to tiles.get. It is the service's
// own and says only how many envelopes went before it.
const MsgTilesEnd = "tiles.end"

// The geometry FTW Cloud keeps.
const (
	// TileRes is the one resolution the box sends.
	TileRes = protocol.ResN5m
	// TileKeepMs is how far back tiles are kept: a tile that ended earlier
	// than this before now is gone.
	TileKeepMs = 48 * 3_600_000
	// TileSettleMs is how long after a bucket ends the box counts it closed.
	// The history writer commits every 15 s.
	TileSettleMs = 30_000
	// TileAppendsMax is how many appends follow a whole piece of the
	// trailing tile before the box sends it whole again: once an hour.
	TileAppendsMax = 11
	// TilePiecesMax is how many pieces of the trailing tile the service
	// holds. A whole piece replaces the ones before it.
	TilePiecesMax = 16
	// TilesClosedMax is how many closed tiles 48 hours can touch.
	TilesClosedMax = 5
	// TileEnvelopeMax bounds one tiles envelope. A tile of the four series
	// is about 2.6 KB, so FTW Cloud drops anything larger, padding with keys
	// it ignores included.
	TileEnvelopeMax = 4 << 10
)

// TileSeries are the columns the box sends, in this order, and the only ones
// a tile may carry. They are what the app asks for over Noise, so a closed
// tile is the same bytes either way.
var TileSeries = []string{
	protocol.FieldNames[protocol.FidGridW],
	protocol.FieldNames[protocol.FidPvW],
	protocol.FieldNames[protocol.FidBatteryW],
	protocol.FieldNames[protocol.FidLoadW],
}

// Tiles is one tile, or one piece of the trailing tile.
type Tiles struct {
	Header
	TileID  string              `cbor:"tileId"`
	Etag    string              `cbor:"etag"`
	Res     protocol.Resolution `cbor:"res"`
	StartMs int64               `cbor:"startMs"`
	StepMs  int64               `cbor:"stepMs"`
	Series  []string            `cbor:"series"`
	// Data is int32 LE, one block per series, protocol.MissingSample where
	// the box has no record.
	Data    []byte `cbor:"data"`
	Partial bool   `cbor:"partial"`
}

// TilesGet asks for tiles. Have maps tile id to etag for the closed tiles the
// phone holds; the service sends every closed tile it holds that is not
// there with that etag, and every piece of the trailing tile.
type TilesGet struct {
	Have map[string]string `cbor:"have"`
}

// TilesEnd closes the answer to tiles.get.
type TilesEnd struct {
	// N is how many tiles envelopes went before it.
	N int `cbor:"n"`
}

// Chunk is the tile as the app protocol carries it, without the header.
func (t Tiles) Chunk() protocol.HistChunk {
	return protocol.HistChunk{TileID: t.TileID, Etag: t.Etag, Res: t.Res, StartMs: t.StartMs,
		StepMs: t.StepMs, Series: slices.Clone(t.Series), Data: slices.Clone(t.Data), Partial: t.Partial}
}

// TileStart is the start of the tile this body belongs to.
func (t Tiles) TileStart() int64 { return protocol.TileStartFor(t.Res, t.StartMs) }

// TileEnd is where that tile ends.
func (t Tiles) TileEnd() int64 { return t.TileStart() + protocol.ResolutionTileSpanMs[t.Res] }

// Points is how many points each series carries.
func (t Tiles) Points() int {
	if len(t.Series) == 0 {
		return 0
	}
	return len(t.Data) / 4 / len(t.Series)
}

// Whole says the body starts at its tile's start: a closed tile, or a whole
// piece that replaces every piece of that tile before it.
func (t Tiles) Whole() bool { return t.StartMs == t.TileStart() }

// ErrTile is a tiles body that breaks the shapes above.
var ErrTile = errors.New("cloudwire: not a tile")

// Check says whether t is one of the three shapes. The service holds no
// other; the phone lays no other.
func (t Tiles) Check() error {
	bad := func(why string) error { return fmt.Errorf("%w: %s", ErrTile, why) }
	if t.Res != TileRes || t.StepMs != protocol.ResolutionStepMs[TileRes] {
		return bad("not 5-minute steps")
	}
	if t.StartMs < 0 || t.StartMs%t.StepMs != 0 {
		return bad("start off the 5-minute grid")
	}
	if t.TileID != protocol.TileIDFor(t.Res, 1, t.TileStart()) {
		return bad("tile id does not name the tile its start is in")
	}
	if !slices.Equal(t.Series, TileSeries) {
		return bad("series are not TileSeries")
	}
	n := t.Points()
	if n < 1 || len(t.Data) != n*4*len(t.Series) {
		return bad("data is not whole columns")
	}
	first := int((t.StartMs - t.TileStart()) / t.StepMs)
	if first+n > int(protocol.PointsPerTile(t.Res)) {
		return bad("data runs past the tile")
	}
	if !t.Partial && (!t.Whole() || n != int(protocol.PointsPerTile(t.Res))) {
		return bad("a closed tile that is not the whole tile")
	}
	if t.Etag != protocol.EtagOf(t.Data) {
		return bad("etag")
	}
	return nil
}

// Piece cuts points [from, to) of each series out of a packed tile, as the
// box sends an append or a whole piece. from 0 starts at the tile's start.
// The header is the caller's.
func Piece(c protocol.HistChunk, from, to int) Tiles {
	per := 0
	if len(c.Series) > 0 {
		per = len(c.Data) / 4 / len(c.Series)
	}
	to = min(to, per)
	from = min(max(from, 0), to)
	data := make([]byte, 0, len(c.Series)*(to-from)*4)
	for s := range c.Series {
		base := s * per * 4
		data = append(data, c.Data[base+from*4:base+to*4]...)
	}
	return Tiles{TileID: c.TileID, Etag: protocol.EtagOf(data), Res: c.Res,
		StartMs: c.StartMs + int64(from)*c.StepMs, StepMs: c.StepMs,
		Series: slices.Clone(c.Series), Data: data, Partial: true}
}

// Lay puts tiles back together, in the order the box sent them, by wallMs
// and then seq, which starts again with the box: a closed tile as it is; the
// trailing tile from its newest whole piece, with each later append laid
// over it; a later closed tile replaces an earlier one. A piece of a tile
// already closed changes nothing, and neither does a body that fails Check. The result has one chunk per tile, oldest first,
// each with every point; a trailing tile stays partial, its etag that of the
// data as laid, never one to offer as held.
func Lay(bodies []Tiles) []protocol.HistChunk {
	byID := map[string]*protocol.HistChunk{}
	for _, t := range bodies {
		if t.Check() != nil {
			continue
		}
		held := byID[t.TileID]
		if held != nil && !held.Partial && t.Partial {
			// A piece of a tile already closed.
			continue
		}
		if !t.Partial {
			// A closed tile, or a later one for the same tile: the box sends
			// a closed tile again when the ledger changed under it.
			c := t.Chunk()
			byID[t.TileID] = &c
			continue
		}
		per := int(protocol.PointsPerTile(t.Res))
		if held == nil || t.Whole() || !slices.Equal(held.Series, t.Series) {
			c := protocol.HistChunk{TileID: t.TileID, Res: t.Res, StartMs: t.TileStart(), StepMs: t.StepMs,
				Series: slices.Clone(t.Series), Data: missing(len(t.Series) * per), Partial: true}
			held = &c
			byID[t.TileID] = held
		}
		n := t.Points()
		at := int((t.StartMs - t.TileStart()) / t.StepMs)
		for s := range t.Series {
			copy(held.Data[(s*per+at)*4:(s*per+at+n)*4], t.Data[s*n*4:(s+1)*n*4])
		}
	}
	out := make([]protocol.HistChunk, 0, len(byID))
	for _, c := range byID {
		if c.Partial {
			c.Etag = protocol.EtagOf(c.Data)
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StartMs < out[b].StartMs })
	return out
}

// missing is n samples of protocol.MissingSample.
func missing(n int) []byte {
	b := make([]byte, n*4)
	m := int32(protocol.MissingSample)
	u := uint32(m)
	for i := 0; i < n; i++ {
		b[i*4], b[i*4+1], b[i*4+2], b[i*4+3] = byte(u), byte(u>>8), byte(u>>16), byte(u>>24)
	}
	return b
}
