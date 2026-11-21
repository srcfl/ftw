// Package heatrun plans the owner's heat zones with heatplan and, when the
// owner has turned writing on, holds each zone's control at the plan's value
// through drivercontrol (docs/adr/0012).
//
// It is shadow first. With heat.write off, the default, it plans every slot,
// shows the plan and what it would save, and writes nothing. With it on, a
// zone is written only when all of these hold: its twin is trusted, its
// control is declared as a setpoint, the plans of every zone together pass
// the owner's least saving, and no one else holds the driver. Anything else
// hands the device back to its own default, so a fault ends in the device
// running itself.
package heatrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/heatplan"
	"github.com/srcfl/ftw/go/internal/thermal"
	"github.com/srcfl/ftw/go/internal/thermaltwin"
)

const (
	// SlotH is the planning slot. Prices come in quarters.
	SlotH = 0.25
	slot  = 15 * time.Minute
	// A horizon shorter than this is not worth planning; one longer is cut.
	minSlots = 8
	maxSlots = 36 * 4
	// A reading older than this is not the zone now.
	maxReadingAge = 15 * time.Minute
	// A day's mean price needs this much of the day known.
	dayMinHours = 22
	// From this local hour the comfort is for tomorrow when its prices are
	// known, as the evening already heats for it.
	tomorrowFromHour = 20
	// How long a written value is held when the driver declares no limit.
	// Each tick writes again, so a value outlives a dead Core by at most
	// this long.
	defaultHold = 30 * time.Minute
)

// Price is one price slot, in the unit the owner's limits are written in
// (öre/kWh including grid fee and tax, as the price service stores it).
type Price struct {
	Start time.Time
	Len   time.Duration
	Value float64
}

// Writer is the part of drivercontrol.Holds the service writes through, so
// planned values and an operator's hold share one lock and one expiry.
type Writer interface {
	// Hold sends value to the control and holds it for d. It returns when
	// the hold expires.
	Hold(ctx context.Context, driver, control string, value float64, d time.Duration) (expiresAtMs int64, err error)
	// Active is the hold on the driver now: its control and expiry, and
	// whether there is one.
	Active(driver string) (control string, expiresAtMs int64, ok bool)
	// Release clears the hold and sends the driver's own default.
	Release(ctx context.Context, driver string) error
}

// Inputs is everything the service reads. Each is read at each tick.
type Inputs struct {
	Config  func() *config.Heat
	Twins   func() []thermaltwin.ZoneStatus
	Latest  func(driver, metric string) (float64, time.Time, bool)
	Prices  func(from, to time.Time) []Price
	Outdoor func(from time.Time, n int) ([]float64, bool)
	// Control is the declared control behind "<driver>/<control>".
	Control func(driver, control string) (drivers.CatalogControl, bool)
	// Writer is nil when nothing may be written, whatever the config says.
	Writer Writer
	// SiteFresh reports whether the site meter is current. Stale site-meter
	// data stops dispatch (AGENTS.md); nil means no meter is known, which
	// also stops it.
	SiteFresh func() bool
	// Shadow receives one record per zone at every plan; nil logs nothing.
	Shadow ShadowSink
	// Now is time.Now unless a test sets it.
	Now func() time.Time
}

// Gate is the owner's least saving as the last tick weighed it.
type Gate struct {
	Set      bool    `json:"set"`
	Limit    float64 `json:"min_per_day,omitempty"`
	PerDay   float64 `json:"per_day"`
	Open     bool    `json:"open"`
	HorizonH float64 `json:"horizon_h"`
}

