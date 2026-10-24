package cloudlink

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// tileWall is 2026-10-12 17:12:00 UTC: the trailing tile started at 12:00,
// and the bucket that ended at 17:10 has settled.
var tileWall = time.UnixMilli(1_791_806_400_000 + (5*60+12)*60_000)

// historySite is the fake house with a ledger that has a bucket for every
// five minutes up to the wall clock, the one still filling included, as
// the real one has.
type historySite struct {
	*fakeSite
	mu   sync.Mutex
	wall time.Time
	// bump is added to every value from bumpFrom on, as a ledger repair
	// would change them.
	bump     float64
	bumpFrom int64
	failing  bool
	reads    int
}

func (s *historySite) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wall
}

func (s *historySite) set(wall time.Time) {
	s.mu.Lock()
	s.wall = wall
	s.mu.Unlock()
}

// value is the ledger's mean for one series and bucket.
func (s *historySite) value(name string, start int64) float64 {
	i := float64(start/300_000%10_000) + float64(len(name))
	if start >= s.bumpFrom {
		i += s.bump
	}
	switch name {
	case "pv_w":
		return -i
	case "battery_w":
		return 2 * i
	}
	return i
}

func (s *historySite) HistoryWindow(_ context.Context, names []string, stepMs, fromMs, toMs int64) (map[string][]protocol.HistorySample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.failing {
		return nil, errors.New("the ledger is busy")
	}
	out := map[string][]protocol.HistorySample{}
	for at := fromMs / stepMs * stepMs; at < toMs && at <= s.wall.UnixMilli(); at += stepMs {
		for _, name := range names {
			out[name] = append(out[name], protocol.HistorySample{StartMs: at, W: s.value(name, at)})
		}
	}
	return out, nil
}

// noise is the tile the box serves over Noise at the same moment.
func (s *historySite) noise(t *testing.T, start int64) protocol.HistChunk {
	t.Helper()
	span := protocol.ResolutionTileSpanMs[cloudwire.TileRes]
	w, err := s.HistoryWindow(context.Background(), cloudwire.TileSeries, 300_000, start, start+span)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.PackTile(cloudwire.TileRes, 1, start, cloudwire.TileSeries, w, s.now().UnixMilli())
}

func newTileHarness(t *testing.T, svc *fakeService, tweak func(*Options)) (*harness, *historySite) {
	t.Helper()
	hs := &historySite{wall: tileWall, bumpFrom: 1 << 62}
	h := newHarnessWith(t, svc, []*ecdsa.PublicKey{&svc.key.PublicKey}, nil, func(o *Options) {
		hs.fakeSite = o.Site.(*fakeSite)
		o.Site, o.Now = hs, hs.now
		if tweak != nil {
			tweak(o)
		}
	})
	return h, hs
}

