//go:build unix

package site

import (
	"math"
	"sort"
)

// Each judge turns a finished run into numbers and pass criteria. The
// numbers and limits are the ones reports/simulatorprov-20261021 used.

func judgeS0(d *runData) (metrics, []criterion) {
	t0, t1 := d.marks["connected"], d.marks["end"]
	w := d.window(t0+20, t1)
	lq, lr, today := counter{}, counter{}, counter{}
	nAllRead := 0
	for _, r := range w {
		st := status(r)
		lq[str(st.LoadQuality)]++
		lr[str(st.LoadReason)]++
		today[str(st.Energy.Today.HistoryQuality)]++
		if allRead(r) {
			nAllRead++
		}
	}
	var last *float64
	if len(w) > 0 {
		last = status(w[len(w)-1]).Energy.Today.LoadWh
	}
	disc := d.disconnects(t0, t1-2) // the rig stops FTW at the end
	m := metrics{"load_quality": lq, "load_reason": lr, "today_history_quality": today,
		"all_read_samples": nAllRead, "samples_window": len(w), "today_load_wh_last": last,
		"disconnects_s": disc}
	return m, []criterion{
		crit("Home use measured in 95 % of samples", float64(lq["measured"]) >= 0.95*float64(len(w))),
		crit("no OCPP disconnects", len(disc) == 0),
	}
}

func judgeS1(d *runData) (metrics, []criterion) {
	t0, t1 := d.marks["charge_now"], d.marks["end"]
	fuse := d.metaF("fuse")
	target := maxW
	if fuse < 25 {
		// Room on a 16 A fuse: fuse - 1 A margin - house, in 1 A steps.
		target = math.Min(maxW, math.Floor(fuse-1-d.metaF("house_a"))*volt*3)
	}
	m := d.carSummary(t0, t1, target)
	m["target_w"] = target
	hold, _ := m["hold_fraction"].(*float64)
	cs := []criterion{
		crit("target within 60 s", le(m["time_to_target_s"].(*float64), 60)),
		crit("holds 95 % of the time", hold != nil && *hold >= 0.95),
		crit("fuse holds", !m["fuse_blown"].(bool)),
	}
	if fuse >= 25 {
		cs = append(cs, crit("no fuse_limit with room to spare", m["fuse_limit_samples"].(int) == 0))
	}
	return m, cs
}

func judgeS2(d *runData) (metrics, []criterion) {
	t0, t1 := d.marks["charge_now"], d.marks["end"]
	pre := d.window(t0-15, t0)
	var exp [3]float64
	for k := 0; k < 3; k++ {
		exp[k] = round(meanOf(pre, func(r *sample) float64 { return phaseI(r)[k] }), 2)
	}
	m := d.carSummary(t0, t1, maxW)
	m["export_a_before"] = exp
	return m, []criterion{
		crit("car starts within 60 s", le(m["time_to_start_s"].(*float64), 60)),
		crit("fuse holds", !m["fuse_blown"].(bool)),
	}
}