// ZonePlan is one zone's plan and what was done with it.
type ZonePlan struct {
	Name    string `json:"name"`
	Sensor  string `json:"sensor"`
	Control string `json:"control,omitempty"`
	// Planned is false when the zone could not be planned; Reason says why.
	Planned bool   `json:"planned"`
	Reason  string `json:"reason,omitempty"`
	Twin    string `json:"twin_status,omitempty"`
	// StartC is the zone now; TargetC the owner's target and DayTargetC the
	// target the day's price moved it to.
	StartC      float64  `json:"start_c,omitempty"`
	TargetC     float64  `json:"target_c"`
	DayTargetC  float64  `json:"day_target_c"`
	DayMean     float64  `json:"day_mean_price,omitempty"`
	PeakSlots   int      `json:"peak_relief_slots,omitempty"`
	Value       *float64 `json:"value,omitempty"`
	Neutral     *float64 `json:"neutral_value,omitempty"`
	Saving      float64  `json:"saving"`
	Cost        float64  `json:"cost"`
	Baseline    float64  `json:"baseline_cost"`
	ExcursionKh float64  `json:"band_excursion_kh"`
	// Write is what was done with the control this tick.
	Write string          `json:"write"`
	Slots []heatplan.Slot `json:"slots,omitempty"`
}

// Snapshot is the last tick.
type Snapshot struct {
	At      time.Time  `json:"at"`
	Enabled bool       `json:"enabled"`
	Write   bool       `json:"write"`
	Gate    Gate       `json:"gate"`
	Zones   []ZonePlan `json:"zones"`
	// Prices is the horizon's price per slot, from At.
	Prices []float64 `json:"prices,omitempty"`
}

type held struct {
	control   string
	value     float64
	expiresMs int64
	changedAt time.Time
	hold      time.Duration
}

// refreshEvery is how often held values are checked between plans. A device
// that drops a written value by itself (CTC resets its control registers
// five minutes after the last write) is written again before its hold ends.
const refreshEvery = 30 * time.Second

// Service runs the plan every slot.
type Service struct {
	in Inputs

	mu       sync.Mutex
	last     Snapshot
	gateOpen bool
	held     map[string]held // by driver: what this service holds

	tracks       map[string]*slotTrack // by zone: the slot in progress
	shadowFailed bool

	cancel context.CancelFunc
	done   chan struct{}
}

// New returns a service that has not started.
func New(in Inputs) *Service {
	if in.Now == nil {
		in.Now = time.Now
	}
	return &Service{in: in, held: map[string]held{}}
}

// Start ticks at every slot boundary until Stop.
func (s *Service) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		// Let telemetry and the twins fill before the first plan.
		next := s.in.Now().Add(time.Minute)
		ticker := time.NewTicker(refreshEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if now := s.in.Now(); !now.Before(next) {
				s.Tick(ctx)
				next = now.Truncate(slot).Add(slot)
			} else {
				s.Refresh(ctx)
			}
		}
	}()
}

// Refresh writes a held value again when its hold would end before the next
// check, and hands the device back if the site meter went stale since the
// plan. It decides nothing new: values change only at Tick.
func (s *Service) Refresh(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.in.Now()
	s.sampleLocked(now)
	if s.in.Writer == nil {
		return
	}
	for driver, h := range s.held {
		if s.in.SiteFresh == nil || !s.in.SiteFresh() {
			s.releaseLocked(ctx, driver)
			continue
		}
		if time.UnixMilli(h.expiresMs).Sub(now) > 3*refreshEvery {
			continue
		}
		if c, exp, ok := s.in.Writer.Active(driver); !ok || c != h.control || exp != h.expiresMs {
			// Expired, or someone else holds it now: not ours to renew.
			delete(s.held, driver)
			continue
		}
		exp, err := s.in.Writer.Hold(ctx, driver, h.control, h.value, h.hold)
		if err != nil {
			delete(s.held, driver)
			slog.Warn("heat: renewing a hold failed", "driver", driver, "err", err)
			continue
		}
		h.expiresMs = exp
		s.held[driver] = h
	}
}

// Stop ends the ticks and hands back every driver this service holds.
func (s *Service) Stop() {
	if s.cancel == nil {
		return
	}
	s.cancel()
	<-s.done
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	for driver := range s.held {
		s.releaseLocked(ctx, driver)
	}
}

// Snapshot returns the last tick.
func (s *Service) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.last
	out.Zones = append([]ZonePlan(nil), s.last.Zones...)
	return out
}

