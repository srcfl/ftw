//go:build unix

package site

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

// scenario is one house and what happens in it. known names the criteria
// that fail on master and the finding behind each: such a criterion must
// keep failing, and once a fix makes it pass the run fails until the fix
// deletes its line. Criteria come from reports/simulatorprov-20261021.
type scenario struct {
	name    string  // what FTW_SITE names it by
	dir     string  // its run directory
	minutes float64 // its run time on a GitHub runner, for sharding
	short   bool    // in the pull-request set
	run     func(c *runCtx) (map[string]any, error)
	judge   func(d *runData) (metrics, []criterion)
	known   map[string]string
	// tolerate names criteria that may fail for a known finding but need
	// not: random cases, where a seed may or may not reach it.
	tolerate map[string]string
}

var scenarios = []scenario{
	{name: "s0-25", dir: "s0-idle-charger-25A", minutes: 5.1, short: true,
		run: func(c *runCtx) (map[string]any, error) { return s0(c, 25, 60) }, judge: judgeS0},
	{name: "s0b-25", dir: "s0b-idle-charger-default-hb-25A", minutes: 5.1,
		run: func(c *runCtx) (map[string]any, error) { return s0(c, 25, 0) }, judge: judgeS0},
	{name: "s1-16", dir: "s1-chargenow-16A", minutes: 3.6, short: true,
		run: func(c *runCtx) (map[string]any, error) { return s1(c, 16) }, judge: judgeS1},
	{name: "s1-25", dir: "s1-chargenow-25A", minutes: 3.6, short: true,
		run: func(c *runCtx) (map[string]any, error) { return s1(c, 25) }, judge: judgeS1},
	{name: "s2-16", dir: "s2-export-16A", minutes: 3.1,
		run: func(c *runCtx) (map[string]any, error) { return s2(c, 16) }, judge: judgeS2,
		known: map[string]string{"car starts within 60 s": "export counted as import (review fuse finding 1), next beta"}},
	{name: "s3-16", dir: "s3-step-16A", minutes: 6.7, short: true,
		run: func(c *runCtx) (map[string]any, error) { return s3(c, 16) }, judge: judgeS3},
	{name: "s3-25", dir: "s3-step-25A", minutes: 6.7, short: true,
		run: func(c *runCtx) (map[string]any, error) { return s3(c, 25) }, judge: judgeS3},
	{name: "s4-25", dir: "s4-unlock-25A", minutes: 3.6,
		run: func(c *runCtx) (map[string]any, error) { return s4(c, 25, "3p") }, judge: judgeS4},
	{name: "s4b-25", dir: "s4b-unlock-nophase-25A", minutes: 3.6,
		run: func(c *runCtx) (map[string]any, error) { return s4(c, 25, "") }, judge: judgeS4},
	{name: "s5-25", dir: "s5-surplus-auto-25A", minutes: 5.2,
		run: func(c *runCtx) (map[string]any, error) { return s5(c, 25, nil) }, judge: judgeS5,
		known: map[string]string{"charges at least the minimum 90 % of the time": "surplus without allowed_steps_w never starts, loadpoint/controller.go surplus3PhaseSteps; next beta"}},
	{name: "s5s-25", dir: "s5s-surplus-auto-steps-25A", minutes: 5.2,
		run: func(c *runCtx) (map[string]any, error) { return s5(c, 25, easeeSteps) }, judge: judgeS5,
		known: map[string]string{"charges at least the minimum 90 % of the time": "phase mode auto stays on 1p under 9 kW surplus (R1); next beta"}},
	{name: "s6-25", dir: "s6-inverter-offline-25A", minutes: 7.7,
		run: func(c *runCtx) (map[string]any, error) { return s6(c, 25) }, judge: judgeS6},
	{name: "s7-25", dir: "s7-restart-25A", minutes: 23,
		run: func(c *runCtx) (map[string]any, error) { return s7(c, 25, false) }, judge: judgeS7},
	// The full s7 runs 15 min before the restart, because the day's Home
	// use may have at most 10 % unknown time. The short one restarts after
	// a minute and leaves the day total out.
	{name: "s7s-25", dir: "s7s-restart-short-25A", minutes: 9, short: true,
		run: func(c *runCtx) (map[string]any, error) { return s7(c, 25, true) }, judge: judgeS7},
	{name: "s8-60-25", dir: "s8-slow60-25A", minutes: 7.5, short: true,
		run: func(c *runCtx) (map[string]any, error) { return s8(c, 25, 60) }, judge: judgeS8},
	{name: "s8-120-25", dir: "s8-slow120-25A", minutes: 12.5,
		run: func(c *runCtx) (map[string]any, error) { return s8(c, 25, 120) }, judge: judgeS8},
	{name: "s9-25", dir: "s9-phase-switch-25A", minutes: 4.7,
		run: func(c *runCtx) (map[string]any, error) { return s9(c, 25, 7360) }, judge: judgeS9},
	{name: "s10-25", dir: "s10-two-chargers-25A", minutes: 7.9,
		run: func(c *runCtx) (map[string]any, error) { return s10(c, 25, 60) }, judge: judgeS10},
	{name: "s11-25", dir: "s11-pause-restart-25A", minutes: 5.1,
		run: func(c *runCtx) (map[string]any, error) { return s11(c, 25) }, judge: judgeS11},
	{name: "s9r-25", dir: "s9r-house-on-l2-25A", minutes: 5,
		run: func(c *runCtx) (map[string]any, error) { return s9r(c, 25, 7360) }, judge: judgeS9},
	{name: "s5t-25", dir: "s5t-surplus-goal-25A", minutes: 6,
		run: func(c *runCtx) (map[string]any, error) { return s5t(c, 25) }, judge: judgeS5t},
	{name: "s12-20", dir: "s12-one-phase-night-20A", minutes: 57,
		run: func(c *runCtx) (map[string]any, error) { return s12(c, 20) }, judge: judgeS12},
	{name: "s13-25", dir: "s13-socket-reconnect-25A", minutes: 6,
		run: func(c *runCtx) (map[string]any, error) { return s13(c, 25) }, judge: judgeS13,
		known: map[string]string{"Charge now goes on after the charger returns": "a socket reconnect turns Charge now into a 0 W pause; srcfl/ftw#1788"}},
	{name: "s15-25", dir: "s15-one-phase-charger-chargenow-25A", minutes: 4.6, short: true,
		run: func(c *runCtx) (map[string]any, error) { return s15(c, 25) }, judge: judgeS15},
	{name: "s15b-25", dir: "s15b-one-phase-charger-1p-25A", minutes: 4.6,
		run: func(c *runCtx) (map[string]any, error) { return s15mode(c, 25, "1p") }, judge: judgeS15},
	{name: "s15c-25", dir: "s15c-one-phase-charger-auto-25A", minutes: 4.6,
		run: func(c *runCtx) (map[string]any, error) { return s15mode(c, 25, "auto") }, judge: judgeS15},
	{name: "s15bat-16", dir: "s15bat-one-phase-charger-battery-rising-16A", minutes: 5.5,
		run: func(c *runCtx) (map[string]any, error) { return s15bat(c, 16) }, judge: judgeS15bat},
	{name: "s15p-25", dir: "s15p-one-phase-charger-plan-25A", minutes: 32,
		run: func(c *runCtx) (map[string]any, error) { return s15p(c, 25) }, judge: judgeS15p},
	{name: "s14-25", dir: "s14-one-phase-car-25A", minutes: 22,
		run: func(c *runCtx) (map[string]any, error) { return s14(c, 25) }, judge: judgeS14},
}

