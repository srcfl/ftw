package appproto

import (
	"testing"

	"github.com/srcfl/ftw/go/internal/control"
)

func TestSnapCarriesTheDictionaryAndEveryFrozenField(t *testing.T) {
	h, _, rec, _ := newRig(t)
	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})
	rec.reset()

	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})
	f := rec.only(t, MsgSnap)
	if f.lane != LaneBulk {
		t.Fatalf("snap went out on lane %d, want bulk", f.lane)
	}

	snap := body[Snap](t, f)
	for _, fid := range []Fid{FidMode, FidGridW, FidPvW, FidBatteryW, FidBatterySoc, FidLoadW} {
		if _, ok := snap.Fields[fidKey(fid)]; !ok {
			t.Fatalf("field %d missing from the snapshot", fid)
		}
		if _, ok := snap.Dict[fidKey(fid)]; !ok {
			t.Fatalf("field %d missing from the dictionary", fid)
		}
	}
	if snap.ControlRev != 7 {
		t.Fatalf("controlRev = %d, want 7", snap.ControlRev)
	}
	// The dictionary is what turns a reading into a freshness claim.
	if got := snap.Dict[fidKey(FidGridW)].SrcID; got == nil || *got != "meter.p1" {
		t.Fatalf("grid_w points at source %v, want meter.p1", got)
	}
	// The mode is the box's own state. A source id of "" would look like a
	// source nobody can resolve, so it goes as null.
	if got := snap.Dict[fidKey(FidMode)].SrcID; got != nil {
		t.Fatalf("mode points at source %q; it is the box's own state, not a reading", *got)
	}
	if got := snap.Dict[fidKey(FidMode)].Unit; got != nil {
		t.Fatalf("mode carries unit %q", *got)
	}
}

// Powers cross the wire in the site convention. This package converts no
// signs; that happens at the driver boundary and nowhere else.
func TestFieldsKeepTheSiteSignConvention(t *testing.T) {
	h, _, rec, _ := newRig(t)
	subscribe(t, h, rec)
	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})

	snap := body[Snap](t, rec.only(t, MsgSnap))
	if got := snap.Fields[fidKey(FidPvW)]; got != -3400 {
		t.Fatalf("pv_w = %d, want -3400: PV generates, which reduces import, so it is negative", got)
	}
	if got := snap.Fields[fidKey(FidBatteryW)]; got != 900 {
		t.Fatalf("battery_w = %d, want 900: charging is a load, so it is positive", got)
	}
	if got := snap.Fields[fidKey(FidGridW)]; got != 1200 {
		t.Fatalf("grid_w = %d, want 1200 (importing)", got)
	}
}

// Field 1 is an index into the catalogue hello_ok sent, which is what keeps
// lane 0 numeric and its frames a fixed size.
func TestModeFieldIsAnIndexIntoTheCatalogue(t *testing.T) {
	h, box, rec, _ := newRig(t)
	box.snap.Mode = control.ModePeakShaving
	subscribe(t, h, rec)
	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})

	snap := body[Snap](t, rec.only(t, MsgSnap))
	want := int64(-1)
	for i, m := range control.ModeCatalog() {
		if m.Key == control.ModePeakShaving {
			want = int64(i)
		}
	}
	if got := snap.Fields[fidKey(FidMode)]; got != want {
		t.Fatalf("mode field = %d, want catalogue index %d", got, want)
	}
}

// State of charge is a fraction inside the box, per the SI rule, and permille
// on the wire, per the registry. The conversion lives at this edge only.
func TestStateOfChargeIsPermilleOnTheWire(t *testing.T) {
	h, box, rec, _ := newRig(t)
	box.snap.BatterySoC = 0.6235
	subscribe(t, h, rec)
	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})

	snap := body[Snap](t, rec.only(t, MsgSnap))
	if got := snap.Fields[fidKey(FidBatterySoc)]; got != 624 {
		t.Fatalf("battery_soc = %d, want 624 permille", got)
	}
}