// Tick plans every zone once and writes what may be written.
func (s *Service) Tick(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.in.Now()
	var cfg *config.Heat
	if s.in.Config != nil {
		cfg = s.in.Config()
	}
	snap := Snapshot{At: now}
	if cfg == nil || len(cfg.Zones) == 0 || cfg.Validate() != nil {
		s.last = snap
		s.releaseAllLocked(ctx)
		return
	}
	snap.Enabled = true
	snap.Write = cfg.Write && s.in.Writer != nil

	start := now.Truncate(slot)
	price := s.horizon(start)
	snap.Prices = price
	twins := map[string]thermaltwin.ZoneStatus{}
	if s.in.Twins != nil {
		for _, t := range s.in.Twins() {
			twins[t.Key] = t
		}
	}

	zones := make([]ZonePlan, len(cfg.Zones))
	plans := make([]heatplan.Plan, 0, len(cfg.Zones))
	byZone := map[int]heatplan.Plan{}
	reqs := make([]*heatplan.Request, len(cfg.Zones))
	for i, zc := range cfg.Zones {
		zp := ZonePlan{Name: zc.Name, Sensor: zc.Sensor, Control: zc.Control, TargetC: zc.TargetC, DayTargetC: zc.TargetC}
		req, err := s.request(zc, twins, price, start, now, &zp)
		if err != nil {
			zp.Reason = err.Error()
			zones[i] = zp
			continue
		}
		plan, err := heatplan.Propose(*req)
		if err != nil {
			zp.Reason = err.Error()
			zones[i] = zp
			continue
		}
		reqs[i] = req
		zp.Planned = true
		zp.Saving, zp.Cost, zp.Baseline, zp.ExcursionKh = plan.Saving(), plan.Cost, plan.BaselineCost, plan.BandExcursionKh
		zp.Slots = plan.Slots
		if len(plan.Slots) > 0 {
			v := plan.Slots[0].Value
			zp.Value = &v
		}
		plans = append(plans, plan)
		byZone[i] = plan
		zones[i] = zp
	}

	gate := heatplan.SavingsGate{}
	if cfg.Savings != nil {
		gate = *cfg.Savings
	}
	horizonH := float64(len(price)) * SlotH
	open, perDay := gate.Open(plans, horizonH, s.gateOpen)
	if len(plans) == 0 {
		open, perDay = false, 0
	}
	s.gateOpen = open
	snap.Gate = Gate{Set: gate.MinPerDay > 0, Limit: gate.MinPerDay, PerDay: perDay, Open: open, HorizonH: horizonH}

	wanted := map[string]bool{}
	for i, zc := range cfg.Zones {
		zones[i].Write = s.write(ctx, cfg, zc, &zones[i], reqs[i], twins, open, snap.Write, now, wanted)
	}
	// A driver this service holds that no zone wants any more goes back.
	for driver := range s.held {
		if !wanted[driver] {
			s.releaseLocked(ctx, driver)
		}
	}
	snap.Zones = zones
	s.last = snap
	s.shadowLocked(cfg, zones, byZone, twins, price, start, now, snap.Write, open)
}

// horizon is the price per slot from start, as far as prices run unbroken.
func (s *Service) horizon(start time.Time) []float64 {
	if s.in.Prices == nil {
		return nil
	}
	rows := s.in.Prices(start, start.Add(maxSlots*slot))
	var out []float64
	for k := 0; k < maxSlots; k++ {
		t := start.Add(time.Duration(k) * slot)
		v, ok := priceAt(rows, t)
		if !ok {
			break
		}
		out = append(out, v)
	}
	return out
}

func priceAt(rows []Price, t time.Time) (float64, bool) {
	for _, r := range rows {
		if !t.Before(r.Start) && t.Before(r.Start.Add(r.Len)) && !math.IsNaN(r.Value) && !math.IsInf(r.Value, 0) {
			return r.Value, true
		}
	}
	return 0, false
}

