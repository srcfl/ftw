package ftwdbshadow

import (
	"context"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestHistoryMappingPreservesSIAndLateVersions(t *testing.T) {
	source := mustID(t, "00112233445566778899aabbccddeeff")
	ticks := []state.CommittedHistory{
		{Sequence: 1, CommittedAtMicros: 4000, Point: state.HistoryPoint{TsMs: 3, GridW: 42, PVW: -1000, BatW: 200, LoadW: 842, BatSoC: 0.75}},
		{Sequence: 3, CommittedAtMicros: 5000, Point: state.HistoryPoint{TsMs: 1, GridW: -42, PVW: -1000, BatW: 200, LoadW: 758, BatSoC: 0.75}},
	}
	prepared, err := prepareHistory(source, "site", ticks)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(prepared.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	batch := decoded.(CommitBatchRequest)
	if batch.Sequence != 3 || len(batch.Points) != 10 {
		t.Fatal("timestamp ordering dropped late history")
	}
	units := map[uint64]string{}
	for _, s := range batch.Series {
		units[s.ID] = s.CanonicalUnit
	}
	for i, p := range batch.Points {
		want := []float64{42, -1000, 200, 842, 0.75, -42, -1000, 200, 758, 0.75}[i]
		if p.Value != want || p.KnowledgeTime != ticks[i/5].CommittedAtMicros || p.ValidTime != ticks[i/5].Point.TsMs*1000 {
			t.Fatalf("changed numeric meaning or time: %+v", p)
		}
		unit := "W"
		if i%5 == 4 {
			unit = "1"
		}
		if units[p.SeriesID] != unit {
			t.Fatalf("wrong SI unit: %s", units[p.SeriesID])
		}
	}
}

func TestBetaDisabledDoesNotNeedIdentityOrStore(t *testing.T) {
	b := Start(context.Background(), nil, "", "", "test")
	defer b.Close()
	if b.Status().Enabled || b.Status().State != "disabled" {
		t.Fatal("candidate enabled itself")
	}
}

func TestHealthOpsPreservesLegacyAndRejectsUnknownPolicy(t *testing.T) {
	source := mustID(t, "00112233445566778899aabbccddeeff")
	for _, ops := range []*HealthOps{nil, {SyncPolicy: 1}, {SyncPolicy: 2}, {SyncPolicy: 3, SyncEveryBytes: 4096}} {
		health := HealthResponse{SourceID: source, Status: HealthHealthy, Ops: ops}
		frame, err := Encode(health)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(frame)
		if err != nil {
			t.Fatal(err)
		}
		got := decoded.(HealthResponse).Ops
		if (got == nil) != (ops == nil) || (got != nil && *got != *ops) {
			t.Fatal("health policy changed")
		}
	}
	for _, ops := range []*HealthOps{{SyncPolicy: 0}, {SyncPolicy: 4}, {SyncPolicy: 1, SyncEveryBytes: 1}, {SyncPolicy: 3}} {
		if _, err := Encode(HealthResponse{SourceID: source, Status: HealthHealthy, Ops: ops}); err == nil {
			t.Fatal("bad sync policy accepted")
		}
	}
}
