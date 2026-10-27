//go:build unix

package site

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The P0 scenarios R1-R5 of the stable plan: a meter that stops answering,
// a battery that drops out, the planner's floor, a hard kill and a start
// into a running site. Their judges are in verdicts_test.go. A known line
// names the Wave 1 item whose fix deletes it.
func init() {
	scenarios = append(scenarios,
		scenario{name: "r1-25", dir: "r1-meter-silent-then-offline-25A", minutes: 10, run: func(c *runCtx) (map[string]any, error) { return r1(c, 25) },
			judge: judgeR1, skip: meterKnob,
			known: map[string]string{"the battery order moves only toward 0 while the meter is silent": "A1: a failed poll keeps the held reading, and ComputeDispatch winds the battery against it (dispatch.go:1446-1449)"}},
		scenario{name: "r2-25", dir: "r2-battery-offline-25A", minutes: 8, run: func(c *runCtx) (map[string]any, error) { return r2(c, 25, 0) },
			judge: judgeR2},
		scenario{name: "r2-60-25", dir: "r2-battery-offline-slow60-25A", minutes: 10, run: func(c *runCtx) (map[string]any, error) { return r2(c, 25, 60) },
			judge: judgeR2},
		scenario{name: "r3-25", dir: "r3-planner-floor-25A", minutes: 13, run: func(c *runCtx) (map[string]any, error) { return r3(c, 25) },
			judge: judgeR3,
			known: map[string]string{
				"no discharge ordered below the planner floor":               "A4: fuse relief discharges to 5 %, below planner.soc_min (dispatch.go:4113-4124, 4495-4499)",
				"the battery order swings at most 100 W over the last 5 min": "A3, A4: at the inverter's empty 5 % Core keeps ordering 3.9-5 kW of discharge (the slew step reopens it, dispatch.go:2148-2219)",
			}},
		scenario{name: "r4-25", dir: "r4-sigkill-25A", minutes: 40, run: func(c *runCtx) (map[string]any, error) { return r4(c, 25) },
			judge: judgeR4,
			known: map[string]string{"session energy counts again after each restart": "E1: a 1.6 transaction is not taken up after a restart, so session_wh stays 0 (handlers.go:615-617, #1787)"}},
		scenario{name: "r5a-25", dir: "r5a-start-car-charging-25A", minutes: 5, run: func(c *runCtx) (map[string]any, error) { return r5a(c, 25) },
			judge: judgeR5a},
		scenario{name: "r5b-25", dir: "r5b-start-stale-plan-25A", minutes: 27, run: func(c *runCtx) (map[string]any, error) { return r5b(c, 25) },
			judge: judgeR5b},
		scenario{name: "r5c-25", dir: "r5c-start-new-session-25A", minutes: 15, run: func(c *runCtx) (map[string]any, error) { return r5c(c, 25) },
			judge: judgeR5c},
	)
}

// ---- R1. The site meter goes silent, then offline, under Charge now ----

// meterKnob is the device-simulator change R1 waits for.
const meterKnob = "TODO(device-simulator): `silence` and `offline` must take the site meter " +
	"(device \"meter\"); at the pinned commit they answer `no device \"meter\"`. Pin a commit " +
	"that has it and delete this skip."

// r1 puts Core's battery relief on a meter that stops answering: Modbus
// timeouts for 120 s, then, two minutes after it is back, a meter gone for
// 90 s. House 15 A per phase and Charge now 16 A put the site over the fuse
// unless the battery helps.
func r1(c *runCtx, fuse float64) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(15), socPct: 50}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		sleepS(30)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		sleepS(90)
		st.mark("silent")
		if err := st.event(houseEvent{Do: "silence", Device: "meter", ForS: 120}); err != nil {
			return err
		}
		sleepS(120)
		st.mark("back")
		sleepS(120)
		st.mark("offline")
		if err := st.event(houseEvent{Do: "offline", Device: "meter", ForS: 90}); err != nil {
			return err
		}
		sleepS(90)
		st.mark("online")
		sleepS(120)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "r1", "fuse": fuse, "house_a": 15, "silent_s": 120, "offline_s": 90}, err
}