func judgeS3(d *runData) (metrics, []criterion) {
	fuse := d.metaF("fuse")
	tCN, tStep, tBack, t1 := d.marks["charge_now"], d.marks["step"], d.marks["step_back"], d.marks["end"]
	pre := d.window(tStep-30, tStep)
	m := metrics{
		"pre_step_car_w": math.Round(meanOf(pre, func(r *sample) float64 { return carW(r, 0) })),
		"pre_step_bat_w": math.Round(meanOf(pre, batSigned)),
		"pre_step_max_i": round(maxOf(pre, maxAbsI), 2),
	}
	m["post_step_max_i"] = round(maxOf(d.window(tStep, tBack), maxAbsI), 2)
	m["over_fuse_longest_s"], m["over_fuse_total_s"] = d.runsOver(tStep, tBack, fuse)
	overLongest, overTotal := d.runsOver(tStep, tBack, fuse-1)
	m["over_fuse_minus1_longest_s"], m["over_fuse_minus1_total_s"] = overLongest, overTotal
	m["i2_over_fuse_minus1_s"], m["i2_peak_a"] = d.i2Over(tCN, t1, fuse-1)
	m["back_under_fuse_minus1_s"] = roundp(d.firstT(tStep, func(r *sample) bool { return maxAbsI(r) <= fuse-1 && r.T > tStep+1 }), 1)
	last := d.window(tBack-60, tBack)
	m["post_step_car_w_last60"] = math.Round(meanOf(last, func(r *sample) float64 { return carW(r, 0) }))
	m["post_step_bat_w_last60"] = math.Round(meanOf(last, batSigned))
	var after []*sample
	for _, r := range d.samples {
		if r.T >= tCN {
			after = append(after, r)
		}
	}
	m["fuse_damage_max"] = round(maxOf(after, damage), 4)
	isBlown := anyBlown(d.samples)
	m["fuse_blown"] = isBlown
	// Device response + meter and tick (2 + 2 s) + 3 s.
	allowed := d.metaF("charger_delay_s") + 2 + 2 + 3
	m["allowed_over_s"] = allowed
	m["run_over_fuse_minus1_longest_s"], _ = d.runsOver(tCN, t1, fuse-1)
	m["run_max_i"] = round(maxOf(after, maxAbsI), 2)
	// Before the step, layers with different margins may hold a phase
	// between fuse - 1 A and fuse - 0.5 A; reported, not judged.
	m["pre_step_over_fuse_minus1_longest_s"], _ = d.runsOver(tCN, tStep, fuse-1)
	m["pre_step_over_fuse_minus_half_longest_s"], _ = d.runsOver(tCN, tStep, fuse-0.5)
	return m, []criterion{
		crit("fuse holds", !isBlown),
		crit("over fuse - 1 A no longer than the response time + 3 s after the step", overLongest <= allowed),
	}
}

func judgeS4(d *runData) (metrics, []criterion) {
	t0, t1 := d.marks["charge_now"], d.marks["end"]
	m := d.carSummary(t0, t1, maxW)
	tx := connector1(profiles(d.frames, t0, t1))
	m["profiles_sent"] = len(tx)
	offers := map[float64]bool{}
	for _, p := range tx {
		offers[math.Round(p.offerW())] = true
	}
	m["distinct_offers_w"] = sortedKeys(offers)
	zeros := 0
	for i := 1; i < len(tx); i++ {
		if tx[i-1].offerW() > 0 && tx[i].offerW() == 0 {
			zeros++
		}
	}
	m["offer_drops_to_zero"] = zeros
	return m, []criterion{
		crit("no stops", m["stops_after_start"].(int) == 0),
		crit("no offer drops to 0 W", zeros == 0),
		crit("car starts", m["time_to_start_s"].(*float64) != nil),
		crit("full power over the last minute", m["car_w_last60_mean"].(float64) >= 0.95*maxW),
	}
}

