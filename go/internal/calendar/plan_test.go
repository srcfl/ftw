package calendar

import (
	"strings"
	"testing"
	"time"
)

func planSlot(now time.Time, offMin, lenMin int, batW, soc float64) PlanSlot {
	s := now.Add(time.Duration(offMin) * time.Minute)
	return PlanSlot{Start: s, End: s.Add(time.Duration(lenMin) * time.Minute), BatteryW: batW, SoCPct: soc}
}

func TestBuildPlanBlocksCoalesces(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	slots := []PlanSlot{
		planSlot(now, 0, 15, 3000, 55),   // charge
		planSlot(now, 15, 15, 3200, 60),  // charge (same run)
		planSlot(now, 30, 15, 50, 60),    // hold → breaks the run, not published
		planSlot(now, 45, 15, -2000, 50), // discharge
		planSlot(now, 60, 15, -2100, 45), // discharge (same run)
	}
	blocks := buildPlanBlocks(slots, now)
	if len(blocks) != 2 {
		t.Fatalf("want 2 blocks (charge, discharge), got %d: %+v", len(blocks), blocks)
	}
	if !strings.Contains(blocks[0].summary, "Charge battery") {
		t.Fatalf("block 0 summary = %q", blocks[0].summary)
	}
	if !blocks[0].start.Equal(now) || !blocks[0].end.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("charge block bounds wrong: [%v,%v)", blocks[0].start, blocks[0].end)
	}
	if !strings.Contains(blocks[1].summary, "Discharge battery") {
		t.Fatalf("block 1 summary = %q", blocks[1].summary)
	}
	if !blocks[1].start.Equal(now.Add(45*time.Minute)) || !blocks[1].end.Equal(now.Add(75*time.Minute)) {
		t.Fatalf("discharge block bounds wrong: [%v,%v)", blocks[1].start, blocks[1].end)
	}
}

func TestBuildPlanBlocksSkipsPast(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	slots := []PlanSlot{
		planSlot(now, -120, 60, 3000, 50), // ended an hour ago → skipped
		planSlot(now, -60, 60, 3000, 60),  // ended exactly at now → skipped (End not after now)
	}
	if blocks := buildPlanBlocks(slots, now); len(blocks) != 0 {
		t.Fatalf("past blocks should be skipped, got %d: %+v", len(blocks), blocks)
	}
}

func TestBuildPlanBlocksSpanningNowIncluded(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	slots := []PlanSlot{planSlot(now, -30, 120, 4000, 70)} // started 30m ago, ends in 90m
	blocks := buildPlanBlocks(slots, now)
	if len(blocks) != 1 {
		t.Fatalf("block spanning now should be included, got %d", len(blocks))
	}
	if !strings.Contains(blocks[0].summary, "4.0 kW") {
		t.Fatalf("expected avg power in summary, got %q", blocks[0].summary)
	}
}

func TestPlanBlockHashStableAndUID(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	slots := []PlanSlot{planSlot(now, 60, 60, 5000, 80)}
	a := buildPlanBlocks(slots, now)
	b := buildPlanBlocks(slots, now)
	if a[0].uid != b[0].uid {
		t.Fatalf("uid not deterministic: %q vs %q", a[0].uid, b[0].uid)
	}
	if a[0].hash() != b[0].hash() {
		t.Fatalf("hash not deterministic")
	}
	if !strings.HasPrefix(a[0].uid, "ftw-plan-chg-") {
		t.Fatalf("unexpected uid: %q", a[0].uid)
	}
}
