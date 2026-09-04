package pvperf

import (
	"context"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/coverage"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/strang"
)

// irradianceSource labels rows persisted by this service.
const irradianceSource = "strang"

// defaultLookbackDays is how far back the backfill scores on each run. Older
// closed days already have an immutable cached score and are skipped.
const defaultLookbackDays = 30

// rescoreTailDays is how many of the most recent days are recomputed on every
// run even if already scored, so the STRÅNG ~1-day analysis lag is corrected as
// data lands (a day scored against partial irradiance is refreshed next run).
const rescoreTailDays = 3

// Service backfills historical STRÅNG irradiance and scores realised PV
// production against it, once at startup and nightly thereafter. It is
// read-only with respect to control: it only fetches weather data and writes
// the irradiance_history + pv_performance_daily tables. Nil when the site has
// no usable PV geometry (scoring is impossible), which the API surfaces as
// {enabled:false}.
type Service struct {
	Store        *state.Store
	Strang       *strang.Client
	Lat, Lon     float64
	Arrays       []Array
	LookbackDays int

	stop chan struct{}
	done chan struct{}
}

// FromConfig builds a scoring Service from the weather config, mirroring how
// forecast.FromConfig derives per-plane geometry (explicit pv_arrays, else a
// single synthesized array from the legacy flat fields). Returns nil when no
// geometry is available — without arrays there is nothing to score against.
func FromConfig(cfg *config.Weather, ratedPVW float64, st *state.Store, userAgent string) *Service {
	if cfg == nil || st == nil {
		return nil
	}
	var arrays []Array
	for _, a := range cfg.PVArrays {
		// CompleteGeometry is the same gate forecast.arrayFromConfig uses: a
		// plane missing its tilt or azimuth is skipped rather than scored as
		// 0° flat, so expected-vs-actual is never measured against a plane
		// the operator never described.
		tiltDeg, azimuthDeg, ratedW, ok := a.CompleteGeometry()
		if !ok {
			continue
		}
		arrays = append(arrays, Array{RatedW: ratedW, TiltDeg: tiltDeg, AzimuthDeg: azimuthDeg})
	}
	if len(arrays) == 0 && ratedPVW > 0 {
		arrays = append(arrays, Array{RatedW: ratedPVW, TiltDeg: cfg.PVTiltDeg, AzimuthDeg: cfg.PVAzimuthDeg})
	}
	if len(arrays) == 0 {
		return nil
	}
	// STRÅNG only models the Nordic domain. Outside it every nightly backfill
	// would spend three HTTP requests to be told nothing, forever, so decline
	// to start at all. GET /api/data-sources is where an operator finds out
	// why — this is a silent no-op by design, not a hidden failure.
	if !coverage.Covers("strang", cfg.Latitude, cfg.Longitude) {
		slog.Info("pvperf: site is outside the STRÅNG domain, PV performance scoring disabled",
			"lat", cfg.Latitude, "lon", cfg.Longitude)
		return nil
	}
	return &Service{
		Store:        st,
		Strang:       strang.NewClient(userAgent),
		Lat:          cfg.Latitude,
		Lon:          cfg.Longitude,
		Arrays:       arrays,
		LookbackDays: defaultLookbackDays,
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
}

// Start runs an initial backfill shortly after boot, then nightly.
func (s *Service) Start(ctx context.Context) {
	go s.loop(ctx)
}

// Stop terminates the backfill loop and waits for it to drain.
func (s *Service) Stop() {
	close(s.stop)
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	// Delay the first run so boot isn't competing with a network fetch, and
	// so telemetry has a moment to settle before we read history.
	first := time.NewTimer(3 * time.Minute)
	defer first.Stop()
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-first.C:
			s.runBackfill(ctx)
		case <-tick.C:
			s.runBackfill(ctx)
		}
	}
}

