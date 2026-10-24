package cloudwire

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"slices"
	"testing"

	"github.com/srcfl/ftw/go/protocol"
)

// testdata/cloud-tile-vectors.json freezes one tile of each shape, signed by
// the vector box key of cloud-vectors.json, how three pieces lay back
// together, and the bodies of tiles.get and tiles.end. srcfl/ftw-cloud checks
// and holds them; the phone verifies, decodes and lays them.
//
//	go test ./cloudwire -run TestTileVectors -update

const tileVectorsPath = "testdata/cloud-tile-vectors.json"

// The vectors' tiles: the tile before 2026-10-12 12:00 UTC, closed, and the
// one after it, still filling at 17:15.
const (
	vectorTileStart = int64(1_791_806_400_000) // 2026-10-12 12:00 UTC
	vectorTilePrev  = vectorTileStart - 43_200_000
)

// vectorWindows is the ledger the vectors pack from: a value for every
// bucket of both tiles before 17:15, but none for the house between 03:00
// and 03:30, and no battery for the first hour of the afternoon.
func vectorWindows() map[string][]protocol.HistorySample {
	out := map[string][]protocol.HistorySample{}
	for at := vectorTilePrev; at < vectorTileStart+5*3_600_000+15*60_000; at += 300_000 {
		i := float64((at - vectorTilePrev) / 300_000)
		out["grid_w"] = append(out["grid_w"], protocol.HistorySample{StartMs: at, W: 800 - 15*i})
		out["pv_w"] = append(out["pv_w"], protocol.HistorySample{StartMs: at, W: -math.Max(0, 3000*math.Sin(i/40))})
		if at < vectorTileStart || at >= vectorTileStart+3_600_000 {
			out["battery_w"] = append(out["battery_w"], protocol.HistorySample{StartMs: at, W: 400 - 3*i})
		}
		if at < vectorTilePrev+3*3_600_000 || at >= vectorTilePrev+3*3_600_000+30*60_000 {
			out["load_w"] = append(out["load_w"], protocol.HistorySample{StartMs: at, W: 450 + 2.5*i})
		}
	}
	return out
}

// vectorPack packs a tile as the box does: buckets from closedThrough on are
// not there yet.
func vectorPack(start, closedThrough int64) protocol.HistChunk {
	windows := map[string][]protocol.HistorySample{}
	for name, samples := range vectorWindows() {
		for _, s := range samples {
			if s.StartMs < closedThrough {
				windows[name] = append(windows[name], s)
			}
		}
	}
	return protocol.PackTile(TileRes, 1, start, TileSeries, windows, closedThrough)
}

func header(seq uint64, wall int64) Header {
	return Header{Seq: seq, Boot: bytes.Repeat([]byte{0xb0}, BootBytes), UptimeMs: 60_000 + int64(seq)*10, WallMs: i64(wall)}
}

// The vectors' bodies, in the order the box sends them.
func vectorTiles() []struct {
	name string
	body Tiles
} {
	closed := vectorPack(vectorTilePrev, vectorTileStart+30_000)
	whole := Piece(vectorPack(vectorTileStart, vectorTileStart+5*3_600_000), 0, 144)
	first := Piece(vectorPack(vectorTileStart, vectorTileStart+5*3_600_000+5*60_000), 60, 61)
	second := Piece(vectorPack(vectorTileStart, vectorTileStart+5*3_600_000+15*60_000), 61, 63)
	closedBody := Tiles{TileID: closed.TileID, Etag: closed.Etag, Res: closed.Res, StartMs: closed.StartMs,
		StepMs: closed.StepMs, Series: closed.Series, Data: closed.Data, Partial: closed.Partial}

	wall := vectorTileStart + 5*3_600_000
	closedBody.Header = header(20, vectorTileStart+30_000)
	whole.Header = header(21, wall+30_000)
	first.Header = header(22, wall+5*60_000+30_000)
	second.Header = header(23, wall+15*60_000+30_000)
	return []struct {
		name string
		body Tiles
	}{{"closed", closedBody}, {"whole", whole}, {"append 17:00", first}, {"append 17:05", second}}
}

func TestTheThreeShapesCheck(t *testing.T) {
	for _, v := range vectorTiles() {
		if err := v.body.Check(); err != nil {
			t.Errorf("%s: %v", v.name, err)
		}
	}
	closed := vectorTiles()[0].body
	if closed.Partial || !closed.Whole() || closed.Points() != 144 {
		t.Fatalf("closed tile: partial %v whole %v points %d", closed.Partial, closed.Whole(), closed.Points())
	}
	if a := vectorTiles()[3].body; a.Whole() || a.Points() != 2 || a.StartMs != vectorTileStart+5*3_600_000+5*60_000 {
		t.Fatalf("append: whole %v points %d start %d", a.Whole(), a.Points(), a.StartMs)
	}
}

