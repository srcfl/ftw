package control

// Golden replay of ComputeDispatch.
//
// testdata/golden/ holds 611 recorded dispatch ticks: nine families covering
// the reactive modes, all four planner modes, fuse pressure in both
// directions, degraded siblings, EV and battery-boost reserves, the slew
// limiter at rates a site actually runs, the three early exits meeting a
// protection that binds, and the incident scenarios that
// forecast_scenarios_test.go already names. Each record stores the inputs to
// one ComputeDispatch call — telemetry, State, slot directive, driver
// capacities, fuse ceiling — next to the per-driver targets and clamp
// attributions that call returned. This test rebuilds each scenario, calls
// the real ComputeDispatch, and checks the answer still matches.
//
// What the corpus is NOT: a statement of what dispatch SHOULD do. It is what
// dispatch DID do at the commit each file records in its ftw_commit — a
// family recorded later carries a later one — clamps, quirks and all. Nothing
// here was reviewed as correct, with one exception: early_exit_protections
// was recorded after the law it covers was fixed, and its doc comment in
// golden_dump_test.go says so. The corpus's value is that it makes any change
// to dispatch visible and specific, which the unit tests around it cannot: they
// pin the cases somebody thought to write down, and dispatch is a long
// function whose paths interact.
//
// So a diff is a question, not a verdict. When this test fails, it is telling
// you which scenarios your change moved and by how many watts. Read them. If
// they are the ones you meant to move, and they moved the way you meant, the
// corpus is stale and you should re-record it — from the go/ directory:
//
//	FTW_GOLDEN_DUMP=1 go test ./internal/control/ -run TestGoldenDump -v
//
// then commit testdata/golden/ with your change, and put the summary of what
// moved in the pull request. Reviewers get to see the behaviour change in
// watts instead of inferring it from a diff of control flow. If scenarios you
// did not expect also moved, you have found something before your users did.
//
// Two things never fix a failure here: deleting the record, and widening the
// tolerance. The tolerance remains 0.01 W absolute so the corpus still reports
// small numeric changes without making the test depend on exact float output;
// golden scenarios now use one injected clock instant for the slot and the
// dispatch calculation. Anything above 0.01 W is a real change in behaviour.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	// goldenCorpusDir is both where the replay reads and where the recorder
	// writes by default. Relative to this package; `go test` runs here.
	goldenCorpusDir = "testdata/golden"

	// goldenToleranceW is the absolute watt tolerance for target and
	// attribution magnitudes. See the file comment: never raise it to make a
	// failure go away.
	goldenToleranceW = 0.01

	// goldenExpectedRecords guards against a half-written re-recording
	// silently shrinking the net.
	goldenExpectedRecords = 611

	// goldenSlewBindingMarginW is how far a record's targets have to move
	// when the slew limiter is switched off before that record counts as
	// one where the limiter genuinely bound. Small enough to catch a
	// tightly-anchored tick, large enough that float noise cannot qualify.
	goldenSlewBindingMarginW = 50

	// goldenMinSlewBindingRecords is the floor on how many such records the
	// corpus holds. The corpus that shipped in #790 had 391 of 435 records
	// running at SlewRateW 10 kW or 100 kW — the limiter was switched off
	// in all but name, and none of its interactions were covered. This
	// number is the tripwire for that happening again; it sits well under
	// what the slew_limiter family records today, so ordinary drift in
	// dispatch does not trip it and a lost family does.
	goldenMinSlewBindingRecords = 60

	// goldenSlewRealisticRateW is the loosest rate that still counts as
	// production-realistic: control.NewState defaults to 500 W and
	// config.Defaults to 3000 W, so anything at or under 3000 W is a rate
	// a site actually runs.
	goldenSlewRealisticRateW = 3000

	// goldenMaxReported caps how many changed records get a full diff, so a
	// change that moves everything still produces readable output.
	goldenMaxReported = 20
)

// goldenFamilyNames is the list the replay reads and the recorder writes. The
// recorder fails if the two disagree.
var goldenFamilyNames = []string{
	"incident_scenarios",
	"targeted_invariants",
	"seeded_reactive",
	"seeded_planner",
	"seeded_fuse",
	"seeded_siblings",
	"seeded_ev_boost",
	"slew_limiter",
	"early_exit_protections",
}