// ---- R2. The battery drops out while the car's room rests on it ----

// r2 holds the battery at a 3 kW discharge under a 20 A house, so the car's
// room rests on it, then takes the inverter off the site for 180 s: each
// phase rises 4-6 A at once. The car takes at most 10 A, so the site stays
// under 1.2 x the fuse, where Core's 60 s heat budget, not an instant cut,
// decides when the car stops (control/fuse_heat.go).
func r2(c *runCtx, fuse, delayS float64) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(20), socPct: 60,
		chargers: []charger{{delayS: delayS, carMaxA: 10}}}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		st.mark("plug_wait")
		sleepS(delayS + 20)
		st.mark("battery_hold")
		if err := st.post("/api/battery/manual_hold", map[string]any{"direction": "discharge", "power_w": 3000, "hold_s": 1800}, nil); err != nil {
			return err
		}
		st.mark("charge_now")
		if err := st.chargeNow("garage", 10, "3p", 0); err != nil {
			return err
		}
		sleepS(delayS + 90)
		st.mark("offline")
		if err := st.event(houseEvent{Do: "offline", Device: "inverter"}); err != nil {
			return err
		}
		sleepS(180)
		st.mark("online")
		if err := st.event(houseEvent{Do: "online", Device: "inverter"}); err != nil {
			return err
		}
		sleepS(120)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "r2", "fuse": fuse, "house_a": 20, "battery_hold_w": -3000,
		"charger_delay_s": delayS, "offline_s": 180}, err
}

// ---- R3. The battery near the planner's floor under fuse pressure ----

// r3 starts the battery at 12 % with the planner's floor at 10 % and the
// inverter's own at 5 %. House 15 A per phase and Charge now 16 A need the
// battery's help; the charger applies a profile 60 s late.
func r3(c *runCtx, fuse float64) (map[string]any, error) {
	const delayS, floor = 60, 0.10
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(15), socPct: 12, plannerSoCMin: floor,
		chargers: []charger{{delayS: delayS}}}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		st.mark("plug_wait")
		sleepS(delayS + 20)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		sleepS(600)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "r3", "fuse": fuse, "house_a": 15, "soc_pct": 12,
		"planner_floor_pct": floor * 100, "charger_delay_s": delayS}, err
}

// ---- R4. Core killed mid-charge and mid-discharge ----

// r4 kills Core with SIGKILL, as a power cut does: no driver cleanup, no
// history flush, an OCPP transaction left open. House 12 A per phase, the
// battery held at a 3 kW discharge, Charge now 16 A. The first kill lasts
// 20 s; two minutes later the second lasts 120 s, and the house steps to
// 16 A per phase while Core is gone. Like s7, the run first waits until the
// day's Home use is known, and long enough after it that one restart stays
// under the 10 % unknown-time rule. Core holds a battery for at most
// 1800 s, so the hold starts 300 s before the first kill and outlasts both.
func r4(c *runCtx, fuse float64) (map[string]any, error) {
	var serial string
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(12), socPct: 60}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		serial = st.serial(0)
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		if waitUntil(900*time.Second, 5*time.Second, func() bool {
			var s ftwStatus
			return st.get("/api/status", &s) == nil && s.Energy.Today.LoadWh != nil
		}) {
			st.mark("today_known")
		}
		sleepS(600)
		st.mark("battery_hold")
		if err := st.post("/api/battery/manual_hold", map[string]any{"direction": "discharge", "power_w": 3000, "hold_s": 1800}, nil); err != nil {
			return err
		}
		sleepS(300)
		st.mark("kill1")
		st.stopFTW(syscall.SIGKILL)
		sleepS(20)
		if err := st.startFTW(); err != nil {
			return err
		}
		st.mark("up1")
		sleepS(120)
		st.mark("kill2")
		st.stopFTW(syscall.SIGKILL)
		st.mark("step")
		if err := st.setLoadsA(16); err != nil {
			return err
		}
		sleepS(120)
		if err := st.startFTW(); err != nil {
			return err
		}
		st.mark("up2")
		sleepS(300)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "r4", "fuse": fuse, "house_a": 12, "step_a": 16, "battery_hold_w": -3000,
		"down_s": []float64{20, 120}, "charger": serial}, err
}