// runCtx is where one scenario runs.
type runCtx struct {
	env    *rigEnv
	lane   lane
	rundir string
}

func (c *runCtx) stack(spec stackSpec) *stack { return newStack(c.env, c.lane, c.rundir, spec) }

func houseA(a float64) [3]float64 { return [3]float64{a * volt, a * volt, a * volt} }

// ---- what a client does ----

type loadpointView struct {
	ID      string `json:"id"`
	Plugged bool   `json:"plugged_in"`
	Charger struct {
		Known bool `json:"known"`
	} `json:"charger"`
}

func (st *stack) loadpoint(id string) (loadpointView, error) {
	var j struct {
		Loadpoints []loadpointView `json:"loadpoints"`
	}
	if err := st.get("/api/loadpoints", &j); err != nil {
		return loadpointView{}, err
	}
	for _, x := range j.Loadpoints {
		if x.ID == id {
			return x, nil
		}
	}
	if len(j.Loadpoints) > 0 {
		return j.Loadpoints[0], nil
	}
	return loadpointView{}, fmt.Errorf("no loadpoints")
}

// chargeNow is what the web, PWA and iOS clients send. An empty phaseMode
// is a bare API call. powerW 0 means amps on three phases, below 0 a pause.
func (st *stack) chargeNow(lpID string, amps float64, phaseMode string, powerW float64) error {
	switch {
	case powerW < 0: // a pause
		powerW = 0
	case powerW == 0:
		powerW = amps * volt * 3
	}
	body := map[string]any{"power_w": powerW, "hold_s": 0}
	if phaseMode != "" {
		body["phase_mode"] = phaseMode
	}
	return st.post("/api/loadpoints/"+lpID+"/manual_hold", body, nil)
}

// chargerOnline waits until charger i talks to FTW.
func (st *stack) chargerOnline(i int) error {
	serial := st.serial(i)
	ok := waitUntil(90*time.Second, time.Second, func() bool {
		var s struct {
			Drivers map[string]json.RawMessage `json:"drivers"`
		}
		if st.get("/api/status", &s) != nil {
			return false
		}
		_, ok := s.Drivers[serial]
		return ok
	})
	if !ok {
		return fmt.Errorf("charger %s never connected to FTW", serial)
	}
	return nil
}