// readGoldenCorpus loads every family from dir and returns the records in
// file order plus an index by scenario ID.
func readGoldenCorpus(t *testing.T, dir string) ([]goldenRecord, map[string]goldenRecord) {
	t.Helper()
	var all []goldenRecord
	byID := map[string]goldenRecord{}
	for _, name := range goldenFamilyNames {
		path := filepath.Join(dir, name+".json")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read golden family %s: %v", path, err)
		}
		var gf goldenFile
		if err := json.Unmarshal(b, &gf); err != nil {
			t.Fatalf("parse golden family %s: %v", path, err)
		}
		if gf.RecordCount != len(gf.Records) {
			t.Errorf("%s: record_count %d but %d records present",
				path, gf.RecordCount, len(gf.Records))
		}
		for _, r := range gf.Records {
			if prev, dup := byID[r.ScenarioID]; dup {
				t.Errorf("duplicate scenario_id %q (already seen in this corpus with %d targets)",
					r.ScenarioID, len(prev.PerDriverTargets))
			}
			byID[r.ScenarioID] = r
			all = append(all, r)
		}
	}
	return all, byID
}

// TestGoldenCorpusReplay is the regression gate: every recorded scenario, back
// through the real ComputeDispatch, compared to what it answered before.
func TestGoldenCorpusReplay(t *testing.T) {
	all, _ := readGoldenCorpus(t, goldenCorpusDir)
	if len(all) != goldenExpectedRecords {
		t.Errorf("golden corpus has %d records, expected %d — was it re-recorded with a changed scenario set?",
			len(all), goldenExpectedRecords)
	}

	var changed, reported int
	for _, want := range all {
		got := runGoldenScenario(goldenScenario{
			ID:     want.ScenarioID,
			Seed:   want.Seed,
			Inputs: want.Inputs,
		})
		lines := diffGoldenRecord(want, got)
		if len(lines) == 0 {
			continue
		}
		changed++
		if reported < goldenMaxReported {
			reported++
			t.Errorf("dispatch changed for %s:\n%s", want.ScenarioID, strings.Join(lines, "\n"))
		}
	}
	if changed > reported {
		t.Errorf("%d more records changed but were not printed; raise goldenMaxReported or narrow the run with -run",
			changed-reported)
	}
	if changed > 0 {
		t.Logf("%d of %d golden records changed. If every one is intended, re-record with "+
			"FTW_GOLDEN_DUMP=1 go test ./internal/control/ -run TestGoldenDump and commit testdata/golden/.",
			changed, len(all))
	}
}

// TestGoldenCorpusCoverage checks the committed corpus still exercises the
// mechanisms it was built to hold still, so a re-recording that quietly loses
// a clamp path is caught at the source rather than years later.
func TestGoldenCorpusCoverage(t *testing.T) {
	all, byID := readGoldenCorpus(t, goldenCorpusDir)
	assertGoldenCoverage(t, all, byID)
}