// dayMean is the mean price of the day the comfort is for: today, or from
// the evening tomorrow when tomorrow is known.
func (s *Service) dayMean(now time.Time) (float64, bool) {
	if s.in.Prices == nil {
		return 0, false
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	mean := func(d time.Time) (float64, bool) {
		end := d.AddDate(0, 0, 1)
		rows := s.in.Prices(d, end)
		var series []float64
		for t := d; t.Before(end); t = t.Add(slot) {
			if v, ok := priceAt(rows, t); ok {
				series = append(series, v)
			}
		}
		return heatplan.DayMean(series, SlotH, dayMinHours)
	}
	if now.Hour() >= tomorrowFromHour {
		if m, ok := mean(day.AddDate(0, 0, 1)); ok {
			return m, true
		}
	}
	return mean(day)
}

func splitKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}

func (s *Service) reading(key string, now time.Time) (float64, error) {
	if s.in.Latest == nil {
		return 0, errors.New("no telemetry")
	}
	driver, metric := splitKey(key)
	v, at, ok := s.in.Latest(driver, metric)
	if !ok {
		return 0, fmt.Errorf("%s has no reading", key)
	}
	if now.Sub(at) > maxReadingAge {
		return 0, fmt.Errorf("%s was last read %s ago", key, now.Sub(at).Round(time.Minute))
	}
	return v, nil
}

