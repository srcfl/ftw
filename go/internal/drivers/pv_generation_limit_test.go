package drivers

import (
	"context"
	"encoding/json"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlannerPVThroughLoadedFerroampAndRelease(t *testing.T) {
	tel := telemetry.NewStore()
	mqtt := &fakeMQTT{}
	d := newFerroampDriverWithConfig(t, tel, mqtt, map[string]any{"pplim_release_w": 15000, "_supports_pv_curtail": true})
	reg := NewRegistry(tel)
	reg.rec["ferroamp"] = &runningDriver{driver: &luaRuntime{d}, cfg: config.Driver{SupportsPVCurtail: true}, generation: 7}
	proof := reg.PVGenerationLimit("ferroamp")
	if proof.Token == "" || proof.MinW != 2 || proof.MaxW != 15000 {
		t.Fatalf("proof=%+v", proof)
	}
	tel.DriverHealthMut("ferroamp").RecordSuccess()
	tel.Update("ferroamp", telemetry.DerPV, -6000, nil, nil)
	tel.Update("ferroamp", telemetry.DerMeter, -5500, nil, nil)
	state := control.NewState(0, 0, "ferroamp")
	state.SupportsPVCurtail = map[string]bool{"ferroamp": true}
	state.PVGenerationLimit = func(name string) mpc.PVCurtailment {
		p := reg.PVGenerationLimit(name)
		return mpc.PVCurtailment{Driver: name, Proof: p.Token, MinW: p.MinW, MaxW: p.MaxW}
	}
	capability := control.PlanningPVCurtailment(state, tel, telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "ferroamp", DerType: telemetry.DerPV}}})
	if !capability.Valid() {
		t.Fatalf("missing Core capability: %+v", capability)
	}
	dir := control.SlotDirectiveFromMPC(mpc.SlotDirective{PVLimitW: 1234, PVCurtailActive: true, PVCurtailment: capability})
	state.SlotDirective = func(time.Time) (control.SlotDirective, bool) { return dir, true }
	send := func(targets []control.CurtailTarget) {
		t.Helper()
		if len(targets) != 1 {
			t.Fatalf("targets=%+v", targets)
		}
		cmd := map[string]any{"action": "curtail", "power_w": targets[0].LimitW}
		if targets[0].LimitW == 0 {
			cmd["action"] = "curtail_disable"
		}
		b, _ := json.Marshal(cmd)
		if err := d.Command(context.Background(), b); err != nil {
			t.Fatal(err)
		}
	}
	mark := len(mqtt.Published())
	send(control.ComputePVCurtail(state, tel))
	if p := publishedSinceMark(mqtt, mark); len(p) != 1 || !strings.Contains(p[0], `"pplim","arg":1234`) {
		t.Fatalf("wrong executed generation cap: %v", p)
	}
	state.SlotDirective = func(time.Time) (control.SlotDirective, bool) { return control.SlotDirective{}, false }
	mark = len(mqtt.Published())
	send(control.ComputePVCurtail(state, tel))
	if p := publishedSinceMark(mqtt, mark); len(p) != 1 || !strings.Contains(p[0], `"pplim","arg":15000`) {
		t.Fatalf("wrong release: %v", p)
	}
	reg.rec["ferroamp"].generation++
	if control.PlanningPVDirectiveValid(state, tel, dir) {
		t.Fatal("old plan survived registry replacement")
	}
}

