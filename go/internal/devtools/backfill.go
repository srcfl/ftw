// Package devtools hosts developer-only utilities that ship inside the
// main FTW binary but stay dormant unless explicitly invoked
// (e.g. via the -backfill flag). Nothing here runs during normal service
// operation.
package devtools

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// backfillMarker is the JSON payload stamped on every synthetic row so
// the production-safety check can recognise its own output and refuse
// to run against any DB that contains rows not written here.
const backfillMarker = `{"source":"backfill"}`

// BackfillConfig parameterises a synthetic history seed.
type BackfillConfig struct {
	// Days of history to synthesise, ending at now. Required (> 0).
	Days int
	// Sample interval. Defaults to 5s if zero.
	Step time.Duration
	// RNG seed. Zero picks time.Now().UnixNano() (random-per-run).
	Seed int64
	// Force bypasses the "don't clobber real data" safety check. Only
	// set this when you genuinely want to seed a DB that already holds
	// non-synthetic rows.
	Force bool
}

// Backfill seeds state.db with synthetic HistoryPoints so the dashboard
// has something to render during local development / perf testing.
// Refuses to run when the target DB already contains non-synthetic
// history rows (any row missing our backfill JSON marker) unless
// cfg.Force is true — this is the prod-database safety gate.
func Backfill(s *state.Store, cfg BackfillConfig, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if cfg.Days <= 0 {
		return errors.New("backfill: days must be > 0")
	}
	step := cfg.Step
	if step <= 0 {
		step = 5 * time.Second
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}

	// ---- Production-safety gate ------------------------------------
	// Any existing history row that doesn't carry our backfill marker
	// is treated as real data — refuse to touch it.
	existing, err := s.CountHistoryWithoutMarker(backfillMarker)
	if err != nil {
		return fmt.Errorf("safety check: %w", err)
	}
	if existing > 0 && !cfg.Force {
		return fmt.Errorf(
			"refusing to backfill: found %d non-synthetic history rows "+
				"(looks like real data). Pass -backfill-force to override, "+
				"or point at a clean state.db", existing)
	}

	rng := rand.New(rand.NewSource(seed))
	log.Info("backfill: rng", "seed", seed)

	now := time.Now()
	start := now.Add(-time.Duration(cfg.Days) * 24 * time.Hour)
	loc := now.Location()

	total := int(now.Sub(start) / step)
	log.Info("backfill: seeding", "days", cfg.Days, "step", step, "rows", total)

	// Per-day weather + behaviour profile — each day keyed by local
	// YYYYMMDD XOR the run seed, so cloudy/busy/sunny stays consistent
	// across every sample within one day.
	dayProfile := func(local time.Time) (cloud, loadScale, pvScale, loadPhase float64) {
		key := local.Year()*10000 + int(local.Month())*100 + local.Day()
		r := rand.New(rand.NewSource(seed ^ int64(key)))
		cloud = 0.4 + 0.6*r.Float64()         // 0.4..1.0 (overcast → clear)
		loadScale = 0.75 + 0.5*r.Float64()    // 0.75..1.25 (quiet → busy household)
		pvScale = 0.85 + 0.3*r.Float64()      // 0.85..1.15 (panel efficiency drift)
		loadPhase = (r.Float64() - 0.5) * 1.0 // ±0.5h on evening peak timing
		return
	}

	// Intra-day AR(1) coloured noise (τ ≈ 90 s) so PV / load wobble
	// like real measurements instead of independent-sample white noise.
	var pvNoise, loadNoise float64
	noiseAlpha := math.Exp(-step.Seconds() / 90.0)

	const capWh = 15000.0
	const batchSize = 5000
	soc := 50.0
	written := 0
	batch := make([]state.HistoryPoint, 0, batchSize)
	reportEvery := total / 10
	if reportEvery == 0 {
		reportEvery = 1
	}

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := s.BulkRecordHistory(batch); err != nil {
			return err
		}
		written += len(batch)
		batch = batch[:0]
		return nil
	}

	for ts := start; ts.Before(now); ts = ts.Add(step) {
		local := ts.In(loc)
		h := float64(local.Hour()) + float64(local.Minute())/60.0 + float64(local.Second())/3600.0

		cloud, loadScale, pvScale, loadPhase := dayProfile(local)

		pvNoise = noiseAlpha*pvNoise + (1-noiseAlpha)*rng.NormFloat64()
		loadNoise = noiseAlpha*loadNoise + (1-noiseAlpha)*rng.NormFloat64()

		// Rare cloud shadow: ~0.08% of samples clip PV briefly.
		cloudDip := 1.0
		if rng.Float64() < 0.0008 {
			cloudDip = 0.3 + 0.3*rng.Float64()
		}

		// PV generation — bell curve centred at 13:00, scaled by day's
		// cloud/eff profile; √(signal)-scaled noise so nights stay quiet.
		pvGen := 0.0
		if h > 6 && h < 20 {
			x := (h - 13.0) / 3.5
			base := 9000 * math.Exp(-x*x) * cloud * pvScale
			pvGen = (base + 180*pvNoise*math.Sqrt(math.Max(base, 1))/95) * cloudDip
			if pvGen < 0 {
				pvGen = 0
			}
		}
		pvW := -pvGen // site convention: PV production is -W

		// Load: baseline + morning + evening peaks + rare appliance spikes.
		load := 350.0 + 80.0*loadNoise
		load += 2500 * math.Exp(-math.Pow((h-7.5)/1.2, 2)) * loadScale
		load += 3500 * math.Exp(-math.Pow((h-(19.0+loadPhase))/1.4, 2)) * loadScale
		if rng.Float64() < 0.0006 {
			load += 1500 + 1500*rng.Float64() // 1.5..3.0 kW spike
		}
		if load < 120 {
			load = 120
		}

		// Battery cascade with small efficiency jitter.
		surplus := pvGen - load
		batW := 0.0
		if surplus > 0 && soc < 95 {
			batW = math.Min(surplus*(0.75+0.1*rng.Float64()), 5000)
		} else if surplus < 0 && soc > 15 {
			batW = math.Max(surplus*(0.65+0.1*rng.Float64()), -4000)
		}

		// Grid residual + measurement noise.
		gridW := load - pvGen + batW + 30*rng.NormFloat64()

		// SoC update — Wh = W * dtH.
		dtH := step.Seconds() / 3600.0
		soc += batW * dtH / capWh * 100
		if soc > 100 {
			soc = 100
		}
		if soc < 0 {
			soc = 0
		}

		p := state.HistoryPoint{
			TsMs:   ts.UnixMilli(),
			GridW:  gridW,
			PVW:    pvW,
			BatW:   batW,
			LoadW:  load,
			BatSoC: soc,
			JSON:   backfillMarker,
		}
		batch = append(batch, p)
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return fmt.Errorf("bulk insert: %w", err)
			}
			log.Info("backfill: progress", "rows", written, "total", total,
				"pct", int(100*float64(written)/float64(total)))
		}
	}
	if err := flush(); err != nil {
		return fmt.Errorf("final flush: %w", err)
	}
	log.Info("backfill: done", "rows_written", written)
	return nil
}
