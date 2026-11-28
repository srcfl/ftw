package mpc

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/battery"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func ptr(v float64) *float64 { return &v }

// The owner's home box (Sungrow SH8.0RT with an SBR096): the inverter holds
// grid import at 4.32 kW while the battery charges from the grid, and the
// battery's DC current stops near 22.4 A when it discharges.
func homeBoxPlateaus() []battery.Plateau {
	return []battery.Plateau{
		{Direction: "charge", Kind: battery.LimitGridImport, Value: 4320, Unit: "W", Seen: 9},
		{Direction: "discharge", Kind: battery.LimitBatteryCurrent, Value: 22.4, Unit: "A", Seen: 12},
	}
}

func TestLearnedLimitsReadTrustedPlateaus(t *testing.T) {
	got := learnedLimits(homeBoxPlateaus(), ptr(188))
	want := LearnedLimits{DischargeW: 22.4 * 188, GridImportW: 4320}
	if got != want {
		t.Fatalf("home box limits %+v, want %+v", got, want)
	}
	// Without the pack's voltage the current limit has no watts.
	if got := learnedLimits(homeBoxPlateaus(), nil); got != (LearnedLimits{GridImportW: 4320}) {
		t.Fatalf("no voltage: %+v", got)
	}
	// A power limit beside the current one: the lower holds.
	both := append(homeBoxPlateaus(), battery.Plateau{Direction: "discharge", Kind: battery.LimitBatteryPower, Value: 4000, Unit: "W"})
	if got := learnedLimits(both, ptr(188)); got.DischargeW != 4000 {
		t.Fatalf("current and power limits: %+v, want 4000 W", got)
	}
	// An export limit is the site's, not the battery's; a limit below what
	// the learner samples, from a wrong voltage, is not one it learned.
	odd := []battery.Plateau{
		{Direction: "discharge", Kind: battery.LimitGridExport, Value: 3000},
		{Direction: "charge", Kind: battery.LimitBatteryCurrent, Value: 20},
		{Direction: "charge", Kind: battery.LimitBatteryPower, Value: 4600},
	}
	if got := learnedLimits(odd, ptr(0.19)); got != (LearnedLimits{ChargeW: 4600}) {
		t.Fatalf("odd plateaus: %+v", got)
	}
	if got := learnedLimits(nil, ptr(188)); got.known() {
		t.Fatalf("no plateaus learned %+v", got)
	}
}

