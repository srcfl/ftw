//go:build unix

package site

import (
	"bufio"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	volt = 230.0
	maxW = 11040.0 // the rig charger, 16 A on three phases
	minW = 4140.0  // the loadpoints' minimum, 6 A on three phases
)

// ---- what the house and the recorder wrote ----

// plantLine is a line of the house's plant.jsonl: a sample a second, an
// event or a mark.
type plantLine struct {
	T       float64                `json:"t"`
	Mark    string                 `json:"mark"`
	Event   json.RawMessage        `json:"event"`
	PhaseA  [3]float64             `json:"phase_a"` // through each fuse, + into the site
	Fuse    *plantFuse             `json:"fuse"`
	Devices map[string]plantDevice `json:"devices"`
}

type plantFuse struct {
	Damage [3]float64 `json:"damage"`
	Blown  [3]bool    `json:"blown"`
}

type plantDevice struct {
	W         float64 `json:"w"`         // a charger's draw
	BatteryW  float64 `json:"battery_w"` // an inverter's battery, + charging
	SoCPct    float64 `json:"soc_pct"`
	EMSCmd    float64 `json:"ems_cmd"` // 170 charge, 187 discharge, 204 stop
	EMSSetW   float64 `json:"ems_setpoint_w"`
	Connected bool    `json:"connected"`
}

type ftwStatus struct {
	Drivers map[string]struct {
		Status string `json:"status"`
	} `json:"drivers"`
	LoadQuality *string  `json:"load_quality"`
	LoadReason  *string  `json:"load_reason"`
	LoadW       *float64 `json:"load_w"`
	Energy      struct {
		Today struct {
			LoadWh         *float64 `json:"load_wh"`
			HistoryQuality *string  `json:"history_quality"`
		} `json:"today"`
	} `json:"energy"`
}

type ftwLoadpoint struct {
	ID      string `json:"id"`
	Plugged bool   `json:"plugged_in"`
	Phases  int    `json:"phases"`
	Charger struct {
		Known bool `json:"known"`
	} `json:"charger"`
	CommandedReason string `json:"commanded_reason"`
	ControlFeedback struct {
		Status   string `json:"status"` // following, not_following, limited...
		Severity string `json:"severity"`
		Reason   string `json:"reason"`
	} `json:"control_feedback"`
}

type ftwLine struct {
	T      float64    `json:"t"`
	Status *ftwStatus `json:"status"`
	LPs    *struct {
		Loadpoints []ftwLoadpoint `json:"loadpoints"`
	} `json:"lps"`
}

// sample is one second: the house's truth and the last FTW reading at or
// before it.
type sample struct {
	T      float64
	PhaseA [3]float64
	Damage [3]float64
	Blown  [3]bool
	CarW   []float64 // by loadpoint, in lpIDs order
	BatW   float64
	Bat    plantDevice // the inverter
	Status *ftwStatus
	LPs    []ftwLoadpoint
}

type frame struct {
	T     float64         `json:"t"`
	Dir   string          `json:"dir"`
	Frame json.RawMessage `json:"frame"`
}

// houseEventLine is a change the house made, as its record has it.
type houseEventLine struct {
	T     float64
	Event houseEvent
}

// runData is one finished scenario's record.
type runData struct {
	dir      string
	samples  []*sample
	marks    map[string]float64
	allMarks []markLine // every mark, in order, also repeated ones
	events   []houseEventLine
	frames   []frame // garage's charger
	meta     map[string]any
}

type markLine struct {
	T     float64
	Label string
}

