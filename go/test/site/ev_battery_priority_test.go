//go:build unix

package site

// #1868: the car has only [0, 4140] W, while the battery can charge at
// 6000 W. The actual devices run over Modbus and OCPP; marks and the
// physical meter record let the judge see which device yielded.
func s18(c *runCtx) (map[string]any, error) {
	err := with(c, stackSpec{fuseA: 16, loadsW: houseA(3), socPct: 40,
		lpExtra: map[string]any{"min_charge_w": 4140, "max_charge_w": 4140, "phase_mode": "3p", "allowed_steps_w": []float64{0, 4140}}}, func(st *stack) error {
		if err := st.plug(0); err != nil {
			return err
		}
		if err := st.post("/api/battery/manual_hold", map[string]any{"direction": "charge", "power_w": 6000, "hold_s": 1800}, nil); err != nil {
			return err
		}
		st.mark("charge_now")
		if err := st.chargeNow("garage", 6, "3p", 4140); err != nil {
			return err
		}
		sleepS(120)
		st.mark("small_step")
		if err := st.setLoadsA(3.1); err != nil {
			return err
		}
		sleepS(90)
		st.mark("no_room")
		if err := st.setLoadsA(16); err != nil {
			return err
		}
		sleepS(90)
		st.mark("room_returns")
		if err := st.setLoadsA(3); err != nil {
			return err
		}
		sleepS(150)
		st.mark("end")
		return nil
	})
	return map[string]any{"scenario": "s18", "fuse": 16, "battery_hold_w": 6000, "car_w": 4140}, err
}

func judgeS18(d *runData) (metrics, []criterion) {
	t0, small, none, back, end := d.marks["charge_now"], d.marks["small_step"], d.marks["no_room"], d.marks["room_returns"], d.marks["end"]
	pre := d.window(small-20, small)
	minor := d.window(small+10, none)
	paused := d.window(back-20, back)
	recovered := d.window(end-30, end)
	carMin := func(w []*sample) float64 { return minOf(w, func(r *sample) float64 { return carW(r, 0) }) }
	restart := d.firstT(back, func(r *sample) bool { return carW(r, 0) >= 4100 })
	batteryBefore, batteryAfter := meanOf(pre, batSigned), meanOf(d.window(none-20, none), batSigned)
	m := metrics{"battery_before_w": batteryBefore, "battery_after_w": batteryAfter, "restart_s": restart,
		"car_before_w": carMin(pre), "car_small_step_w": carMin(minor), "car_recovered_w": carMin(recovered),
		"fuse_blown": anyBlown(d.samples), "max_i_before_house_overload": maxOf(d.window(t0, none), maxAbsI)}
	return m, []criterion{
		crit("setup: car and battery charge together", len(pre) > 0 && carMin(pre) >= 4100 && batteryBefore > 1000),
		crit("battery yields before the minimum-step car", len(minor) > 0 && carMin(minor) >= 4100 && batteryAfter < batteryBefore-40),
		crit("car stops when house leaves no room", len(paused) > 0 && maxOf(paused, func(r *sample) float64 { return carW(r, 0) }) < 100),
		crit("car restarts automatically within 150 s", le(restart, 150) && len(recovered) > 0 && carMin(recovered) >= 4100),
		crit("own commands stay inside the rated fuse", maxOf(d.window(t0, none), maxAbsI) <= 16.01),
		crit("fuse holds", !anyBlown(d.samples)),
	}
}