// plug plugs a car into charger i once the charger is online, and waits
// for FTW to see it.
func (st *stack) plug(i int) error {
	if err := st.chargerOnline(i); err != nil {
		return err
	}
	time.Sleep(2 * time.Second)
	label := "plug"
	if i > 0 {
		label = fmt.Sprintf("plug%d", i)
	}
	st.mark(label)
	on := true
	if err := st.event(houseEvent{Do: "plug", Device: lpIDs[i], On: &on}); err != nil {
		return err
	}
	ok := waitUntil(90*time.Second, time.Second, func() bool {
		lp, err := st.loadpoint(lpIDs[i])
		return err == nil && lp.Plugged && lp.Charger.Known
	})
	if !ok {
		return fmt.Errorf("loadpoint %s never saw the car plugged in", lpIDs[i])
	}
	return nil
}

func sleepS(s float64) { time.Sleep(time.Duration(s * float64(time.Second))) }

// with brings a stack up, runs body and always takes the stack down.
func with(c *runCtx, spec stackSpec, body func(st *stack) error) error {
	st := c.stack(spec)
	defer st.down()
	if err := st.up(); err != nil {
		return err
	}
	if err := body(st); err != nil {
		return err
	}
	return st.markErr
}

// ---- 0. Adopted OCPP charger, no car: is Home use measured? ----

func s0(c *runCtx, fuse float64, heartbeatS int) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 50, heartbeatS: heartbeatS}, func(st *stack) error {
		if err := st.chargerOnline(0); err != nil {
			return err
		}
		st.mark("connected")
		// Several heartbeats: #1779 keeps the idle 0 W fresh with them.
		sleepS(300)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s0", "fuse": fuse, "house_a": 3, "heartbeat_s": heartbeatS}, err
}

// ---- 1. Charge now, 3 A house load per phase ----

func s1(c *runCtx, fuse float64) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 50}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		sleepS(180)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s1", "fuse": fuse, "house_a": 3}, err
}

// ---- 2. Charge now during PV export of 9 A per phase, 16 A fuse ----

func s2(c *runCtx, fuse float64) (map[string]any, error) {
	// Full battery so it cannot soak up the export. PV 7 kW, house 1 A:
	// 6.3 kW export, 9.1 A per phase out of the site.
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(1), pvW: 7000, socPct: 100}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		sleepS(150)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s2", "fuse": fuse, "house_a": 1, "pv_w": 7000, "soc": 100}, err
}

// ---- 3. House load steps to near the fuse while car and battery charge ----

func s3(c *runCtx, fuse float64) (map[string]any, error) {
	stepA := map[float64]float64{16: 13, 25: 21}[fuse]
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 40}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		st.mark("battery_hold")
		if err := st.post("/api/battery/manual_hold", map[string]any{"direction": "charge", "power_w": 3000, "hold_s": 1800}, nil); err != nil {
			return err
		}
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		sleepS(120)
		st.mark("step")
		if err := st.setLoadsA(stepA); err != nil {
			return err
		}
		sleepS(180)
		st.mark("step_back")
		if err := st.setLoadsA(3); err != nil {
			return err
		}
		sleepS(60)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s3", "fuse": fuse, "house_a": 3, "step_a": stepA, "battery_hold_w": 3000, "charger_delay_s": 0}, err
}

// ---- 4. surplus_unlock_bat_soc saved but not armed, Charge now ----

func s4(c *runCtx, fuse float64, phaseMode string) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 50}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(20)
		st.mark("schedule")
		// Only the unlock, as in #1774's R2 test: a SoC goal would make the
		// plan charge at once and hide the fault.
		var resp json.RawMessage
		if err := st.post("/api/loadpoints/garage/target", map[string]any{"schedule": map[string]any{
			"soc": 0, "time_of_day_min": 7 * 60, "time_of_day_min_utc": 5 * 60,
			"recurring": false, "days": 0, "surplus_unlock_bat_soc": 0.9}}, &resp); err != nil {
			return err
		}
		os.WriteFile(filepath.Join(st.rundir, "target-response.json"), resp, 0o644)
		sleepS(10)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, phaseMode, 0); err != nil {
			return err
		}
		sleepS(180)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s4", "fuse": fuse, "house_a": 3, "unlock": 0.9, "soc": 50, "phase_mode": phaseMode}, err
}

// ---- 5. Surplus only, phase mode auto, ~9 kW surplus ----

// easeeSteps are Easee's whole-amp offers at 230 V, as in #1774's tests:
// 6-16 A on one phase, then 6-16 A on three.
var easeeSteps = []float64{0, 1380, 1610, 1840, 2070, 2300, 2530, 2760, 2990, 3220, 3450, 3680,
	4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11040}