func loadRun(dir string, meta map[string]any) (*runData, error) {
	d := &runData{dir: dir, marks: map[string]float64{}, meta: meta}
	var ftw []ftwLine
	if err := readLines(filepath.Join(dir, "ftw.jsonl"), func(b []byte) {
		var l ftwLine
		if json.Unmarshal(b, &l) == nil {
			ftw = append(ftw, l)
		}
	}); err != nil {
		return nil, err
	}
	next := 0
	var last *ftwLine
	err := readLines(filepath.Join(dir, "house", "plant.jsonl"), func(b []byte) {
		var l plantLine
		if json.Unmarshal(b, &l) != nil {
			return
		}
		switch {
		case l.Mark != "":
			if _, ok := d.marks[l.Mark]; !ok {
				d.marks[l.Mark] = l.T
			}
			d.allMarks = append(d.allMarks, markLine{l.T, l.Mark})
			return
		case l.Event != nil:
			var ev houseEvent
			if json.Unmarshal(l.Event, &ev) == nil {
				d.events = append(d.events, houseEventLine{l.T, ev})
			}
			return
		case l.Fuse == nil:
			return
		}
		for next < len(ftw) && ftw[next].T <= l.T {
			last = &ftw[next]
			next++
		}
		s := &sample{T: l.T, PhaseA: l.PhaseA, Damage: l.Fuse.Damage, Blown: l.Fuse.Blown,
			BatW: l.Devices["inverter"].BatteryW, Bat: l.Devices["inverter"]}
		for _, id := range lpIDs {
			if dev, ok := l.Devices[id]; ok {
				s.CarW = append(s.CarW, dev.W)
			}
		}
		// A reading older than 3 s is no reading: FTW was down or slow.
		if last != nil && l.T-last.T <= 3 {
			s.Status = last.Status
			if last.LPs != nil {
				s.LPs = last.LPs.Loadpoints
			}
		}
		d.samples = append(d.samples, s)
	})
	if err != nil {
		return nil, err
	}
	d.frames = d.loadFrames(0)
	return d, nil
}

// loadFrames is the OCPP frames charger i read and wrote, from its own log.
func (d *runData) loadFrames(i int) []frame {
	var out []frame
	readLines(filepath.Join(d.dir, "house", "ocpp-"+lpIDs[i]+".jsonl"), func(b []byte) {
		var f frame
		if json.Unmarshal(b, &f) == nil {
			out = append(out, f)
		}
	})
	return out
}

func readLines(path string, fn func([]byte)) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	return sc.Err()
}

func (d *runData) metaF(k string) float64 {
	v, _ := d.meta[k].(float64)
	if i, ok := d.meta[k].(int); ok {
		v = float64(i)
	}
	return v
}

// longestGap is the longest time between two samples, and when it began.
func (d *runData) longestGap() (gap, at float64) {
	for i := 1; i < len(d.samples); i++ {
		if g := d.samples[i].T - d.samples[i-1].T; g > gap {
			gap, at = g, d.samples[i-1].T-d.samples[0].T
		}
	}
	return gap, at
}

// ---- per-sample readings ----

func carW(r *sample, i int) float64 {
	if i < len(r.CarW) {
		return r.CarW[i]
	}
	return 0
}

// phaseI is the current through each phase's fuse, + into the site.
func phaseI(r *sample) [3]float64 { return r.PhaseA }

func maxAbsI(r *sample) float64 {
	m := 0.0
	for _, i := range r.PhaseA {
		m = math.Max(m, math.Abs(i))
	}
	return m
}

func damage(r *sample) float64 { return math.Max(r.Damage[0], math.Max(r.Damage[1], r.Damage[2])) }

func blown(r *sample) bool { return r.Blown[0] || r.Blown[1] || r.Blown[2] }

func firstLP(r *sample) ftwLoadpoint {
	if len(r.LPs) > 0 {
		return r.LPs[0]
	}
	return ftwLoadpoint{}
}

