//go:build unix

package site

import (
	"encoding/json"
	"math"
	"path/filepath"
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
		// Room on a 16 A fuse: fuse - house, in 1 A steps. Core works to
		// the rating; safety_margin_a is a site's opt-in and the rig sets 0.
		// The target is a floor, not the line: the battery (SoC 50) covers
		// the house, so a Core that stops at fuse - 1 A also passes.
		target = math.Min(maxW, math.Floor(fuse-d.metaF("house_a"))*volt*3)
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
	overLongest, overTotal := d.runsOver(tStep, tBack, fuse)
	m["over_fuse_longest_s"], m["over_fuse_total_s"] = overLongest, overTotal
	m["i2_over_fuse_s"], m["i2_peak_a"] = d.i2Over(tCN, t1, fuse)
	m["back_under_fuse_s"] = roundp(d.firstT(tStep, func(r *sample) bool { return maxAbsI(r) <= fuse && r.T > tStep+1 }), 1)
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
	m["run_over_fuse_longest_s"], _ = d.runsOver(tCN, t1, fuse)
	m["run_max_i"] = round(maxOf(after, maxAbsI), 2)
	m["pre_step_over_fuse_longest_s"], _ = d.runsOver(tCN, tStep, fuse)
	return m, []criterion{
		crit("fuse holds", !isBlown),
		crit("over the fuse no longer than the response time + 3 s after the step", overLongest <= allowed),
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
	overLongest, overTotal := d.runsOver(t0, t1, fuse)
	m["over_fuse_longest_s"], m["over_fuse_total_s"] = overLongest, overTotal
	m["i2_over_fuse_s"], m["i2_peak_a"] = d.i2Over(t0, t1, fuse)
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
		crit("over the fuse no longer than the charger's delay + 7 s", overLongest <= allowed),
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
		crit("over the fuse at most 3 s on 3p", before["over_limit_longest_s"].(float64) <= 3),
		crit("over the fuse at most 3 s after the switch to 1p", after["over_limit_longest_s"].(float64) <= 3),
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
		crit("over the fuse at most 3 s after the step", after["over_limit_longest_s"].(float64) <= 3),
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
		crit("over the fuse at most 3 s", run["over_limit_longest_s"].(float64) <= 3),
		crit("fuse holds", !run["fuse_blown"].(bool)),
	}
}

// ---- R1-R5, the stable plan's P0 scenarios ----

// fuseInstantFactor is Core's: above this multiple of the fuse it cuts the
// car at once, with no heat budget (control/fuse_heat.go). mcbHourFactor is
// the current a miniature circuit breaker carries for an hour without a
// trip, the lower of the two ratings Core's heat model keeps under.
const fuseInstantFactor, mcbHourFactor = 1.2, 1.13

// judgeR1: while the meter is silent Core raises nothing and its battery
// order only moves toward 0; within the 60 s watchdog and two ticks it
// stands the charger down and hands the battery back; it never shows the
// meter as live; Charge now returns after the meter does and runs until the
// next event.
func judgeR1(d *runData) (metrics, []criterion) {
	tCN, tS, tB, t1 := d.marks["charge_now"], d.marks["silent"], d.marks["back"], d.marks["end"]
	tOff, tOn := d.marks["offline"], d.marks["online"]
	var o0 float64
	if r := d.at(tS); r != nil {
		o0 = batOrder(r)
	}
	wound := 0
	for _, r := range d.window(tS+2, tB) {
		if o := batOrder(r); math.Abs(o) > math.Abs(o0)+50 || o*o0 < 0 {
			wound++
		}
	}
	live := 0
	for _, w := range [][2]float64{{tS + 10, tB}, {tOff + 10, tOn}} {
		for _, r := range d.window(w[0], w[1]) {
			if dr, ok := status(r).Drivers["meter"]; ok && dr.Status == "ok" {
				live++
			}
		}
	}
	charging := func(r *sample) bool { return carW(r, 0) >= 0.95*minW }
	standdown := d.firstOffer(tS, func(p profile) bool { return p.Limit == 0 })
	handBack := d.firstT(tS, func(r *sample) bool { return !batForced(r) })
	back1, back2 := d.holdsFrom(tB, tOff, charging), d.holdsFrom(tOn, t1, charging)
	rises := d.offerRises(tS, tB)
	m := metrics{"battery_order_at_silence_w": math.Round(o0), "battery_wound_samples": wound,
		"offer_rises_while_silent": rises, "standdown_s": roundp(standdown, 1), "battery_hand_back_s": roundp(handBack, 1),
		"meter_live_while_stale_samples": live, "charge_now_back_s": []*float64{roundp(back1, 1), roundp(back2, 1)}}
	return m, []criterion{
		crit("the battery relieved the fuse when the meter fell silent", o0 <= -500), // the setup
		crit("no EV offer rises while the meter is silent", rises == 0),
		crit("the battery order moves only toward 0 while the meter is silent", wound == 0),
		crit("a 0 W charger order within 64 s of the silence", le(standdown, 64)),
		crit("the battery back on its own within 64 s of the silence", le(handBack, 64)),
		crit("the meter never shows live while it is silent or offline", live == 0),
		crit("Charge now back at its minimum within 95 s of each return, and held", le(back1, 95) && le(back2, 95)),
		crit("fuse holds", !anyBlown(d.window(tCN, t1))),
	}
}

