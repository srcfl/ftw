package heatrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/heatplan"
	"github.com/srcfl/ftw/go/internal/thermaltwin"
)

// The shadow log is how a plan is checked against the house before it is
// allowed to write. At every slot boundary each zone gets one record: what
// the plan says for the slot starting now, and how the slot that just ended
// came out against what was said for it a slot ago.
//
// In shadow nothing is written, so the device runs itself and the house
// follows the planner's baseline, not its plan. The baseline's predicted
// temperature and electricity are therefore what the readings can confirm.
// The plan's own figures for the same slot are logged beside them, so the
// claimed saving can be set against a baseline the readings either bear out
// or do not.

// SlotForecast is what one plan said for one slot.
type SlotForecast struct {
	// Value is the control value the plan would hold.
	Value       float64 `json:"value"`
	PlanTempC   float64 `json:"plan_temp_c"`
	PlanElecKWh float64 `json:"plan_elec_kwh"`
	PlanHeatKWh float64 `json:"plan_heat_kwh"`
	PlanCost    float64 `json:"plan_cost"`
	// Base* is the device left alone through the slot.
	BaseTempC   float64 `json:"base_temp_c"`
	BaseElecKWh float64 `json:"base_elec_kwh"`
	BaseHeatKWh float64 `json:"base_heat_kwh"`
	BaseCost    float64 `json:"base_cost"`
}

// HorizonForecast is the whole plan as the savings gate weighs it.
type HorizonForecast struct {
	Hours        float64 `json:"hours"`
	Saving       float64 `json:"saving"`
	Cost         float64 `json:"cost"`
	BaselineCost float64 `json:"baseline_cost"`
	ExcursionKh  float64 `json:"band_excursion_kh"`
}

// SlotOutcome is the slot that just ended: what was said for it and what
// the readings showed.
type SlotOutcome struct {
	Start    time.Time     `json:"start"`
	Forecast *SlotForecast `json:"forecast,omitempty"`
	Price    *float64      `json:"price,omitempty"`
	// StartC and EndC are the zone's sensor at the slot's two ends; nil
	// when it had no fresh reading.
	StartC *float64 `json:"start_c,omitempty"`
	EndC   *float64 `json:"end_c,omitempty"`
	// InputKWh is the heat input integrated over the slot, from the same
	// "<driver>/<metric>" the twin learns against: a heat pump's space heat,
	// or a floor's estimated load.
	InputKWh *float64 `json:"input_kwh,omitempty"`
	// InputCoverage is the share of the slot covered by fresh samples. A
	// slot well below 1 says too little to judge.
	InputCoverage float64 `json:"input_coverage"`

	// ModelEndC is where the planner's model of the zone, as it stood at
	// the slot's start, puts the zone at its end given the heat that
	// actually went in. Set against EndC it tests the model FTW plans with,
	// whoever controlled the device, so it holds in shadow while another
	// controller runs the zone and with heat.write on alike.
	ModelEndC *float64 `json:"model_end_c,omitempty"`

	// ElecKWh is the electricity over the slot. For direct electric heat it
	// is the heat input itself. For a heat pump it is the pump's
	// "<driver>/hp_power_w", the whole pump, hot water included, as the
	// driver does not split its draw. ActualCost is ElecKWh at the slot's
	// price, set only when the samples cover most of the slot.
	ElecKWh      *float64 `json:"elec_kwh,omitempty"`
	ElecCoverage float64  `json:"elec_coverage"`
	ActualCost   *float64 `json:"actual_cost,omitempty"`
}

// ShadowRecord is one zone at one slot boundary.
type ShadowRecord struct {
	// At is the start of the slot the plan is for.
	At        time.Time `json:"at"`
	Zone      string    `json:"zone"`
	Sensor    string    `json:"sensor"`
	HeatInput string    `json:"heat_input,omitempty"`
	Source    string    `json:"source"`
	Twin      string    `json:"twin_status,omitempty"`
	// WriteOn is heat.write; Write is what was done with the control.
	WriteOn bool   `json:"write_on"`
	Write   string `json:"write"`
	Planned bool   `json:"planned"`
	Reason  string `json:"reason,omitempty"`
	GateOK  bool   `json:"gate_open"`

	Price   *float64         `json:"price,omitempty"`
	Next    *SlotForecast    `json:"next,omitempty"`
	Horizon *HorizonForecast `json:"horizon,omitempty"`
	Last    *SlotOutcome     `json:"last,omitempty"`
}

// ShadowSink receives each tick's records. It is called with the service's
// lock held, so it must not call back into the service.
type ShadowSink interface {
	Append(records []ShadowRecord) error
}

// inputFreshFor is how old a heat input sample may be and still count for
// the time since the last one.
const inputFreshFor = 5 * time.Minute

// judgeCoverage is the share of a slot the samples must cover before the
// slot's energy is turned into a model temperature or a cost.
const judgeCoverage = 0.9

// meter integrates one "<driver>/<metric>" in W over a slot.
type meter struct {
	key     string
	wh      float64
	covered time.Duration
}