// assertGoldenCoverage is shared with the recorder, which runs it against what
// it just wrote.
func assertGoldenCoverage(t *testing.T, all []goldenRecord, byID map[string]goldenRecord) {
	t.Helper()

	// 1. Floored-sibling reallocation: the blocked battery parks, the capable
	//    one takes the load.
	if fs, ok := byID["targeted/floored_sibling_discharge"]; !ok {
		t.Error("missing targeted/floored_sibling_discharge")
	} else {
		var parkedOK, siblingOK bool
		for _, tg := range fs.PerDriverTargets {
			if tg.Driver == "ferroamp" && math.Abs(tg.TargetW) <= 1 && tg.Clamped {
				parkedOK = true
			}
			if tg.Driver == "sungrow" && tg.TargetW < -1 {
				siblingOK = true
			}
		}
		if !parkedOK || !siblingOK {
			t.Errorf("floored-sibling reallocation not exercised: parked=%v sibling=%v targets=%v",
				parkedOK, siblingOK, fs.PerDriverTargets)
		}
	}

	// 2. forceFuseDischarge from idle mode.
	if ff, ok := byID["targeted/force_fuse_discharge_idle"]; !ok {
		t.Error("missing targeted/force_fuse_discharge_idle")
	} else if sumGoldenTargets(ff) >= -100 {
		t.Errorf("forceFuseDischarge not exercised: idle-mode over-fuse record sum=%.0f targets=%v",
			sumGoldenTargets(ff), ff.PerDriverTargets)
	}

	// 3. Per-phase overage: aggregate under the fuse, one phase over it.
	if pp, ok := byID["targeted/per_phase_overage_import"]; !ok {
		t.Error("missing targeted/per_phase_overage_import")
	} else {
		if pp.ClampAttributions.PerPhaseOverageW <= 0 {
			t.Errorf("per-phase overage not attributed: %+v", pp.ClampAttributions)
		}
		if sumGoldenTargets(pp) >= 0 {
			t.Errorf("per-phase import overage should force discharge; sum=%.0f", sumGoldenTargets(pp))
		}
	}

	// 4. Plan/exec sign floor.
	if sf, ok := byID["targeted/plan_sign_floor_discharge_slot"]; !ok {
		t.Error("missing targeted/plan_sign_floor_discharge_slot")
	} else if !sf.ClampAttributions.PlanSignFloorZeroed || len(sf.PerDriverTargets) == 0 {
		t.Errorf("plan sign floor not exercised: attr=%+v targets=%v",
			sf.ClampAttributions, sf.PerDriverTargets)
	}

	assertGoldenSlewCoverage(t, all, byID)
	assertGoldenEarlyExitCoverage(t, all, byID)

	// Generic scans across the whole corpus.
	var nFuseSaturated, nHoldLatched, nPerPhase, nStale, nBlocked, nEmpty int
	for _, r := range all {
		if r.ClampAttributions.FuseSaturated {
			nFuseSaturated++
		}
		if r.ClampAttributions.FuseHoldLatched {
			nHoldLatched++
		}
		if r.ClampAttributions.PerPhaseOverageW > 0 {
			nPerPhase++
		}
		if r.ClampAttributions.PlanStale {
			nStale++
		}
		if len(r.PerDriverTargets) == 0 {
			nEmpty++
		}
		for _, b := range r.Inputs.Batteries {
			if b.DischargeBlocked || b.ChargeBlocked {
				nBlocked++
				break
			}
		}
	}
	t.Logf("coverage: fuse_saturated=%d hold_latched=%d per_phase=%d stale_plan=%d blocked_batteries=%d no_dispatch=%d",
		nFuseSaturated, nHoldLatched, nPerPhase, nStale, nBlocked, nEmpty)
	if nFuseSaturated == 0 || nHoldLatched == 0 || nPerPhase == 0 || nStale == 0 {
		t.Error("coverage gap: one of the generic families never fired its clamp")
	}
}