func TestBatteryPackNowReadsTemperatureAndVoltage(t *testing.T) {
	for _, c := range []struct {
		data          string
		tempC, voltsV *float64
	}{
		{`{"temperature_C":12.5,"V":191.2}`, ptr(12.5), ptr(191.2)},
		{`{"temp_c":-3,"v":52}`, ptr(-3), ptr(52)},
		{`{"temperature_C":140,"V":-1}`, nil, nil},
		{`{}`, nil, nil},
		{``, nil, nil},
		{`not json`, nil, nil},
	} {
		tempC, voltsV := batteryPackNow(json.RawMessage(c.data))
		same := func(a, b *float64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
		if !same(tempC, c.tempC) || !same(voltsV, c.voltsV) {
			t.Fatalf("%s: %v %v", c.data, tempC, voltsV)
		}
	}
}

func TestSlotLimitsFollowTheSlotsLoadAndPV(t *testing.T) {
	s := StorageAssetSpec{ID: "sungrow", MaxChargeW: 5000, MaxDischargeW: 5000,
		Learned: LearnedLimits{GridImportW: 4320, DischargeW: 4200}}
	for _, c := range []struct {
		loadW, pvW, charge float64
	}{
		{600, 0, 3720},      // a quiet night
		{1500, 0, 2820},     // a busy night
		{1000, -3000, 5000}, // PV gives the battery its full power
		{5000, 0, 0},        // the house alone takes the import limit
	} {
		charge, discharge := slotLimits([]Slot{{LoadW: c.loadW, PVW: c.pvW}}, []StorageAssetSpec{s})
		if charge[0][0] != c.charge || discharge[0][0] != 4200 {
			t.Fatalf("load %v pv %v: %v / %v, want %v / 4200", c.loadW, c.pvW, charge, discharge, c.charge)
		}
	}
	// A learned limit never raises the configured one.
	s.MaxDischargeW, s.Learned.ChargeW = 3000, 9000
	if charge, discharge := slotLimits([]Slot{{PVW: -9000}}, []StorageAssetSpec{s}); charge[0][0] != 5000 || discharge[0][0] != 3000 {
		t.Fatalf("raised: %v / %v", charge, discharge)
	}
}

// Two batteries behind one site that both learned its grid hold share the
// hold's room by their configured charge limits; together they never get
// more than the room. A battery without a hold keeps its own limits.
func TestBatteriesShareTheSitesGridHold(t *testing.T) {
	storages := []StorageAssetSpec{
		{ID: "a", MaxChargeW: 5000, MaxDischargeW: 5000, Learned: LearnedLimits{GridImportW: 4320}},
		{ID: "b", MaxChargeW: 3000, MaxDischargeW: 3000, Learned: LearnedLimits{GridImportW: 4400}},
		{ID: "c", MaxChargeW: 2000, MaxDischargeW: 2000},
	}
	slots := []Slot{{LoadW: 1000}, {LoadW: 600, PVW: -4000}, {LoadW: 5000}}
	charge, _ := slotLimits(slots, storages)
	// The room is the least hold, 4320 W, less the house: 3320 W, then
	// 7720 W with PV, then nothing.
	for i, room := range []float64{3320, 7720, 0} {
		a, b := charge[0][i], charge[1][i]
		if math.Abs(a-min(5000, room*5/8)) > 1e-9 || math.Abs(b-min(3000, room*3/8)) > 1e-9 || a+b > room+1e-9 {
			t.Fatalf("slot %d: a %v b %v, room %v", i, a, b, room)
		}
		if charge[2][i] != 2000 {
			t.Fatalf("slot %d: the battery without a hold got %v", i, charge[2][i])
		}
	}
	// The fleet's limit for Core's own planner is the shared room too.
	fleet, _ := fleetSlotLimitsW(slots, Params{MaxChargeW: 10000, MaxDischargeW: 10000, Storages: storages})
	if math.Abs(fleet[0]-(3320+2000)) > 1e-9 {
		t.Fatalf("fleet night charge %v, want the room plus the third battery", fleet[0])
	}
	// One battery gets the whole room, exactly.
	one, _ := slotLimits(slots[:1], storages[:1])
	if one[0][0] != 3320 {
		t.Fatalf("one battery: %v", one[0][0])
	}
}

type slotLimitTransport struct{ feature bool }

func (c *slotLimitTransport) RoundTrip(context.Context, []byte) ([]byte, error) {
	features := []string{"champion", "ev_duty", "published_prices"}
	if c.feature {
		features = append(features, "storage_slot_limits")
	}
	return json.Marshal(map[string]any{"name": "ftw-solver", "version": "test", "protocol_version": 1, "features": features})
}
func (*slotLimitTransport) Close() error { return nil }

func preparedRequest(t *testing.T, feature bool, slots []Slot, p Params) []byte {
	t.Helper()
	external := &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: time.Second}, transport: &slotLimitTransport{feature: feature}}
	o := &EnergyplanOptimizer{ExternalOptimizer: external}
	r := external.buildRequest(slots, p)
	if err := o.prepareEnergyplanRequest(context.Background(), &r, slots, p); err != nil {
		t.Fatal(err)
	}
	r.RequestID = "fixed"
	out, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEnergyplanRequestCarriesSlotLimitsOnlyWhenLearned(t *testing.T) {
	slots, p := homeBoxNight()
	baseline := preparedRequest(t, false, slots, p)
	// Nothing learned: the request is the same, whatever the worker takes.
	if got := preparedRequest(t, true, slots, p); !bytes.Equal(got, baseline) {
		t.Fatalf("request changed without learned limits:\n%s\n%s", got, baseline)
	}
	if bytes.Contains(baseline, []byte("_by_slot")) {
		t.Fatalf("unlearned request carries slot limits: %s", baseline)
	}
	learned := p
	learned.Storages = append([]StorageAssetSpec(nil), p.Storages...)
	learned.Storages[0].Learned = LearnedLimits{GridImportW: 4320, DischargeW: 4200}
	// A worker without the feature gets today's request; Core's own
	// planner still holds the limits.
	if got := preparedRequest(t, false, slots, learned); !bytes.Equal(got, baseline) {
		t.Fatalf("an old worker got new fields:\n%s", got)
	}
	var req externalRequest
	if err := json.Unmarshal(preparedRequest(t, true, slots, learned), &req); err != nil {
		t.Fatal(err)
	}
	s := req.Storages[0]
	if len(s.MaxChargeWBySlot) != len(slots) || len(s.MaxDischargeWBySlot) != len(slots) {
		t.Fatalf("slot limits %+v", s)
	}
	for i, slot := range slots {
		want := min(5000, max(0, 4320-slot.LoadW-slot.PVW))
		if math.Abs(s.MaxChargeWBySlot[i]-want) > 1e-9 || s.MaxDischargeWBySlot[i] != 4200 {
			t.Fatalf("slot %d: %v / %v, want %v / 4200", i, s.MaxChargeWBySlot[i], s.MaxDischargeWBySlot[i], want)
		}
	}
	if s.MaxChargeW != 5000 || s.MaxDischargeW != 5000 {
		t.Fatalf("scalar limits changed: %+v", s)
	}
}