// zoneModel is the planner's zone as one slot's plan saw it.
type zoneModel struct {
	tauH, capKWhPerK, gainKPerH, boundaryC float64
}

// endC is the zone after h hours at a steady heat of kw, as the planner
// steps it: towards the equilibrium the heat holds, at the zone's inertia.
func (m zoneModel) endC(startC, kw, h float64) float64 {
	eq := m.boundaryC + (kw/m.capKWhPerK+m.gainKPerH)*m.tauH
	return eq + (startC-eq)*math.Exp(-h/m.tauH)
}

// slotTrack is one zone's slot in progress.
type slotTrack struct {
	start    time.Time
	forecast *SlotForecast
	price    *float64
	startC   *float64
	// openedAt is when startC was read: the slot's start, or later when
	// the slot opened late, as the first plan after Core starts.
	openedAt time.Time
	input    string
	heat     *meter
	elec     *meter // nil when the heat input is the electricity
	model    *zoneModel
	sampleAt time.Time
}

// elecKey is what measures the zone's electricity: the heat input itself
// for direct electric heat and for a pump whose input already is its draw,
// otherwise the input driver's own heat power.
func elecKey(zc config.HeatZone, input string) string {
	if input == "" || zc.Source != config.HeatSourceHeatPump {
		return ""
	}
	driver, metric := splitKey(input)
	if metric == thermaltwin.HeatPowerMetric {
		return ""
	}
	return driver + "/" + thermaltwin.HeatPowerMetric
}

func fptr(v float64) *float64 { return &v }

// inputKey is the "<driver>/<metric>" the zone's heat is measured by: the
// owner's heat_input, or the source the twin found.
func inputKey(zc config.HeatZone, twin thermaltwin.ZoneStatus) string {
	if zc.HeatInput != "" {
		return zc.HeatInput
	}
	if twin.PowerSource == "" {
		return ""
	}
	if driver, metric := splitKey(twin.PowerSource); metric != "" {
		return driver + "/" + metric
	}
	return twin.PowerSource + "/" + thermaltwin.HeatPowerMetric
}

// sampleLocked adds the time since each zone's last sample to its slot,
// at the heat input read now. A gap longer than inputFreshFor, or a stale
// reading, adds nothing, and the coverage shows it.
func (s *Service) sampleLocked(now time.Time) {
	if s.tracks == nil || s.in.Latest == nil {
		return
	}
	for _, tr := range s.tracks {
		prev := tr.sampleAt
		tr.sampleAt = now
		if prev.IsZero() || !now.After(prev) {
			continue
		}
		dt := now.Sub(prev)
		if dt > inputFreshFor {
			continue
		}
		for _, m := range []*meter{tr.heat, tr.elec} {
			if m == nil {
				continue
			}
			driver, metric := splitKey(m.key)
			w, at, ok := s.in.Latest(driver, metric)
			if !ok || now.Sub(at) > inputFreshFor || w < 0 {
				continue
			}
			m.wh += w * dt.Hours()
			m.covered += dt
		}
	}
}

// coverage is the share of span the meter's samples cover, at most one:
// a sample a few seconds past the boundary still counts toward the slot.
func (m *meter) coverage(span time.Duration) float64 {
	if m == nil || span <= 0 {
		return 0
	}
	return math.Min(1, m.covered.Seconds()/span.Seconds())
}