func s5(c *runCtx, fuse float64, steps []float64) (map[string]any, error) {
	extra := map[string]any{"surplus_only": true, "phase_mode": "auto"}
	if steps != nil {
		extra["allowed_steps_w"] = steps
	}
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(1), pvW: 10000, socPct: 100, lpExtra: extra}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		st.mark("plugged")
		sleepS(300)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s5", "fuse": fuse, "house_a": 1, "pv_w": 10000, "soc": 100, "phase_mode": "auto", "steps": steps != nil}, err
}

// ---- 6. PV inverter offline for 5 minutes ----

func s6(c *runCtx, fuse float64) (map[string]any, error) {
	// A car plugged in, so that case 0 does not interfere.
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), pvW: 1500, socPct: 50}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(60)
		st.mark("offline")
		if err := st.event(houseEvent{Do: "offline", Device: "inverter"}); err != nil {
			return err
		}
		sleepS(300)
		st.mark("online")
		if err := st.event(houseEvent{Do: "online", Device: "inverter"}); err != nil {
			return err
		}
		sleepS(90)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s6", "fuse": fuse, "house_a": 3, "pv_w": 1500, "offline_s": 300}, err
}

// ---- 7. Core restart of ~20 s while charging ----

func s7(c *runCtx, fuse float64, short bool) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 50}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		if short {
			sleepS(60)
		} else {
			// A new box has minutes of history; unknown time may be at most
			// 10 % of it (history_dashboard.go). Wait until the day total is
			// known, then long enough that the restart's ~110 s of unknown
			// time stays below the rule, as on a box that has run since morning.
			if waitUntil(900*time.Second, 5*time.Second, func() bool {
				var s ftwStatus
				return st.get("/api/status", &s) == nil && s.Energy.Today.LoadWh != nil
			}) {
				st.mark("today_known")
			}
			sleepS(900)
		}
		st.mark("ftw_stop")
		st.stopFTW()
		sleepS(20)
		st.mark("ftw_start")
		if err := st.startFTW(); err != nil {
			return err
		}
		st.mark("ftw_up")
		// Long enough to see whether the car gets back to full power.
		sleepS(420)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s7", "fuse": fuse, "house_a": 3, "down_s": 20, "short": short}, err
}

// ---- 8. Slow charger: profiles apply 60 s or 120 s late ----

func s8(c *runCtx, fuse, delayS float64) (map[string]any, error) {
	// Battery at the rig's 5 % floor: it cannot cover the house. At 8 % FTW
	// forced it to discharge for the fuse and the meter showed room that
	// vanished when it ran empty, which hid the stacking (see the report).
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(15), socPct: 5, chargers: []charger{{delayS: delayS}}}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		// The charger draws its maximum until FTW's first profile lands,
		// delayS after plug-in. Start from there, at 0 W.
		st.mark("plug_wait")
		sleepS(delayS + 20)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		// Long enough for the last rise and the cut after it to land.
		sleepS(max(300, 4*delayS+120))
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s8", "fuse": fuse, "house_a": 15, "charger_delay_s": delayS, "soc_pct": 5}, err
}

// ---- 9. Charge now switched from 3p to 1p at the same watts ----

func s9(c *runCtx, fuse, watts float64) (map[string]any, error) {
	// A 32 A charger that can switch phases. 7 360 W is 10.7 A on three
	// phases and 32 A on one. House 10 A per phase, empty battery: one phase
	// has 14 A of room below FTW's 24 A limit.
	spec := stackSpec{fuseA: fuse, loadsW: houseA(10), socPct: 5, chargers: []charger{{kw: 22.08, phaseSwitch: true}},
		lpExtra: map[string]any{"min_charge_w": 1380, "max_charge_w": 22080, "phase_mode": "auto"}}
	err := with(c, spec, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 0, "3p", watts); err != nil {
			return err
		}
		sleepS(90)
		st.mark("switch")
		if err := st.chargeNow("garage", 0, "1p", watts); err != nil {
			return err
		}
		sleepS(150)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s9", "fuse": fuse, "house_a": 10, "watts": watts, "charger_a": 32}, err
}

// ---- 10. Two chargers: one raised while the other has just been cut ----

func s10(c *runCtx, fuse, delayS float64) (map[string]any, error) {
	// garage answers at once; carport is a cloud charger that applies a
	// profile delayS late. House 3 A per phase, empty battery. Both charge
	// at 10 A (23 A per phase). Then carport is cut to 6 A and garage raised
	// to 16 A in the same second: the room carport frees is not there until
	// its cut lands.
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 5, chargers: []charger{{}, {delayS: delayS}}}, func(st *stack) error {
		if err := st.plug(1); err != nil {
			return err
		}
		// The slow charger draws its maximum until FTW's first profile lands.
		st.mark("plug_wait")
		sleepS(delayS + 20)
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(20)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 10, "3p", 0); err != nil {
			return err
		}
		if err := st.chargeNow("carport", 10, "3p", 0); err != nil {
			return err
		}
		sleepS(delayS + 60)
		st.mark("step")
		if err := st.chargeNow("carport", 6, "3p", 0); err != nil {
			return err
		}
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		sleepS(2*delayS + 120)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s10", "fuse": fuse, "house_a": 3, "charger_delay_s": []float64{0, delayS}}, err
}

