package heatrun

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
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
	// "<driver>/<metric>" the twin learns against. It is whatever that
	// metric is: a heat pump's electric draw, or a floor's estimated load.
	InputKWh *float64 `json:"input_kwh,omitempty"`
	// InputCoverage is the share of the slot covered by fresh samples. A
	// slot well below 1 says too little to judge.
	InputCoverage float64 `json:"input_coverage"`
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

// slotTrack is one zone's slot in progress.
type slotTrack struct {
	start    time.Time
	forecast *SlotForecast
	price    *float64
	startC   *float64
	input    string
	wh       float64
	covered  time.Duration
	sampleAt time.Time
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
		if tr.input == "" {
			continue
		}
		prev := tr.sampleAt
		tr.sampleAt = now
		if prev.IsZero() || !now.After(prev) {
			continue
		}
		dt := now.Sub(prev)
		if dt > inputFreshFor {
			continue
		}
		driver, metric := splitKey(tr.input)
		w, at, ok := s.in.Latest(driver, metric)
		if !ok || now.Sub(at) > inputFreshFor || w < 0 {
			continue
		}
		tr.wh += w * dt.Hours()
		tr.covered += dt
	}
}

// shadowLocked closes the slot that just ended for every zone and opens the
// one starting at start. It runs at Tick, after the plans.
func (s *Service) shadowLocked(cfg *config.Heat, zones []ZonePlan, plans map[int]heatplan.Plan, twins map[string]thermaltwin.ZoneStatus, price []float64, start, now time.Time, writeOn, gateOpen bool) {
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
		if zp.StartC != 0 || zp.Planned {
			nowC = fptr(zp.StartC)
		} else if v, err := s.reading(zc.Sensor, now); err == nil {
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
			if tr.input != "" {
				span := start.Sub(tr.start)
				if span > slot {
					span = slot
				}
				if span > 0 {
					out.InputCoverage = tr.covered.Seconds() / span.Seconds()
				}
				if tr.covered > 0 {
					out.InputKWh = fptr(tr.wh / 1000)
				}
			}
			rec.Last = out
		}
		if tr, ok := s.tracks[zc.Name]; !ok || tr.start.Before(start) {
			s.tracks[zc.Name] = &slotTrack{
				start: start, forecast: rec.Next, price: priceNow, startC: nowC,
				input: rec.HeatInput, sampleAt: now,
			}
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
func (l *JSONLLog) Append(records []ShadowRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("encode %s: %w", r.Zone, err)
		}
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
	return f.Close()
}
