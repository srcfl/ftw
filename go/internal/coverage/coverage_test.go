package coverage

import (
	"sort"
	"testing"

	"github.com/srcfl/ftw/go/internal/prices"
)

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
	priceSources := ForKind(KindPrice)
	if len(priceSources) == 0 {
		t.Fatal("no price sources registered")
	}
	for _, s := range priceSources {
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

// The country list the coverage answer carries must be the zone table the
// pickers and fetchers actually use — one registry, not two. A bidding zone
// added to prices/zones.go fails here until the coverage list follows, and a
// country invented here (Ireland once was) fails because no zone backs it.
func TestEuropeanPriceCountriesMatchZoneTable(t *testing.T) {
	want := map[string]bool{}
	for _, z := range prices.Zones() {
		// A zone code starts with its ISO 3166-1 alpha-2 country: "SE3" is
		// Sweden, "IT-SARDINIA" is Italy, "NO2NSL" is Norway.
		code := ""
		for _, r := range z.Code {
			if r < 'A' || r > 'Z' {
				break
			}
			code += string(r)
		}
		if len(code) != 2 {
			t.Fatalf("zone %q: expected a 2-letter country prefix, got %q", z.Code, code)
		}
		want[code] = true
	}

	got := map[string]bool{}
	for _, c := range europeanPriceCountries {
		if got[c] {
			t.Errorf("%s: duplicated in europeanPriceCountries", c)
		}
		got[c] = true
	}

	var missing, invented []string
	for c := range want {
		if !got[c] {
			missing = append(missing, c)
		}
	}
	for c := range got {
		if !want[c] {
			invented = append(invented, c)
		}
	}
	sort.Strings(missing)
	sort.Strings(invented)
	if len(missing) > 0 {
		t.Errorf("countries in prices/zones.go but not declared here: %v", missing)
	}
	if len(invented) > 0 {
		t.Errorf("countries declared here that no bidding zone backs: %v", invented)
	}
	if !sort.StringsAreSorted(europeanPriceCountries) {
		t.Error("europeanPriceCountries should stay sorted so diffs are readable")
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