// ---- 11. Pause and restart under a long declared response time (#1793) ----

func s11(c *runCtx, fuse float64) (map[string]any, error) {
	// OCPP declares no response time, so Core reserves 180 s for each order,
	// as for a charger that declares 180 s. Charge now 16 A, pause (0 W) for
	// 25 s, then start at 6 A: the cut must end the old order's reservation.
	// The charger applies a profile 5 s late (ev-104's plant).
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 50, chargers: []charger{{delayS: 5}}}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		sleepS(60)
		st.mark("pause")
		if err := st.chargeNow("garage", 0, "3p", -1); err != nil {
			return err
		}
		sleepS(25)
		st.mark("restart")
		if err := st.chargeNow("garage", 6, "3p", 0); err != nil {
			return err
		}
		sleepS(180)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s11", "fuse": fuse, "house_a": 3, "pause_s": 25, "charger_delay_s": 5}, err
}

// ---- planned charging ----

// planTo tells FTW the car is at socPct and must reach goalPct by the first
// quarter hour at least readyIn from now. It returns the ready time.
func (st *stack) planTo(lp string, socPct, goalPct float64, readyIn time.Duration) (time.Time, error) {
	if err := st.post("/api/loadpoints/"+lp+"/soc", map[string]any{"soc_pct": socPct}, nil); err != nil {
		return time.Time{}, err
	}
	ready := time.Now().Add(readyIn).Truncate(15 * time.Minute).Add(15 * time.Minute)
	if err := st.post("/api/loadpoints/"+lp+"/target", map[string]any{"schedule": map[string]any{
		"soc": goalPct / 100, "time_of_day_min": ready.Hour()*60 + ready.Minute(), "recurring": false, "days": 0}}, nil); err != nil {
		return time.Time{}, err
	}
	return ready, nil
}

// ---- 9r. s9 reversed: a 1p order raised to 3p beside a loaded L2 ----

func s9r(c *runCtx, fuse, watts float64) (map[string]any, error) {
	// House 2/20/2 A on 25 A: L2 has 4 A left under FTW's 24 A limit. A
	// 32 A charger that switches phases gets Charge now 7 360 W on 1p
	// (L1 has room), then the same watts on 3p, which would put 10.7 A on
	// L2 too.
	spec := stackSpec{fuseA: fuse, loadsW: [3]float64{2 * volt, 20 * volt, 2 * volt}, socPct: 5,
		chargers: []charger{{kw: 22.08, phaseSwitch: true}},
		lpExtra:  map[string]any{"min_charge_w": 1380, "max_charge_w": 22080, "phase_mode": "auto"}}
	err := with(c, spec, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 0, "1p", watts); err != nil {
			return err
		}
		sleepS(90)
		st.mark("switch")
		if err := st.chargeNow("garage", 0, "3p", watts); err != nil {
			return err
		}
		sleepS(150)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s9", "fuse": fuse, "house_a": []float64{2, 20, 2}, "watts": watts, "charger_a": 32, "order": "1p then 3p"}, err
}

// ---- 5t. s5s with a goal: surplus below the three-phase minimum ----

func s5t(c *runCtx, fuse float64) (map[string]any, error) {
	// PV 4.5 kW, house 1 A per phase, full battery: about 3.8 kW of surplus,
	// under the 4 140 W three-phase minimum. With a goal saved, a
	// surplus-only loadpoint must still charge, on one phase: the charger
	// switches phases, as an Easee does.
	extra := map[string]any{"surplus_only": true, "phase_mode": "auto", "allowed_steps_w": easeeSteps}
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(1), pvW: 4500, socPct: 100, lpExtra: extra,
		chargers: []charger{{phaseSwitch: true}}}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		if _, err := st.planTo("garage", 40, 80, 8*time.Hour); err != nil {
			return err
		}
		st.mark("plugged")
		sleepS(300)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s5t", "fuse": fuse, "house_a": 1, "pv_w": 4500, "soc": 100}, err
}

func judgeS5t(d *runData) (metrics, []criterion) {
	m, cs := judgeS5(d)
	t0, t1 := d.marks["plugged"], d.marks["end"]
	w := d.window(t0+60, t1)
	n := 0
	for _, r := range w {
		if carW(r, 0) >= 0.95*1380 {
			n++
		}
	}
	frac := round(float64(n)/math.Max(1, float64(len(w))), 3)
	ones := 0
	prs := connector1(profiles(d.frames, t0, t1))
	for _, p := range prs {
		if p.Limit > 0 && p.phasesOr3() == 1 {
			ones++
		}
	}
	m["charging_fraction_ge_1p_min"], m["one_phase_offers"] = frac, ones
	out := []criterion{}
	for _, c := range cs {
		if c.Name == "no offer above the charger's rating" || c.Name == "fuse holds" {
			out = append(out, c)
		}
	}
	return m, append(out, crit("charges at least 6 A on one phase 90 % of the time", frac >= 0.9))
}

