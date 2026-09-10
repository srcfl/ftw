package state

import (
	"context"
	"testing"
	"time"
)

func TestHourlySeriesRollupMatchesRawYearBuckets(t *testing.T) {
	s := freshStore(t)
	const hours = 48
	samples := make([]Sample, 0, hours*2)
	for i := 0; i < hours; i++ {
		base := int64(i) * seriesHourMs
		samples = append(samples,
			Sample{Driver: "meter", Metric: "pv_w", TsMs: base, Value: 100},
			Sample{Driver: "meter", Metric: "pv_w", TsMs: base + seriesHourMs/2, Value: 9000},
		)
	}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.ensureSeriesHours(ctx); err != nil {
		t.Fatal(err)
	}
	until := int64(hours)*seriesHourMs - 1
	got, err := s.LoadSeriesBuckets("meter", "pv_w", 0, until, hours)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != hours {
		t.Fatalf("hour buckets = %d, want %d: %+v", len(got), hours, got)
	}
	for i, p := range got {
		if p.Min != 100 || p.Max != 9000 || p.N != 2 {
			t.Fatalf("hour %d envelope = min:%v max:%v n:%d", i, p.Min, p.Max, p.N)
		}
		if p.V != 4550 {
			t.Fatalf("hour %d avg = %v, want 4550", i, p.V)
		}
		wantTs := int64(i)*seriesHourMs + seriesHourMs/2
		if p.TsMs != wantTs {
			t.Fatalf("hour %d ts = %d, want %d", i, p.TsMs, wantTs)
		}
	}
}

func TestHourlySeriesRollupKeepsPartialWindowEdges(t *testing.T) {
	s := freshStore(t)
	samples := []Sample{
		{Driver: "meter", Metric: "grid_w", TsMs: seriesHourMs - 1, Value: 10},
		{Driver: "meter", Metric: "grid_w", TsMs: seriesHourMs, Value: 20},
		{Driver: "meter", Metric: "grid_w", TsMs: 2*seriesHourMs - 1, Value: 30},
		{Driver: "meter", Metric: "grid_w", TsMs: 2 * seriesHourMs, Value: 40},
	}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.ensureSeriesHours(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSeriesBuckets("meter", "grid_w", seriesHourMs-1, 2*seriesHourMs, 1)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	var min, max float64
	if len(got) == 0 {
		t.Fatal("empty series")
	}
	min, max = got[0].Min, got[0].Max
	for _, p := range got {
		n += p.N
		if p.Min < min {
			min = p.Min
		}
		if p.Max > max {
			max = p.Max
		}
	}
	if n != 4 || min != 10 || max != 40 {
		t.Fatalf("edge window n=%d min=%v max=%v points=%+v", n, min, max, got)
	}
}

func TestHourlySeriesRollupUpdatesAfterLiveWrite(t *testing.T) {
	s := freshStore(t)
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "pv_w", TsMs: 0, Value: 1}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.ensureSeriesHours(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "pv_w", TsMs: 1, Value: 3}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSeriesBuckets("meter", "pv_w", 0, seriesHourMs+1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].N != 2 || got[0].V != 2 || got[0].Min != 1 || got[0].Max != 3 {
		t.Fatalf("live hour = %+v", got)
	}
}