// judgeR2: when the battery that carried the car's room drops out, the fuse
// holds, no offer counts the battery's last -3 kW as room, and Home use says
// it is estimated (the stable plan's R2). Under 1.2 x the fuse Core spends a
// 60 s heat budget before it cuts the car (control/fuse_heat.go), so the
// overage is judged by that contract: above 1.2 x it lasts no longer than
// the charger takes to answer, and the 60 s heat stays under the 1.13 x a
// miniature circuit breaker carries for an hour. How long the site stays
// over the fuse and when Core sends the cut are recorded.
func judgeR2(d *runData) (metrics, []criterion) {
	fuse, delay := d.metaF("fuse"), d.metaF("charger_delay_s")
	tCN, tOff, tOn, t1 := d.marks["charge_now"], d.marks["offline"], d.marks["online"], d.marks["end"]
	pre := meanOf(d.window(tOff-30, tOff), batSigned)
	allowed := delay + 7
	instant, _ := d.runsOver(tOff, tOn+60, fuseInstantFactor*fuse)
	overFuse, _ := d.runsOver(tOff, tOn+60, fuse)
	heatOver, heatPeak := d.i2Over(tOff-60, tOn+60, mcbHourFactor*fuse)
	during := summarize(d.window(tOff+15, tOn))
	marked := 0
	for k, v := range during.LoadQuality {
		if k != "null" && k != "" && k != "measured" {
			marked += v
		}
	}
	rises := d.offerRises(tOff, tOn)
	cut := d.firstOffer(tOff, func(p profile) bool { return p.Limit == 0 })
	m := metrics{"battery_w_before": math.Round(pre), "allowed_over_s": allowed,
		"over_fuse_x1_2_longest_s": instant, "over_fuse_longest_s": overFuse,
		"heat_peak_a": heatPeak, "heat_over_x1_13_s": heatOver, "cut_sent_s": roundp(cut, 1),
		"offer_rises_while_offline": rises, "during": during,
		"max_i_after_drop": round(maxOf(d.window(tOff, tOn+60), maxAbsI), 2)}
	return m, []criterion{
		crit("the battery discharged 2 kW or more before the drop", pre <= -2000), // the setup
		crit("fuse holds", !anyBlown(d.window(tCN, t1))),
		crit("over 1.2 x the fuse no longer than the charger's delay + 7 s after the drop", instant <= allowed),
		crit("the 60 s heat stays under 1.13 x the fuse", heatOver == 0),
		crit("no EV offer rises while the battery is offline", rises == 0),
		crit("Home use shown as estimated throughout the outage",
			during.Samples > 0 && during.LoadWShown == during.Samples && marked == during.Samples),
	}
}