// Silence would itself be information: it would tell the relay operator that
// nothing happened in the house that second.
func TestNothingChangedStillSendsATick(t *testing.T) {
	h, _, rec, _ := newRig(t)
	subscribe(t, h, rec)

	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}
	f := rec.only(t, MsgTick)
	if f.lane != LaneControl {
		t.Fatalf("tick went out on lane %d, want 0", f.lane)
	}
	if f.size != 512 {
		t.Fatalf("tick is %d bytes, want exactly the 512-byte bucket", f.size)
	}
}

// Lane 0 is one bucket, always. A frame length that varied with what happened
// in the house would leak the load pattern through perfect encryption.
func TestLaneZeroFramesAreAllTheSameSize(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)

	for i := 0; i < 6; i++ {
		clock.uptimeMs += 1000
		box.snap.GridW += float64(i * 1731)
		box.snap.LoadW += float64(i * 977)
		for j := range box.snap.Sources {
			box.snap.Sources[j].LastOkUptimeMs = clock.uptimeMs
		}
		if err := h.Tick(); err != nil {
			t.Fatal(err)
		}
	}
	// One quiet second, so both a delta and a tick are in the sample.
	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}

	if len(rec.frames) != 7 {
		t.Fatalf("got %d frames from 7 ticks", len(rec.frames))
	}
	for _, f := range rec.frames {
		if f.lane != LaneControl {
			t.Fatalf("%s went out on lane %d", f.env.T, f.lane)
		}
		if f.size != 512 {
			t.Fatalf("%s frame is %d bytes; lane 0 is one bucket, always", f.env.T, f.size)
		}
	}
}

func TestDeltaCarriesOnlyWhatMoved(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)

	clock.uptimeMs += 1000
	for j := range box.snap.Sources {
		box.snap.Sources[j].LastOkUptimeMs = clock.uptimeMs
	}
	box.snap.GridW = 1500
	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}

	d := body[Delta](t, rec.only(t, MsgDelta))
	if len(d.Fields) != 1 {
		t.Fatalf("delta carried %v, want only grid_w", d.Fields)
	}
	if d.Fields[fidKey(FidGridW)] != 1500 {
		t.Fatalf("grid_w = %d", d.Fields[fidKey(FidGridW)])
	}
	if d.Seq != 1 {
		t.Fatalf("seq = %d, want 1", d.Seq)
	}
}

// A device that goes quiet mid-session is exactly what the freshness model
// exists to show. Source state that only ever appeared in a snapshot would
// leave the client drawing a dead number as live.
func TestSourceGoingQuietRidesAlongInADelta(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)

	// Everything answers on time for one tick.
	clock.uptimeMs += 1000
	for j := range box.snap.Sources {
		box.snap.Sources[j].LastOkUptimeMs = clock.uptimeMs
	}
	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}
	rec.reset()

	// Now the inverter stops answering. Nothing else changes.
	clock.uptimeMs += 40_000
	box.snap.Sources[0].LastOkUptimeMs = clock.uptimeMs // meter still fine
	box.snap.Sources[2].LastOkUptimeMs = clock.uptimeMs // battery still fine
	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}

	d := body[Delta](t, rec.only(t, MsgDelta))
	if d.Sources == nil {
		t.Fatal("a source changed state and the delta said nothing")
	}
	if got := d.Sources["inverter.sungrow"].State; got != SourceStale {
		t.Fatalf("inverter state = %q, want %q", got, SourceStale)
	}
	if got := d.Sources["meter.p1"].State; got != SourceLive {
		t.Fatalf("meter state = %q, want live", got)
	}
}

// dispatchBlockedBy is how the app says why nothing is happening instead of
// looking broken. It moves with the sources.
func TestDispatchBlockedByReachesTheClientInADelta(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)

	clock.uptimeMs += 30_000
	box.snap.DispatchBlockedBy = []string{"meter.p1"}
	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}

	d := body[Delta](t, rec.only(t, MsgDelta))
	if len(d.DispatchBlockedBy) != 1 || d.DispatchBlockedBy[0] != "meter.p1" {
		t.Fatalf("dispatchBlockedBy = %v, want [meter.p1]", d.DispatchBlockedBy)
	}
}