// A closed tile is what the box serves over Noise for the same series: the
// same id, etag and bytes.
// The series are the registry's names, and a whole tile fits the bound
// FTW Cloud holds a tiles envelope to.
func TestTileSeriesAreTheRegistrysAndATileFitsItsBound(t *testing.T) {
	if !slices.Equal(TileSeries, []string{"grid_w", "pv_w", "battery_w", "load_w"}) {
		t.Fatalf("TileSeries %v", TileSeries)
	}
	b := vectorTiles()[0].body
	env, err := Seal(nil, "home", MsgTiles, b)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := env.Encode()
	if len(raw)+64+6 > TileEnvelopeMax {
		t.Fatalf("a closed tile with its signature is %d bytes", len(raw)+64)
	}
}

func TestAClosedTileIsTheNoiseTile(t *testing.T) {
	noise := protocol.PackTile(TileRes, 1, vectorTilePrev, TileSeries, vectorWindows(), vectorTileStart+3_600_000)
	got := vectorTiles()[0].body.Chunk()
	if got.TileID != noise.TileID || got.Etag != noise.Etag || !bytes.Equal(got.Data, noise.Data) || got.Partial != noise.Partial {
		t.Fatalf("closed tile %s %s differs from Noise's %s %s", got.TileID, got.Etag, noise.TileID, noise.Etag)
	}
}

func TestCheckRefusesWhatIsNotATile(t *testing.T) {
	good := vectorTiles()[2].body
	cases := map[string]func(*Tiles){
		"etag":              func(b *Tiles) { b.Etag = "00000000" },
		"another tile's id": func(b *Tiles) { b.TileID = protocol.TileIDFor(TileRes, 1, vectorTilePrev) },
		"stride 2 id":       func(b *Tiles) { b.TileID = protocol.TileIDFor(TileRes, 2, vectorTileStart) },
		"hourly":            func(b *Tiles) { b.Res = protocol.ResN1h },
		"step":              func(b *Tiles) { b.StepMs = 600_000 },
		"off the grid":      func(b *Tiles) { b.StartMs++ },
		"half a column": func(b *Tiles) {
			b.Data = b.Data[:len(b.Data)-2]
			b.Etag = protocol.EtagOf(b.Data)
		},
		"no series": func(b *Tiles) { b.Series = nil },
		"another series": func(b *Tiles) {
			b.Series = []string{"grid_w", "pv_w", "battery_w", "ev_w"}
		},
		"one series less": func(b *Tiles) {
			b.Series = b.Series[:3]
			b.Data = b.Data[:3*b.Points()*4]
			b.Etag = protocol.EtagOf(b.Data)
		},
		"twice the same series": func(b *Tiles) {
			b.Series = []string{"grid_w", "grid_w", "pv_w", "load_w"}
		},
		"past the tile": func(b *Tiles) {
			b.StartMs = vectorTileStart + 143*300_000
			b.Data = append(b.Data, b.Data...)
			b.Etag = protocol.EtagOf(b.Data)
		},
		"closed but a piece": func(b *Tiles) { b.Partial = false },
	}
	for name, mutate := range cases {
		b := good
		b.Series = slices.Clone(good.Series)
		b.Data = slices.Clone(good.Data)
		mutate(&b)
		if err := b.Check(); !errors.Is(err, ErrTile) {
			t.Errorf("%s: Check() = %v, want ErrTile", name, err)
		}
	}
}

func sample(c protocol.HistChunk, series string, point int) int32 {
	s := slices.Index(c.Series, series)
	per := len(c.Data) / 4 / len(c.Series)
	return int32(binary.LittleEndian.Uint32(c.Data[(s*per+point)*4:]))
}