// tilesSent decodes every tiles body the service has received, in order.
func tilesSent(t *testing.T, svc *fakeService) []cloudwire.Tiles {
	t.Helper()
	var out []cloudwire.Tiles
	for _, env := range svc.received() {
		if env.T != cloudwire.MsgTiles {
			continue
		}
		var b cloudwire.Tiles
		if err := cloudwire.Unmarshal(env.B, &b); err != nil {
			t.Fatal(err)
		}
		if err := b.Check(); err != nil {
			t.Fatalf("the box sent a tile of no shape: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// waitTiles waits until the service has n tiles bodies, and a little more,
// so one too many shows too.
func waitTiles(t *testing.T, svc *fakeService, n int) []cloudwire.Tiles {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(tilesSent(t, svc)) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	got := tilesSent(t, svc)
	if len(got) != n {
		t.Fatalf("the service has %d tiles, want %d: %v", len(got), n, tileNames(got))
	}
	return got
}

func tileNames(ts []cloudwire.Tiles) []string {
	var out []string
	for _, b := range ts {
		out = append(out, b.TileID+"@"+time.UnixMilli(b.StartMs).UTC().Format("02 15:04"))
	}
	return out
}

func point(b cloudwire.Tiles, series string, i int) int32 {
	s := slices.Index(b.Series, series)
	return int32(binary.LittleEndian.Uint32(b.Data[(s*b.Points()+i)*4:]))
}

func tileID(start int64) string { return protocol.TileIDFor(cloudwire.TileRes, 1, start) }

const (
	trailStart = int64(1_791_806_400_000) // 2026-10-12 12:00 UTC
	tileSpan   = int64(43_200_000)
)

func grantHistory(t *testing.T, h *harness) {
	t.Helper()
	h.grant(t, protocol.ScopeLiveRead, protocol.ScopeHistoryRead)
}

func TestAtConnectTheTrailingTileGoesWholeAndTheClosedOnesAsOverNoise(t *testing.T) {
	svc := newFakeService(t)
	h, hs := newTileHarness(t, svc, nil)
	grantHistory(t, h)
	got := waitTiles(t, svc, 5)

	whole := got[0]
	if whole.TileID != tileID(trailStart) || !whole.Partial || !whole.Whole() || whole.Points() != 144 {
		t.Fatalf("first tile %v, want the trailing tile whole", tileNames(got[:1]))
	}
	// 17:05 has closed; 17:10 has not settled, and the bucket still filling
	// in the ledger stays out.
	at1705, at1710 := int((17*60+5-12*60)/5), int((17*60+10-12*60)/5)
	if v := point(whole, "grid_w", at1705); v != protocol.ClampSample(hs.value("grid_w", trailStart+int64(at1705)*300_000)) {
		t.Fatalf("17:05 reads %d", v)
	}
	if point(whole, "grid_w", at1710) != protocol.MissingSample || point(whole, "load_w", 143) != protocol.MissingSample {
		t.Fatal("a bucket that has not closed went as a value")
	}

	// Then the closed tiles of the last 48 hours, newest first, each the
	// Noise tile byte for byte.
	want := []int64{trailStart - tileSpan, trailStart - 2*tileSpan, trailStart - 3*tileSpan, trailStart - 4*tileSpan}
	for i, start := range want {
		b := got[i+1]
		n := hs.noise(t, start)
		if b.Partial || b.TileID != n.TileID || b.Etag != n.Etag || !bytes.Equal(b.Data, n.Data) || b.StartMs != n.StartMs {
			t.Fatalf("closed tile %d is %v, not the Noise tile %s", i, tileNames([]cloudwire.Tiles{b}), n.TileID)
		}
	}
	// Caps say so.
	var hello cloudwire.Hello
	payload, _ := cloudwire.DecodeFrame(svc.hello)
	env, _ := cloudwire.DecodeEnvelope(payload)
	if cloudwire.Unmarshal(env.B, &hello) != nil || !slices.Contains(hello.Caps, cloudwire.MsgTiles) {
		t.Fatalf("hello.caps %v lacks tiles", hello.Caps)
	}
}

func TestEachClosedBucketGoesAsAnAppendUntilTheTileGoesWholeAgain(t *testing.T) {
	svc := newFakeService(t)
	h, hs := newTileHarness(t, svc, nil)
	grantHistory(t, h)
	waitTiles(t, svc, 5)

	// The bucket 17:10–17:15 settles at 17:15:30.
	hs.set(tileWall.Add(3*time.Minute + 29*time.Second))
	time.Sleep(200 * time.Millisecond)
	if n := len(tilesSent(t, svc)); n != 5 {
		t.Fatalf("a tile went before its bucket settled: %d", n)
	}
	for i := 1; i <= cloudwire.TileAppendsMax+1; i++ {
		hs.set(tileWall.Add(time.Duration(i)*5*time.Minute - 2*time.Minute + 31*time.Second))
		got := waitTiles(t, svc, 5+i)
		b := got[len(got)-1]
		if i <= cloudwire.TileAppendsMax {
			start := trailStart + int64((17*60+10-12*60+5*(i-1)))*60_000
			if !b.Partial || b.Whole() || b.Points() != 1 || b.StartMs != start {
				t.Fatalf("append %d is %v", i, tileNames([]cloudwire.Tiles{b}))
			}
			if point(b, "pv_w", 0) != protocol.ClampSample(hs.value("pv_w", start)) {
				t.Fatalf("append %d carries %d", i, point(b, "pv_w", 0))
			}
			continue
		}
		if !b.Partial || !b.Whole() || b.Points() != 144 {
			t.Fatalf("after %d appends the tile went as %v, not whole", cloudwire.TileAppendsMax, tileNames([]cloudwire.Tiles{b}))
		}
	}
	// The pieces lay into the tile the box holds.
	laid := cloudwire.Lay(tilesSent(t, svc))
	if last := laid[len(laid)-1]; last.TileID != tileID(trailStart) || !last.Partial {
		t.Fatalf("laid %v", last.TileID)
	}
}

func TestATileGoesClosedWhenItsLastBucketCloses(t *testing.T) {
	svc := newFakeService(t)
	h, hs := newTileHarness(t, svc, nil)
	hs.set(time.UnixMilli(trailStart + tileSpan - 2*60_000)) // 23:58
	grantHistory(t, h)
	waitTiles(t, svc, 5)

	hs.set(time.UnixMilli(trailStart + tileSpan + 31_000)) // 00:00:31
	got := waitTiles(t, svc, 6)
	closed := got[5]
	n := hs.noise(t, trailStart)
	if closed.Partial || closed.TileID != tileID(trailStart) || closed.Etag != n.Etag {
		t.Fatalf("at midnight %v went, not the closed tile", tileNames(got[5:]))
	}
	// Nothing of the new tile until its first bucket settles.
	hs.set(time.UnixMilli(trailStart + tileSpan + 5*60_000 + 31_000))
	got = waitTiles(t, svc, 7)
	first := got[6]
	if first.TileID != tileID(trailStart+tileSpan) || !first.Whole() || !first.Partial ||
		point(first, "grid_w", 0) == protocol.MissingSample || point(first, "grid_w", 1) != protocol.MissingSample {
		t.Fatalf("the new tile began as %v", tileNames(got[6:]))
	}
}

func TestTheServicesHaveSavesItsClosedTiles(t *testing.T) {
	svc := newFakeService(t)
	probe := &historySite{wall: tileWall, bumpFrom: 1 << 62}
	held := probe.noise(t, trailStart-tileSpan)
	svc.have = map[string]string{
		held.TileID:                     held.Etag,
		tileID(trailStart - 3*tileSpan): "00000000", // an older etag
		"5m/1/1":                        "ffffffff", // nothing the box has
	}
	h, _ := newTileHarness(t, svc, nil)
	grantHistory(t, h)
	got := waitTiles(t, svc, 4)
	ids := []string{got[1].TileID, got[2].TileID, got[3].TileID}
	if slices.Contains(ids, held.TileID) || !slices.Contains(ids, tileID(trailStart-3*tileSpan)) {
		t.Fatalf("sent %v with have %v", ids, svc.have)
	}
}

func TestAResyncOfTilesSendsThemAgainOnceInFiveMinutes(t *testing.T) {
	svc := newFakeService(t)
	conns := make(chan uplinkConn, 4)
	svc.afterHello = func(conn uplinkConn, _ string) { conns <- conn }
	h, _ := newTileHarness(t, svc, nil)
	grantHistory(t, h)
	waitTiles(t, svc, 5)
	conn := <-conns

	resync := func(named ...string) {
		svc.write(conn, svc.key, svc.home, cloudwire.MsgResync, cloudwire.Resync{What: []string{cloudwire.ResyncTiles}, Tiles: named})
	}
	resync()
	got := waitTiles(t, svc, 10)
	if !got[5].Whole() || !got[5].Partial || got[6].Partial || got[9].TileID != tileID(trailStart-4*tileSpan) {
		t.Fatalf("a resync sent %v", tileNames(got[5:]))
	}
	// A second within five minutes is ignored.
	resync()
	waitTiles(t, svc, 10)

	// One that names a tile sends that one alone.
	h.link.mu.Lock()
	h.link.resyncAt = time.Time{}
	h.link.mu.Unlock()
	resync(tileID(trailStart - 2*tileSpan))
	got = waitTiles(t, svc, 11)
	if got[10].TileID != tileID(trailStart-2*tileSpan) || got[10].Partial {
		t.Fatalf("a named resync sent %v", tileNames(got[10:]))
	}
}

func TestAReconnectSendsWhatTheServiceLacks(t *testing.T) {
	svc := newFakeService(t)
	svc.kick = 1
	h, _ := newTileHarness(t, svc, nil)
	grantHistory(t, h)
	// The first connection drops right after hello_ok, before any tile; the
	// second sends the trailing tile and the closed ones.
	got := waitTiles(t, svc, 5)
	if svc.conns < 2 || !got[0].Whole() || !got[0].Partial {
		t.Fatalf("after a reconnect: %d connections, %v", svc.conns, tileNames(got))
	}
}

func TestTheHourlyPassResendsATileTheLedgerRepaired(t *testing.T) {
	old := tilesRecheckEvery
	tilesRecheckEvery = 300 * time.Millisecond
	t.Cleanup(func() { tilesRecheckEvery = old })

	svc := newFakeService(t)
	h, hs := newTileHarness(t, svc, nil)
	grantHistory(t, h)
	waitTiles(t, svc, 5)

	// The ledger fills a gap two tiles back.
	repaired := trailStart - 2*tileSpan
	hs.mu.Lock()
	hs.bump, hs.bumpFrom = 7, repaired+tileSpan/2
	hs.mu.Unlock()
	// The trailing tile changes too, but it goes only as it closes.
	got := waitTiles(t, svc, 7)
	again := got[5:]
	ids := []string{again[0].TileID, again[1].TileID}
	if !slices.Contains(ids, tileID(repaired)) || !slices.Contains(ids, tileID(repaired+tileSpan)) {
		t.Fatalf("after the repair %v went", tileNames(again))
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(tilesSent(t, svc)); n != 7 {
		t.Fatalf("an unchanged tile went again: %d", n)
	}
}

func TestNoTilesWithoutTheHistoryScopeAClockOrTheBudget(t *testing.T) {
	cases := map[string]struct {
		scopes []string
		tweak  func(*Options)
		budget bool
	}{
		"live only": {scopes: []string{protocol.ScopeLiveRead}},
		"no clock": {scopes: []string{protocol.ScopeLiveRead, protocol.ScopeHistoryRead},
			tweak: func(o *Options) { o.ClockSynced = func() bool { return false } }},
		"past the budget": {scopes: []string{protocol.ScopeLiveRead, protocol.ScopeHistoryRead}, budget: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			svc := newFakeService(t)
			h, hs := newTileHarness(t, svc, c.tweak)
			if c.budget {
				h.link.mu.Lock()
				h.link.budget.add(h.link.uptimeMs(), budgetBytes)
				h.link.mu.Unlock()
			}
			h.grant(t, c.scopes...)
			svc.waitFor(cloudwire.MsgRoster, 5*time.Second)
			time.Sleep(time.Second)
			if n := len(tilesSent(t, svc)); n != 0 {
				t.Fatalf("%d tiles went", n)
			}
			if name == "live only" {
				hs.mu.Lock()
				reads := hs.reads
				hs.mu.Unlock()
				if reads != 0 {
					t.Fatalf("the ledger was read %d times for nothing", reads)
				}
			}
		})
	}
}

func TestALedgerThatFailsHoldsTilesBackAndNothingElse(t *testing.T) {
	svc := newFakeService(t)
	h, hs := newTileHarness(t, svc, nil)
	hs.failing = true
	grantHistory(t, h)
	svc.waitFor(cloudwire.MsgLive, 5*time.Second)
	svc.waitFor(cloudwire.MsgHB, 20*time.Second)
	if n := len(tilesSent(t, svc)); n != 0 {
		t.Fatalf("%d tiles went from a failing ledger", n)
	}
	hs.mu.Lock()
	reads := hs.reads
	hs.mu.Unlock()
	if reads > 2 {
		t.Fatalf("a failing ledger was read %d times within its retry", reads)
	}
}

// History taken out of the grant and put back: the service dropped what it
// held, so every tile goes again, as at connect, on the same link.
func TestHistoryTakenOutAndPutBackSendsEveryTileAgain(t *testing.T) {
	svc := newFakeService(t)
	h, _ := newTileHarness(t, svc, nil)
	grantHistory(t, h)
	waitTiles(t, svc, 5)
	conns := svc.conns

	h.grant(t, protocol.ScopeLiveRead)
	time.Sleep(300 * time.Millisecond)
	if n := len(tilesSent(t, svc)); n != 5 {
		t.Fatalf("%d tiles went without history in the grant", n)
	}
	grantHistory(t, h)
	got := waitTiles(t, svc, 10)
	if svc.conns != conns {
		t.Fatalf("the link dialled again: %d connections", svc.conns)
	}
	if !got[5].Whole() || !got[5].Partial || got[6].Partial || got[9].TileID != tileID(trailStart-4*tileSpan) {
		t.Fatalf("after history came back: %v", tileNames(got[5:]))
	}
}