// assertGoldenSlewCoverage holds the corpus to the slew limiter.
//
// The limiter is the one stage in dispatch that raises a target's magnitude
// without a safety reason to: the fuse guard and the boost reserve shrink
// toward zero, forceFuseDischarge enlarges only to stop a trip, and slew
// enlarges because the battery is not where the plan wants it yet. It also
// anchors on the measured output rather than the previous command, so it is
// the one clamp whose answer depends on what the hardware is doing this
// second. A corpus that runs it at 100 kW is a corpus that never sees it.
//
// The records named here are the shapes that must not quietly leave the
// corpus. Where the assertion states an outcome, that outcome is a safety
// invariant somebody would want back if it broke. Where it only states that
// the shape is present, dispatch's behaviour on that shape is a known bug and
// the record exists to make the fix visible — see the family's doc comment in
// golden_dump_test.go.
func assertGoldenSlewCoverage(t *testing.T, all []goldenRecord, byID map[string]goldenRecord) {
	t.Helper()

	// A fuse overflow cannot wait for the ramp: forceFuseDischarge runs
	// after the limiter for that reason. Hold the ordering — the commanded
	// discharge is several slew steps away from where the battery is.
	if fr, ok := byID["slew/fuse_relief_beats_smoothing_500"]; !ok {
		t.Error("missing slew/fuse_relief_beats_smoothing_500")
	} else {
		move := math.Abs(sumGoldenTargets(fr) - goldenLiveBatteryW(fr))
		if sumGoldenTargets(fr) >= 0 || move <= 2*fr.Inputs.State.SlewRateW {
			t.Errorf("record no longer shows fuse relief out-ranking the ramp: target sum %.0f W is %.0f W "+
				"from the live %.0f W anchor, which is inside two %.0f W slew steps",
				sumGoldenTargets(fr), move, goldenLiveBatteryW(fr), fr.Inputs.State.SlewRateW)
		}
	}

	// planner_self planned an export this slot. The snap-to-zero carve-out
	// in the slew loop is what stops the limiter walking a charging battery
	// back into that surplus one step at a time.
	if cz, ok := byID["slew/carveout_export_surplus_gate_snaps_to_zero"]; !ok {
		t.Error("missing slew/carveout_export_surplus_gate_snaps_to_zero")
	} else if math.Abs(sumGoldenTargets(cz)) > 1 {
		t.Errorf("snap-to-zero carve-out not exercised: target sum %.0f W from a live %.0f W anchor",
			sumGoldenTargets(cz), goldenLiveBatteryW(cz))
	}

	// The discharge side of the same problem IS floored after the limiter,
	// by floorNegativeTargets. This record is the reference the charge-side
	// records below are compared against.
	if nd, ok := byID["slew/no_self_discharge_charge_slot_live_discharging"]; !ok {
		t.Error("missing slew/no_self_discharge_charge_slot_live_discharging")
	} else if sumGoldenTargets(nd) < -1 {
		t.Errorf("noSelfDischarge floor not exercised: target sum %.0f W on a charge slot",
			sumGoldenTargets(nd))
	}

	// The charge side is not floored. These records hold the shape — a
	// charge block armed, a battery live on the charge side, and a rate at
	// which one step does not cover the anchor — without asserting the
	// watts, which are today's bug and tomorrow's fix.
	for _, id := range []string{
		"slew/bug_no_self_charge_idle_slot_leak_500",
		"slew/bug_no_self_charge_stale_plan_leak",
		"slew/blocked_sibling_slew_reopens_parked_charge",
	} {
		r, ok := byID[id]
		if !ok {
			t.Errorf("missing %s — it is the corpus's record of the slew charge leak", id)
			continue
		}
		live := goldenLiveBatteryW(r)
		if live <= r.Inputs.State.SlewRateW {
			t.Errorf("%s no longer exercises the leak: live battery %.0f W is within one %.0f W step, so the limiter reaches 0 W anyway",
				id, live, r.Inputs.State.SlewRateW)
		}
	}

	// The corpus as a whole: how many records does the limiter actually
	// change the answer on, and does it bind at a rate a site would run.
	var binding, bindingAtRealisticRate int
	var largest float64
	var largestID string
	for _, r := range all {
		effect := goldenSlewEffectW(r)
		if effect <= goldenSlewBindingMarginW {
			continue
		}
		binding++
		if r.Inputs.State.SlewRateW <= goldenSlewRealisticRateW {
			bindingAtRealisticRate++
		}
		if effect > largest {
			largest, largestID = effect, r.ScenarioID
		}
	}
	t.Logf("slew coverage: %d records bind (>%0.f W), %d of them at a production-realistic rate (<= %.0f W/tick); largest effect %.0f W on %s",
		binding, float64(goldenSlewBindingMarginW), bindingAtRealisticRate,
		float64(goldenSlewRealisticRateW), largest, largestID)
	if binding < goldenMinSlewBindingRecords {
		t.Errorf("coverage gap: only %d records where the slew limiter binds by more than %.0f W, want at least %d. "+
			"A corpus recorded at unreachable slew rates cannot see the limiter at all — re-record the slew_limiter family.",
			binding, float64(goldenSlewBindingMarginW), goldenMinSlewBindingRecords)
	}
	if bindingAtRealisticRate < goldenMinSlewBindingRecords {
		t.Errorf("coverage gap: only %d binding records run at <= %.0f W/tick; the rest bind only at rates no site configures",
			bindingAtRealisticRate, float64(goldenSlewRealisticRateW))
	}
}