// ---- R5. Core starts into a running site ----

// r5a starts Core while the car charges on its own: plugged when the house
// comes up, it starts a local transaction and draws all it can, 16 A on
// 14 A of house load. The battery sits at the inverter's 5 % floor, so it
// cannot carry the car: the site runs 30 A per phase until Core acts.
func r5a(c *runCtx, fuse float64) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(14), socPct: 5, startAfterS: 60,
		chargers: []charger{{plugged: true}}}, func(st *stack) error {
		st.mark("ftw_up")
		sleepS(180)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "r5a", "fuse": fuse, "house_a": 14, "soc_pct": 5}, err
}

// r5b saves a goal with hours of slack, so no fresh plan need charge now,
// then stops Core for 20 min and starts it on the plan it saved. For the
// first minute the rig asks for the plan once a second (plan.jsonl). On
// master it does not reach D6: the saved plan is younger than MaxPlanAge
// (30 min) and a fresh one lands within seconds, so the deadline reserve
// never acts. D6's proof is its unit repro; this run guards the start.
func r5b(c *runCtx, fuse float64) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 50}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		if _, err := st.planTo("garage", 40, 60, 8*time.Hour); err != nil {
			return err
		}
		st.mark("plan")
		sleepS(120)
		st.mark("ftw_stop")
		st.stopFTW(syscall.SIGTERM)
		sleepS(1200)
		st.mark("ftw_start")
		if err := st.startFTW(); err != nil {
			return err
		}
		st.mark("ftw_up")
		f, err := os.Create(filepath.Join(st.rundir, "plan.jsonl"))
		if err != nil {
			return err
		}
		defer f.Close()
		for end := time.Now().Add(60 * time.Second); time.Now().Before(end); sleepS(1) {
			var p struct {
				Plan *struct {
					GeneratedAtMs int64 `json:"generated_at_ms"`
				} `json:"plan"`
			}
			if st.get("/api/mpc/plan", &p) == nil && p.Plan != nil {
				b, _ := json.Marshal(map[string]any{"t": now(), "generated_at_ms": p.Plan.GeneratedAtMs})
				f.Write(append(b, '\n'))
			}
		}
		sleepS(60)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "r5b", "fuse": fuse, "house_a": 3, "down_s": 1200}, err
}

// r5c holds Charge now on one car, stops Core for 10 min, in which that car
// leaves and another plugs in, and starts Core into the new session.
func r5c(c *runCtx, fuse float64) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: fuse, loadsW: houseA(3), socPct: 50}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		st.mark("charge_now")
		if err := st.chargeNow("garage", 16, "3p", 0); err != nil {
			return err
		}
		sleepS(60)
		st.mark("ftw_stop")
		st.stopFTW(syscall.SIGTERM)
		sleepS(300)
		off, on := false, true
		st.mark("unplug")
		if err := st.event(houseEvent{Do: "plug", Device: "garage", On: &off}); err != nil {
			return err
		}
		sleepS(30)
		st.mark("replug")
		if err := st.event(houseEvent{Do: "plug", Device: "garage", On: &on}); err != nil {
			return err
		}
		sleepS(270)
		st.mark("ftw_start")
		if err := st.startFTW(); err != nil {
			return err
		}
		st.mark("ftw_up")
		sleepS(180)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "r5c", "fuse": fuse, "house_a": 3, "down_s": 600}, err
}
