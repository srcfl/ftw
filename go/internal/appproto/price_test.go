package appproto

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
)

// fakePrices stands in for the box's price service: a zone, a currency and
// whatever the test decided the store holds. It serves the overlap window the
// port promises, so a handler that widened or narrowed the window on its own
// would show up here rather than in production.
//
// askedFrom/askedTo record the window that reached the store. A reply can look
// right while the query behind it read years of rows, so anything about how
// much the box reads has to be asserted here rather than on the reply.
type fakePrices struct {
	zone     string
	currency string
	vat      float64
	points   []PricePoint
	err      error

	askedFrom int64
	askedTo   int64
}

func (f *fakePrices) Zone() string        { return f.zone }
func (f *fakePrices) Currency() string    { return f.currency }
func (f *fakePrices) VATPercent() float64 { return f.vat }

func (f *fakePrices) Slots(_ context.Context, fromMs, toMs int64) ([]PricePoint, error) {
	f.askedFrom, f.askedTo = fromMs, toMs
	if f.err != nil {
		return nil, f.err
	}
	var out []PricePoint
	for _, p := range f.points {
		end := p.StartMs + int64(p.LenMin)*int64(time.Minute/time.Millisecond)
		if end <= fromMs || p.StartMs >= toMs {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// quarterHours is a run of quarter-hour slots, the shape most of Europe's
// day-ahead market has published since 2025.
func quarterHours(startMs int64, n int, spot, total float64) []PricePoint {
	out := make([]PricePoint, 0, n)
	for i := range n {
		out = append(out, PricePoint{
			StartMs:    startMs + int64(i)*900_000,
			LenMin:     15,
			SpotMinor:  spot,
			TotalMinor: total,
		})
	}
	return out
}

// hourly is a run of hour-long slots, the shape the older day-ahead market
// publishes and the one several zones still carry.
func hourly(startMs int64, n int, spot, total float64) []PricePoint {
	out := make([]PricePoint, 0, n)
	for i := range n {
		out = append(out, PricePoint{
			StartMs:    startMs + int64(i)*3_600_000,
			LenMin:     60,
			SpotMinor:  spot,
			TotalMinor: total,
		})
	}
	return out
}

func newPriceRig(t *testing.T, reader PriceReader) (*Handler, *recorder, *fakeClock) {
	t.Helper()
	clock := &fakeClock{uptimeMs: 60_000, now: time.UnixMilli(1_760_000_000_000)}
	box := &fakeBox{
		identity:     Identity{ID: "ftw-0001", Build: "test", TZ: "Europe/Stockholm"},
		mode:         control.ModePlannerPassiveArbitrage,
		snap:         Snapshot{Mode: control.ModePlannerPassiveArbitrage, DispatchBlockedBy: []string{}},
		observedMode: control.ModePlannerPassiveArbitrage,
		observedOK:   true,
	}
	rec := &recorder{}
	h, err := New(Config{
		Clock: clock, Site: box, Info: box, Modes: box, Plans: box,
		Prices: reader,
		Codec:  testCodec{}, Sender: rec,
		Caller: ownerCaller(), Grants: newGrants(), Commands: NewCmdLog(),
		SrcGrid: "meter.p1", SrcPV: "meter.p1", SrcBattery: "meter.p1",
		NewLeaseID: func() string { return "lease-test" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	subscribe(t, h, rec)
	return h, rec, clock
}

func TestPriceGetAnswersTheAskingRequestOnTheBulkLane(t *testing.T) {
	at := int64(1_760_000_000_000)
	h, rec, _ := newPriceRig(t, &fakePrices{
		zone: "SE3", currency: "SEK", points: quarterHours(at, 8, 42, 91),
	})

	deliver(t, h, MsgPriceGet, ptrU32(11), PriceGet{FromMs: at, ToMs: at + 2*3_600_000})

	f := rec.only(t, MsgPrice)
	if f.env.ID == nil || *f.env.ID != 11 {
		t.Fatalf("price did not echo the request id: %v", f.env.ID)
	}
	if f.lane != LaneBulk {
		t.Fatalf("price went out on lane %d; a window whose size varies with the market belongs on bulk", f.lane)
	}

	p := body[Price](t, f)
	if p.Zone != "SE3" || p.Currency != "SEK" {
		t.Fatalf("zone/currency = %q/%q, want SE3/SEK — unlabelled numbers cannot be read", p.Zone, p.Currency)
	}
	if len(p.Slots) != 8 {
		t.Fatalf("%d slots, want the 8 the store holds", len(p.Slots))
	}
}

// The store keeps floats and the wire carries money. Rounding once, here, is
// what keeps the app and the box from disagreeing about what 18.7 öre is.
func TestPriceRoundsToWholeMinorUnits(t *testing.T) {
	at := int64(1_760_000_000_000)
	h, rec, _ := newPriceRig(t, &fakePrices{
		zone: "SE3", currency: "SEK",
		points: []PricePoint{
			{StartMs: at, LenMin: 15, SpotMinor: 18.7, TotalMinor: 24.5},
			// Negative spot is a real market, not an error case.
			{StartMs: at + 900_000, LenMin: 15, SpotMinor: -12.6, TotalMinor: 187.4},
		},
	})

	deliver(t, h, MsgPriceGet, ptrU32(1), PriceGet{FromMs: at, ToMs: at + 1_800_000})

	p := body[Price](t, rec.only(t, MsgPrice))
	if got := p.Slots[0]; got.SpotMinor != 19 || got.TotalMinor != 25 {
		t.Fatalf("18.7/24.5 became %d/%d, want 19/25", got.SpotMinor, got.TotalMinor)
	}
	if got := p.Slots[1]; got.SpotMinor != -13 || got.TotalMinor != 187 {
		t.Fatalf("-12.6/187.4 became %d/%d, want -13/187", got.SpotMinor, got.TotalMinor)
	}
	if p.Slots[0].DurationMs != 900_000 {
		t.Fatalf("a 15 minute slot is %d ms on the wire, want 900000", p.Slots[0].DurationMs)
	}
}

// The answer carries the VAT rate the box taxed the totals at, so an app can
// show spot with VAT at the box's rate rather than its own guess. Integer
// permille, rounded once like the money, and sent at zero too: 0 is a rate,
// and only a box without the field leaves it out.
func TestPriceCarriesTheAppliedVATRate(t *testing.T) {
	at := int64(1_760_000_000_000)
	for _, tc := range []struct {
		vat  float64
		want int64
	}{
		{25, 250},
		{25.5, 255}, // Finland since September 2024
		{0, 0},
	} {
		h, rec, _ := newPriceRig(t, &fakePrices{
			zone: "SE3", currency: "SEK", vat: tc.vat, points: quarterHours(at, 4, 42, 91),
		})

		deliver(t, h, MsgPriceGet, ptrU32(7), PriceGet{FromMs: at, ToMs: at + 3_600_000})

		f := rec.only(t, MsgPrice)
		if p := body[Price](t, f); p.VATPermille != tc.want {
			t.Errorf("VAT %v %% went out as %d permille, want %d", tc.vat, p.VATPermille, tc.want)
		}
		keys := body[map[string]any](t, f)
		if _, ok := keys["vatPermille"]; !ok {
			t.Errorf("VAT %v %%: no vatPermille key; an absent rate reads as unknown, not as zero", tc.vat)
		}
	}
}

// Tomorrow's rates arrive in the afternoon. Until they do, the honest answer
// ends early and says so — the app stops drawing rather than showing a cliff
// the market did not have.
func TestPriceSaysSoWhenTheWindowOutrunsTheStore(t *testing.T) {
	at := int64(1_760_000_000_000)
	store := &fakePrices{zone: "SE3", currency: "SEK", points: quarterHours(at, 8, 42, 91)}
	h, rec, _ := newPriceRig(t, store)

	// Two hours stored, four hours asked for.
	deliver(t, h, MsgPriceGet, ptrU32(1), PriceGet{FromMs: at, ToMs: at + 4*3_600_000})
	if p := body[Price](t, rec.only(t, MsgPrice)); !p.Stale {
		t.Fatal("a window running two hours past the last stored slot came back fresh")
	}

	// The same store, asked only for what it covers, is complete. Without this
	// half the test would pass against a box that always claims staleness.
	rec.reset()
	deliver(t, h, MsgPriceGet, ptrU32(2), PriceGet{FromMs: at, ToMs: at + 2*3_600_000})
	if p := body[Price](t, rec.only(t, MsgPrice)); p.Stale {
		t.Fatal("a fully covered window was marked stale")
	}
}

// The prices table is never pruned, so an unbounded window is a query over
// every row the box has ever stored. Nobody has to mean harm for one to
// arrive: the app derives its window from the phone's clock, and a phone whose
// clock is wrong asks from 1970.
func TestAWindowFromABadClockReachesTheStoreBounded(t *testing.T) {
	now := int64(1_760_000_000_000)
	store := &fakePrices{zone: "SE3", currency: "SEK", points: quarterHours(now, 8, 42, 91)}
	h, rec, _ := newPriceRig(t, store)

	deliver(t, h, MsgPriceGet, ptrU32(7), PriceGet{FromMs: 0, ToMs: now})

	// The window, not the reply: an unclamped box answers this request too,
	// having read fifty-six years of rows to do it.
	if store.askedFrom != 0 {
		t.Errorf("asked from %d; the near end of the window belongs to the caller", store.askedFrom)
	}
	if span := store.askedTo - store.askedFrom; span != maxPriceWindowMs {
		t.Fatalf("the store was asked for %d ms, want it bounded at %d", span, maxPriceWindowMs)
	}
	if p := body[Price](t, rec.only(t, MsgPrice)); !p.Stale {
		t.Fatal("a clamped answer claimed to cover the window that was asked for")
	}

	// And an ordinary window still crosses untouched — without this the test
	// would pass against a box that clamped everything down to nothing.
	rec.reset()
	deliver(t, h, MsgPriceGet, ptrU32(8), PriceGet{FromMs: now, ToMs: now + 2*3_600_000})
	if store.askedFrom != now || store.askedTo != now+2*3_600_000 {
		t.Fatalf("a two-hour window reached the store as [%d, %d)", store.askedFrom, store.askedTo)
	}
}

// One failed midday fetch leaves a store holding the morning and the evening
// and nothing between. A window judged by its tail alone calls that a covered
// day, and the app draws six hours as a market that said nothing.
func TestAHoleInTheMiddleIsAlsoStale(t *testing.T) {
	at := int64(1_760_000_000_000)
	const day = 86_400_000
	morning := quarterHours(at, 24, 42, 91)              // 00:00–06:00
	evening := quarterHours(at+12*3_600_000, 48, 42, 91) // 12:00–24:00
	h, rec, _ := newPriceRig(t, &fakePrices{
		zone: "SE3", currency: "SEK", points: append(morning, evening...),
	})

	deliver(t, h, MsgPriceGet, ptrU32(9), PriceGet{FromMs: at, ToMs: at + day})
	if p := body[Price](t, rec.only(t, MsgPrice)); !p.Stale {
		t.Fatal("a day with a six-hour hole in it came back complete")
	}

	// The same day unbroken is not stale, so this cannot pass by calling
	// every window with more than one slot in it stale.
	whole, recWhole, _ := newPriceRig(t, &fakePrices{
		zone: "SE3", currency: "SEK", points: quarterHours(at, 96, 42, 91),
	})
	deliver(t, whole, MsgPriceGet, ptrU32(10), PriceGet{FromMs: at, ToMs: at + day})
	if p := body[Price](t, recWhole.only(t, MsgPrice)); p.Stale {
		t.Fatal("an unbroken day was marked stale")
	}
}

// The other edge of the same defect. A store holding 06:00 onwards, asked for
// the day from midnight, covers eighteen hours of twenty-four — and a
// judgement that only looks at the tail and the joins calls that a whole day.
// It is the quieter half: a short tail makes the app stop drawing early, a
// missing head makes it draw the evening's prices with nothing said about the
// morning at all.
func TestAMissingHeadIsAlsoStale(t *testing.T) {
	at := int64(1_760_000_000_000)
	const day = 86_400_000
	const hour = 3_600_000
	store := &fakePrices{zone: "SE3", currency: "SEK", points: hourly(at+6*hour, 18, 42, 91)}
	h, rec, _ := newPriceRig(t, store)

	deliver(t, h, MsgPriceGet, ptrU32(12), PriceGet{FromMs: at, ToMs: at + day})
	p := body[Price](t, rec.only(t, MsgPrice))
	if len(p.Slots) != 18 {
		t.Fatalf("%d slots, want the 18 the store holds", len(p.Slots))
	}
	if !p.Stale {
		t.Fatal("a day whose first six hours are unpriced came back complete")
	}

	// The same store asked for what it actually covers is complete, so this
	// cannot pass against a box that calls every window stale.
	rec.reset()
	deliver(t, h, MsgPriceGet, ptrU32(13), PriceGet{FromMs: at + 6*hour, ToMs: at + day})
	if p := body[Price](t, rec.only(t, MsgPrice)); p.Stale {
		t.Fatal("a window that starts where the prices start was marked stale")
	}

	// And the slot already running at the near end covers it. The app asks
	// from "now", the port promises the slot in progress, and a head judged by
	// an exact match would call every such window stale — which is the app
	// saying "we do not know" about the hour the house is paying for.
	rec.reset()
	deliver(t, h, MsgPriceGet, ptrU32(14), PriceGet{FromMs: at + 6*hour + 1_800_000, ToMs: at + day})
	if p := body[Price](t, rec.only(t, MsgPrice)); p.Stale {
		t.Fatal("a window opening mid-slot was marked stale; that slot is the price right now")
	}
}

func TestPriceWithNothingStoredStillLabelsTheNumbers(t *testing.T) {
	at := int64(1_760_000_000_000)
	h, rec, _ := newPriceRig(t, &fakePrices{zone: "NO2", currency: "NOK"})

	deliver(t, h, MsgPriceGet, ptrU32(3), PriceGet{FromMs: at, ToMs: at + 86_400_000})

	p := body[Price](t, rec.only(t, MsgPrice))
	if len(p.Slots) != 0 {
		t.Fatalf("an empty store produced %d slots", len(p.Slots))
	}
	if !p.Stale {
		t.Fatal("holding no prices at all is the stalest a window gets")
	}
	if p.Zone != "NO2" || p.Currency != "NOK" {
		t.Fatalf("zone/currency = %q/%q; the labels hold even when the slots do not", p.Zone, p.Currency)
	}
}

// The plan taught this lesson the hard way: a body past the largest bulk
// bucket used to fail its encode and take the session down with it. A week of
// quarter-hour prices is the same wall, and must end in a short answer that
// admits it is short — never in a dropped session.
func TestAnOversizedPriceWindowIsTruncatedNotFatal(t *testing.T) {
	at := int64(1_760_000_000_000)
	const week = 7 * 24 * 4 // 672 quarter-hour slots
	h, rec, _ := newPriceRig(t, &fakePrices{
		zone: "SE3", currency: "SEK", points: quarterHours(at, week, 42, 91),
	})

	deliver(t, h, MsgPriceGet, ptrU32(4), PriceGet{FromMs: at, ToMs: at + 7*86_400_000})

	f := rec.only(t, MsgPrice)
	if f.size > BulkBuckets[len(BulkBuckets)-1] {
		t.Fatalf("the frame is %d bytes, past the largest bulk bucket", f.size)
	}

	p := body[Price](t, f)
	if len(p.Slots) >= week {
		t.Fatalf("%d slots went out; a week of them fits no bucket", len(p.Slots))
	}
	if len(p.Slots) < 192 {
		t.Fatalf("only %d slots survived; two days of quarter-hours should fit", len(p.Slots))
	}
	if p.Slots[0].StartMs != at {
		t.Fatal("truncation dropped the near slots instead of the far ones")
	}
	if !p.Stale {
		t.Fatal("a truncated window claimed to cover everything that was asked for")
	}
}

func TestPriceGetWithoutAServiceSaysUnavailable(t *testing.T) {
	h, _, rec, _ := newRig(t)
	subscribe(t, h, rec)

	deliver(t, h, MsgPriceGet, ptrU32(5), PriceGet{FromMs: 0, ToMs: 86_400_000})

	e := rec.only(t, MsgError)
	if e.env.ID == nil || *e.env.ID != 5 {
		t.Fatal("the error is not addressed to the request that caused it")
	}
	if b := body[ErrorBody](t, e); b.Code != ErrUnavailable {
		t.Fatalf("code = %s, want %s", b.Code, ErrUnavailable)
	}
}

// A store that cannot answer is one chart the user can narrow, not a reason to
// make the whole app look broken.
func TestAPriceStoreErrorStaysScopedToTheRequest(t *testing.T) {
	h, rec, _ := newPriceRig(t, &fakePrices{
		zone: "SE3", currency: "SEK", err: errors.New("database is locked"),
	})

	deliver(t, h, MsgPriceGet, ptrU32(6), PriceGet{FromMs: 0, ToMs: 86_400_000})

	e := rec.only(t, MsgError)
	if e.env.ID == nil || *e.env.ID != 6 {
		t.Fatal("a failed read raised a session-wide error instead of answering the request")
	}
	if rec.has(MsgSessionTerminate) {
		t.Fatal("a failed price read ended the session")
	}
}