// assertGoldenEarlyExitCoverage holds the corpus to the three branches of
// ComputeDispatch that walk away from a cycle: idle mode, the holdoff window
// and the reactive deadband.
//
// The corpus before this family had 33 records returning no targets and not
// one of them could have caught a protection going missing at an early exit:
// the 8 deadband records all ran with site_fuse_amps=0 and
// peak_import_ceiling_w=0, so nothing was over any limit to defend. That is
// the gap these assertions exist to keep closed. Every claim below is a
// safety outcome, not a quirk — this family was recorded after #803 fixed the
// deadband exit, so a record that moves is a protection that stopped
// protecting.
//
// Each exit needs both halves. Without a firing record the corpus cannot see
// a protection disappear; without a quiet one it cannot see the fuse-saver
// start discharging on ticks where nothing binds, and an over-fire drains the
// pack just as surely as an under-fire trips the breaker.
func assertGoldenEarlyExitCoverage(t *testing.T, all []goldenRecord, byID map[string]goldenRecord) {
	t.Helper()

	for _, exit := range []string{"deadband", "holdoff"} {
		prefix := "early_exit/" + exit + "_"
		var fired, quiet int
		for _, r := range all {
			if !strings.HasPrefix(r.ScenarioID, prefix) {
				continue
			}
			if len(r.PerDriverTargets) == 0 {
				quiet++
				continue
			}
			fired++
			// A fuse-saver discharge, and nothing else, is what an
			// early exit is allowed to produce: every target marked
			// clamped so the dispatch trace names the saver, and the
			// fleet total on the discharge side.
			for _, tg := range r.PerDriverTargets {
				if !tg.Clamped || tg.TargetW > 0 {
					t.Errorf("%s: an early exit may only command clamped discharge, got %+.0f W clamped=%v on %s",
						r.ScenarioID, tg.TargetW, tg.Clamped, tg.Driver)
				}
			}
			if sum := sumGoldenTargets(r); sum > -1 {
				t.Errorf("%s: recorded as firing but the fleet total is %+.0f W", r.ScenarioID, sum)
			}
		}
		if fired == 0 {
			t.Errorf("coverage gap: no %s record where a protection binds — the exit is unguarded again", exit)
		}
		if quiet == 0 {
			t.Errorf("coverage gap: no %s record where nothing binds; a corpus of only-firing records cannot see an over-fire", exit)
		}
	}

	// Idle is held to a different law than the two exits above, and the
	// difference is the point of it: it does not leave the cycle, so it is
	// never allowed to be silent. Its quiet case is a commanded zero — the
	// hold that stops a battery keeping the last setpoint it accepted, or
	// (Ferroamp, 2026-06-10) letting a forced mode expire back into charging
	// from the grid. Its firing case is the fuse-saver, which must still
	// reach past that hold. A record with no targets at all would mean idle
	// had gone back to trusting the vendor, so it is the one thing this
	// cannot accept.
	var idleHeld, idleFired int
	for _, r := range all {
		if !strings.HasPrefix(r.ScenarioID, "early_exit/idle_") {
			continue
		}
		if len(r.PerDriverTargets) == 0 {
			t.Errorf("%s: idle commanded nothing — the fleet is holding its last setpoint, not stopped", r.ScenarioID)
			continue
		}
		if sum := sumGoldenTargets(r); sum < -1 {
			idleFired++
			for _, tg := range r.PerDriverTargets {
				if !tg.Clamped || tg.TargetW > 0 {
					t.Errorf("%s: the fuse-saver may only command clamped discharge, got %+.0f W clamped=%v on %s",
						r.ScenarioID, tg.TargetW, tg.Clamped, tg.Driver)
				}
			}
			continue
		}
		idleHeld++
		for _, tg := range r.PerDriverTargets {
			if math.Abs(tg.TargetW) > goldenToleranceW {
				t.Errorf("%s: idle holds at 0 W, got %+.2f W on %s", r.ScenarioID, tg.TargetW, tg.Driver)
			}
		}
	}
	if idleFired == 0 {
		t.Error("coverage gap: no idle record where a protection binds — the fuse-saver no longer reaches past the hold")
	}
	if idleHeld == 0 {
		t.Error("coverage gap: no idle record where nothing binds; a corpus of only-firing records cannot see an over-fire")
	}

	// The aggregate ceiling. The operator's tariff peak sits below the fuse,
	// the meter sits above the peak, and the battery bridges the difference
	// exactly — over-discharging here costs cycles, under-discharging costs
	// the peak charge the ceiling exists to avoid.
	if r, ok := byID["early_exit/deadband_peak_ceiling_binds"]; !ok {
		t.Error("missing early_exit/deadband_peak_ceiling_binds")
	} else {
		want := -(r.Inputs.GridW - r.ClampAttributions.EffectiveImportCeilingW)
		if math.Abs(sumGoldenTargets(r)-want) > 1 {
			t.Errorf("deadband peak-ceiling bridge = %.0f W, want %.0f W (meter %.0f W, ceiling %.0f W)",
				sumGoldenTargets(r), want, r.Inputs.GridW, r.ClampAttributions.EffectiveImportCeilingW)
		}
	}

	// Per-phase relief is the worst phase's overage converted to aggregate
	// watts by the site's phase count — the conversion #812 gave one owner.
	// A three-phase site relieves three times the phase overage; a
	// single-phase site relieves it once, because the phase and the
	// aggregate are the same wire. Tripling it there would drive the meter
	// through zero into an export-side violation.
	for id, phases := range map[string]float64{
		"early_exit/deadband_per_phase_3p_binds": 3,
		"early_exit/idle_per_phase_3p_binds":     3,
		"early_exit/holdoff_per_phase_3p_binds":  3,
		"early_exit/deadband_per_phase_1p_binds": 1,
		"early_exit/idle_per_phase_1p_binds":     1,
		"early_exit/holdoff_per_phase_1p_binds":  1,
	} {
		r, ok := byID[id]
		if !ok {
			t.Errorf("missing %s", id)
			continue
		}
		overage := r.ClampAttributions.PerPhaseOverageW
		if overage <= 0 {
			t.Errorf("%s: no per-phase overage recorded, so the record no longer exercises the breaker", id)
			continue
		}
		if got, want := sumGoldenTargets(r), -overage*phases; math.Abs(got-want) > 1 {
			t.Errorf("%s: fleet total = %.0f W, want %.0f W — %.0f W over on the worst phase, across %.0f phases",
				id, got, want, overage, phases)
		}
		if float64(r.Inputs.State.SiteFusePhases) != phases {
			t.Errorf("%s: recorded with site_fuse_phases=%d, not the %.0f the assertion names",
				id, r.Inputs.State.SiteFusePhases, phases)
		}
	}

	// Direction. forceFuseDischarge's only lever is more discharge, which
	// relieves an import-side phase and pushes an export-side one further
	// over the breaker. Its import-only gate is what stops that, and this
	// record is a phase 10 A over on the export side that must stay quiet.
	if r, ok := byID["early_exit/deadband_per_phase_export_side_quiet"]; !ok {
		t.Error("missing early_exit/deadband_per_phase_export_side_quiet")
	} else {
		if r.ClampAttributions.PerPhaseOverageW <= 0 {
			t.Errorf("export-side control no longer has a phase over the breaker: %+v", r.ClampAttributions)
		}
		if len(r.PerDriverTargets) != 0 {
			t.Errorf("per-phase relief fired on the export side: %v — it would push the over-current phase further over",
				r.PerDriverTargets)
		}
	}
}