// runBackfill fetches the lookback window from STRÅNG, persists the irradiance,
// and scores each closed day that is missing or within the rescore tail.
func (s *Service) runBackfill(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	windowStart := todayMidnight.AddDate(0, 0, -s.LookbackDays)

	// STRÅNG takes calendar dates (UTC). Pad by a day each side so local-day
	// boundaries are fully covered regardless of the UTC offset.
	hours, err := s.Strang.FetchWindow(fetchCtx, s.Lat, s.Lon,
		windowStart.UTC().AddDate(0, 0, -1), todayMidnight.UTC())
	if err != nil {
		slog.Warn("pvperf: STRÅNG fetch failed", "err", err)
		return
	}
	if len(hours) == 0 {
		slog.Info("pvperf: STRÅNG returned no data for window", "lat", s.Lat, "lon", s.Lon)
		return
	}

	fetchedAtMs := now.UnixMilli()
	rows := make([]state.IrradianceRow, 0, len(hours))
	for _, h := range hours {
		rows = append(rows, state.IrradianceRow{
			SlotTsMs:    h.HourStart.UnixMilli(),
			GHIWm2:      h.GHIWm2,
			DHIWm2:      h.DHIWm2,
			Source:      irradianceSource,
			FetchedAtMs: fetchedAtMs,
		})
	}
	if err := s.Store.SaveIrradiance(rows); err != nil {
		slog.Warn("pvperf: save irradiance failed", "err", err)
		return
	}

	scored := 0
	// Score closed days only (strictly before today's midnight).
	for i := s.LookbackDays; i >= 1; i-- {
		dayStart := todayMidnight.AddDate(0, 0, -i)
		dayEnd := dayStart.AddDate(0, 0, 1)
		day := dayStart.Format("2006-01-02")

		// Skip days already scored, except the recent tail we always refresh.
		if i > rescoreTailDays {
			if _, ok, _ := s.Store.LoadPVPerformanceDay(day); ok {
				continue
			}
		}
		if s.scoreDay(day, dayStart, dayEnd, hours, fetchedAtMs) {
			scored++
		}
	}
	slog.Info("pvperf: backfill complete", "irradiance_rows", len(rows), "days_scored", scored)
}

// scoreDay computes and persists one day's performance score. Returns false
// (and persists nothing) when there is no measured PV history for the day.
func (s *Service) scoreDay(day string, dayStart, dayEnd time.Time, hours []strang.IrradianceHour, fetchedAtMs int64) bool {
	startMs, endMs := dayStart.UnixMilli(), dayEnd.UnixMilli()

	dayHours := make([]Irradiance, 0, 24)
	for _, h := range hours {
		ms := h.HourStart.UnixMilli()
		if ms < startMs || ms >= endMs {
			continue
		}
		dayHours = append(dayHours, Irradiance{HourStart: h.HourStart, GHIWm2: h.GHIWm2, DHIWm2: h.DHIWm2})
	}

	de, err := s.Store.DailyEnergy(startMs, endMs-1)
	if err != nil {
		slog.Warn("pvperf: read actual energy failed", "day", day, "err", err)
		return false
	}
	if de.Intervals == 0 {
		// No measured history for this day — nothing to score against.
		return false
	}

	expectedWh := ExpectedWh(s.Lat, s.Lon, s.Arrays, dayHours)
	rec := state.PVPerformanceDay{
		Day:              day,
		ExpectedWh:       expectedWh,
		ActualWh:         de.PVWh,
		StrangDataDateMs: &fetchedAtMs,
	}
	if pr, ok := PerformanceRatio(expectedWh, de.PVWh); ok {
		rec.PR = &pr
	}
	if err := s.Store.SavePVPerformance(rec); err != nil {
		slog.Warn("pvperf: save score failed", "day", day, "err", err)
		return false
	}
	return true
}

// Load returns scored days in [sinceDay, untilDay] (inclusive YYYY-MM-DD).
func (s *Service) Load(sinceDay, untilDay string) ([]state.PVPerformanceDay, error) {
	return s.Store.LoadPVPerformance(sinceDay, untilDay)
}