// ---- 12. One-phase night: planned charging on auto with Easee's steps ----

// fieldSteps is the field site's step table (22 Oct): one-phase levels
// 1 600-3 800 W and three-phase levels 4 800-11 100 W.
var fieldSteps = []float64{0, 1600, 1840, 2070, 2300, 2530, 2760, 2990, 3220, 3450, 3680, 3800,
	4800, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350, 11100}

func s12(c *runCtx, fuse float64) (map[string]any, error) {
	// 3 × 20 A, house 1.6/1.7/2.3 A, an Easee-like charger applying in
	// 40 s, a three-phase car, and a plan for 6.6 kWh by the first quarter
	// hour 40 min out: 7-10 kW on three phases. One phase cannot deliver
	// that, so this tests the cuts and the shortfall of the field night,
	// not its one-phase choice (#1799 cause 1, which needs a lighter plan).
	extra := map[string]any{"phase_mode": "auto", "allowed_steps_w": fieldSteps, "min_charge_w": 1600, "max_charge_w": 11100}
	var ready time.Time
	err := with(c, stackSpec{fuseA: fuse, loadsW: [3]float64{1.6 * volt, 1.7 * volt, 2.3 * volt}, socPct: 5,
		chargers: []charger{{delayS: 40}}, lpExtra: extra}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(20)
		var err error
		if ready, err = st.planTo("garage", 40, 51, 40*time.Minute); err != nil {
			return err
		}
		st.mark("plan")
		sleepS(time.Until(ready).Seconds())
		st.mark("ready")
		sleepS(30)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s12", "fuse": fuse, "plan_wh": 0.11 * 60000, "charger_delay_s": 40}, err
}

func judgeS12(d *runData) (metrics, []criterion) {
	fuse := d.metaF("fuse")
	t0, tReady := d.marks["plan"], d.marks["ready"]
	m := metrics{}
	// Energy the car took between the plan and the ready time.
	wh := 0.0
	w := d.window(t0, tReady)
	for i := 1; i < len(w); i++ {
		wh += carW(w[i], 0) * (w[i].T - w[i-1].T) / 3600
	}
	plan := d.metaF("plan_wh")
	m["delivered_wh"], m["plan_wh"] = math.Round(wh), plan
	prs := connector1(profiles(d.frames, t0, tReady))
	ones, cuts := 0, 0
	for i, p := range prs {
		if p.Limit > 0 && p.phasesOr3() == 1 {
			ones++
		}
		// A lower order while every phase has room is a cut on room alone,
		// unless the plan itself asked for less (a pause between slots).
		if i > 0 && p.offerW() < prs[i-1].offerW()-1 {
			if r := d.at(t0 + p.T); r != nil && maxAbsI(r) < fuse-1-0.5 {
				if after := d.at(t0 + p.T + 2); after == nil || firstLP(after).CommandedReason != "plan" {
					cuts++
				}
			}
		}
	}
	// Seconds FTW gave fuse_limit while the worst phase had 6 A of room:
	// recorded, not judged, since a slow charger's pending order may hold it.
	limited := 0
	for _, r := range w {
		if r.LPs != nil && firstLP(r).CommandedReason == "fuse_limit" && maxAbsI(r) < fuse-1-6 {
			limited++
		}
	}
	m["one_phase_orders"], m["cuts_with_room"], m["profiles"], m["fuse_limit_with_room_s"] = ones, cuts, len(prs), limited
	isBlown := anyBlown(d.window(t0, d.marks["end"]))
	return m, []criterion{
		crit("fuse holds", !isBlown),
		crit("planned charging never orders one phase", ones == 0),
		crit("no order cut while every phase has room", cuts == 0),
		crit("at least 90 % of the plan by the ready time", wh >= 0.9*plan),
	}
}

// at is the last sample at or before t.
func (d *runData) at(t float64) *sample {
	var last *sample
	for _, r := range d.samples {
		if r.T > t {
			break
		}
		last = r
	}
	return last
}

// ---- 13. Charge now across a charger socket reconnect ----

func s13(c *runCtx, fuse float64) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 50}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		sleepS(60)
		st.mark("drop")
		if err := st.event(houseEvent{Do: "reconnect", Device: "garage", ForS: 10}); err != nil {
			return err
		}
		sleepS(180)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s13", "fuse": fuse, "house_a": 3, "drop_s": 10}, err
}