// judgeR3: Core orders no discharge below the planner's floor, its battery
// order settles, and the car answers when the battery stops.
func judgeR3(d *runData) (metrics, []criterion) {
	fuse, delay, floor := d.metaF("fuse"), d.metaF("charger_delay_s"), d.metaF("planner_floor_pct")
	t0, tCN, t1 := d.marks["plug"], d.marks["charge_now"], d.marks["end"]
	run := d.window(t0, t1)
	below := 0
	for _, r := range run {
		if batOrder(r) < -50 && r.Bat.SoCPct < floor-0.5 {
			below++
		}
	}
	last := d.window(t1-300, t1)
	swing := 0.0
	if len(last) > 0 {
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, r := range last {
			lo, hi = math.Min(lo, batOrder(r)), math.Max(hi, batOrder(r))
		}
		swing = hi - lo
	}
	minSoC := minOf(run, func(r *sample) float64 { return r.Bat.SoCPct })
	// Core may work up to the fuse rating; safety_margin_a is a site's
	// opt-in, so the overage that counts is above the fuse. Between the
	// fuse and 1.2 x the fuse Core's 60 s heat is the budget (heatOver).
	overInstant, _ := d.runsOver(tCN, t1, fuse)
	over := d.heatRunsOver(tCN, t1, fuse)
	allowed := delay + 7
	m := metrics{"discharge_below_floor_samples": below, "battery_order_swing_last5min_w": math.Round(swing),
		"soc_min_pct": round(minSoC, 2), "over_fuse_longest_s": overInstant, "over_fuse_heat_longest_s": over,
		"allowed_over_s": allowed, "max_i": round(maxOf(run, maxAbsI), 2)}
	return m, []criterion{
		crit("the battery reached the planner floor", len(run) > 0 && minSoC <= floor+0.5), // the setup
		crit("no discharge ordered below the planner floor", below <= 10),
		crit("the battery order swings at most 100 W over the last 5 min", len(last) > 0 && swing <= 100),
		crit("over the fuse no longer than the charger's delay + 7 s", over <= allowed),
		crit("fuse holds", !anyBlown(run)),
	}
}

// judgeR4: after each hard kill Charge now is back within a minute and runs
// until the next kill or the end (a car still drawing from before the kill
// and stopped later is not back), the session energy counts again and
// state.db opens clean; the day's Home use
// survives the first kill. The second outage's 120 s would break the 10 %
// unknown-time rule on a run this short, so its day total is recorded only.
func judgeR4(d *runData) (metrics, []criterion) {
	fuse := d.metaF("fuse")
	tCN, k1, u1, k2, u2, t1 := d.marks["charge_now"], d.marks["kill1"], d.marks["up1"], d.marks["kill2"], d.marks["up2"], d.marks["end"]
	charging := func(r *sample) bool { return carW(r, 0) >= 0.95*minW }
	back1, back2 := d.holdsFrom(u1, k2, charging), d.holdsFrom(u2, t1, charging)
	before, after := todayLoadWh(d.window(k1-30, k1)), todayLoadWh(d.window(u1, k2))
	nullAfter := 0
	for _, r := range d.window(u1, k2) {
		if r.Status != nil && r.Status.Energy.Today.LoadWh == nil {
			nullAfter++
		}
	}
	kept := before != nil && after != nil && nullAfter == 0 && *after >= 0.99**before
	charger, _ := d.meta["charger"].(string)
	session := func(t0, t1 float64) float64 {
		v := 0.0
		for _, r := range d.window(t0, t1) {
			if dr, ok := status(r).Drivers[charger]; ok && dr.EVSessionWh != nil {
				v = math.Max(v, *dr.EVSessionWh)
			}
		}
		return v
	}
	recovery := d.logLines("database corrupt", "INTEGRITY CHECK FAILED")
	overAfter, _ := d.runsOver(u2, t1, fuse)
	m := metrics{"charge_now_back_s": []*float64{roundp(back1, 1), roundp(back2, 1)},
		"today_load_wh_before_kill1": before, "today_load_wh_before_kill2": after, "today_load_wh_null_after_kill1": nullAfter,
		"today_load_wh_end":        todayLoadWh(d.window(u2, t1)),
		"session_wh_before_kills":  []float64{session(k1-30, k1), session(k2-30, k2)},
		"session_wh_90s_after_ups": []float64{session(u1, u1+90), session(u2, u2+90)},
		"recovery_log_lines":       len(recovery), "over_fuse_longest_after_up2_s": overAfter}
	return m, []criterion{
		crit("Charge now back at its minimum within 60 s of each restart, and held", le(back1, 60) && le(back2, 60)),
		crit("the day's Home use kept within 1 % across the first kill", kept),
		crit("session energy counts again after each restart", session(u1, u1+90) > 0 && session(u2, u2+90) > 0),
		crit("state.db opens with no recovery error", len(recovery) == 0),
		crit("fuse holds", !anyBlown(d.window(tCN, t1))),
	}
}

// homeUseBack is the seconds from t0 to the first sample with Home use shown.
func (d *runData) homeUseBack(t0 float64) *float64 {
	return d.firstT(t0, func(r *sample) bool { return status(r).LoadW != nil })
}