func TestPVProofUsesLoadedBytesAndEffectiveInit(t *testing.T) {
	mqtt := &fakeMQTT{}
	d := newFerroampDriverWithConfig(t, telemetry.NewStore(), mqtt, map[string]any{"pplim_release_w": 15000, "_supports_pv_curtail": true})
	before := d.PVGenerationLimit()
	original, err := os.ReadFile(d.Path)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "arbitrary-name.lua")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	d.Path = path
	if d.PVGenerationLimit() != before {
		t.Fatal("path name changed loaded proof")
	}
	if err := os.WriteFile(path, append(original, []byte("\n-- edited source\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if d.PVGenerationLimit() != before {
		t.Fatal("disk changes were mistaken for loaded bytes")
	}
	d.mu.Lock()
	err = d.reprobeLocked(context.Background())
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if d.PVGenerationLimit().Token != "" {
		t.Fatal("edited VM inherited old proof")
	}
	// The known file with absent, zero, invalid, or unapproved config never qualifies.
	for _, cfg := range []map[string]any{nil, {"pplim_release_w": 15000}, {"pplim_release_w": 0, "_supports_pv_curtail": true}, {"pplim_release_w": "invalid", "_supports_pv_curtail": true}} {
		q := newFerroampDriverWithConfig(t, telemetry.NewStore(), &fakeMQTT{}, cfg)
		if q.PVGenerationLimit().Token != "" {
			t.Fatalf("unsupported effective config: %v", cfg)
		}
	}
	q := newFerroampDriverWithConfig(t, telemetry.NewStore(), &fakeMQTT{}, map[string]any{"pplim_release_w": 15000, "_supports_pv_curtail": true})
	old := q.PVGenerationLimit()
	if err := q.Init(context.Background(), map[string]any{"pplim_release_w": 12000, "_supports_pv_curtail": true}); err != nil {
		t.Fatal(err)
	}
	if p := q.PVGenerationLimit(); p.Token == old.Token || p.MaxW != 12000 {
		t.Fatalf("init proof not replaced: %+v", p)
	}
}

func TestPVRevocationStopsEVAndLegacyPlanConsumers(t *testing.T) {
	for _, reason := range []string{"generation", "release", "health", "command fault"} {
		t.Run(reason, func(t *testing.T) {
			tel := telemetry.NewStore()
			d := newFerroampDriverWithConfig(t, tel, &fakeMQTT{}, map[string]any{"pplim_release_w": 15000, "_supports_pv_curtail": true})
			reg := NewRegistry(tel)
			reg.rec["pv"] = &runningDriver{driver: &luaRuntime{d}, cfg: config.Driver{SupportsPVCurtail: true}, generation: 7}
			tel.DriverHealthMut("pv").RecordSuccess()
			tel.Update("pv", telemetry.DerPV, -6000, nil, nil)
			lookup := func(name string) mpc.PVCurtailment {
				p := reg.PVGenerationLimit(name)
				return mpc.PVCurtailment{Driver: name, Proof: p.Token, MinW: p.MinW, MaxW: p.MaxW}
			}
			proof := lookup("pv")
			svc := &mpc.Service{PVExecutionAllowed: func(p mpc.PVCurtailment) bool { return control.PVGenerationProofValid(tel, time.Now(), p, lookup) }}
			now := time.Now()
			svc.InstallPlan(mpc.Plan{GeneratedAtMs: now.UnixMilli(), Actions: []mpc.Action{{SlotStartMs: now.Add(-time.Minute).UnixMilli(), SlotLenMin: 15, BatteryW: 500, LoadpointPowerW: map[string]float64{"ev": 2000}, PVCurtailActive: true, PVLimitW: 100}}}, mpc.Params{Mode: mpc.ModeArbitrage, PVCurtailment: proof}, "ev")
			if dir, ok := svc.SlotDirectiveAt(now); !ok || dir.LoadpointEnergyWh["ev"] != 500 {
				t.Fatalf("initial EV directive=%+v %v", dir, ok)
			}
			if _, _, _, ok := svc.SlotAt(now); !ok {
				t.Fatal("initial legacy directive missing")
			}
			switch reason {
			case "generation":
				reg.rec["pv"].generation++
			case "release":
				if err := d.Init(context.Background(), map[string]any{"pplim_release_w": 12000, "_supports_pv_curtail": true}); err != nil {
					t.Fatal(err)
				}
			case "health":
				tel.Update("pv", telemetry.DerPV, 0, nil, nil)
				tel.DriverHealthMut("pv").SetOffline()
			}
			if reason == "command fault" {
				tel.DriverHealthMut("pv").SetCommandFault(true, "refused")
			}
			if _, ok := svc.SlotDirectiveAt(now); ok {
				t.Fatal("EV/battery plan kept revoked capability")
			}
			if _, _, _, ok := svc.SlotAt(now); ok {
				t.Fatal("legacy plan kept revoked capability")
			}
			if snapshot := svc.PlanSnapshot(); !snapshot.Outdated || snapshot.Plan == nil {
				t.Fatalf("history/execution distinction lost: %+v", snapshot)
			}
		})
	}
}
