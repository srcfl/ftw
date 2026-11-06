package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestEndpointCommandRejectsIncompleteOrInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		nil, {"write"}, {"prepare"}, {"apply"}, {"prepare", "--unknown"},
		{"prepare", "--state", "state.db", "--plan", "plan.json", "extra"},
		{"prepare", "--state", "state.db", "--plan", "plan.json", "--since", "2026-10-19T00:00:00", "--until", "2026-10-19T01:00:00Z"},
		{"prepare", "--state", "state.db", "--plan", "plan.json", "--since", "2026-10-19T00:00:00Z", "--until", "bad"},
		{"prepare", "--state", "state.db", "--plan", "plan.json", "--since", "2026-10-19T00:00:00Z", "--until", "2026-10-19T01:00:00Z", "--max-gap", "31m", "--reason", "test"},
		{"prepare", "--state", "state.db", "--plan", "plan.json", "--since", "2026-10-19T00:00:00Z", "--until", "2026-10-19T01:00:00Z"},
		{"apply", "--state", "state.db", "--plan", "plan.json", "--backup", "backup.db", "extra"},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out); err == nil {
			t.Fatal("invalid command accepted", args)
		}
		if out.Len() != 0 {
			t.Fatal("failure emitted success JSON", out.String())
		}
	}
}

func TestEndpointCommandPrepareApplyAndRetry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "state.db")
	s, err := state.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	for i := 0; i <= 180; i++ {
		at := base.Add(time.Duration(i) * 10 * time.Second).UnixMilli()
		v := &state.HistorySources{AtMS: at, Topology: "meter", GridW: 1000}
		if i >= 60 && i < 120 {
			v.Missing = state.MissingLoad
		}
		p := &state.HistoryPoint{TsMs: at, GridW: 1000, LoadW: 1000, JSON: `{}`, Sources: v}
		if err = s.EnqueueTelemetryTick(p, nil, nil); err != nil {
			t.Fatal(err)
		}
		if i%10 == 0 {
			if err = s.FlushHistory(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, state.HistoryFilename))
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backup.db")
	if _, err = db.Exec(`VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	db.Close()
	planFile := filepath.Join(dir, "plan.json")
	args := []string{"prepare", "--state", source, "--plan", planFile, "--since", base.Format(time.RFC3339), "--until", base.Add(30 * time.Minute).Format(time.RFC3339), "--max-gap", "15m", "--reason", "CLI endpoint regression"}
	var out bytes.Buffer
	if err = run(ctx, args, &out); err != nil {
		t.Fatal(err)
	}
	var prepared struct {
		SHA     string `json:"plan_sha256"`
		Minutes int    `json:"correction_minutes"`
	}
	if err = json.Unmarshal(out.Bytes(), &prepared); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatal(err)
	}
	var plan state.EndpointRepairPlan
	if err = json.Unmarshal(b, &plan); err != nil {
		t.Fatal(err)
	}
	if len(prepared.SHA) != 64 || prepared.SHA != plan.SHA256 || prepared.Minutes != len(plan.Minutes) || prepared.Minutes == 0 {
		t.Fatal("invalid plan/output", out.String())
	}
	info, err := os.Stat(planFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0077 != 0 {
		t.Fatal("plan accessible to other users")
	}
	out.Reset()
	if err = run(ctx, args, &out); err == nil {
		t.Fatal("existing reviewed plan overwritten")
	}
	after, err := os.ReadFile(planFile)
	if err != nil || !bytes.Equal(after, b) {
		t.Fatal("existing plan changed", err)
	}
	apply := []string{"apply", "--state", source, "--plan", planFile, "--backup", backup, "--sha256", strings.Repeat("0", 64)}
	out.Reset()
	if err = run(ctx, apply, &out); err == nil {
		t.Fatal("unreviewed plan accepted")
	}
	apply[len(apply)-1] = plan.SHA256
	for i := 0; i < 2; i++ {
		out.Reset()
		if err = run(ctx, apply, &out); err != nil {
			t.Fatal(err)
		}
		var result struct {
			SHA     string `json:"plan_sha256"`
			Minutes int    `json:"applied_minutes"`
			Method  string `json:"method"`
		}
		if err = json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.SHA != plan.SHA256 || result.Minutes != len(plan.Minutes) || result.Method != "endpoint_mean_v1" {
			t.Fatal("invalid apply output", out.String())
		}
	}
	if err = os.WriteFile(planFile, []byte(`{broken`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = run(ctx, apply, &out); err == nil || out.Len() != 0 {
		t.Fatal("malformed plan accepted", err, out.String())
	}
}
