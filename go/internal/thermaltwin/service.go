package thermaltwin

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/modelstate"
)

// HeatPowerMetric is the canonical electrical input of a heat pump, the same
// metric the Heating view discovers pumps by (docs/nibe-local.md).
const HeatPowerMetric = "hp_power_w"

const (
	tick = time.Minute
	// A reading older than this is not the zone now.
	maxReadingAge = 5 * time.Minute
	// A window needs most of its minutes; a sensor that reported for ten of
	// thirty is not a measurement of the window.
	minWindowSamples = 25
	// Temperatures at each end of a window are means of this many readings,
	// which is what keeps 0.1 K sensor steps from dominating.
	edgeSamples  = 3
	stateKeyRoot = "thermaltwin/"
)

// Zone is one declared temperature sensor the twin learns.
type Zone struct {
	// Key identifies the zone across restarts: driver name and metric.
	Key          string `json:"key"`
	SensorDriver string `json:"sensor_driver"`
	SensorMetric string `json:"sensor_metric"`
}

// MetricReader is the part of the telemetry store the service reads.
type MetricReader interface {
	LatestMetric(driver, name string) (float64, time.Time, bool)
}

// ConfigStore is the part of the state store the service persists to.
type ConfigStore interface {
	LoadConfig(key string) (string, bool)
	SaveConfig(key, value string) error
}

// Service samples every declared zone once a minute and feeds closed windows
// to its twin. It reads and stores; it commands nothing.
type Service struct {
	Store ConfigStore
	Tele  MetricReader
	// Temp is outdoor temperature at a time, the same source the load model
	// learns its heating coefficient from.
	Temp func(time.Time) (float64, bool)
	// Zones lists the declared sensors to learn. Called at start and at each
	// window boundary, so a config change is picked up within a window.
	Zones func() []Zone
	// PowerDrivers lists configured drivers that may report HeatPowerMetric.
	PowerDrivers func() []string

	mu        sync.Mutex
	zones     map[string]*zoneState
	refreshed time.Time

	stop chan struct{}
	done chan struct{}
}

type reading struct {
	at time.Time
	v  float64
}

type zoneState struct {
	zone  Zone
	twin  *Twin
	start time.Time
	tIn   []reading
	power []float64
	src   string
	gap   bool
	// lastEnd is the end of the last observed window, for contiguity.
	lastEnd time.Time
}

// NewService returns a stopped service.
func NewService(store ConfigStore, tele MetricReader) *Service {
	return &Service{Store: store, Tele: tele, zones: map[string]*zoneState{}}
}

// Start runs the sampling loop until Stop or ctx ends.
func (s *Service) Start(ctx context.Context) {
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		t := time.NewTicker(tick)
		defer t.Stop()
		s.Tick(time.Now())
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			case now := <-t.C:
				s.Tick(now)
			}
		}
	}()
}

// Stop ends the loop and waits for it.
func (s *Service) Stop() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done
	s.stop = nil
}

// Tick takes one minute's readings. Exported for tests and for a caller
// that drives time itself.
func (s *Service) Tick(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	windowStart := now.Truncate(Window)
	if !s.refreshed.Equal(windowStart) {
		s.refreshZonesLocked()
		s.refreshed = windowStart
	}
	for _, zs := range s.zones {
		if !zs.start.Equal(windowStart) {
			if !zs.start.IsZero() {
				s.closeWindowLocked(zs)
			}
			zs.start, zs.tIn, zs.power, zs.src, zs.gap = windowStart, nil, nil, "", false
		}
		s.sampleLocked(zs, now)
	}
}

func (s *Service) refreshZonesLocked() {
	if s.Zones == nil {
		return
	}
	want := map[string]Zone{}
	for _, z := range s.Zones() {
		if z.Key != "" {
			want[z.Key] = z
		}
	}
	for key := range s.zones {
		if _, ok := want[key]; !ok {
			delete(s.zones, key)
		}
	}
	for key, z := range want {
		if zs, ok := s.zones[key]; ok {
			zs.zone = z
			continue
		}
		s.zones[key] = &zoneState{zone: z, twin: s.restore(key)}
	}
}

func (s *Service) sampleLocked(zs *zoneState, now time.Time) {
	v, at, ok := s.read(zs.zone.SensorDriver, zs.zone.SensorMetric, now)
	if !ok {
		zs.gap = true
		return
	}
	src := s.powerSourceLocked(zs.zone, now)
	if len(zs.tIn) > 0 && src != zs.src {
		// Heat input appeared, vanished or moved mid-window; the window's
		// mean would mix two meanings.
		zs.gap = true
	}
	zs.src = src
	if src != "" {
		p, _, ok := s.read(src, HeatPowerMetric, now)
		if !ok {
			zs.gap = true
			return
		}
		zs.power = append(zs.power, p/1000)
	}
	zs.tIn = append(zs.tIn, reading{at: at, v: v})
}

