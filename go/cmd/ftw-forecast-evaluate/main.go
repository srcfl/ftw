// ftw-forecast-evaluate reads the bounded forecast archive and prints a causal
// JSON score report. It opens state.db read-only and never runs migrations.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

const maxEvaluationSamples = state.MaxForecastErrors

type report struct {
	AsOf               string                         `json:"as_of"`
	Since              string                         `json:"since"`
	IssueCount         int                            `json:"issue_count"`
	TruthCount         int                            `json:"truth_count"`
	PrimarySeries      string                         `json:"primary_series"`
	ReferenceSeries    string                         `json:"reference_series"`
	PerLead            []forecasting.Metric           `json:"per_lead"`
	BandCoverage       []bandCoverage                 `json:"band_coverage"`
	PairedMetrics      []forecasting.PairMetric       `json:"paired_metrics"`
	CumulativeNetError []forecasting.CumulativeMetric `json:"cumulative_net_errors"`
}

type bandCoverage struct {
	Series                       string  `json:"series"`
	Signal                       string  `json:"signal"`
	Lead                         int     `json:"lead_bucket"`
	EmpiricalSamples             int     `json:"empirical_samples"`
	EmpiricalCoverage80          float64 `json:"empirical_coverage_80"`
	ColdStartSamples             int     `json:"cold_start_samples"`
	ProvisionalSamples           int     `json:"provisional_samples"`
	ProvisionalRangeHitRate      float64 `json:"provisional_range_hit_rate"`
	ProvisionalInputCoverageMean float64 `json:"provisional_input_coverage_mean"`
}

type pointKey struct {
	series, config string
	start, end     int64
	lead           int
}