func sortedKeys(set map[float64]bool) []float64 {
	out := make([]float64, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Float64s(out)
	return out
}

func judgeS5(d *runData) (metrics, []criterion) {
	t0, t1 := d.marks["plugged"], d.marks["end"]
	prs := connector1(profiles(d.frames, t0, t1))
	ratingA := maxW / (3 * volt)
	var bad []profile
	over, maxOffer := 0, 0.0
	offers := map[float64]bool{}
	type ap struct {
		A      float64
		Phases float64
	}
	offersAP := map[ap]bool{}
	for _, p := range prs {
		offers[math.Round(p.offerW())] = true
		offersAP[ap{round(p.Limit, 2), p.phasesOr3()}] = true
		maxOffer = math.Max(maxOffer, p.perPhaseA())
		if p.Limit <= 0 {
			continue
		}
		a := p.perPhaseA()
		// A valid step is 6-16 A per phase: the charger is rated 16 A.
		if a < 6-1e-6 || a > ratingA+1e-6 {
			bad = append(bad, p)
		}
		if a > ratingA+1e-6 {
			over++
		}
	}
	w := d.window(t0+60, t1)
	pre := d.window(t0-10, t0+5)
	surplus := meanOf(pre, func(r *sample) float64 {
		i := phaseI(r)
		return -(i[0] + i[1] + i[2]) * volt
	})
	carMean := math.Round(meanOf(w, func(r *sample) float64 { return carW(r, 0) }))
	charging := 0
	for _, r := range w {
		if carW(r, 0) >= 0.95*minW {
			charging++
		}
	}
	frac := round(float64(charging)/math.Max(1, float64(len(w))), 3)
	rs := counter{}
	for _, r := range w {
		for _, x := range reasons(r) {
			rs[x]++
		}
	}
	all := d.window(t0, t1)
	isBlown := anyBlown(all)
	if len(bad) > 5 {
		bad = bad[:5]
	}
	m := metrics{
		"surplus_w_at_plug":        math.Round(surplus),
		"profiles_sent":            len(prs),
		"invalid_examples":         bad,
		"offers_w":                 firstN(sortedKeys(offers), 12),
		"car_w_mean_after_60s":     carMean,
		"charging_fraction_ge_min": frac,
		"time_to_start_s":          roundp(d.firstT(t0, func(r *sample) bool { return carW(r, 0) >= 0.95*minW }), 1),
		"reasons":                  rs.top(6),
		"offers_over_rating":       over,
		"max_offer_a":              round(maxOffer, 2),
		"fuse_blown":               isBlown,
		"fuse_damage_max":          round(maxOf(all, damage), 4),
		"surplus_used_fraction":    round(carMean/math.Max(1, math.Round(surplus)), 3),
	}
	nInvalid := 0
	for _, p := range prs {
		if a := p.perPhaseA(); p.Limit > 0 && (a < 6-1e-6 || a > ratingA+1e-6) {
			nInvalid++
		}
	}
	m["invalid_steps"] = nInvalid
	return m, []criterion{
		// The beta criteria (Fredrik, 21 Oct): no offer above the charger's
		// rating per phase, and the fuse never trips.
		crit("no offer above the charger's rating", over == 0),
		crit("fuse holds", !isBlown),
		crit("only valid steps (6-16 A)", nInvalid == 0),
		crit("charges at least the minimum 90 % of the time", frac >= 0.9),
	}
}

func firstN(xs []float64, n int) []float64 {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}

type qualitySummary struct {
	Samples     int            `json:"samples"`
	LoadWShown  int            `json:"load_w_shown"`
	LoadQuality map[string]int `json:"load_quality"`
	TodayFirst  *float64       `json:"today_load_wh_first"`
	TodayLast   *float64       `json:"today_load_wh_last"`
}

func summarize(ws []*sample) qualitySummary {
	q := qualitySummary{Samples: len(ws), LoadQuality: counter{}}
	for i, r := range ws {
		st := status(r)
		q.LoadQuality[str(st.LoadQuality)]++
		if st.LoadW != nil {
			q.LoadWShown++
		}
		if i == 0 {
			q.TodayFirst = st.Energy.Today.LoadWh
		}
		q.TodayLast = st.Energy.Today.LoadWh
	}
	return q
}

func judgeS6(d *runData) (metrics, []criterion) {
	tOff, tOn, t1 := d.marks["offline"], d.marks["online"], d.marks["end"]
	before := summarize(d.window(tOff-40, tOff))
	during := summarize(d.window(tOff+15, tOn))
	after := summarize(d.window(tOn+30, t1))
	// From 20 s after plug-in: FTW's start and the charger's first
	// MeterValues after plug-in are reported apart (plug_in_approx_s).
	plugT, ok := d.marks["plug"]
	if !ok && len(d.samples) > 0 {
		plugT = d.samples[0].T
	}
	var viol []*sample
	for _, r := range d.samples {
		if r.T >= plugT+20 && allRead(r) && str(status(r).LoadQuality) != "measured" && !(tOff <= r.T && r.T < tOn+30) {
			viol = append(viol, r)
		}
	}
	plugIn := 0
	for _, r := range d.window(plugT, plugT+20) {
		if allRead(r) && str(status(r).LoadQuality) != "measured" {
			plugIn++
		}
	}
	var firstViol *float64
	if len(viol) > 0 {
		v := round(viol[0].T-tOff, 1)
		firstViol = &v
	}
	marked := 0
	for k, v := range during.LoadQuality {
		if k != "null" && k != "" && k != "measured" {
			marked += v
		}
	}
	m := metrics{"before": before, "during": during, "after": after,
		"approx_while_all_read": len(viol), "approx_while_all_read_first": firstViol,
		"load_w_missing_during": during.Samples - during.LoadWShown, "plug_in_approx_s": plugIn}
	return m, []criterion{
		crit("Home use shown throughout the outage", during.LoadWShown == during.Samples),
		crit("Home use marked estimated throughout the outage", marked == during.Samples),
		crit("measured before the outage", before.LoadQuality["measured"] == before.Samples),
		crit("measured after the outage", after.LoadQuality["measured"] == after.Samples),
		crit("never estimated while every driver reads", len(viol) == 0),
	}
}

func judgeS7(d *runData) (metrics, []criterion) {
	tCN, tStop, tUp, t1 := d.marks["charge_now"], d.marks["ftw_stop"], d.marks["ftw_up"], d.marks["end"]
	car := func(r *sample) float64 { return carW(r, 0) }
	pre := d.window(tStop-30, tStop)
	preCar := meanOf(pre, car)
	post := d.window(tUp, t1)
	m := metrics{"pre_car_w": math.Round(preCar), "downtime_s": round(tUp-tStop, 1)}
	var before *float64
	if len(pre) > 0 {
		m["pre_load_w"] = status(pre[len(pre)-1]).LoadW
		before = status(pre[len(pre)-1]).Energy.Today.LoadWh
	}
	m["today_load_wh_before"] = before
	var afterFirst, afterLast *float64
	nullAfter := 0
	hq, lq := counter{}, counter{}
	for _, r := range post {
		// A sample before the rig's first status read after the restart
		// has no reading; only a status without the day total counts.
		if r.Status == nil {
			continue
		}
		st := status(r)
		v := st.Energy.Today.LoadWh
		if v == nil {
			nullAfter++
		} else if afterFirst == nil {
			afterFirst = v
		}
		afterLast = v
		hq[str(st.Energy.Today.HistoryQuality)]++
		lq[str(st.LoadQuality)]++
	}
	m["today_load_wh_after_first"], m["today_load_wh_null_after"], m["today_load_wh_after_last"] = afterFirst, nullAfter, afterLast
	m["history_quality_after"], m["load_quality_after"] = hq, lq
	loadBack := d.firstT(tUp, func(r *sample) bool { return status(r).LoadW != nil })
	m["load_w_back_s"] = roundp(loadBack, 1)
	m["car_back_s"] = roundp(d.firstT(tUp, func(r *sample) bool { return preCar > 0 && car(r) >= 0.95*preCar }), 1)
	m["car_resumed_s"] = roundp(d.firstT(tUp, func(r *sample) bool { return car(r) >= 0.95*minW }), 1)
	m["car_w_150s_after_up"] = math.Round(meanOf(d.window(tUp+90, tUp+150), car))
	rs := counter{}
	for _, r := range d.window(tUp+10, t1) {
		for _, x := range reasons(r) {
			rs[x]++
		}
	}
	m["reasons_after_up"] = rs.top(4)
	m["car_min_during_restart"] = math.Round(minOf(d.window(tStop, tUp+60), car))
	m["car_w_last60"] = math.Round(meanOf(d.window(t1-60, t1), car))
	if tk, ok := d.marks["today_known"]; ok {
		m["today_known_after_s"] = math.Round(tk - tCN)
	}
	minAfterStop := minOf(d.window(tStop, t1), car)
	m["car_min_after_stop"] = math.Round(minAfterStop)
	var fullFrom *float64
	dip, dipMin := 0, preCar
	for _, r := range post {
		if car(r) >= 0.95*preCar {
			if fullFrom == nil {
				v := r.T - tUp
				fullFrom = &v
			}
		} else {
			fullFrom = nil
			dip++
			dipMin = math.Min(dipMin, car(r))
		}
	}
	m["full_power_from_s"], m["dip_s"], m["dip_min_w"] = roundp(fullFrom, 1), dip, math.Round(dipMin)
	cs := []criterion{
		crit("Home use back within 30 s", le(loadBack, 30)),
		// Charge now continues without a stop and is back at full power
		// within 420 s of Core being up, for at least the last 30 s. The
		// dip is recorded.
		crit("Charge now never stops", minAfterStop >= 0.95*minW),
		crit("full power within 420 s", fullFrom != nil && *fullFrom <= 420 && t1-tUp-*fullFrom >= 30),
	}
	if d.meta["short"] != true {
		cs = append([]criterion{crit("the day's Home use is kept", before != nil && nullAfter == 0 &&
			afterLast != nil && *afterLast >= *before*0.99)}, cs...)
	}
	return m, cs
}

func minOf(ws []*sample, f func(*sample) float64) float64 {
	if len(ws) == 0 {
		return 0
	}
	m := math.Inf(1)
	for _, r := range ws {
		m = math.Min(m, f(r))
	}
	return m
}

func judgeS8(d *runData) (metrics, []criterion) {
	fuse := d.metaF("fuse")
	t0, t1 := d.marks["charge_now"], d.marks["end"]
	m := d.carSummary(t0, t1, maxW)
	m["over_fuse_longest_s"], m["over_fuse_total_s"] = d.runsOver(t0, t1, fuse)
	overLongest, overTotal := d.runsOver(t0, t1, fuse-1)
	m["over_fuse_minus1_longest_s"], m["over_fuse_minus1_total_s"] = overLongest, overTotal
	m["i2_over_fuse_minus1_s"], m["i2_peak_a"] = d.i2Over(t0, t1, fuse-1)
	var prs [][2]float64
	for i, p := range connector1(profiles(d.frames, t0, t1)) {
		if i >= 20 {
			break
		}
		prs = append(prs, [2]float64{p.T, math.Round(p.offerW())})
	}
	m["profiles"] = prs
	allowed := d.metaF("charger_delay_s") + 2 + 2 + 3
	m["allowed_over_s"] = allowed
	if plug, ok := d.marks["plug"]; ok {
		// Before FTW's first profile reaches the charger it draws its maximum.
		m["plugin_max_i"] = round(maxOf(d.window(plug, t0), maxAbsI), 2)
		m["plugin_over_fuse_longest_s"], _ = d.runsOver(plug, t0, fuse)
	}
	return m, []criterion{
		crit("fuse holds", !m["fuse_blown"].(bool)),
		crit("over fuse - 1 A no longer than the charger's delay + 7 s", overLongest <= allowed),
	}
}

func judgeS9(d *runData) (metrics, []criterion) {
	fuse := d.metaF("fuse")
	tCN, tSw, t1 := d.marks["charge_now"], d.marks["switch"], d.marks["end"]
	before, after := d.overage(tCN, tSw, fuse), d.overage(tSw, t1, fuse)
	car := func(r *sample) float64 { return carW(r, 0) }
	m := metrics{"before": before, "after": after,
		"before_phase_a": d.phaseMean(tSw-30, tSw), "after_phase_a_last60": d.phaseMean(t1-60, t1),
		"car_w_before": math.Round(meanOf(d.window(tSw-30, tSw), car)),
		"car_w_last60": math.Round(meanOf(d.window(t1-60, t1), car))}
	var seq [][3]any
	for _, p := range connector1(profiles(d.frames, tSw-5, t1)) {
		a := round(p.Limit, 2)
		if len(seq) == 0 || seq[len(seq)-1][1] != a || !samePhases(seq[len(seq)-1][2], p.Phases) {
			seq = append(seq, [3]any{p.T, a, p.Phases})
		}
	}
	if len(seq) > 12 {
		seq = seq[:12]
	}
	m["profiles_after_switch"] = seq
	return m, []criterion{
		crit("fuse holds", !before["fuse_blown"].(bool) && !after["fuse_blown"].(bool)),
		crit("over fuse - 1 A at most 3 s on 3p", before["over_limit_longest_s"].(float64) <= 3),
		crit("over fuse - 1 A at most 3 s after the switch to 1p", after["over_limit_longest_s"].(float64) <= 3),
	}
}

func samePhases(a any, b *float64) bool {
	pa, _ := a.(*float64)
	if pa == nil || b == nil {
		return pa == nil && b == nil
	}
	return *pa == *b
}

func judgeS10(d *runData) (metrics, []criterion) {
	fuse := d.metaF("fuse")
	tCN, tSt, t1 := d.marks["charge_now"], d.marks["step"], d.marks["end"]
	before, after := d.overage(tCN, tSt, fuse), d.overage(tSt, t1, fuse)
	m := metrics{"before": before, "after": after}
	pre, last := d.window(tSt-30, tSt), d.window(t1-60, t1)
	var preW, lastW [2]float64
	for i := 0; i < 2; i++ {
		i := i
		preW[i] = math.Round(meanOf(pre, func(r *sample) float64 { return carW(r, i) }))
		lastW[i] = math.Round(meanOf(last, func(r *sample) float64 { return carW(r, i) }))
	}
	m["pre_step_w"], m["last60_w"] = preW, lastW
	for i, name := range lpIDs {
		var seq [][2]float64
		for _, p := range connector1(profiles(d.loadFrames(i), tSt-5, t1)) {
			a := round(p.Limit, 1)
			if len(seq) == 0 || seq[len(seq)-1][1] != a {
				seq = append(seq, [2]float64{p.T, a})
			}
		}
		if len(seq) > 10 {
			seq = seq[:10]
		}
		m[name+"_profiles_after_step"] = seq
	}
	return m, []criterion{
		crit("fuse holds", !before["fuse_blown"].(bool) && !after["fuse_blown"].(bool)),
		crit("over fuse - 1 A at most 3 s after the step", after["over_limit_longest_s"].(float64) <= 3),
	}
}

func judgeS11(d *runData) (metrics, []criterion) {
	fuse := d.metaF("fuse")
	tCN, tP, tR, t1 := d.marks["charge_now"], d.marks["pause"], d.marks["restart"], d.marks["end"]
	car := func(r *sample) float64 { return carW(r, 0) }
	prs := connector1(profiles(d.frames, tCN-1, t1))
	var firstPos, paused *profile
	for i := range prs {
		p := &prs[i]
		if firstPos == nil && p.T >= tR-tCN && p.Limit > 0 {
			firstPos = p
		}
		if paused == nil && p.T >= tP-tCN && p.Limit == 0 {
			paused = p
		}
	}
	m := metrics{
		"car_w_before_pause": math.Round(meanOf(d.window(tP-20, tP), car)),
		"car_started_s":      roundp(d.firstT(tR, func(r *sample) bool { return car(r) >= 0.95*minW }), 1),
		"car_w_last60":       math.Round(meanOf(d.window(t1-60, t1), car)),
	}
	var firstPosS *float64
	if paused != nil {
		m["pause_profile_at_s"] = round(paused.T-(tP-tCN), 1)
	}
	if firstPos != nil {
		v := round(firstPos.T-(tR-tCN), 1)
		firstPosS = &v
		m["first_positive_offer_a"] = firstPos.Limit
	}
	m["first_positive_offer_s"] = firstPosS
	rs := counter{}
	for _, r := range d.window(tR+2, t1) {
		for _, x := range reasons(r) {
			rs[x]++
		}
	}
	m["reasons_after_restart"] = rs.top(4)
	run := d.overage(tCN, t1, fuse)
	for k, v := range run {
		m["run_"+k] = v
	}
	return m, []criterion{
		crit("an offer within 60 s of the restart", le(firstPosS, 60)),
		crit("over fuse - 1 A at most 3 s", run["over_limit_longest_s"].(float64) <= 3),
		crit("fuse holds", !run["fuse_blown"].(bool)),
	}
}