func reasons(r *sample) []string {
	lp := firstLP(r)
	set := map[string]bool{}
	for _, k := range []string{lp.CommandedReason, lp.ControlFeedback.Reason} {
		if k != "" {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

func status(r *sample) ftwStatus {
	if r.Status != nil {
		return *r.Status
	}
	return ftwStatus{}
}

func str(p *string) string {
	if p == nil {
		return "null"
	}
	return *p
}

func allRead(r *sample) bool {
	st := status(r)
	if len(st.Drivers) == 0 {
		return false
	}
	for _, d := range st.Drivers {
		if d.Status != "ok" {
			return false
		}
	}
	return true
}

func batSigned(r *sample) float64 { return r.BatW }

// ---- windows ----

func (d *runData) window(t0, t1 float64) []*sample {
	var out []*sample
	for _, r := range d.samples {
		if t0 <= r.T && r.T < t1 {
			out = append(out, r)
		}
	}
	return out
}

// firstT is the seconds from t0 to the first sample from t0 on that
// satisfies pred, or nil.
func (d *runData) firstT(t0 float64, pred func(*sample) bool) *float64 {
	for _, r := range d.samples {
		if r.T >= t0 && pred(r) {
			v := r.T - t0
			return &v
		}
	}
	return nil
}

// runsOver is the longest contiguous stretch and the total seconds with the
// worst phase above limit.
func (d *runData) runsOver(t0, t1, limit float64) (longest, total float64) {
	var curStart, prevT *float64
	for _, r := range d.window(t0, t1) {
		t := r.T
		if maxAbsI(r) > limit {
			if curStart == nil {
				curStart = &t
			}
			if prevT != nil {
				total += t - *prevT
			}
		} else {
			if curStart != nil {
				longest = math.Max(longest, t-*curStart)
			}
			curStart = nil
		}
		prevT = &t
	}
	if curStart != nil && prevT != nil {
		longest = math.Max(longest, *prevT-*curStart)
	}
	return round(longest, 1), round(total, 1)
}

// i2Over is the seconds the worst phase's I² filter (tau 60 s) is above
// limit, and the filter's peak.
func (d *runData) i2Over(t0, t1, limit float64) (over, peak float64) {
	const tau = 60.0
	var y, prev float64
	started := false
	for _, r := range d.window(t0, t1) {
		x := math.Pow(maxAbsI(r), 2)
		if !started {
			y, started = x, true
		} else {
			dt := r.T - prev
			y += (1 - math.Exp(-dt/tau)) * (x - y)
			if math.Sqrt(y) > limit {
				over += dt
			}
		}
		prev = r.T
		peak = math.Max(peak, math.Sqrt(y))
	}
	return round(over, 1), round(peak, 2)
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func meanOf(ws []*sample, f func(*sample) float64) float64 {
	xs := make([]float64, len(ws))
	for i, r := range ws {
		xs[i] = f(r)
	}
	return mean(xs)
}

func maxOf(ws []*sample, f func(*sample) float64) float64 {
	m := 0.0
	for _, r := range ws {
		m = math.Max(m, f(r))
	}
	return m
}

func round(x float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(x*p) / p
}

func roundp(p *float64, n int) *float64 {
	if p == nil {
		return nil
	}
	v := round(*p, n)
	return &v
}

// counter counts strings and lists the commonest first.
type counter map[string]int

func (c counter) top(n int) map[string]int {
	type kv struct {
		k string
		v int
	}
	var all []kv
	for k, v := range c {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	out := map[string]int{}
	for i, x := range all {
		if i >= n {
			break
		}
		out[x.k] = x.v
	}
	return out
}

// ---- OCPP profiles FTW sent ----

type profile struct {
	T       float64  `json:"t"`
	Conn    int      `json:"conn"`
	Purpose string   `json:"purpose"`
	Unit    string   `json:"unit"`
	Limit   float64  `json:"limit"`
	Phases  *float64 `json:"phases"`
}

func (p profile) phasesOr3() float64 {
	if p.Phases == nil || *p.Phases == 0 {
		return 3
	}
	return *p.Phases
}

func (p profile) amps() bool { return strings.HasPrefix(strings.ToUpper(p.Unit), "A") }

func (p profile) offerW() float64 {
	if p.amps() {
		return p.Limit * volt * p.phasesOr3()
	}
	return p.Limit
}

// perPhaseA is the offer in amperes per phase.
func (p profile) perPhaseA() float64 {
	if p.amps() {
		return p.Limit
	}
	return p.Limit / (volt * p.phasesOr3())
}

// profiles lists the SetChargingProfile CALLs FTW sent in [t0, t1), timed
// from t0.
func profiles(frames []frame, t0, t1 float64) []profile {
	var out []profile
	for _, f := range frames {
		if f.T < t0 || f.T >= t1 || f.Dir != "cs->cp" {
			continue
		}
		var arr []json.RawMessage
		if json.Unmarshal(f.Frame, &arr) != nil || len(arr) < 4 {
			continue
		}
		var typ int
		var action string
		json.Unmarshal(arr[0], &typ)
		json.Unmarshal(arr[2], &action)
		if typ != 2 || action != "SetChargingProfile" {
			continue
		}
		var req struct {
			ConnectorID int `json:"connectorId"`
			Profiles    struct {
				Purpose  string `json:"chargingProfilePurpose"`
				Schedule struct {
					Unit    string `json:"chargingRateUnit"`
					Periods []struct {
						Limit  float64  `json:"limit"`
						Phases *float64 `json:"numberPhases"`
					} `json:"chargingSchedulePeriod"`
				} `json:"chargingSchedule"`
			} `json:"csChargingProfiles"`
		}
		json.Unmarshal(arr[3], &req)
		p := profile{T: round(f.T-t0, 1), Conn: req.ConnectorID, Purpose: req.Profiles.Purpose, Unit: req.Profiles.Schedule.Unit}
		if per := req.Profiles.Schedule.Periods; len(per) > 0 {
			p.Limit, p.Phases = per[0].Limit, per[0].Phases
		}
		out = append(out, p)
	}
	return out
}

func connector1(ps []profile) []profile {
	var out []profile
	for _, p := range ps {
		if p.Conn == 1 {
			out = append(out, p)
		}
	}
	return out
}

// ---- verdicts ----

// metrics are a run's numbers, written to result.json.
type metrics map[string]any

// criterion is one pass condition. A scenario's known map names the
// criteria that fail on master and why.
type criterion struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

func crit(name string, ok bool) criterion { return criterion{Name: name, OK: ok} }

// ---- shared summaries ----

func (d *runData) carSummary(t0, t1, targetW float64) metrics {
	w := d.window(t0, t1)
	ts := d.firstT(t0, func(r *sample) bool { return carW(r, 0) >= 0.95*minW })
	tm := d.firstT(t0, func(r *sample) bool { return carW(r, 0) >= 0.98*targetW })
	var hold *float64
	if tm != nil {
		var after []*sample
		for _, r := range w {
			if r.T >= t0+*tm {
				after = append(after, r)
			}
		}
		n := 0
		for _, r := range after {
			if carW(r, 0) >= 0.95*targetW {
				n++
			}
		}
		h := round(float64(n)/math.Max(1, float64(len(after))), 3)
		hold = &h
	}
	drops, on := 0, false
	for _, r := range w {
		c := carW(r, 0)
		if c >= 0.9*minW {
			on = true
		} else if c < 500 && on {
			drops++
			on = false
		}
	}
	rs := counter{}
	for _, r := range w {
		for _, x := range reasons(r) {
			rs[x]++
		}
	}
	fuseLimit := 0
	for k, v := range rs {
		if strings.Contains(k, "fuse") {
			fuseLimit += v
		}
	}
	var last60 []float64
	for _, r := range w {
		if r.T >= t1-60 {
			last60 = append(last60, carW(r, 0))
		}
	}
	var minAfter *float64
	if ts != nil {
		m := math.Inf(1)
		for _, r := range w {
			if r.T >= t0+*ts {
				m = math.Min(m, carW(r, 0))
			}
		}
		m = math.Round(m)
		minAfter = &m
	}
	return metrics{
		"time_to_start_s":       roundp(ts, 1),
		"time_to_target_s":      roundp(tm, 1),
		"hold_fraction":         hold,
		"stops_after_start":     drops,
		"car_w_last60_mean":     math.Round(mean(last60)),
		"car_w_min_after_start": minAfter,
		"fuse_limit_samples":    fuseLimit,
		"reasons":               rs.top(6),
		"max_abs_i":             round(maxOf(w, maxAbsI), 2),
		"fuse_damage_max":       round(maxOf(w, damage), 4),
		"fuse_blown":            anyBlown(w),
	}
}

func anyBlown(ws []*sample) bool {
	for _, r := range ws {
		if blown(r) {
			return true
		}
	}
	return false
}

func le(p *float64, limit float64) bool { return p != nil && *p <= limit }

func (d *runData) overage(t0, t1, fuse float64) metrics {
	w := d.window(t0, t1)
	m := metrics{"max_i": round(maxOf(w, maxAbsI), 2)}
	m["over_limit_longest_s"], m["over_limit_total_s"] = d.runsOver(t0, t1, fuse-1)
	m["over_fuse_longest_s"], _ = d.runsOver(t0, t1, fuse)
	m["fuse_damage_max"] = round(maxOf(w, damage), 4)
	m["fuse_blown"] = anyBlown(w)
	return m
}

func (d *runData) phaseMean(t0, t1 float64) [3]float64 {
	w := d.window(t0, t1)
	var out [3]float64
	for k := 0; k < 3; k++ {
		out[k] = round(meanOf(w, func(r *sample) float64 { return math.Abs(phaseI(r)[k]) }), 1)
	}
	return out
}

// disconnects lists "OCPP charger disconnected" lines in FTW's log between
// t0 and t1, in seconds from t0.
func (d *runData) disconnects(t0, t1 float64) []float64 {
	out := []float64{}
	readLines(filepath.Join(d.dir, "ftw.log"), func(b []byte) {
		line := string(b)
		if !strings.Contains(line, "OCPP charger disconnected") || !strings.HasPrefix(line, "time=") {
			return
		}
		ts, _, _ := strings.Cut(strings.TrimPrefix(line, "time="), " ")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return
		}
		at := float64(t.UnixMicro()) / 1e6
		if t0 <= at && at <= t1 {
			out = append(out, round(at-t0, 1))
		}
	})
	return out
}
