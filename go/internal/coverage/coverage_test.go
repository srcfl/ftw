package coverage

import "testing"

// The STRÅNG cases below are the live-API probe results recorded on 2026-07-31
// (parameter 117, 2026-06-21). They are the reason the box has the bounds it
// does, so if someone widens it these fail and say why.
func TestStrangCoversProbedNordicPoints(t *testing.T) {
	in := []struct {
		name     string
		lat, lon float64
	}{
		{"Stockholm", 59.33, 18.07},
		{"Tromsø", 69.65, 18.96},
		{"Helsinki", 60.17, 24.94},
		{"Copenhagen", 55.68, 12.57},
	}
	for _, c := range in {
		if !Covers("strang", c.lat, c.lon) {
			t.Errorf("%s (%.2f,%.2f): want covered, got not covered", c.name, c.lat, c.lon)
		}
	}
}

func TestStrangRejectsProbedOutsidePoints(t *testing.T) {
	// Every one of these returned no data from the live API.
	out := []struct {
		name     string
		lat, lon float64
	}{
		{"Berlin", 52.52, 13.40},
		{"London", 51.51, -0.13},
		{"Paris", 48.86, 2.35},
		{"Reykjavík", 64.15, -21.94},
		{"Sydney", -33.87, 151.21},
		{"New York", 40.71, -74.01},
	}
	for _, c := range out {
		if Covers("strang", c.lat, c.lon) {
			t.Errorf("%s (%.2f,%.2f): want not covered, got covered", c.name, c.lat, c.lon)
		}
	}
}

// The declared box is a superset of the rotated grid: these four corners are
// inside the box yet every one returned no data when probed live on 2026-07-31.
// That gap is intentional and documented — Covers()==true means "worth asking",
// not "guaranteed". Pinned so nobody tightens the box into a false promise, or
// starts treating a true result as a guarantee.
func TestStrangBoxIsAdvisorySupersetAtCorners(t *testing.T) {
	corners := [][2]float64{
		{53.5, -0.5}, {53.5, 32.0}, {72.5, -0.5}, {72.5, 32.0},
	}
	for _, c := range corners {
		if !Covers("strang", c[0], c[1]) {
			t.Errorf("(%.1f,%.1f): corner should pass the advisory box test", c[0], c[1])
		}
	}
}

func TestForecastProvidersAreWorldwide(t *testing.T) {
	for _, id := range []string{"met_no", "openweather", "open_meteo", "forecast_solar"} {
		s, ok := ByID(id)
		if !ok {
			t.Fatalf("%s: not registered", id)
		}
		if !s.Worldwide() {
			t.Errorf("%s: want worldwide", id)
		}
		// A worldwide source must cover anywhere, including the far south.
		if !s.Covers(-33.87, 151.21) {
			t.Errorf("%s: worldwide source must cover Sydney", id)
		}
	}
}

// The whole point of #726: price data is Europe-only. If someone adds a global
// price provider this test should be updated deliberately, not incidentally.
func TestPriceProvidersAreEuropeOnly(t *testing.T) {
	prices := ForKind(KindPrice)
	if len(prices) == 0 {
		t.Fatal("no price sources registered")
	}
	for _, s := range prices {
		if s.Worldwide() {
			t.Errorf("%s: price sources are not worldwide", s.ID)
		}
		if s.Covers(-33.87, 151.21) {
			t.Errorf("%s: must not claim to cover Sydney", s.ID)
		}
		if s.Covers(40.71, -74.01) {
			t.Errorf("%s: must not claim to cover New York", s.ID)
		}
	}
}

func TestSwedishPriceProviderIsNarrowerThanEuropean(t *testing.T) {
	// Berlin: served by the European providers, not by the Swedish one.
	if Covers("elprisetjustnu", 52.52, 13.40) {
		t.Error("elprisetjustnu must not claim Berlin")
	}
	if !Covers("sourceful", 52.52, 13.40) {
		t.Error("sourceful should cover Berlin")
	}
	if !Covers("elprisetjustnu", 59.33, 18.07) {
		t.Error("elprisetjustnu should cover Stockholm")
	}
}

// An unknown id must not be treated as universally available.
func TestUnknownSourceIsNotCovered(t *testing.T) {
	if Covers("does_not_exist", 59.33, 18.07) {
		t.Error("unknown source must report not covered")
	}
	if _, ok := ByID("does_not_exist"); ok {
		t.Error("unknown source must not resolve")
	}
}

func TestBBoxContainsIsInclusive(t *testing.T) {
	b := BBox{MinLat: 10, MinLon: 20, MaxLat: 30, MaxLon: 40}
	for _, c := range []struct {
		lat, lon float64
		want     bool
	}{
		{10, 20, true},     // min corner
		{30, 40, true},     // max corner
		{20, 30, true},     // interior
		{9.99, 30, false},  // just south
		{20, 40.01, false}, // just east
	} {
		if got := b.Contains(c.lat, c.lon); got != c.want {
			t.Errorf("Contains(%v,%v) = %v, want %v", c.lat, c.lon, got, c.want)
		}
	}
}

// Longitude is intentionally not wrapped; a nonsense coordinate must stay a
// miss rather than being folded into range.
func TestBBoxDoesNotWrapLongitude(t *testing.T) {
	b := BBox{MinLat: -90, MinLon: -180, MaxLat: 90, MaxLon: 180}
	if b.Contains(0, 200) {
		t.Error("lon 200 must not wrap to -160")
	}
}

func TestRegistryIsInternallyConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range All() {
		if s.ID == "" || s.Label == "" || s.Area == "" {
			t.Errorf("%+v: id, label and area are all required", s)
		}
		if seen[s.ID] {
			t.Errorf("%s: duplicate id", s.ID)
		}
		seen[s.ID] = true
		if s.BBox != nil {
			if s.BBox.MinLat > s.BBox.MaxLat || s.BBox.MinLon > s.BBox.MaxLon {
				t.Errorf("%s: inverted bbox %+v", s.ID, *s.BBox)
			}
		}
	}
}

// All() must hand out a copy: a caller mutating the result must not corrupt the
// registry for everyone else in the process.
func TestAllReturnsACopy(t *testing.T) {
	got := All()
	original := got[0].ID
	got[0].ID = "mutated"
	if All()[0].ID != original {
		t.Fatal("All() exposed the backing array")
	}
}