// A source state that stays the same must not be resent, or every delta would
// carry the whole table and lane 0 would have nothing left to spend on
// readings.
func TestUnchangedSourcesAreNotResent(t *testing.T) {
	h, box, rec, clock := newRig(t)
	subscribe(t, h, rec)

	for i := 0; i < 3; i++ {
		clock.uptimeMs += 1000
		for j := range box.snap.Sources {
			box.snap.Sources[j].LastOkUptimeMs = clock.uptimeMs
		}
		box.snap.GridW += 10
		if err := h.Tick(); err != nil {
			t.Fatal(err)
		}
	}

	for _, f := range rec.frames {
		if f.env.T != MsgDelta {
			continue
		}
		if d := body[Delta](t, f); d.Sources != nil {
			t.Fatal("a delta resent an unchanged source table")
		}
	}
}

// The bucket never grows and the frame never fragments, because both would
// make frame size a function of what happened in the house.
func TestAnOversizedDeltaTruncatesInsteadOfGrowing(t *testing.T) {
	h, box, rec, clock := newRig(t)

	// A site with many devices, so the source table alone overruns lane 0.
	box.snap.Sources = nil
	for i := 0; i < 24; i++ {
		box.snap.Sources = append(box.snap.Sources, Source{
			ID:             "driver.long.identifier." + string(rune('a'+i)),
			Kind:           "inverter",
			Name:           "A device with a comfortably long display name",
			LastOkUptimeMs: 60_000,
			StaleAfterMs:   15_000,
		})
	}
	subscribe(t, h, rec)

	clock.uptimeMs += 60_000 // every source ages out at once
	box.snap.GridW = 4321
	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}

	f := rec.only(t, MsgDelta)
	if f.size != 512 {
		t.Fatalf("truncated delta is %d bytes, want the 512-byte bucket", f.size)
	}
	if f.flags&FlagTrunc == 0 {
		t.Fatal("a delta that did not fit was sent without TRUNC")
	}
	d := body[Delta](t, f)
	if d.Sources != nil {
		t.Fatal("the source table survived truncation; readings should have won the space")
	}
	if d.Fields[fidKey(FidGridW)] != 4321 {
		t.Fatalf("the reading that changed did not survive truncation: %v", d.Fields)
	}
}

// A field dropped to fit the bucket was never sent, so it is still different
// on the next tick and goes then.
func TestTruncatedSourcesAreRetriedOnTheNextTick(t *testing.T) {
	h, box, rec, clock := newRig(t)
	box.snap.Sources = nil
	for i := 0; i < 24; i++ {
		box.snap.Sources = append(box.snap.Sources, Source{
			ID:             "driver.long.identifier." + string(rune('a'+i)),
			Kind:           "inverter",
			Name:           "A device with a comfortably long display name",
			LastOkUptimeMs: 60_000,
			StaleAfterMs:   15_000,
		})
	}
	subscribe(t, h, rec)

	clock.uptimeMs += 60_000
	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}
	rec.reset()

	if err := h.Tick(); err != nil {
		t.Fatal(err)
	}
	f := rec.last(t, MsgDelta)
	if f.flags&FlagTrunc == 0 {
		t.Fatal("the retry did not report itself truncated either")
	}
}

func TestSourceStateLadder(t *testing.T) {
	base := Source{ID: "x", StaleAfterMs: 10_000, LastOkUptimeMs: 100_000}

	cases := []struct {
		name     string
		uptimeMs int64
		src      Source
		want     SourceState
	}{
		{"fresh", 105_000, base, SourceLive},
		{"just inside", 110_000, base, SourceLive},
		{"lagging", 118_000, base, SourceLagging},
		{"stale", 150_000, base, SourceStale},
		{"long gone", 200_000, base, SourceDown},
		{"never answered", 105_000, Source{ID: "x", StaleAfterMs: 10_000, LastOkUptimeMs: -1}, SourceNever},
		// The driver saying a device is unreachable is a stronger claim
		// than an old reading, and outranks the age.
		{"driver reports offline", 100_500, Source{ID: "x", StaleAfterMs: 10_000, LastOkUptimeMs: 100_000, Offline: true}, SourceDown},
		// Without a declared cadence there is nothing to age against, and
		// "live" would be an assertion nothing supports.
		{"no cadence declared", 100_500, Source{ID: "x", LastOkUptimeMs: 100_000}, SourceNever},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sourceState(c.src, c.uptimeMs); got != c.want {
				t.Fatalf("state = %q, want %q", got, c.want)
			}
		})
	}
}