func TestOnlineFleetParamsReadsLearnedLimitsAtTheBatteryTemperature(t *testing.T) {
	tel := telemetry.NewStore()
	soc := 0.3
	tel.Update("sungrow", telemetry.DerBattery, 0, &soc, json.RawMessage(`{"temperature_C":14,"V":190}`))
	tel.DriverHealthMut("sungrow").RecordSuccess()
	var asked *float64
	s := &Service{Tele: tel, LearnedBatteryLimits: func(driver string, tempC *float64) []battery.Plateau {
		if driver != "sungrow" {
			t.Fatalf("asked for %q", driver)
		}
		asked = tempC
		return homeBoxPlateaus()
	}}
	p, ok := s.onlineFleetParams(Params{}, []BatteryFleetMember{{Driver: "sungrow", CapacityWh: 9600, MaxChargeW: 5000, MaxDischargeW: 5000}}, 0)
	if !ok || len(p.Storages) != 1 {
		t.Fatalf("params %+v", p)
	}
	if asked == nil || *asked != 14 {
		t.Fatalf("asked at %v, want 14 °C", asked)
	}
	if got := p.Storages[0].Learned; got != (LearnedLimits{DischargeW: 22.4 * 190, GridImportW: 4320}) {
		t.Fatalf("learned %+v", got)
	}
	// Without the hook the planner keeps the configured limits alone.
	s.LearnedBatteryLimits = nil
	if p, _ := s.onlineFleetParams(Params{}, []BatteryFleetMember{{Driver: "sungrow", CapacityWh: 9600, MaxChargeW: 5000, MaxDischargeW: 5000}}, 0); p.Storages[0].Learned.known() {
		t.Fatalf("learned without a model: %+v", p.Storages[0].Learned)
	}
}

func TestWorkerPlanPastItsSlotLimitsIsRejected(t *testing.T) {
	request := externalRequest{Storages: []externalStorage{{ID: "b", MaxChargeW: 5000, MaxDischargeW: 5000,
		MaxChargeWBySlot: []float64{3000, 3000}, MaxDischargeWBySlot: []float64{4200, 4200}}}}
	plan := func(w ...float64) externalPlan {
		var p externalPlan
		for _, v := range w {
			p.Actions = append(p.Actions, externalAction{StoragePowerW: map[string]float64{"b": v}})
		}
		return p
	}
	if err := validateStorageSlotLimits(request, plan(3001.5, -4201.5)); err != nil {
		t.Fatalf("within 2 W: %v", err)
	}
	if err := validateStorageSlotLimits(request, plan(3003, 0)); err == nil {
		t.Fatal("accepted a charge past its slot limit")
	}
	if err := validateStorageSlotLimits(request, plan(0, -4203)); err == nil {
		t.Fatal("accepted a discharge past its slot limit")
	}
	request.Storages[0].MaxChargeWBySlot, request.Storages[0].MaxDischargeWBySlot = nil, nil
	if err := validateStorageSlotLimits(request, plan(5000, -5000)); err != nil {
		t.Fatalf("no slot limits: %v", err)
	}
}