// shadowLocked closes the slot that just ended for every zone and opens the
// one starting at start. It runs at Tick, after the plans.
func (s *Service) shadowLocked(cfg *config.Heat, zones []ZonePlan, plans map[int]heatplan.Plan, reqs []*heatplan.Request, twins map[string]thermaltwin.ZoneStatus, price []float64, start, now time.Time, writeOn, gateOpen bool) {
	if s.tracks == nil {
		s.tracks = map[string]*slotTrack{}
	}
	s.sampleLocked(now)
	horizonH := float64(len(price)) * SlotH
	var priceNow *float64
	if len(price) > 0 {
		priceNow = fptr(price[0])
	}
	records := make([]ShadowRecord, 0, len(cfg.Zones))
	seen := map[string]bool{}
	for i, zc := range cfg.Zones {
		zp := zones[i]
		twin := twins[zc.Sensor]
		rec := ShadowRecord{
			At: start, Zone: zc.Name, Sensor: zc.Sensor, Source: zc.Source,
			HeatInput: inputKey(zc, twin), Twin: zp.Twin,
			WriteOn: writeOn, Write: zp.Write, Planned: zp.Planned, Reason: zp.Reason,
			GateOK: gateOpen, Price: priceNow,
		}
		var nowC *float64
		// A planned zone read its sensor this tick; any other zone reads it
		// here, so a 0.0 °C start is never mistaken for a missing one.
		if zp.Planned {
			nowC = fptr(zp.StartC)
		} else if v, err := s.reading(zc.Sensor, now); err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
			nowC = fptr(v)
		}
		if plan, ok := plans[i]; ok && len(plan.Slots) > 0 {
			f := SlotForecast{
				Value:       plan.Slots[0].Value,
				PlanTempC:   plan.Slots[0].TempC,
				PlanElecKWh: plan.Slots[0].ElecKWh,
				PlanHeatKWh: plan.Slots[0].HeatKWh,
				PlanCost:    plan.Slots[0].Cost,
			}
			if len(plan.Baseline) > 0 {
				b := plan.Baseline[0]
				f.BaseTempC, f.BaseElecKWh, f.BaseHeatKWh, f.BaseCost = b.TempC, b.ElecKWh, b.HeatKWh, b.Cost
			}
			rec.Next = &f
			rec.Horizon = &HorizonForecast{
				Hours: horizonH, Saving: plan.Saving(), Cost: plan.Cost,
				BaselineCost: plan.BaselineCost, ExcursionKh: plan.BandExcursionKh,
			}
		}

		if tr, ok := s.tracks[zc.Name]; ok && tr.start.Before(start) {
			out := &SlotOutcome{Start: tr.start, Price: tr.price, StartC: tr.startC, EndC: nowC}
			// Only a forecast made for this very slot is compared; after a
			// gap (Core down, a slot without a plan) it is left out.
			if tr.start.Add(slot).Equal(start) {
				out.Forecast = tr.forecast
			}
			span := start.Sub(tr.start)
			if span > slot {
				span = slot
			}
			if tr.heat != nil {
				out.InputCoverage = tr.heat.coverage(span)
				if tr.heat.covered > 0 {
					out.InputKWh = fptr(tr.heat.wh / 1000)
				}
				// Only a whole slot from where it started: after a gap the
				// start temperature is not this slot's.
				if tr.model != nil && tr.startC != nil && span == slot && tr.start.Add(slot).Equal(start) &&
					out.InputCoverage >= judgeCoverage {
					kw := tr.heat.wh / 1000 / tr.heat.covered.Hours()
					out.ModelEndC = fptr(tr.model.endC(*tr.startC, kw, start.Sub(tr.openedAt).Hours()))
				}
			}
			elec := tr.elec
			if elec == nil {
				elec = tr.heat
			}
			if elec != nil {
				out.ElecCoverage = elec.coverage(span)
				if elec.covered > 0 {
					out.ElecKWh = fptr(elec.wh / 1000)
					if tr.price != nil && out.ElecCoverage >= judgeCoverage {
						out.ActualCost = fptr(*out.ElecKWh * *tr.price)
					}
				}
			}
			rec.Last = out
		}
		if tr, ok := s.tracks[zc.Name]; !ok || tr.start.Before(start) {
			nt := &slotTrack{
				start: start, forecast: rec.Next, price: priceNow, startC: nowC,
				openedAt: now, input: rec.HeatInput, sampleAt: now,
			}
			if rec.HeatInput != "" {
				nt.heat = &meter{key: rec.HeatInput}
				if k := elecKey(zc, rec.HeatInput); k != "" {
					nt.elec = &meter{key: k}
				}
			}
			if i < len(reqs) && reqs[i] != nil && len(reqs[i].Zone.BoundaryC) > 0 {
				z := reqs[i].Zone
				nt.model = &zoneModel{tauH: z.TauH, capKWhPerK: z.CapacityKWhPerK, gainKPerH: z.OtherGainKPerH, boundaryC: z.BoundaryC[0]}
			}
			s.tracks[zc.Name] = nt
		}
		seen[zc.Name] = true
		records = append(records, rec)
	}
	for name := range s.tracks {
		if !seen[name] {
			delete(s.tracks, name)
		}
	}
	if s.in.Shadow == nil || len(records) == 0 {
		return
	}
	if err := s.in.Shadow.Append(records); err != nil && !s.shadowFailed {
		s.shadowFailed = true
		slog.Warn("heat: writing the shadow log failed", "err", err)
	} else if err == nil {
		s.shadowFailed = false
	}
}

// JSONLLog appends records to a file, one JSON object per line. When the
// file passes MaxBytes it is moved to Path+".1", replacing the one before,
// so the log never holds much more than twice MaxBytes.
type JSONLLog struct {
	Path     string
	MaxBytes int64

	mu sync.Mutex
}

// Append writes records and rotates when the file has grown past MaxBytes.
// A record that cannot be encoded, such as one holding NaN, is left out and
// reported; the other zones' records are still written.
func (l *JSONLLog) Append(records []ShadowRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var buf bytes.Buffer
	var bad []error
	for _, r := range records {
		line, err := json.Marshal(r)
		if err != nil {
			bad = append(bad, fmt.Errorf("encode %s: %w", r.Zone, err))
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if buf.Len() == 0 {
		return errors.Join(bad...)
	}
	if l.MaxBytes > 0 {
		if fi, err := os.Stat(l.Path); err == nil && fi.Size()+int64(buf.Len()) > l.MaxBytes {
			if err := os.Rename(l.Path, l.Path+".1"); err != nil {
				return fmt.Errorf("rotate: %w", err)
			}
		}
	}
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return errors.Join(bad...)
}