// powerSourceLocked prefers the sensor's own driver, then the single other
// driver reporting heat power. Two candidates is no answer: the twin cannot
// tell which one heats this zone.
func (s *Service) powerSourceLocked(z Zone, now time.Time) string {
	if _, _, ok := s.read(z.SensorDriver, HeatPowerMetric, now); ok {
		return z.SensorDriver
	}
	if s.PowerDrivers == nil {
		return ""
	}
	var found []string
	for _, d := range s.PowerDrivers() {
		if _, _, ok := s.read(d, HeatPowerMetric, now); ok {
			found = append(found, d)
		}
	}
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

func (s *Service) read(driver, metric string, now time.Time) (float64, time.Time, bool) {
	if s.Tele == nil || driver == "" {
		return 0, time.Time{}, false
	}
	v, at, ok := s.Tele.LatestMetric(driver, metric)
	if !ok || now.Sub(at) > maxReadingAge || at.After(now.Add(time.Minute)) {
		return 0, time.Time{}, false
	}
	return v, at, true
}

func (s *Service) closeWindowLocked(zs *zoneState) {
	end := zs.start.Add(Window)
	if zs.gap || len(zs.tIn) < minWindowSamples || (zs.src != "" && len(zs.power) != len(zs.tIn)) {
		return
	}
	if s.Temp == nil {
		return
	}
	tOut, ok := s.Temp(zs.start.Add(Window / 2))
	if !ok {
		return
	}
	startC, startAt := edgeMean(zs.tIn[:edgeSamples])
	endC, endAt := edgeMean(zs.tIn[len(zs.tIn)-edgeSamples:])
	var powerKW float64
	for _, p := range zs.power {
		powerKW += p
	}
	if len(zs.power) > 0 {
		powerKW /= float64(len(zs.power))
	}

	if zs.src != "" && zs.twin.PowerSource != zs.src {
		if zs.twin.Samples > 0 {
			slog.Info("thermal twin restarts: heat input source changed",
				"zone", zs.zone.Key, "from", zs.twin.PowerSource, "to", zs.src)
			zs.twin = New()
		}
		zs.twin.PowerSource = zs.src
	}
	learned := zs.twin.Observe(Sample{
		TInStartC:  startC,
		TInEndC:    endC,
		TOutC:      tOut,
		PowerKW:    powerKW,
		DurationH:  endAt.Sub(startAt).Hours(),
		Contiguous: zs.lastEnd.Equal(zs.start),
	})
	if learned {
		zs.lastEnd = end
	}
	s.persist(zs)
}

func edgeMean(rs []reading) (float64, time.Time) {
	var sum float64
	var at int64
	for _, r := range rs {
		sum += r.v
		at += r.at.UnixNano()
	}
	n := int64(len(rs))
	return sum / float64(n), time.Unix(0, at/n)
}

func (s *Service) restore(key string) *Twin {
	t := New()
	if s.Store == nil {
		return t
	}
	js, ok := s.Store.LoadConfig(stateKeyRoot + key)
	if !ok {
		return t
	}
	var stored Twin
	res := modelstate.Unwrap(js, FeatureHash(), "", &stored)
	if !res.OK() {
		slog.Info("thermal twin cold start", "zone", key, "reason", res.Reason)
		return t
	}
	return &stored
}

func (s *Service) persist(zs *zoneState) {
	if s.Store == nil {
		return
	}
	js, err := modelstate.Wrap(FeatureHash(), zs.twin)
	if err == nil {
		err = s.Store.SaveConfig(stateKeyRoot+zs.zone.Key, js)
	}
	if err != nil {
		slog.Warn("thermal twin persist", "zone", zs.zone.Key, "err", err)
	}
}

// ZoneStatus is one zone's twin, as the API shows it.
type ZoneStatus struct {
	Zone
	PowerSource string  `json:"power_source,omitempty"`
	Status      Status  `json:"status"`
	Params      Params  `json:"params"`
	Samples     int64   `json:"samples"`
	Rejected    int64   `json:"rejected"`
	Evaluations int64   `json:"evaluations"`
	MAE3hK      float64 `json:"mae_3h_k"`
}

// Snapshot returns every zone's twin, sorted by key.
func (s *Service) Snapshot() []ZoneStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ZoneStatus, 0, len(s.zones))
	for _, zs := range s.zones {
		out = append(out, ZoneStatus{
			Zone:        zs.zone,
			PowerSource: zs.twin.PowerSource,
			Status:      zs.twin.Status(),
			Params:      zs.twin.Params(),
			Samples:     zs.twin.Samples,
			Rejected:    zs.twin.Rejected,
			Evaluations: zs.twin.Evaluations,
			MAE3hK:      zs.twin.MAE3hK,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