// homeBoxNight is eight cheap night hours with 0.6-1.5 kW of house load,
// cheapest at three to five, then eight dear evening hours of 2 kW with a
// peak at the third, on the home box's 9.6 kWh battery at 10 %. Its
// configured limits are 5 kW both ways.
func homeBoxNight() ([]Slot, Params) {
	start := time.Now().Truncate(time.Hour).Add(time.Hour).UnixMilli()
	nightLoad := []float64{600, 800, 1500, 1200, 700, 900, 1400, 1000}
	nightPrice := []float64{40, 35, 30, 12, 10, 14, 32, 38}
	slots := make([]Slot, 16)
	for i := range slots {
		slots[i] = Slot{StartMs: start + int64(i)*3600000, LenMin: 60, PriceOre: 300, SpotOre: 200, LoadW: 2000,
			Limits: PowerLimits{MaxImportW: 17000, MaxExportW: 17000}}
		if i < len(nightLoad) {
			slots[i].PriceOre, slots[i].SpotOre, slots[i].LoadW = nightPrice[i], nightPrice[i]-10, nightLoad[i]
		}
		if i == 10 {
			slots[i].PriceOre, slots[i].SpotOre = 600, 500
		}
	}
	p := Params{Mode: ModeArbitrage, CapacityWh: 9600, SoCMin: 0.05, SoCMax: 0.95, InitialSoC: 0.1,
		MaxChargeW: 5000, MaxDischargeW: 5000, ChargeEfficiency: 0.95, DischargeEfficiency: 0.95,
		SoCLevels: 49, ActionLevels: 21,
		Storages: []StorageAssetSpec{{ID: "sungrow", CapacityWh: 9600, InitialEnergyWh: 960, MinEnergyWh: 480,
			MaxEnergyWh: 9120, MaxChargeW: 5000, MaxDischargeW: 5000, ChargeEfficiency: 0.95, DischargeEfficiency: 0.95}}}
	return slots, p
}

// checkHomeBoxNight holds a plan of homeBoxNight to the box's learned
// limits and returns how many night hours charge.
func checkHomeBoxNight(t *testing.T, slots []Slot, p Params, plan Plan, limited bool) int {
	t.Helper()
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	hours, nightWh := 0, 0.0
	powers := make([]float64, len(plan.Actions))
	limitsC, limitsD := slotLimits(slots, p.Storages)
	for i, a := range plan.Actions {
		charge, discharge := limitsC[0][i], limitsD[0][i]
		if limited && (a.BatteryW > charge+2 || a.BatteryW < -discharge-2) {
			t.Fatalf("slot %d: battery %.0f W outside -%.0f..%.0f W", i, a.BatteryW, discharge, charge)
		}
		if i < 8 && a.BatteryW > 1 {
			hours++
			nightWh += a.BatteryW * slots[i].DurationHours()
		}
		powers[i] = math.Round(a.BatteryW)
	}
	t.Logf("learned limits %v: %d night hours charge %.0f Wh; battery W %v; cost %.1f ore", limited, hours, nightWh, powers, plan.TotalCostOre)
	return hours
}

// On the home box the inverter holds grid import at 4.32 kW while the
// battery charges, so a night hour gives the battery 4.32 kW less the house:
// 2.8-3.7 kW, not 5 kW. Core's planner then charges over more night hours,
// never past what the battery can take, and discharges at most 4.2 kW.
func TestCoreDPPlansTheHomeBoxNightWithinItsLearnedLimits(t *testing.T) {
	slots, p := homeBoxNight()
	free := Optimize(slots, p)
	freeHours := checkHomeBoxNight(t, slots, p, free, false)
	learned := p
	learned.Storages = append([]StorageAssetSpec(nil), p.Storages...)
	learned.Storages[0].Learned = LearnedLimits{GridImportW: 4320, DischargeW: 4200}
	limited := Optimize(slots, learned)
	hours := checkHomeBoxNight(t, slots, learned, limited, true)
	if hours <= freeHours {
		t.Fatalf("charges in %d night hours with the limits, %d without", hours, freeHours)
	}
	if !planExceedsLearnedLimits(slots, learned, free) {
		t.Fatal("the plan without the limits fits them: the test proves nothing")
	}
}

func planExceedsLearnedLimits(slots []Slot, p Params, plan Plan) bool {
	limitsC, limitsD := slotLimits(slots, p.Storages)
	for i, a := range plan.Actions {
		charge, discharge := limitsC[0][i], limitsD[0][i]
		if a.BatteryW > charge+2 || a.BatteryW < -discharge-2 {
			return true
		}
	}
	return false
}

