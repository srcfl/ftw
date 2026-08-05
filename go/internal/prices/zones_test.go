package prices

import (
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// Every entry has to be complete: a zone missing its EIC breaks the direct
// ENTSO-E provider, and one missing its currency would silently price a
// household in öre.
func TestZoneCatalogIsComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, z := range Zones() {
		switch {
		case z.Code == "":
			t.Errorf("%+v: empty code", z)
		case z.Country == "":
			t.Errorf("%s: no country", z.Code)
		case z.Currency == "":
			t.Errorf("%s: no currency", z.Code)
		case len(z.EIC) != 16:
			t.Errorf("%s: EIC %q is %d chars, want 16", z.Code, z.EIC, len(z.EIC))
		}
		if seen[z.Code] {
			t.Errorf("%s: duplicate zone code", z.Code)
		}
		seen[z.Code] = true
	}
	// Sanity: the zones people actually ask about are all there.
	for _, code := range []string{"SE3", "BE", "NL", "DE", "FR", "ES", "PL", "IT-NORTH", "NO5"} {
		if !seen[code] {
			t.Errorf("%s missing from the catalog", code)
		}
	}
}

func TestLookupZoneIgnoresCaseAndSpace(t *testing.T) {
	for _, in := range []string{"be", " BE ", "Be"} {
		z, ok := LookupZone(in)
		if !ok || z.Code != "BE" {
			t.Errorf("LookupZone(%q) = %+v, %v; want BE", in, z, ok)
		}
	}
	if _, ok := LookupZone("ZZ9"); ok {
		t.Error("unknown zone should not resolve")
	}
}

func TestZoneNameDistinguishesMultiZoneCountries(t *testing.T) {
	be, _ := LookupZone("BE")
	if be.Name() != "Belgium" {
		t.Errorf("single-zone country: %q", be.Name())
	}
	se3, _ := LookupZone("SE3")
	if se3.Name() != "Sweden — Stockholm" {
		t.Errorf("multi-zone country: %q", se3.Name())
	}
}

// The whole point of the zone picker: choosing Belgium must not leave the
// household paying in Swedish öre.
func TestFromConfigTakesCurrencyFromZone(t *testing.T) {
	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()

	for _, tc := range []struct {
		zone, configured, want string
	}{
		{"BE", "", "EUR"},
		{"NO1", "", "NOK"},
		{"SE3", "", "SEK"},
		{"", "", "SEK"},      // no zone → old default
		{"BE", "SEK", "SEK"}, // an explicit choice wins
		{"XX9", "", "SEK"},   // unknown zone → old default
	} {
		s := FromConfig(&config.Price{Provider: "sourceful", Zone: tc.zone, Currency: tc.configured}, st, nil)
		if s == nil {
			t.Fatalf("zone %q: expected a service", tc.zone)
		}
		if s.Currency != tc.want {
			t.Errorf("zone %q currency %q: got %s, want %s", tc.zone, tc.configured, s.Currency, tc.want)
		}
		if sp, ok := s.Provider.(*SourcefulProvider); ok && sp.Currency != tc.want {
			t.Errorf("zone %q: provider currency %s, want %s", tc.zone, sp.Currency, tc.want)
		}
	}
}

// Cached rows are minor units with no currency attached, so switching
// currency has to empty them — otherwise cost history adds öre to cent.
func TestCurrencyChangeClearsCachedPrices(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	rows := []state.PricePoint{{
		Zone: "SE3", SlotTsMs: 1_700_000_000_000, SlotLenMin: 60,
		SpotOreKwh: 80, TotalOreKwh: 190, Source: "sourceful", FetchedAtMs: 1,
	}}
	if err := st.SavePrices(rows); err != nil {
		t.Fatal(err)
	}
	stored := func() int {
		got, err := st.LoadPrices("SE3", 0, 1_800_000_000_000)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}

	// First boot records the currency and keeps what's there.
	(&Service{Store: st, Zone: "SE3", Currency: "SEK"}).syncCachedCurrency()
	if stored() != 1 {
		t.Fatal("first boot should not clear the cache")
	}
	// Same currency again: still untouched.
	(&Service{Store: st, Zone: "SE3", Currency: "SEK"}).syncCachedCurrency()
	if stored() != 1 {
		t.Fatal("unchanged currency should not clear the cache")
	}
	// Switching to EUR drops the öre rows.
	(&Service{Store: st, Zone: "SE3", Currency: "EUR"}).syncCachedCurrency()
	if n := stored(); n != 0 {
		t.Fatalf("currency change left %d rows, want 0", n)
	}
	// And the new currency is what's remembered.
	if got, _ := st.LoadConfig(priceCurrencyKey); got != "EUR" {
		t.Errorf("recorded currency %q, want EUR", got)
	}
}