func judgeS13(d *runData) (metrics, []criterion) {
	tDrop, t1 := d.marks["drop"], d.marks["end"]
	car := func(r *sample) float64 { return carW(r, 0) }
	minAfter := minOf(d.window(tDrop, t1), car)
	plugged := 0
	for _, r := range d.window(tDrop, t1) {
		if lp := firstLP(r); r.LPs != nil && !lp.Plugged {
			plugged++
		}
	}
	m := metrics{"car_min_after_drop": math.Round(minAfter), "car_w_last60": math.Round(meanOf(d.window(t1-60, t1), car)),
		"unplugged_samples": plugged}
	return m, []criterion{
		crit("Charge now goes on after the charger returns", minAfter >= 0.95*minW),
		crit("no unplug seen", plugged == 0),
	}
}

// ---- 14. A one-phase car under a three-phase order ----

func s14(c *runCtx, fuse float64) (map[string]any, error) {
	// A one-phase car on an 11 kW charger draws on L1 whatever FTW offers.
	// A plan asks for more than it can take, so FTW should plan its 16 A
	// on one phase (3.5-3.7 kW) and not call the charger not following.
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 5, chargers: []charger{{carPhases: 1}}}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(20)
		// 24 kWh in about 45 min: more than even 11 kW gives, so FTW must
		// charge from the start.
		if _, err := st.planTo("garage", 40, 80, 30*time.Minute); err != nil {
			return err
		}
		st.mark("plan")
		sleepS(20 * 60)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s14", "fuse": fuse, "house_a": 3, "car_phases": 1}, err
}

func judgeS14(d *runData) (metrics, []criterion) {
	t0, t1 := d.marks["plan"], d.marks["end"]
	car := func(r *sample) float64 { return carW(r, 0) }
	last := meanOf(d.window(t1-600, t1), car)
	notFollowing := 0
	for _, r := range d.window(t0+180, t1) {
		if firstLP(r).ControlFeedback.Status == "not_following" {
			notFollowing++
		}
	}
	m := metrics{"car_w_last10min": math.Round(last), "not_following_samples": notFollowing}
	return m, []criterion{
		crit("delivers 3.4-3.8 kW over the last 10 min", last >= 3400 && last <= 3800),
		crit("never called not following", notFollowing == 0),
	}
}

// ---- 15. A charger wired on one phase: Charge now and planned charging ----

// onePhaseCharger is an Easee wired on L1: 6-16 A there, nothing on three
// phases. Its loadpoint names no phase mode, as most do.
var onePhaseCharger = charger{kw: 3.68, wiredPhases: 1}

var onePhaseExtra = map[string]any{"min_charge_w": 1380, "max_charge_w": 3680,
	"allowed_steps_w": easeeSteps[:12]} // 0, 1 380 ... 3 680 W

// chargeNowAsTheApp sends Charge now at 16 A on the phases /api/loadpoints
// reports, as the app does.
func (st *stack) chargeNowAsTheApp(lpID string) (int, error) {
	var lps struct {
		Loadpoints []ftwLoadpoint `json:"loadpoints"`
	}
	if err := st.get("/api/loadpoints", &lps); err != nil {
		return 0, err
	}
	phases := 3
	for _, lp := range lps.Loadpoints {
		if lp.ID == lpID && lp.Phases > 0 {
			phases = lp.Phases
		}
	}
	mode := "3p"
	if phases == 1 {
		mode = "1p"
	}
	return phases, st.chargeNow(lpID, 0, mode, 16*volt*float64(phases))
}

func s15(c *runCtx, fuse float64) (map[string]any, error) { return s15mode(c, fuse, "") }

// s15mode is s15 with the loadpoint's phase mode set ("" leaves it out).
func s15mode(c *runCtx, fuse float64, mode string) (map[string]any, error) {
	extra := map[string]any{}
	for k, v := range onePhaseExtra {
		extra[k] = v
	}
	if mode != "" {
		extra["phase_mode"] = mode
	}
	phases := 0
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 50, chargers: []charger{onePhaseCharger},
		lpExtra: extra}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		st.mark("charge_now")
		var err error
		if phases, err = st.chargeNowAsTheApp("garage"); err != nil {
			return err
		}
		sleepS(180)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s15", "fuse": fuse, "house_a": 3, "api_phases": phases, "phase_mode": mode}, err
}

func judgeS15(d *runData) (metrics, []criterion) {
	t0, t1 := d.marks["charge_now"], d.marks["end"]
	const target = 16 * volt // all a one-phase charger gives
	m := d.carSummary(t0, t1, target)
	m["target_w"], m["api_phases"] = target, d.meta["api_phases"]
	hold, _ := m["hold_fraction"].(*float64)
	return m, []criterion{
		crit("16 A on L1 within 60 s", le(m["time_to_target_s"].(*float64), 60)),
		crit("holds 95 % of the time", hold != nil && *hold >= 0.95),
		crit("fuse holds", !m["fuse_blown"].(bool)),
		crit("no fuse_limit with room to spare", m["fuse_limit_samples"].(int) == 0),
	}
}