// The same night through the worker: it gets the slot limits, plans within
// them, and Core accepts its plan.
func TestNativeEnergyplanPlansTheHomeBoxNightWithinItsLearnedLimits(t *testing.T) {
	binary := os.Getenv("FTW_NATIVE_SOLVER")
	if binary == "" {
		t.Skip("run make native-solver-test to include the Rust worker")
	}
	o, err := NewEnergyplanOptimizer(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := o.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !optimizerHasFeature(info, "storage_slot_limits") {
		t.Fatalf("the bundled worker %s does not take storage_slot_limits", info.Version)
	}
	slots, p := homeBoxNight()
	free, err := o.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatal(err)
	}
	freeHours := checkHomeBoxNight(t, slots, p, free, false)
	learned := p
	learned.Storages = append([]StorageAssetSpec(nil), p.Storages...)
	learned.Storages[0].Learned = LearnedLimits{GridImportW: 4320, DischargeW: 4200}
	limited, err := o.Optimize(context.Background(), slots, learned)
	if err != nil {
		t.Fatal(err)
	}
	if limited.Solver == nil || limited.Solver.Fallback {
		t.Fatalf("solver %+v; want the worker's own plan", limited.Solver)
	}
	var req externalRequest
	if err := json.Unmarshal(limited.OptimizerInput, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Storages[0].MaxChargeWBySlot) != len(slots) {
		t.Fatalf("the worker got no slot limits: %+v", req.Storages[0])
	}
	hours := checkHomeBoxNight(t, slots, learned, limited, true)
	if hours <= freeHours || !planExceedsLearnedLimits(slots, learned, free) {
		t.Fatalf("charges in %d night hours with the limits, %d without", hours, freeHours)
	}
}

// A car charging in the same night slot takes import the battery then cannot
// get, and the slot limits leave the car out. That makes the plan
// optimistic for the battery only: the plan still keeps the site's import
// limit and the owner's hourly import ceiling, which ValidatePlan holds it to.
func TestACarBesideTheGridHoldKeepsCoresImportLimits(t *testing.T) {
	slots, p := homeBoxNight()
	for i := range slots {
		slots[i].Limits.MaxImportW = 11000
	}
	p.Storages[0].Learned = LearnedLimits{GridImportW: 4320, DischargeW: 4200}
	p.Loadpoint = &LoadpointSpec{ID: "car", CapacityWh: 60000, Levels: 21, SoCMax: 1, InitialSoC: .3, PluggedIn: true,
		TargetSoC: .5, TargetSlotIdx: 7, MaxChargeW: 11000, AllowedStepsW: []float64{0, 4140, 6900}, ChargeEfficiency: .9}
	p.ImportMeanW = 9000
	for _, s := range slots {
		p.ImportHours = append(p.ImportHours, ImportHour{StartMs: s.StartMs, EndMs: s.StartMs + 3600000})
	}
	plan := Optimize(slots, p)
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatalf("Core's limits rejected the plan: %v", err)
	}
	charge, discharge := slotLimits(slots, p.Storages)
	both := 0
	for i, a := range plan.Actions {
		if a.BatteryW > charge[0][i]+2 || a.BatteryW < -discharge[0][i]-2 {
			t.Fatalf("slot %d: battery %.0f W outside its slot limits", i, a.BatteryW)
		}
		if a.GridW > slots[i].Limits.MaxImportW+solverGridLimitToleranceW || a.GridW > p.ImportMeanW+solverGridLimitToleranceW {
			t.Fatalf("slot %d: grid %.0f W past the import limits", i, a.GridW)
		}
		if a.BatteryW > 1 && a.LoadpointW > 1 {
			both++
		}
	}
	t.Logf("%d night slots charge the car and the battery together; battery W %v", both, func() []float64 {
		out := make([]float64, len(plan.Actions))
		for i, a := range plan.Actions {
			out[i] = math.Round(a.BatteryW)
		}
		return out
	}())
	if both == 0 {
		t.Fatal("no slot charges the car and the battery together: the case is not covered")
	}
	if plan.Actions[7].LoadpointSoC+1e-9 < .5 {
		t.Fatalf("the car missed its goal: %.3f", plan.Actions[7].LoadpointSoC)
	}
}