// The whole piece and two appends lay back into the tile the box would pack
// at 17:15, less the bucket that has not closed.
func TestLayPutsThePiecesBackTogether(t *testing.T) {
	v := vectorTiles()
	laid := Lay([]Tiles{v[0].body, v[1].body, v[2].body, v[3].body})
	if len(laid) != 2 || laid[0].TileID != v[0].body.TileID || laid[1].TileID != v[1].body.TileID {
		t.Fatalf("laid %d tiles", len(laid))
	}
	want := vectorPack(vectorTileStart, vectorTileStart+5*3_600_000+15*60_000)
	got := laid[1]
	if !got.Partial || got.StartMs != vectorTileStart || !bytes.Equal(got.Data, want.Data) || got.Etag != protocol.EtagOf(want.Data) {
		t.Fatalf("trailing tile does not match the box's own pack")
	}
	if sample(got, "battery_w", 3) != protocol.MissingSample || sample(got, "grid_w", 63) != protocol.MissingSample {
		t.Fatal("a hole, or a bucket not closed yet, reads as a value")
	}
	if laid[0].Partial || laid[0].Etag != v[0].body.Etag {
		t.Fatal("the closed tile changed")
	}
}

func TestLayKeepsTheNewestWholeAndTheClosedTile(t *testing.T) {
	v := vectorTiles()
	// An append, then a whole piece sent later: the whole one wins, and so
	// the append before it is gone.
	laid := Lay([]Tiles{v[2].body, v[1].body})
	if sample(laid[0], "grid_w", 60) != protocol.MissingSample {
		t.Fatal("an append before a whole piece survived it")
	}
	// A piece of a tile already closed changes nothing.
	piece := Piece(vectorPack(vectorTilePrev, vectorTilePrev+3_600_000), 0, 144)
	laid = Lay([]Tiles{v[0].body, piece})
	if laid[0].Partial || laid[0].Etag != v[0].body.Etag {
		t.Fatal("a piece reopened a closed tile")
	}
	// A closed tile sent again after the ledger changed replaces the first.
	fixed := v[0].body
	fixed.Data = slices.Clone(fixed.Data)
	fixed.Data[0] ^= 1
	fixed.Etag = protocol.EtagOf(fixed.Data)
	laid = Lay([]Tiles{v[0].body, fixed, piece})
	if laid[0].Partial || laid[0].Etag != fixed.Etag {
		t.Fatal("a later closed tile did not replace the first")
	}
	// A body that fails Check is skipped.
	bad := v[3].body
	bad.Etag = "00000000"
	laid = Lay([]Tiles{v[1].body, v[2].body, bad})
	if sample(laid[0], "grid_w", 61) != protocol.MissingSample {
		t.Fatal("a body with a wrong etag was laid")
	}
}

type tileVectorFile struct {
	BoxKey vectorKey           `json:"boxKey"`
	Series []string            `json:"series"`
	Tiles  []tileVector        `json:"tiles"`
	Forged tileVector          `json:"forged"`
	Laid   []tileVectorLaid    `json:"laid"`
	Get    tileVectorExchange  `json:"tilesGet"`
	End    tileVectorExchange  `json:"tilesEnd"`
	Notes  map[string]string   `json:"notes"`
	Have   map[string][]string `json:"have"`
}

type tileVector struct {
	Name              string `json:"name"`
	TileID            string `json:"tileId"`
	Etag              string `json:"etag"`
	StartMs           int64  `json:"startMs"`
	Points            int    `json:"points"`
	Partial           bool   `json:"partial"`
	Seq               uint64 `json:"seq"`
	Body              string `json:"body"`
	SignedInputSHA256 string `json:"signedInputSha256"`
	Sig               string `json:"sig"`
	Envelope          string `json:"envelope"`
	Frame             string `json:"frame"`
}

type tileVectorLaid struct {
	Of      []string `json:"of"`
	TileID  string   `json:"tileId"`
	Etag    string   `json:"etag"`
	StartMs int64    `json:"startMs"`
	Partial bool     `json:"partial"`
	Data    string   `json:"data"`
}

type tileVectorExchange struct {
	Body     string `json:"body"`
	Envelope string `json:"envelope"`
}

