package mpc

import (
	"context"
	"math"
	"testing"
	"time"
)

func nativeBenchmarkFixture(ev bool) ([]Slot, Params) {
	_, p := externalTestFixture()
	p.Mode, p.CapacityWh, p.InitialSoC, p.SoCMin, p.SoCMax = ModeSelfConsumption, 20000, .45, .1, .95
	p.SoCLevels, p.ActionLevels, p.MaxChargeW, p.MaxDischargeW, p.TerminalSoCPrice = 201, 401, 9000, 9000, 160
	slots := make([]Slot, 193)
	for i := range slots {
		h := float64(i%96) / 4
		pv := 0.0
		if h > 5 && h < 21 {
			pv = -7000 * math.Sin(math.Pi*(h-5)/16)
		}
		if i >= 96 {
			pv *= .4
		}
		spot := 12.0
		if h >= 5 {
			spot = 35 + 20*math.Sin(math.Pi*(h-5)/14) + 75*math.Exp(-(h-18)*(h-18)/3)
		}
		load := 450 + 900*math.Exp(-(h-7)*(h-7)/1.5) + 1400*math.Exp(-(h-19)*(h-19)/2)
		slots[i] = Slot{StartMs: int64(i) * 900000, LenMin: 15, Confidence: 1, PVW: pv, LoadW: load, PriceOre: spot*1.25 + 95, SpotOre: spot, Limits: PowerLimits{MaxImportW: 11040, MaxExportW: 11040}}
	}
	if ev {
		p.Loadpoint = &LoadpointSpec{ID: "garage", CapacityWh: 60000, Levels: 11, InitialSoC: .35, SoCMax: 1, PluggedIn: true, TargetSoC: .8, TargetSlotIdx: 64, MaxChargeW: 11000, ChargeEfficiency: .9, AllowedStepsW: []float64{0, 4140, 6900, 11000}, NoBatteryToEV: true}
	}
	return slots, p
}

func BenchmarkNativePlanner193(b *testing.B) {
	for _, ev := range []bool{false, true} {
		name := "battery"
		if ev {
			name = "battery_ev"
		}
		slots, p := nativeBenchmarkFixture(ev)
		for _, native := range []bool{false, true} {
			engine := "current_dp"
			if native {
				engine = "rust_worker"
			}
			b.Run(name+"/"+engine, func(b *testing.B) {
				var o *ExternalOptimizer
				if native {
					o = nativeWorker(b, 100*time.Millisecond)
					defer o.Close()
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if native {
						if _, err := o.Optimize(context.Background(), slots, p); err != nil {
							b.Fatal(err)
						}
					} else {
						_ = Optimize(slots, p)
					}
				}
			})
		}
	}
}
