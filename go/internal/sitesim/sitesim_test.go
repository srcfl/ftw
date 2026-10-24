package sitesim

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/device-simulator/fuse"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// The plant's fuse is the fuse package's gG at its default 25 °C, stepped a
// second at a time: it blows in the second the model melts, export heats it
// as import does, a warm fuse blows sooner and the other phases stay whole.
func TestFuseFollowsTheFuseModel(t *testing.T) {
	gG, _ := fuse.Lookup(fuse.IecGg)
	blowAfter := func(f Fuse, a float64) float64 {
		for s := 1; s <= 7200; s++ {
			f.step([3]float64{-a, 0, 0}, 1)
			if f.Blown[0] {
				if f.Blown[1] || f.Blown[2] {
					t.Fatalf("%.0f A on L1 blew another phase", a)
				}
				return float64(s)
			}
		}
		return math.Inf(1)
	}
	for _, tc := range []struct{ ratingA, multiple float64 }{{16, 2}, {20, 2}, {25, 2}, {25, 3}, {25, 1.6}} {
		want := math.Ceil(gG.MeltSeconds(tc.multiple, tc.ratingA, fuse.DefaultAmbientC))
		if got := blowAfter(Fuse{RatingA: tc.ratingA}, tc.multiple*tc.ratingA); got != want {
			t.Errorf("%.1f× %.0f A: blew after %v s, want %v s", tc.multiple, tc.ratingA, got, want)
		}
	}
	if got := blowAfter(Fuse{RatingA: 25}, 1.25*25); !math.IsInf(got, 1) {
		t.Errorf("1.25× 25 A blew after %v s, want never", got)
	}
	warm := Fuse{RatingA: 25}
	if err := warm.Preheat([3]float64{23, 23, 23}); err != nil {
		t.Fatal(err)
	}
	if cold, hot := blowAfter(Fuse{RatingA: 25}, 50), blowAfter(warm, 50); hot >= cold {
		t.Errorf("2× 25 A: warm blew after %v s, cold after %v s", hot, cold)
	}
	defer func() {
		if recover() == nil {
			t.Error("a 10 A gG fuse, which the model does not cover, stepped without a panic")
		}
	}()
	(&Fuse{RatingA: 10}).step([3]float64{5, 5, 5}, 1)
}

// PV export shows as negative phase currents, or as magnitudes on an
// unsigned meter, and a slow charger and a slow battery answer late.
func TestPlantSignsAndDelays(t *testing.T) {
	p := New(t0, 25)
	p.HouseW = [3]float64{460, 460, 460}
	p.PVW = -6900
	p.Meter.Unsigned = true
	p.Battery = &Battery{Phases: 1, MaxChargeW: 3000, CapacityWh: 10000, SoC: .5, Latency: 2 * time.Second}
	p.EVSE = &EVSE{Phases: 3, MinA: 6, MaxA: 16, Delay: 5 * time.Second, ReadbackLag: 10 * time.Second}
	p.Car = &Car{Phases: 3, MaxW: 11040, Plugged: true}
	p.Events = []Event{{At: 3 * time.Second, Kind: MeterSilent, For: 2 * time.Second}}
	if tick := p.Step(); tick.Meter.W != -5520 || tick.Meter.PhaseA != [3]float64{8, 8, 8} || p.A[0] != -8 {
		t.Fatalf("export: meter %+v, physical %v", tick.Meter, p.A)
	}
	p.Battery.Command(p.Now, 2300)
	p.EVSE.Command(p.Now, 6900, "", 0, 230)
	var heard []float64
	for s := 2; s <= 17; s++ {
		tick := p.Step()
		if (tick.Meter == nil) != (s == 3 || s == 4) {
			t.Fatalf("%d s: meter reading %v", s, tick.Meter)
		}
		if want := s >= 3; (p.Battery.W == 2300) != want {
			t.Fatalf("%d s: battery %.0f W", s, p.Battery.W)
		}
		wantL2 := -8.0
		if s >= 6 {
			wantL2 = 2
		}
		if p.A[1] != wantL2 {
			t.Fatalf("%d s: L2 %.1f A, want %.0f A", s, p.A[1], wantL2)
		}
		heard = append(heard, tick.EV.W)
	}
	if heard[13] != 0 || heard[14] != 6900 {
		t.Fatalf("readback %v, want the draw from 6 s at 16 s", heard)
	}
}