func constant(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// request builds one zone's request, or says why it cannot.
func (s *Service) request(zc config.HeatZone, twins map[string]thermaltwin.ZoneStatus, price []float64, start, now time.Time, zp *ZonePlan) (*heatplan.Request, error) {
	n := len(price)
	if n < minSlots {
		return nil, fmt.Errorf("only %d quarters of prices are known", n)
	}
	twin, ok := twins[zc.Sensor]
	if !ok {
		return nil, errors.New("no thermal twin learns this sensor")
	}
	zp.Twin = string(twin.Status)
	p := twin.Params
	if p.TauH == nil || p.CapacityKWhPerK == nil {
		return nil, errors.New("the twin has no physical model yet")
	}
	startC, err := s.reading(zc.Sensor, now)
	if err != nil {
		return nil, err
	}
	zp.StartC = startC

	var outdoor []float64
	if s.in.Outdoor != nil {
		if o, ok := s.in.Outdoor(start, n); ok && len(o) == n {
			outdoor = o
		}
	}
	var boundary []float64
	if zc.Boundary == config.HeatBoundaryOutdoor {
		if outdoor == nil {
			return nil, errors.New("no outdoor forecast for the horizon")
		}
		boundary = outdoor
	} else {
		b, err := s.reading(zc.Boundary, now)
		if err != nil {
			return nil, fmt.Errorf("boundary: %w", err)
		}
		boundary = constant(n, b)
	}
	if outdoor == nil {
		if zc.Source == config.HeatSourceHeatPump {
			return nil, errors.New("a heat pump needs the outdoor forecast for the horizon")
		}
		// Direct electric heat does not depend on the weather; the
		// planner still wants a series.
		outdoor = boundary
	}

	z := heatplan.Zone{
		TauH:            *p.TauH,
		CapacityKWhPerK: *p.CapacityKWhPerK,
		OtherGainKPerH:  p.OtherGainKPerH,
		StartC:          startC,
		BoundaryC:       boundary,
		TargetC:         constant(n, zc.TargetC),
		MinC:            constant(n, zc.MinC),
		MaxC:            constant(n, zc.MaxC),
		ComfortWeight:   zc.ComfortWeight,
	}
	if zc.PriceComfort != nil && zc.PriceComfort.Enabled() {
		if mean, ok := s.dayMean(now); ok {
			zp.DayMean = mean
			z = zc.PriceComfort.Apply(z, mean)
			zp.DayTargetC = z.TargetC[0]
		}
	}
	if zc.PeakRelief != nil && zc.PeakRelief.Enabled() {
		for _, v := range zc.PeakRelief.Slots(price, SlotH) {
			if v {
				zp.PeakSlots++
			}
		}
		z = zc.PeakRelief.Apply(z, price, SlotH)
	}

	act, neutral, err := s.actuator(zc)
	if err != nil {
		return nil, err
	}
	if neutral != nil {
		zp.Neutral = neutral
	}
	return &heatplan.Request{SlotH: SlotH, Price: price, OutdoorC: outdoor, Zone: z, Actuator: act}, nil
}

// actuator is the zone's control as the plan sees it. A zone with no
// control is planned as if a thermostat could be set anywhere in its band
// in half-degree steps, so the owner sees what a control would do.
func (s *Service) actuator(zc config.HeatZone) (heatplan.Actuator, *float64, error) {
	act := heatplan.Actuator{Strategy: thermal.InfluenceSetpoint, MaxHeatKW: zc.HeatKW, Source: zc.PlanSource()}
	lo, hi, step := zc.MinC, zc.MaxC, 0.5
	var declaredDefault *float64
	if zc.Control != "" {
		if s.in.Control == nil {
			return heatplan.Actuator{}, nil, errors.New("the control's declaration is not known")
		}
		c, ok := s.in.Control(splitKey(zc.Control))
		if !ok {
			return heatplan.Actuator{}, nil, fmt.Errorf("%s is not a declared control", zc.Control)
		}
		if c.Thermal == nil {
			return heatplan.Actuator{}, nil, fmt.Errorf("%s does not say what it does to heat", zc.Control)
		}
		in := c.Input
		if in.Min == nil || in.Max == nil {
			return heatplan.Actuator{}, nil, fmt.Errorf("%s declares no range", zc.Control)
		}
		if in.Step != nil && *in.Step > 0 {
			step = *in.Step
		}
		declaredDefault = in.Default
		switch c.Thermal.Strategy {
		case thermal.InfluenceSetpoint:
			lo, hi = math.Max(lo, *in.Min), math.Min(hi, *in.Max)
		case thermal.InfluenceCurveOffset:
			if zc.CurveEffect == nil {
				return heatplan.Actuator{}, nil, fmt.Errorf("%s is a curve offset; the zone needs curve_effect", zc.Control)
			}
			act.Strategy = thermal.InfluenceCurveOffset
			act.ZoneKPerUnit, act.SupplyKPerUnit = zc.CurveEffect.ZoneKPerUnit, zc.CurveEffect.SupplyKPerUnit
			lo, hi = *in.Min, *in.Max
		default:
			return heatplan.Actuator{}, nil, fmt.Errorf("%s is %s; only setpoints and curve offsets are planned yet", zc.Control, c.Thermal.Strategy)
		}
	}
	act.Values = candidates(lo, hi, step)
	if len(act.Values) == 0 {
		return heatplan.Actuator{}, nil, errors.New("the control can reach nothing inside the band")
	}
	// The device's own value: its declared default, or for a setpoint the
	// step nearest the target, for an offset the step nearest zero.
	aim := zc.TargetC
	if act.Strategy == thermal.InfluenceCurveOffset {
		aim = 0
	}
	if declaredDefault != nil {
		aim = *declaredDefault
	}
	neutral := act.Values[0]
	for _, v := range act.Values {
		if math.Abs(v-aim) < math.Abs(neutral-aim) {
			neutral = v
		}
	}
	return act, &neutral, nil
}

// candidates are the control's steps from lo to hi, at most 16; a fine step
// over a wide band is coarsened by whole multiples so lo and hi stay in.
func candidates(lo, hi, step float64) []float64 {
	if !(step > 0) || hi < lo {
		return nil
	}
	first := math.Ceil(lo/step-1e-9) * step
	count := int(math.Floor((hi-first)/step+1e-9)) + 1
	if count < 1 {
		return nil
	}
	mult := 1
	for (count-1)/mult+1 > 16 {
		mult++
	}
	var out []float64
	for i := 0; i < count; i += mult {
		out = append(out, math.Round((first+float64(i)*step)*1000)/1000)
	}
	return out
}

// write does what may be done with one zone's control and says what it did.
func (s *Service) write(ctx context.Context, cfg *config.Heat, zc config.HeatZone, zp *ZonePlan, req *heatplan.Request, twins map[string]thermaltwin.ZoneStatus, gateOpen, writeOn bool, now time.Time, wanted map[string]bool) string {
	if zc.Control == "" {
		return "shadow: no control"
	}
	if !writeOn {
		return "shadow: writing is off"
	}
	driver, control := splitKey(zc.Control)
	// The zone may claim the driver only while it would write to it.
	switch {
	case !zp.Planned || zp.Value == nil:
		return s.giveBack(ctx, driver, "not planned")
	case twins[zc.Sensor].Status != thermaltwin.StatusTrusted:
		return s.giveBack(ctx, driver, "twin not trusted")
	case !gateOpen:
		return s.giveBack(ctx, driver, "below the least saving")
	case s.in.SiteFresh == nil || !s.in.SiteFresh():
		return s.giveBack(ctx, driver, "site meter is stale")
	}
	if wanted[driver] {
		return "skipped: another zone writes to this driver"
	}
	value := *zp.Value
	if zp.Neutral != nil && value == *zp.Neutral {
		return s.giveBack(ctx, driver, "plan asks for the device's own")
	}
	ours, mine := s.held[driver]
	if c, exp, ok := s.in.Writer.Active(driver); ok && (!mine || c != ours.control || exp != ours.expiresMs) {
		return "skipped: held by someone else"
	}
	wanted[driver] = true

	hold := defaultHold
	minInterval := time.Duration(0)
	if c, ok := s.in.Control(driver, control); ok && c.Thermal != nil {
		if c.Thermal.DeviceHoldMaxS != nil && *c.Thermal.DeviceHoldMaxS > 0 {
			hold = min(hold, time.Duration(*c.Thermal.DeviceHoldMaxS)*time.Second)
		}
		if c.Thermal.MinIntervalS != nil {
			minInterval = time.Duration(*c.Thermal.MinIntervalS) * time.Second
		}
	}
	if mine && ours.value != value && now.Sub(ours.changedAt) < minInterval {
		return fmt.Sprintf("held %.1f: the control may change again in %s", ours.value, (minInterval - now.Sub(ours.changedAt)).Round(time.Minute))
	}
	if mine && ours.value == value && time.UnixMilli(ours.expiresMs).Sub(now) > 3*refreshEvery {
		return fmt.Sprintf("holding %.1f", value)
	}
	exp, err := s.in.Writer.Hold(ctx, driver, control, value, hold)
	if err != nil {
		delete(s.held, driver)
		slog.Warn("heat: write failed", "zone", zc.Name, "control", zc.Control, "err", err)
		return "write failed: " + err.Error()
	}
	changed := now
	if mine && ours.value == value {
		changed = ours.changedAt
	}
	s.held[driver] = held{control: control, value: value, expiresMs: exp, changedAt: changed, hold: hold}
	return fmt.Sprintf("wrote %.1f", value)
}

func (s *Service) giveBack(ctx context.Context, driver, why string) string {
	if _, mine := s.held[driver]; !mine {
		return "device runs itself: " + why
	}
	s.releaseLocked(ctx, driver)
	return "released: " + why
}

func (s *Service) releaseAllLocked(ctx context.Context) {
	for driver := range s.held {
		s.releaseLocked(ctx, driver)
	}
}

// releaseLocked hands the driver back only while the hold is still this
// service's: an operator's newer hold is not undone.
func (s *Service) releaseLocked(ctx context.Context, driver string) {
	ours := s.held[driver]
	delete(s.held, driver)
	if s.in.Writer == nil {
		return
	}
	if c, exp, ok := s.in.Writer.Active(driver); ok && (c != ours.control || exp != ours.expiresMs) {
		return
	}
	if err := s.in.Writer.Release(ctx, driver); err != nil {
		slog.Warn("heat: release failed", "driver", driver, "err", err)
	}
}