type cumulativeKey struct {
	series, config string
	start, end     int64
	lead, hours    int
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "ftw-forecast-evaluate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("ftw-forecast-evaluate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	statePath := fs.String("state", "state.db", "path to state.db")
	sinceText := fs.String("since", "", "first issue time, RFC3339 (default: 30 days before until)")
	untilText := fs.String("until", "", "last issue and truth time, RFC3339 (default: now)")
	primary := fs.String("primary", "champion", "actual primary series to compare")
	reference := fs.String("reference", "legacy_shadow", "frozen reference series from the same issue")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *primary == "" || *reference == "" || len(*primary) > 80 || len(*reference) > 80 || *primary == *reference {
		return errors.New("-primary and -reference must be distinct nonempty series names of at most 80 bytes")
	}
	until, err := parseBound(*untilText, now.UTC())
	if err != nil {
		return fmt.Errorf("parse -until: %w", err)
	}
	sinceDefault := until.Add(-state.ForecastIssueRetention)
	since, err := parseBound(*sinceText, sinceDefault)
	if err != nil {
		return fmt.Errorf("parse -since: %w", err)
	}
	if !since.Before(until) {
		return errors.New("-since must be before -until")
	}
	if until.Sub(since) > state.ForecastIssueRetention {
		return fmt.Errorf("evaluation window exceeds %s retention", state.ForecastIssueRetention)
	}

	store, err := state.OpenBackupSource(*statePath)
	if err != nil {
		return err
	}
	defer store.Close()

	observations, err := store.LoadForecastObservations(ctx, since.UnixMilli(), until.UnixMilli())
	if err != nil {
		return fmt.Errorf("load forecast truth: %w", err)
	}
	available := observations[:0]
	for _, observation := range observations {
		if observation.AvailableAtMS <= until.UnixMilli() {
			available = append(available, observation)
		}
	}
	observations = available
	points := make(map[pointKey]forecasting.ErrorSample)
	cumulative := make(map[cumulativeKey]forecasting.CumulativeEnergySample)
	issues := 0
	err = store.VisitForecastIssues(ctx, since.UnixMilli(), until.UnixMilli(), func(issue forecasting.Issue) error {
		issues++
		for _, sample := range forecasting.Errors([]forecasting.Issue{issue}, observations, until.UnixMilli()) {
			key := pointKey{sample.Series, sample.ConfigVersion, sample.StartMS, sample.EndMS, sample.Lead}
			if old, ok := points[key]; !ok || laterPoint(sample, old) {
				if !ok && len(points) == maxEvaluationSamples {
					return errors.New("point evaluation exceeds memory bound")
				}
				points[key] = sample
			}
		}
		// Build each window within one issue before choosing among origins. This
		// preserves the temporal correlation of one issued forecast.
		for _, sample := range forecasting.CumulativeNetEnergy([]forecasting.Issue{issue}, observations, until.UnixMilli()) {
			key := cumulativeKey{sample.Series, sample.ConfigVersion, sample.StartMS, sample.EndMS, sample.Lead, sample.Hours}
			if old, ok := cumulative[key]; !ok || laterCumulative(sample, old) {
				if !ok && len(cumulative) == maxEvaluationSamples {
					return errors.New("cumulative evaluation exceeds memory bound")
				}
				cumulative[key] = sample
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("visit forecast issues: %w", err)
	}

	pointSamples := sortedPoints(points)
	cumulativeSamples := sortedCumulative(cumulative)
	report := report{
		AsOf:               until.UTC().Format(time.RFC3339),
		Since:              since.UTC().Format(time.RFC3339),
		IssueCount:         issues,
		TruthCount:         len(observations),
		PrimarySeries:      *primary,
		ReferenceSeries:    *reference,
		PerLead:            forecasting.Metrics(pointSamples),
		BandCoverage:       summarizeBands(pointSamples),
		PairedMetrics:      forecasting.CompareFrozenSeries(pointSamples, *primary, *reference),
		CumulativeNetError: forecasting.CumulativeMetrics(cumulativeSamples),
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func parseBound(text string, fallback time.Time) (time.Time, error) {
	if text == "" {
		return fallback, nil
	}
	value, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return time.Time{}, err
	}
	return value.UTC(), nil
}

func laterPoint(a, b forecasting.ErrorSample) bool {
	if a.OriginMS != b.OriginMS {
		return a.OriginMS > b.OriginMS
	}
	if a.IssuedAtMS != b.IssuedAtMS {
		return a.IssuedAtMS > b.IssuedAtMS
	}
	return a.IssueID > b.IssueID
}

func laterCumulative(a, b forecasting.CumulativeEnergySample) bool {
	if a.OriginMS != b.OriginMS {
		return a.OriginMS > b.OriginMS
	}
	if a.IssuedAtMS != b.IssuedAtMS {
		return a.IssuedAtMS > b.IssuedAtMS
	}
	return a.IssueID > b.IssueID
}

func sortedPoints(chosen map[pointKey]forecasting.ErrorSample) []forecasting.ErrorSample {
	out := make([]forecasting.ErrorSample, 0, len(chosen))
	for _, sample := range chosen {
		out = append(out, sample)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.StartMS != b.StartMS {
			return a.StartMS < b.StartMS
		}
		if a.Series != b.Series {
			return a.Series < b.Series
		}
		if a.ConfigVersion != b.ConfigVersion {
			return a.ConfigVersion < b.ConfigVersion
		}
		if a.Lead != b.Lead {
			return a.Lead < b.Lead
		}
		return a.IssueID < b.IssueID
	})
	return out
}

func sortedCumulative(chosen map[cumulativeKey]forecasting.CumulativeEnergySample) []forecasting.CumulativeEnergySample {
	out := make([]forecasting.CumulativeEnergySample, 0, len(chosen))
	for _, sample := range chosen {
		out = append(out, sample)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.StartMS != b.StartMS {
			return a.StartMS < b.StartMS
		}
		if a.Series != b.Series {
			return a.Series < b.Series
		}
		if a.ConfigVersion != b.ConfigVersion {
			return a.ConfigVersion < b.ConfigVersion
		}
		if a.Hours != b.Hours {
			return a.Hours < b.Hours
		}
		if a.Lead != b.Lead {
			return a.Lead < b.Lead
		}
		return a.IssueID < b.IssueID
	})
	return out
}

func summarizeBands(samples []forecasting.ErrorSample) []bandCoverage {
	type key struct {
		series, signal string
		lead           int
	}
	type acc struct {
		bandCoverage
		empiricalHit, provisionalHit float64
	}
	all := make(map[key]*acc)
	for _, sample := range samples {
		if sample.Validate() != nil {
			continue
		}
		for _, signal := range []string{"pv", "load", "net"} {
			actual, known, band, evidence := bandInputs(sample, signal)
			if !known {
				continue
			}
			key := key{sample.Series, signal, sample.Lead}
			a := all[key]
			if a == nil {
				a = &acc{bandCoverage: bandCoverage{Series: sample.Series, Signal: signal, Lead: sample.Lead}}
				all[key] = a
			}
			switch band.Method {
			case forecasting.BandMethodEmpirical:
				a.EmpiricalSamples++
				if actual >= band.LowW && actual <= band.HighW {
					a.empiricalHit++
				}
			case forecasting.BandMethodColdStart:
				a.ColdStartSamples++
			}
			if evidence != nil {
				a.ProvisionalSamples++
				a.ProvisionalInputCoverageMean += evidence.Coverage
				if actual >= evidence.LowerW && actual <= evidence.UpperW {
					a.provisionalHit++
				}
			}
		}
	}
	keys := make([]key, 0, len(all))
	for key := range all {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].series != keys[j].series {
			return keys[i].series < keys[j].series
		}
		if keys[i].signal != keys[j].signal {
			return keys[i].signal < keys[j].signal
		}
		return keys[i].lead < keys[j].lead
	})
	out := make([]bandCoverage, 0, len(keys))
	for _, key := range keys {
		a := all[key]
		if a.EmpiricalSamples > 0 {
			a.EmpiricalCoverage80 = a.empiricalHit / float64(a.EmpiricalSamples)
		}
		if a.ProvisionalSamples > 0 {
			a.ProvisionalRangeHitRate = a.provisionalHit / float64(a.ProvisionalSamples)
			a.ProvisionalInputCoverageMean /= float64(a.ProvisionalSamples)
		}
		out = append(out, a.bandCoverage)
	}
	return out
}

func bandInputs(sample forecasting.ErrorSample, signal string) (float64, bool, forecasting.Band, *forecasting.ModelEstimateEvidence) {
	switch signal {
	case "pv":
		return sample.Prediction.PVW + sample.PVErrorW, sample.PVKnown, sample.Prediction.PVBand, sample.Prediction.ModelPV
	case "load":
		return sample.Prediction.LoadW + sample.LoadErrorW, sample.LoadKnown, sample.Prediction.LoadBand, sample.Prediction.ModelLoad
	case "net":
		actual := sample.Prediction.LoadW - sample.Prediction.PVW + sample.LoadErrorW - sample.PVErrorW
		return actual, sample.LoadKnown && sample.PVKnown, sample.Prediction.NetBand, nil
	default:
		return math.NaN(), false, forecasting.Band{}, nil
	}
}