func s15p(c *runCtx, fuse float64) (map[string]any, error) {
	// 600 Wh by the first quarter hour 15 min out: about 10 min at 3.68 kW.
	var ready time.Time
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 5, chargers: []charger{onePhaseCharger},
		lpExtra: onePhaseExtra}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(20)
		var err error
		if ready, err = st.planTo("garage", 40, 41, 15*time.Minute); err != nil {
			return err
		}
		st.mark("plan")
		sleepS(time.Until(ready).Seconds())
		st.mark("ready")
		sleepS(30)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s15p", "fuse": fuse, "house_a": 3, "plan_wh": 0.01 * 60000}, err
}

func judgeS15p(d *runData) (metrics, []criterion) {
	t0, tReady := d.marks["plan"], d.marks["ready"]
	wh := 0.0
	w := d.window(t0, tReady)
	for i := 1; i < len(w); i++ {
		wh += carW(w[i], 0) * (w[i].T - w[i-1].T) / 3600
	}
	plan := d.metaF("plan_wh")
	notFollowing := 0
	for _, r := range w {
		if r.LPs != nil && firstLP(r).ControlFeedback.Status == "not_following" {
			notFollowing++
		}
	}
	m := metrics{"delivered_wh": math.Round(wh), "plan_wh": plan, "not_following_samples": notFollowing,
		"car_w_max": math.Round(maxOf(w, func(r *sample) float64 { return carW(r, 0) }))}
	return m, []criterion{
		crit("fuse holds", !anyBlown(d.window(t0, d.marks["end"]))),
		crit("at least 90 % of the plan by the ready time", wh >= 0.9*plan),
		crit("never called not following", notFollowing == 0),
	}
}

// s15bat is Charge now on a one-phase charger while Core is raising its own
// battery order: PV climbs every 30 s (+2.5 kW for the second step, 1.5 kW
// for the others) and self-consumption charges the battery with it. The
// battery follows 8 s after each write, and Charge now goes 6 s after the
// second step, while Core's raise is on its way. On
// 16 A with 3 A of house load a raise of more than
// (15 - 3) x 230 - (1 380 + 230) = 1 150 W on its way once kept the car from
// starting at all (#1805). With the battery's 2 s the car started 25 s late
// when Charge now caught a raise in flight, and at once when it did not.
func s15bat(c *runCtx, fuse float64) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 50, chargers: []charger{onePhaseCharger},
		lpExtra: onePhaseExtra, batteryDelayS: 8}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		for i, pv := range []float64{2100, 4600, 6100, 7600, 9100} {
			if err := st.event(houseEvent{Do: "pv", Device: "inverter", PVW: f64(pv)}); err != nil {
				return err
			}
			if i == 1 {
				sleepS(6)
				st.mark("charge_now")
				if _, err := st.chargeNowAsTheApp("garage"); err != nil {
					return err
				}
				sleepS(24)
				continue
			}
			sleepS(30)
		}
		sleepS(90)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s15bat", "fuse": fuse, "house_a": 3, "battery_delay_s": 8}, err
}

func judgeS15bat(d *runData) (metrics, []criterion) {
	t0, t1 := d.marks["charge_now"], d.marks["end"]
	fuse := d.metaF("fuse")
	// Core's battery order: the EMS command and setpoint it wrote.
	order := func(r *sample) float64 {
		switch r.Bat.EMSCmd {
		case 170:
			return r.Bat.EMSSetW
		case 187:
			return -r.Bat.EMSSetW
		}
		return 0
	}
	rise, low := 0.0, math.Inf(1)
	for _, r := range d.window(t0-20, t0) {
		low = math.Min(low, order(r))
		rise = math.Max(rise, order(r)-low)
	}
	start := d.firstT(t0, func(r *sample) bool { return carW(r, 0) >= 0.95*1380 })
	over, longest := 0.0, 0.0
	for _, r := range d.window(t0, t1) {
		if maxAbsI(r) > fuse-1 {
			over++
			longest = math.Max(longest, over)
		} else {
			over = 0
		}
	}
	m := metrics{"battery_rise_w_20s_before": math.Round(rise), "car_start_s": roundp(start, 1),
		"car_w_last60":               math.Round(meanOf(d.window(t1-60, t1), func(r *sample) float64 { return carW(r, 0) })),
		"over_fuse_minus1_longest_s": longest, "max_abs_i": round(maxOf(d.window(t0, t1), maxAbsI), 1)}
	return m, []criterion{
		crit("the battery order rose over 1 150 W in the 20 s before Charge now", rise > 1150), // the setup
		crit("car charging within 10 s of Charge now", le(start, 10)),
		crit("fuse holds", !anyBlown(d.window(t0, t1))),
		crit("over fuse - 1 A at most 3 s", longest <= 3),
	}
}
