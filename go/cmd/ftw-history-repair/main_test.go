package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/srcfl/ftw/go/internal/state"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepairCommandRequiresExplicitFlowsAndReviewedPlan(t *testing.T) {
	for _, args := range [][]string{nil, {"write"}, {"apply", "--workdir", "/not-a-repair"}, {"prepare", "--state", "state.db", "--workdir", "new", "--since", "2026-10-19T00:00:00+02:00", "--until", "2026-10-19T14:00:00+02:00", "--grid", "meter:meter_w"}} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out); err == nil {
			t.Fatal("incomplete repair command succeeded", args)
		}
	}
	var out bytes.Buffer
	err := run(context.Background(), []string{"prepare", "--state", "state.db", "--workdir", "new", "--since", "2026-10-19T00:00:00", "--until", "2026-10-19T14:00:00+02:00"}, &out)
	if err == nil || !strings.Contains(err.Error(), "cannot parse") {
		t.Fatal("timezone-less date accepted", err)
	}
}

func TestRepairCommandPrepareThenApplyChangesHistoricalCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	for i := int64(0); i <= 90; i++ {
		at := base.Add(time.Duration(i) * 10 * time.Second).UnixMilli()
		v := &state.HistorySources{AtMS: at, Topology: "meter", GridW: 1000}
		if i >= 42 && i <= 54 {
			// Invalid household configuration cannot be bridged as a source hold.
			v.Missing = state.MissingLoad
		}
		p := &state.HistoryPoint{TsMs: at, GridW: 1000, LoadW: 1000, JSON: `{}`, Sources: v}
		if err := s.EnqueueTelemetryTick(p, nil, nil); err != nil {
			t.Fatal(err)
		}
		if i%10 == 0 {
			if err := s.FlushHistory(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	var samples []state.Sample
	for i := int64(-6); i <= 186; i++ {
		samples = append(samples, state.Sample{Driver: "meter", Metric: "meter_w", TsMs: base.Add(time.Duration(i) * 5 * time.Second).UnixMilli(), Value: 1000, Unit: "W"})
	}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "repair")
	var out bytes.Buffer
	args := []string{"prepare", "--state", path, "--workdir", work, "--since", base.Format(time.RFC3339), "--until", base.Add(15 * time.Minute).Format(time.RFC3339), "--grid", "meter:meter_w", "--pv", "none", "--battery", "none", "--ev", "none", "--v2x", "none", "--reason", "CLI repair regression"}
	if err := run(ctx, args, &out); err != nil {
		t.Fatal(err)
	}
	var plan state.HistoryRepairPlan
	if err := json.Unmarshal(out.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(ctx, []string{"apply", "--workdir", work, "--sha256", plan.SHA256}, &out); err != nil {
		t.Fatal(err)
	}
	var result state.HistoryRepairResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.BeforeWh != nil || result.AfterWh == nil || !result.Estimated {
		t.Fatal("CLI did not repair history", result)
	}
	if wh := result.AfterWh.(float64); wh < 249.99 || wh > 250.01 {
		t.Fatal("wrong corrected total", wh)
	}
	t.Logf("REPAIR_RESULT_JSON=%s", strings.TrimSpace(out.String()))
}