// A driver that lost its device can hand up a NaN. Dropping the whole frame
// over it would take the healthy readings down with it; the source state is
// what tells the app not to trust the number.
func TestNonFiniteWattsBecomeZeroRatherThanBreakingTheFrame(t *testing.T) {
	h, box, rec, _ := newRig(t)
	box.snap.PVW = nan()
	subscribe(t, h, rec)
	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})

	snap := body[Snap](t, rec.only(t, MsgSnap))
	if snap.Fields[fidKey(FidPvW)] != 0 {
		t.Fatalf("pv_w = %d, want 0", snap.Fields[fidKey(FidPvW)])
	}
	if snap.Fields[fidKey(FidGridW)] != 1200 {
		t.Fatal("one bad reading took a good one with it")
	}
}

func nan() float64 {
	var zero float64
	return zero / zero
}

// controlRev is what makes expect.rev a real check. When it moves mid-session
// the client has to hear the new number, and only the snapshot carries it —
// otherwise every command from then on is refused for a conflict the app
// cannot see or resolve without reconnecting.
func TestAChangedControlRevResendsTheSnapshot(t *testing.T) {
	h, box, rec, _ := newRig(t)
	deliver(t, h, MsgHello, nil, Hello{Proto: ProtoRange{Min: 0, Max: ProtoMax}})
	deliver(t, h, MsgSub, nil, Sub{Bucket: 512, Hz: 1})
	rec.reset()

	if err := h.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if rec.has(MsgSnap) {
		t.Fatal("an unchanged controlRev cost a snapshot")
	}

	box.snap.ControlRev = 8
	rec.reset()
	if err := h.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	snap := body[Snap](t, rec.only(t, MsgSnap))
	if snap.ControlRev != 8 {
		t.Fatalf("controlRev = %d, want 8", snap.ControlRev)
	}

	// Once, not on every tick after.
	rec.reset()
	if err := h.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if rec.has(MsgSnap) {
		t.Fatal("the snapshot went out again with nothing changed")
	}
}

// Field 10 is frozen like the rest, but conditional like battery_soc: a site
// without a charger sends nothing, and the app draws no EV node rather than a
// dead one holding an invented zero.
func TestEvFieldIsSentOnlyWhenAChargerExists(t *testing.T) {
	without := fieldValues(Snapshot{}, nil)
	if _, ok := without[fidKey(FidEvW)]; ok {
		t.Fatal("a site with no charger sent field 10")
	}

	idle := fieldValues(Snapshot{EVWKnown: true}, nil)
	if got, ok := idle[fidKey(FidEvW)]; !ok || got != 0 {
		t.Fatalf("an idle charger is a real 0 W reading; got %v, present=%v", got, ok)
	}

	charging := fieldValues(Snapshot{EVWKnown: true, EVW: 7360.4}, nil)
	if got := charging[fidKey(FidEvW)]; got != 7360 {
		t.Fatalf("ev_w = %d, want 7360", got)
	}

	// And the dictionary names it, with no source of its own — several
	// chargers can feed the sum, so absence of the value, not a per-driver
	// age, is the freshness story.
	dict := fieldDict("meter.p1", "inv", "bat")
	def, ok := dict[fidKey(FidEvW)]
	if !ok {
		t.Fatal("field 10 missing from the dictionary")
	}
	if def.Name != "ev_w" || def.SrcID != nil {
		t.Fatalf("field 10 = %+v, want name ev_w and no source", def)
	}
}