// goldenSlewEffectW replays a record twice — as recorded, and with the slew
// limiter opted out — and returns the largest per-driver difference in watts.
//
// It is measured rather than stored because a record holds only the final
// targets, and by then the limiter's contribution is indistinguishable from
// every other clamp's. SlewEnabled=false skips exactly the ramp clamp and
// nothing else (not the snap-to-zero carve-out above it, not the re-clamp
// below it), so the difference between the two runs is the limiter's effect on
// the answer that reached the hardware.
func goldenSlewEffectW(r goldenRecord) float64 {
	if !r.Inputs.State.SlewEnabled {
		return 0
	}
	on := runGoldenScenario(goldenScenario{ID: r.ScenarioID, Seed: r.Seed, Inputs: r.Inputs})
	off := r.Inputs
	off.State.SlewEnabled = false
	unslewed := runGoldenScenario(goldenScenario{ID: r.ScenarioID, Seed: r.Seed, Inputs: off})

	byDriver := map[string]float64{}
	for _, tg := range on.PerDriverTargets {
		byDriver[tg.Driver] += tg.TargetW
	}
	for _, tg := range unslewed.PerDriverTargets {
		byDriver[tg.Driver] -= tg.TargetW
	}
	var largest float64
	for _, d := range byDriver {
		if math.Abs(d) > largest {
			largest = math.Abs(d)
		}
	}
	return largest
}