// judgeR5a: Core starting beside a car that already draws its maximum, with
// the site over the fuse, brings the site under the limit within 30 s and
// keeps it there. The limit is the fuse: Core may work up to the rating,
// and safety_margin_a is a site's opt-in. Once under it, a reading between
// the fuse and 1.2 x the fuse counts only while the 60 s heat is over the
// fuse too (heatOver), as Core's cut does.
func judgeR5a(d *runData) (metrics, []criterion) {
	fuse := d.metaF("fuse")
	tS, tUp, t1 := d.marks["ftw_start"], d.marks["ftw_up"], d.marks["end"]
	pre := meanOf(d.window(tS-20, tS), func(r *sample) float64 { return carW(r, 0) })
	preI := maxOf(d.window(tS-20, tS), maxAbsI)
	fit := d.firstT(tUp, func(r *sample) bool { return maxAbsI(r) <= fuse })
	overAfter := 0.0
	if fit != nil {
		overAfter = d.heatRunsOver(tUp+*fit, t1, fuse)
	}
	shown := d.homeUseBack(tUp)
	m := metrics{"car_w_before_start": math.Round(pre), "under_limit_s": roundp(fit, 1), "over_after_that_longest_s": overAfter,
		"home_use_shown_s": roundp(shown, 1), "max_i_before_start": round(preI, 2)}
	return m, []criterion{
		crit("the car charged before Core started", pre >= minW),            // the setup
		crit("the site was over the fuse before Core started", preI > fuse), // the setup
		crit("under the fuse within 30 s of Core answering", le(fit, 30)),
		crit("over the fuse at most 3 s once under it", fit != nil && overAfter <= 3),
		crit("Home use shown within 30 s of Core answering", le(shown, 30)),
		crit("fuse holds", !anyBlown(d.window(tS, t1))),
	}
}

// judgeR5b: a Core that starts on a stale plan orders no charge until a
// fresh plan asks for it, at least for its first 30 s.
func judgeR5b(d *runData) (metrics, []criterion) {
	tS, tUp, t1 := d.marks["ftw_start"], d.marks["ftw_up"], d.marks["end"]
	fresh, polls := math.Inf(1), 0
	readLines(filepath.Join(d.dir, "plan.jsonl"), func(b []byte) {
		var l struct {
			T             float64 `json:"t"`
			GeneratedAtMs int64   `json:"generated_at_ms"`
		}
		if json.Unmarshal(b, &l) != nil {
			return
		}
		polls++
		if gen := float64(l.GeneratedAtMs) / 1000; gen >= tS {
			fresh = math.Min(fresh, gen)
		}
	})
	early := 0
	for _, p := range d.offers() {
		if p.T >= tS && p.T < tUp+30 && p.T < fresh && p.Limit > 0 {
			early++
		}
	}
	shown := d.homeUseBack(tUp)
	var freshS *float64
	if !math.IsInf(fresh, 1) {
		v := round(fresh-tUp, 1)
		freshS = &v
	}
	m := metrics{"plan_polls": polls, "fresh_plan_s": freshS, "positive_orders_before_fresh_plan": early,
		"home_use_shown_s": roundp(shown, 1)}
	return m, []criterion{
		crit("no positive EV order in the first 30 s without a fresh plan", early == 0),
		crit("Home use shown within 30 s of Core answering", le(shown, 30)),
		crit("fuse holds", !anyBlown(d.window(tS, t1))),
	}
}

// judgeR5c: Core starting into a session that began while it was away sees
// the car and shows Home use. Whether the new session keeps the old car's
// Charge now waits for the owner (ev-088): recorded, not judged.
func judgeR5c(d *runData) (metrics, []criterion) {
	tS, tUp, t1 := d.marks["ftw_start"], d.marks["ftw_up"], d.marks["end"]
	held, seen := 0, 0
	for _, r := range d.window(tUp+30, t1) {
		if r.LPs == nil {
			continue
		}
		seen++
		if firstLP(r).ManualActive {
			held++
		}
	}
	plugged := d.firstT(tUp, func(r *sample) bool { return r.LPs != nil && firstLP(r).Plugged })
	shown := d.homeUseBack(tUp)
	m := metrics{"charge_now_kept_samples": held, "samples": seen, "plugged_s": roundp(plugged, 1),
		"home_use_shown_s": roundp(shown, 1),
		"car_w_mean":       math.Round(meanOf(d.window(tUp+30, t1), func(r *sample) float64 { return carW(r, 0) }))}
	return m, []criterion{
		crit("Core sees the car plugged in within 60 s of answering", le(plugged, 60)),
		crit("Home use shown within 30 s of Core answering", le(shown, 30)),
		crit("fuse holds", !anyBlown(d.window(tS, t1))),
	}
}