// failingWorker announces storage_slot_limits, keeps each request and
// answers it with an error code.
type failingWorker struct {
	code     string
	requests []externalRequest
}

func (f *failingWorker) RoundTrip(_ context.Context, payload []byte) ([]byte, error) {
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &head)
	if head.Type == "handshake" {
		return json.Marshal(map[string]any{"name": "ftw-solver", "version": "test", "protocol_version": 1,
			"features": []string{"champion", "ev_duty", "published_prices", "storage_slot_limits"}})
	}
	var request externalRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, err
	}
	f.requests = append(f.requests, request)
	return json.Marshal(map[string]any{"schema_version": 1, "request_id": request.RequestID, "ok": false,
		"error": map[string]string{"code": f.code, "message": "test"}})
}
func (*failingWorker) Close() error { return nil }

// When the worker fails on a request with slot limits, Core's own planner
// takes over within the same limits: the battery never gets a plan the
// worker was told it cannot do.
func TestWorkerFailureFallsBackWithinTheSameSlotLimits(t *testing.T) {
	for _, code := range []string{"numerical_failure", "deadline_exceeded"} {
		t.Run(code, func(t *testing.T) {
			svc := shadowTestService(t)
			svc.Defaults.Mode = ModeArbitrage
			svc.Defaults.SoCLevels, svc.Defaults.ActionLevels = 21, 21
			svc.UpdateBatteryFleet([]BatteryFleetMember{{Driver: "battery", CapacityWh: 10000, MaxChargeW: 5000, MaxDischargeW: 5000}}, 10000, 5000, 5000)
			svc.Tele = telemetry.NewStore()
			soc := .2
			svc.Tele.Update("battery", telemetry.DerBattery, 0, &soc, json.RawMessage(`{"V":190}`))
			svc.Tele.DriverHealthMut("battery").RecordSuccess()
			svc.LearnedBatteryLimits = func(string, *float64) []battery.Plateau {
				return []battery.Plateau{
					{Direction: "charge", Kind: battery.LimitGridImport, Value: 2000, Unit: "W"},
					{Direction: "discharge", Kind: battery.LimitBatteryCurrent, Value: 12, Unit: "A"},
				}
			}
			worker := &failingWorker{code: code}
			external := &ExternalOptimizer{cfg: ExternalOptimizerConfig{Timeout: 2 * time.Second}, transport: worker, timeBudget: energyplanTimeBudget}
			o := &EnergyplanOptimizer{ExternalOptimizer: external}
			external.prepareRequest = o.prepareEnergyplanRequest
			svc.Optimizer = o
			plan := svc.Replan(context.Background())
			if plan == nil || plan.Solver == nil || !plan.Solver.Fallback {
				t.Fatalf("no Core fallback: %+v", plan)
			}
			if len(worker.requests) != 1 || len(worker.requests[0].Storages[0].MaxChargeWBySlot) == 0 ||
				len(worker.requests[0].Storages[0].MaxDischargeWBySlot) == 0 {
				t.Fatalf("the worker got no slot limits: %+v", worker.requests)
			}
			sent := worker.requests[0].Storages[0]
			if err := ValidatePlan(svc.lastSlots, svc.lastParams, plan); err != nil {
				t.Fatal(err)
			}
			atLimit := false
			for i, a := range plan.Actions {
				if a.BatteryW > sent.MaxChargeWBySlot[i]+2 || a.BatteryW < -sent.MaxDischargeWBySlot[i]-2 {
					t.Fatalf("slot %d: fallback battery %.0f W outside the limits sent, -%.0f..%.0f W",
						i, a.BatteryW, sent.MaxDischargeWBySlot[i], sent.MaxChargeWBySlot[i])
				}
				atLimit = atLimit || math.Abs(a.BatteryW-sent.MaxChargeWBySlot[i]) < 2 && a.BatteryW > 1 ||
					math.Abs(a.BatteryW+sent.MaxDischargeWBySlot[i]) < 2 && a.BatteryW < -1
			}
			if !atLimit {
				t.Fatalf("the fallback never reached a slot limit, so the case shows nothing: %+v", plan.Actions)
			}
			svc.shadowWG.Wait()
		})
	}
}
