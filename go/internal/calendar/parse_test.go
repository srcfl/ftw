package calendar

import (
	"testing"
	"time"
)

func testParser() *parser {
	return newParser(
		[]string{"away", "vacation", "holiday"},
		[]string{"ev", "car", "charge"},
		"garage",
		80,
	)
}

func TestClassifyAway(t *testing.T) {
	p := testParser()
	start := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 5, 18, 0, 0, 0, time.UTC)

	iv, ev := p.classify("Away — visiting family", start, end, "uid-1")
	if ev != nil {
		t.Fatalf("away event misclassified as EV: %+v", ev)
	}
	if iv == nil {
		t.Fatal("expected an away interval")
	}
	if !iv.Start.Equal(start) || !iv.End.Equal(end) {
		t.Fatalf("interval bounds wrong: got [%v,%v)", iv.Start, iv.End)
	}
	if iv.UID != "uid-1" {
		t.Fatalf("uid not carried: %q", iv.UID)
	}
}

func TestClassifyAwayNoEndDefaultsToOneDay(t *testing.T) {
	p := testParser()
	start := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)

	iv, _ := p.classify("Vacation", start, time.Time{}, "")
	if iv == nil {
		t.Fatal("expected an away interval")
	}
	if want := start.Add(24 * time.Hour); !iv.End.Equal(want) {
		t.Fatalf("expected 24h default end %v, got %v", want, iv.End)
	}
}

func TestClassifyEVWithPercent(t *testing.T) {
	p := testParser()
	start := time.Date(2026, 7, 2, 7, 30, 0, 0, time.UTC)

	iv, ev := p.classify("Charge car 65 % before work", start, start.Add(time.Hour), "uid-ev")
	if iv != nil {
		t.Fatalf("EV event misclassified as away: %+v", iv)
	}
	if ev == nil {
		t.Fatal("expected an EV deadline")
	}
	if ev.TargetSoCPct != 65 {
		t.Fatalf("target soc: want 65, got %v", ev.TargetSoCPct)
	}
	if !ev.Departure.Equal(start) {
		t.Fatalf("departure should be the event start: got %v", ev.Departure)
	}
	if ev.LoadpointID != "garage" {
		t.Fatalf("loadpoint should fall back to default: got %q", ev.LoadpointID)
	}
}

func TestClassifyEVDefaultTargetWhenNoPercent(t *testing.T) {
	p := testParser()
	start := time.Date(2026, 7, 2, 7, 30, 0, 0, time.UTC)

	_, ev := p.classify("EV ready", start, start.Add(time.Hour), "")
	if ev == nil {
		t.Fatal("expected an EV deadline")
	}
	if ev.TargetSoCPct != 80 {
		t.Fatalf("want default 80, got %v", ev.TargetSoCPct)
	}
}

func TestClassifyEVExplicitLoadpoint(t *testing.T) {
	p := testParser()
	start := time.Date(2026, 7, 2, 7, 30, 0, 0, time.UTC)

	_, ev := p.classify("Charge to 90% lp:carport", start, start.Add(time.Hour), "")
	if ev == nil {
		t.Fatal("expected an EV deadline")
	}
	if ev.LoadpointID != "carport" {
		t.Fatalf("explicit loadpoint not honoured: got %q", ev.LoadpointID)
	}
	if ev.TargetSoCPct != 90 {
		t.Fatalf("want 90, got %v", ev.TargetSoCPct)
	}
}

func TestClassifyPercentClamped(t *testing.T) {
	p := testParser()
	start := time.Now()
	_, ev := p.classify("charge 150%", start, start.Add(time.Hour), "")
	if ev == nil || ev.TargetSoCPct != 100 {
		t.Fatalf("percent should clamp to 100: %+v", ev)
	}
}

func TestClassifyCaseInsensitive(t *testing.T) {
	p := testParser()
	start := time.Now()
	iv, _ := p.classify("AWAY", start, start.Add(time.Hour), "")
	if iv == nil {
		t.Fatal("uppercase keyword should still match")
	}
}

func TestClassifyNonMatchingIgnored(t *testing.T) {
	p := testParser()
	start := time.Now()
	iv, ev := p.classify("Dentist appointment", start, start.Add(time.Hour), "")
	if iv != nil || ev != nil {
		t.Fatalf("unrelated event should be ignored: iv=%+v ev=%+v", iv, ev)
	}
}

func TestClassifyEmptyTitleIgnored(t *testing.T) {
	p := testParser()
	start := time.Now()
	if iv, ev := p.classify("   ", start, start.Add(time.Hour), ""); iv != nil || ev != nil {
		t.Fatal("blank title should be ignored")
	}
}

func TestClassifyZeroStartIgnored(t *testing.T) {
	p := testParser()
	if iv, ev := p.classify("Away", time.Time{}, time.Time{}, ""); iv != nil || ev != nil {
		t.Fatal("event without a start time should be ignored")
	}
}