// goldenLiveBatteryW sums the measured output of the batteries the record
// dispatched — the anchor the slew limiter ramps from.
func goldenLiveBatteryW(r goldenRecord) float64 {
	dispatched := map[string]bool{}
	for _, tg := range r.PerDriverTargets {
		dispatched[tg.Driver] = true
	}
	var sum float64
	for _, b := range r.Inputs.Batteries {
		if dispatched[b.Driver] {
			sum += b.CurrentW
		}
	}
	return sum
}

func sumGoldenTargets(r goldenRecord) float64 {
	var s float64
	for _, tg := range r.PerDriverTargets {
		s += tg.TargetW
	}
	return s
}

// ---- diff -----------------------------------------------------------------

func goldenWattsEqual(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	return math.Abs(a-b) <= goldenToleranceW
}

// diffGoldenRecord returns one human line per difference, empty when the
// replay reproduced the record. It names the drivers that moved and by how
// much, because that is the question a reader has when this test fails.
func diffGoldenRecord(want, got goldenRecord) []string {
	var lines []string

	// Per-driver targets. Grouped by driver rather than compared positionally
	// so that a driver appearing or disappearing reads as such.
	wantByDriver := map[string][]goldenTarget{}
	gotByDriver := map[string][]goldenTarget{}
	drivers := map[string]bool{}
	for _, tg := range want.PerDriverTargets {
		wantByDriver[tg.Driver] = append(wantByDriver[tg.Driver], tg)
		drivers[tg.Driver] = true
	}
	for _, tg := range got.PerDriverTargets {
		gotByDriver[tg.Driver] = append(gotByDriver[tg.Driver], tg)
		drivers[tg.Driver] = true
	}
	names := make([]string, 0, len(drivers))
	for d := range drivers {
		names = append(names, d)
	}
	sort.Strings(names)

	var targetLines []string
	for _, d := range names {
		w, g := wantByDriver[d], gotByDriver[d]
		switch {
		case len(w) == 0:
			targetLines = append(targetLines, fmt.Sprintf(
				"    %s: not dispatched before, now %s", d, formatGoldenTargets(g)))
		case len(g) == 0:
			targetLines = append(targetLines, fmt.Sprintf(
				"    %s: was %s, now not dispatched", d, formatGoldenTargets(w)))
		case len(w) != len(g):
			targetLines = append(targetLines, fmt.Sprintf(
				"    %s: was %s, now %s", d, formatGoldenTargets(w), formatGoldenTargets(g)))
		default:
			for i := range w {
				if !goldenWattsEqual(w[i].TargetW, g[i].TargetW) {
					targetLines = append(targetLines, fmt.Sprintf(
						"    %s: was %+.2f W, now %+.2f W (moved %+.2f W)",
						d, w[i].TargetW, g[i].TargetW, g[i].TargetW-w[i].TargetW))
				}
				if w[i].Clamped != g[i].Clamped {
					targetLines = append(targetLines, fmt.Sprintf(
						"    %s: clamped was %v, now %v", d, w[i].Clamped, g[i].Clamped))
				}
			}
		}
	}
	if len(targetLines) > 0 {
		lines = append(lines, "  per-driver targets:")
		lines = append(lines, targetLines...)
	}

	// Clamp attributions: why dispatch landed where it did.
	wa, ga := want.ClampAttributions, got.ClampAttributions
	var attrLines []string
	addBool := func(name string, w, g bool) {
		if w != g {
			attrLines = append(attrLines, fmt.Sprintf("    %s: was %v, now %v", name, w, g))
		}
	}
	addWatts := func(name string, w, g float64) {
		if !goldenWattsEqual(w, g) {
			attrLines = append(attrLines, fmt.Sprintf("    %s: was %.2f W, now %.2f W (moved %+.2f W)",
				name, w, g, g-w))
		}
	}
	if strings.Join(wa.ClampedDrivers, ",") != strings.Join(ga.ClampedDrivers, ",") {
		attrLines = append(attrLines, fmt.Sprintf("    clamped_drivers: was [%s], now [%s]",
			strings.Join(wa.ClampedDrivers, " "), strings.Join(ga.ClampedDrivers, " ")))
	}
	addBool("all_targets_zero_clamped", wa.AllTargetsZeroClamped, ga.AllTargetsZeroClamped)
	if wa.PlanSignIntent != ga.PlanSignIntent {
		attrLines = append(attrLines, fmt.Sprintf("    plan_sign_intent: was %d, now %d",
			wa.PlanSignIntent, ga.PlanSignIntent))
	}
	addBool("plan_sign_floor_zeroed", wa.PlanSignFloorZeroed, ga.PlanSignFloorZeroed)
	addBool("plan_stale", wa.PlanStale, ga.PlanStale)
	addBool("fuse_saturated", wa.FuseSaturated, ga.FuseSaturated)
	addWatts("fuse_ev_max_w", wa.FuseEVMaxW, ga.FuseEVMaxW)
	addBool("fuse_hold_latched", wa.FuseHoldLatched, ga.FuseHoldLatched)
	addWatts("fuse_hold_max_charge_w", wa.FuseHoldMaxChargeW, ga.FuseHoldMaxChargeW)
	addWatts("fuse_hold_max_discharge_w", wa.FuseHoldMaxDischargeW, ga.FuseHoldMaxDischargeW)
	addWatts("per_phase_overage_w", wa.PerPhaseOverageW, ga.PerPhaseOverageW)
	addWatts("effective_import_ceiling_w", wa.EffectiveImportCeilingW, ga.EffectiveImportCeilingW)
	addWatts("effective_export_ceiling_w", wa.EffectiveExportCeilingW, ga.EffectiveExportCeilingW)
	addBool("import_ceiling_binding", wa.ImportCeilingBinding, ga.ImportCeilingBinding)
	addBool("export_ceiling_binding", wa.ExportCeilingBinding, ga.ExportCeilingBinding)
	if len(attrLines) > 0 {
		lines = append(lines, "  clamp attributions:")
		lines = append(lines, attrLines...)
	}

	// Site totals: the one-number summary of what the fleet was asked to do.
	ws, gs := want.SiteTotals, got.SiteTotals
	var totalLines []string
	addTotal := func(name string, w, g float64) {
		if !goldenWattsEqual(w, g) {
			totalLines = append(totalLines, fmt.Sprintf("    %s: was %.2f W, now %.2f W (moved %+.2f W)",
				name, w, g, g-w))
		}
	}
	addTotal("battery_current_w", ws.BatteryCurrentW, gs.BatteryCurrentW)
	addTotal("battery_target_sum_w", ws.BatteryTargetSumW, gs.BatteryTargetSumW)
	addTotal("raw_grid_w", ws.RawGridW, gs.RawGridW)
	addTotal("projected_grid_w", ws.ProjectedGridW, gs.ProjectedGridW)
	if len(totalLines) > 0 {
		lines = append(lines, "  site totals:")
		lines = append(lines, totalLines...)
	}

	if len(lines) > 0 {
		lines = append(lines, fmt.Sprintf("  scenario: mode=%s grid=%.0f W fuse=%.0f W batteries=%d slot=%s",
			want.Inputs.State.Mode, want.Inputs.GridW, want.Inputs.FuseMaxW,
			len(want.Inputs.Batteries), describeGoldenSlot(want.Inputs.Slot)))
	}
	return lines
}

func formatGoldenTargets(tgs []goldenTarget) string {
	parts := make([]string, 0, len(tgs))
	for _, tg := range tgs {
		s := fmt.Sprintf("%+.2f W", tg.TargetW)
		if tg.Clamped {
			s += " (clamped)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func describeGoldenSlot(s *goldenSlot) string {
	switch {
	case s == nil:
		return "none"
	case !s.Present:
		return "stale"
	default:
		return fmt.Sprintf("%s %.0f Wh, %.0f s left", s.Strategy, s.BatteryEnergyWh, s.RemainingS)
	}
}
