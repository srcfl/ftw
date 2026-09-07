package prices

import (
	"context"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestStaticFlatRateFillsADay(t *testing.T) {
	loc := time.FixedZone("test", 2*3600)
	p := &StaticProvider{OreKwh: 12.5, Loc: loc}
	day := time.Date(2026, 9, 3, 15, 0, 0, 0, loc)
	rows, err := p.Fetch(context.Background(), "ignored", day)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 96 {
		t.Fatalf("slots = %d, want 96", len(rows))
	}
	if rows[0].SlotStart.Hour() != 0 || rows[0].SlotStart.Minute() != 0 {
		t.Errorf("first slot = %s, want local midnight", rows[0].SlotStart)
	}
	if rows[0].SlotLenMin != 15 {
		t.Errorf("slot = %d", rows[0].SlotLenMin)
	}
	// 12.5 minor units → 0.125 major, the unit Apply multiplies by 100.
	if rows[0].SEKPerKWh != 0.125 {
		t.Errorf("major = %g, want 0.125", rows[0].SEKPerKWh)
	}
	last := rows[len(rows)-1]
	if last.SlotStart.Hour() != 23 || last.SlotStart.Minute() != 45 {
		t.Errorf("last slot = %s", last.SlotStart)
	}
	if last.SEKPerKWh != rows[0].SEKPerKWh {
		t.Errorf("flat rate drifted: first %g last %g", rows[0].SEKPerKWh, last.SEKPerKWh)
	}
}

func TestStaticTOUOverridesFlat(t *testing.T) {
	loc := time.FixedZone("test", 0)
	p := &StaticProvider{
		OreKwh: 8,
		Loc:    loc,
		TOU: []config.TOUWindow{
			{Start: "07:00", End: "23:00", OreKwh: 22},
		},
	}
	day := time.Date(2026, 9, 3, 0, 0, 0, 0, loc) // Thursday
	rows, err := p.Fetch(context.Background(), "", day)
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) float64 {
		t.Helper()
		want := time.Date(2026, 9, 3, h, m, 0, 0, loc)
		for _, r := range rows {
			if r.SlotStart.Equal(want) {
				return r.SEKPerKWh * 100
			}
		}
		t.Fatalf("missing slot %02d:%02d", h, m)
		return 0
	}
	if got := at(6, 45); got != 8 {
		t.Errorf("06:45 = %g, want off-peak 8", got)
	}
	if got := at(7, 0); got != 22 {
		t.Errorf("07:00 = %g, want peak 22", got)
	}
	if got := at(22, 45); got != 22 {
		t.Errorf("22:45 = %g, want peak 22", got)
	}
	if got := at(23, 0); got != 8 {
		t.Errorf("23:00 = %g, want off-peak 8", got)
	}
}

func TestStaticOvernightWindow(t *testing.T) {
	loc := time.UTC
	p := &StaticProvider{
		OreKwh: 20,
		Loc:    loc,
		TOU: []config.TOUWindow{
			{Start: "22:00", End: "06:00", OreKwh: 5},
		},
	}
	day := time.Date(2026, 9, 3, 12, 0, 0, 0, loc)
	rows, err := p.Fetch(context.Background(), "", day)
	if err != nil {
		t.Fatal(err)
	}
	ore := func(h int) float64 {
		t.Helper()
		want := time.Date(2026, 9, 3, h, 0, 0, 0, loc)
		for _, r := range rows {
			if r.SlotStart.Equal(want) {
				return r.SEKPerKWh * 100
			}
		}
		t.Fatalf("missing hour %d", h)
		return 0
	}
	if ore(22) != 5 || ore(5) != 5 {
		t.Errorf("overnight: 22h=%g 5h=%g, want 5", ore(22), ore(5))
	}
	if ore(6) != 20 || ore(12) != 20 {
		t.Errorf("daytime: 6h=%g 12h=%g, want 20", ore(6), ore(12))
	}
}

func TestStaticWeekdayWindows(t *testing.T) {
	loc := time.UTC
	p := &StaticProvider{
		OreKwh: 10,
		Loc:    loc,
		TOU: []config.TOUWindow{
			{Start: "00:00", End: "24:00", OreKwh: 30, Days: []string{"sat", "sunday"}},
		},
	}
	thu := time.Date(2026, 9, 3, 0, 0, 0, 0, loc) // Thursday
	sat := time.Date(2026, 9, 5, 0, 0, 0, 0, loc)
	thuRows, err := p.Fetch(context.Background(), "", thu)
	if err != nil {
		t.Fatal(err)
	}
	satRows, err := p.Fetch(context.Background(), "", sat)
	if err != nil {
		t.Fatal(err)
	}
	if thuRows[48].SEKPerKWh*100 != 10 {
		t.Errorf("Thursday noon = %g, want weekday 10", thuRows[48].SEKPerKWh*100)
	}
	if satRows[48].SEKPerKWh*100 != 30 {
		t.Errorf("Saturday noon = %g, want weekend 30", satRows[48].SEKPerKWh*100)
	}
}

func TestStaticZeroIsARealPrice(t *testing.T) {
	p := &StaticProvider{OreKwh: 0, Loc: time.UTC}
	rows, err := p.Fetch(context.Background(), "", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 96 {
		t.Fatalf("slots = %d", len(rows))
	}
	if rows[0].SEKPerKWh != 0 {
		t.Errorf("got %g", rows[0].SEKPerKWh)
	}
}

func TestFromConfigStatic(t *testing.T) {
	s := FromConfig(&config.Price{
		Provider:     "static",
		Currency:     "USD",
		StaticOreKwh: 14,
	}, nil, nil)
	if s == nil {
		t.Fatal("expected service")
	}
	if s.Provider.Name() != "static" {
		t.Errorf("name = %s", s.Provider.Name())
	}
	if s.Zone != "STATIC" {
		t.Errorf("zone = %s, want STATIC when none is set", s.Zone)
	}
	if s.Currency != "USD" {
		t.Errorf("currency = %s", s.Currency)
	}
}

func TestParseClockMinutes(t *testing.T) {
	got, err := parseClockMinutes("7:00")
	if err != nil || got != 7*60 {
		t.Errorf("7:00 → %d %v", got, err)
	}
	got, err = parseClockMinutes("24:00")
	if err != nil || got != 24*60 {
		t.Errorf("24:00 → %d %v", got, err)
	}
	if _, err := parseClockMinutes("25:00"); err == nil {
		t.Error("25:00 should fail")
	}
}