func TestTileVectors(t *testing.T) {
	box := testKey(t, "ftw.cloud vector box")
	home, boxPK := homeOf(t, box)

	var stored tileVectorFile
	raw, err := os.ReadFile(tileVectorsPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatal(err)
		}
	case !*update:
		t.Fatalf("%v (run with -update to create it)", err)
	}

	out := tileVectorFile{
		BoxKey: vectorKey{D: hex.EncodeToString(box.D.FillBytes(make([]byte, 32))), PK: hex.EncodeToString(boxPK), Home: home},
		Series: TileSeries,
		Notes: map[string]string{
			"tiles":  "One tile of each shape, in the order the box sends them, signed by boxKey. Each passes Tiles.Check.",
			"forged": "The last append with its data changed after signing: it must not verify, and is never held or laid.",
			"laid":   "What Lay makes of the named tiles, in that order. A trailing tile stays partial; its etag is that of the data as laid.",
			"have":   "The tiles.get have that holds the closed tile: the service answers it with the pieces alone.",
		},
	}
	signed := func(i int, name string, b Tiles) tileVector {
		body, err := Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		input, err := SignedInput(home, MsgTiles, body)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(input)
		v := tileVector{Name: name, TileID: b.TileID, Etag: b.Etag, StartMs: b.StartMs, Points: b.Points(), Partial: b.Partial,
			Seq: b.Seq, Body: hex.EncodeToString(body), SignedInputSHA256: hex.EncodeToString(digest[:])}
		var sig []byte
		if i < len(stored.Tiles) && stored.Tiles[i].Name == name && stored.Tiles[i].SignedInputSHA256 == v.SignedInputSHA256 {
			sig, _ = hex.DecodeString(stored.Tiles[i].Sig)
		} else if *update {
			if sig, err = Sign(rand.Reader, box, home, MsgTiles, body); err != nil {
				t.Fatal(err)
			}
		} else {
			t.Fatalf("tile vector %d is not %q as stored; regenerate with -update", i, name)
		}
		env := Envelope{T: MsgTiles, B: body, Sig: sig}
		if err := Verify(&box.PublicKey, home, env); err != nil {
			t.Fatalf("%s: stored signature does not verify: %v", name, err)
		}
		v.Sig = hex.EncodeToString(sig)
		envBytes, err := env.Encode()
		if err != nil {
			t.Fatal(err)
		}
		frame, err := EncodeFrame(envBytes)
		if err != nil {
			t.Fatal(err)
		}
		v.Envelope, v.Frame = hex.EncodeToString(envBytes), hex.EncodeToString(frame)
		return v
	}

	tiles := vectorTiles()
	var bodies []Tiles
	for i, v := range tiles {
		out.Tiles = append(out.Tiles, signed(i, v.name, v.body))
		bodies = append(bodies, v.body)
	}

	// The forged tile: the last append's signature over other data.
	last := out.Tiles[len(out.Tiles)-1]
	forged := tiles[len(tiles)-1].body
	forged.Data = slices.Clone(forged.Data)
	forged.Data[0] ^= 1
	forged.Etag = protocol.EtagOf(forged.Data)
	fb, err := Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := hex.DecodeString(last.Sig)
	fenv := Envelope{T: MsgTiles, B: fb, Sig: sig}
	if Verify(&box.PublicKey, home, fenv) == nil {
		t.Fatal("the forged tile verifies")
	}
	fraw, err := fenv.Encode()
	if err != nil {
		t.Fatal(err)
	}
	fframe, err := EncodeFrame(fraw)
	if err != nil {
		t.Fatal(err)
	}
	out.Forged = tileVector{Name: "forged append", TileID: forged.TileID, Etag: forged.Etag, StartMs: forged.StartMs,
		Points: forged.Points(), Partial: true, Seq: forged.Seq, Body: hex.EncodeToString(fb), Sig: last.Sig,
		Envelope: hex.EncodeToString(fraw), Frame: hex.EncodeToString(fframe)}

	for _, of := range [][]int{{0}, {1}, {1, 2, 3}, {0, 1, 2, 3}, {2, 1}} {
		var in []Tiles
		var names []string
		for _, i := range of {
			in = append(in, bodies[i])
			names = append(names, tiles[i].name)
		}
		for _, c := range Lay(in) {
			out.Laid = append(out.Laid, tileVectorLaid{Of: names, TileID: c.TileID, Etag: c.Etag, StartMs: c.StartMs,
				Partial: c.Partial, Data: hex.EncodeToString(c.Data)})
		}
	}

	have := map[string]string{bodies[0].TileID: bodies[0].Etag}
	out.Have = map[string][]string{"answer": {"whole", "append 17:00", "append 17:05"}}
	for _, ex := range []struct {
		dst  *tileVectorExchange
		t    string
		body any
	}{{&out.Get, MsgTilesGet, TilesGet{Have: have}}, {&out.End, MsgTilesEnd, TilesEnd{N: 3}}} {
		body, err := Marshal(ex.body)
		if err != nil {
			t.Fatal(err)
		}
		env, err := Unsigned(ex.t, ex.body)
		if err != nil {
			t.Fatal(err)
		}
		*ex.dst = tileVectorExchange{Body: hex.EncodeToString(body), Envelope: hex.EncodeToString(env)}
	}

	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		if err := os.WriteFile(tileVectorsPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("%s is out of date; regenerate with -update", tileVectorsPath)
	}
}
