// Package coverage records where each external data source FTW talks to
// actually returns usable data.
//
// FTW runs outside the Nordics, but several of its sources are regional:
// STRÅNG models only the Nordic domain, and every price provider is European.
// Nothing in the code said so, so a site in Australia would get an empty price
// curve and an unscored PV history with no explanation. This package is that
// missing explanation, in one place, so the API and the UI can tell an operator
// *before* they select a source that it cannot serve their location.
//
// Bounds here are ADVISORY, and deliberately generous. Coverage is declared as
// a lat/lon box, but STRÅNG's grid is rotated relative to lat/lon, so the box is
// a superset of the real domain: a point near a corner can pass Covers and still
// return no data. Read Covers()==false as "definitely not supported, do not
// bother asking" and Covers()==true as "worth trying" — the upstream API stays
// authoritative. Nothing here is a safety input; it only decides what we show
// and whether we skip a pointless fetch.
package coverage

// Kind groups sources by what they supply, so the UI can present forecast,
// irradiance and price coverage separately.
type Kind string

const (
	KindForecast   Kind = "forecast"
	KindIrradiance Kind = "irradiance"
	KindPrice      Kind = "price"
	KindGeodata    Kind = "geodata"
)

// BBox is an inclusive latitude/longitude bounding box in WGS84 degrees.
type BBox struct {
	MinLat float64 `json:"min_lat"`
	MinLon float64 `json:"min_lon"`
	MaxLat float64 `json:"max_lat"`
	MaxLon float64 `json:"max_lon"`
}

// Contains reports whether (lat, lon) falls inside the box. Longitude is not
// wrapped: no source described here spans the antimeridian, and silently
// wrapping would turn a nonsense coordinate into a plausible-looking hit.
func (b BBox) Contains(lat, lon float64) bool {
	return lat >= b.MinLat && lat <= b.MaxLat && lon >= b.MinLon && lon <= b.MaxLon
}

// Source describes one external data source and where it works.
type Source struct {
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	// Area is the human-readable coverage, shown in the UI.
	Area string `json:"area"`
	// Countries lists ISO 3166-1 alpha-2 codes when the source is bounded to a
	// known set. Empty means either worldwide or "bounded by BBox, not by
	// borders" — check Worldwide() rather than inferring from length.
	Countries []string `json:"countries,omitempty"`
	// BBox bounds the source geographically. nil means worldwide.
	BBox *BBox `json:"bbox,omitempty"`
	// RequiresKey is true when the operator must supply their own credential.
	RequiresKey bool   `json:"requires_key"`
	License     string `json:"license,omitempty"`
	Note        string `json:"note,omitempty"`
}

// Worldwide reports whether the source is unbounded geographically.
func (s Source) Worldwide() bool { return s.BBox == nil }

// Covers reports whether the source plausibly serves (lat, lon). Worldwide
// sources always do. See the package doc: a true result is advisory.
func (s Source) Covers(lat, lon float64) bool {
	if s.BBox == nil {
		return true
	}
	return s.BBox.Contains(lat, lon)
}

// strangDomain is the STRÅNG model domain, measured against the live API rather
// than taken from documentation (SMHI's apidocs pages 404 as of 2026-07).
// Probing parameter 117 found data at lat 53.5 and 72.5 but not 53.0 or 73.5,
// and at lon 0 and 30 but not -2 or 35. The corner (53.5, -4) returns nothing
// even though it is inside this box, which is the rotated grid showing through —
// hence "advisory superset" in the package doc.
var strangDomain = &BBox{MinLat: 53.0, MinLon: -1.0, MaxLat: 73.0, MaxLon: 33.0}

// sources is the registry. Keep it ordered by kind then id so the API response
// is stable and diffs stay readable.
var sources = []Source{
	{
		ID: "met_no", Kind: KindForecast, Label: "MET Norway",
		Area:    "Worldwide",
		License: "NLOD / CC BY 4.0",
		Note:    "Cloud cover only — no irradiance, so PV is derived from a cloud-derated clear-sky prior.",
	},
	{
		ID: "openweather", Kind: KindForecast, Label: "OpenWeather",
		Area:        "Worldwide",
		RequiresKey: true,
		Note:        "Cloud cover only — same cloud-derated prior as MET Norway.",
	},
	{
		ID: "open_meteo", Kind: KindForecast, Label: "Open-Meteo",
		Area:    "Worldwide",
		License: "CC BY 4.0",
		Note:    "Publishes shortwave radiation, so PV is irradiance-derived rather than cloud-derated.",
	},
	{
		ID: "forecast_solar", Kind: KindForecast, Label: "Forecast.Solar",
		Area: "Worldwide",
		Note: "Returns site-calibrated watts from the configured array geometry; free tier is rate-limited.",
	},
	{
		ID: "strang", Kind: KindIrradiance, Label: "SMHI STRÅNG",
		Area:      "Nordic region",
		Countries: []string{"SE", "NO", "FI", "DK", "EE", "LV", "LT"},
		BBox:      strangDomain,
		License:   "CC BY 4.0",
		Note:      "Historical only (1999 to ~1 day ago). Used for PV performance scoring and forecast calibration, never as a forward forecast.",
	},
	{
		ID: "lantmateriet", Kind: KindGeodata, Label: "Lantmäteriet (buildings + LiDAR)",
		Area:      "Sweden",
		Countries: []string{"SE"},
		// Sweden's national extent. Generous at the edges for the same reason
		// as STRÅNG: a box cannot trace a coastline, and the upstream STAC
		// search returning no tiles is the authoritative answer.
		BBox:        &BBox{MinLat: 55.0, MinLon: 10.5, MaxLat: 69.5, MaxLon: 24.5},
		RequiresKey: true,
		License:     "CC BY 4.0",
		Note:        "Free open data, but gated behind a Geotorget account the operator orders themselves. Used to derive roof tilt/azimuth; everywhere else that stays a manual entry.",
	},
	{
		ID: "sourceful", Kind: KindPrice, Label: "Sourceful (cached ENTSO-E)",
		Area:      "Europe",
		Countries: europeanPriceCountries,
		BBox:      &BBox{MinLat: 34.0, MinLon: -25.0, MaxLat: 72.0, MaxLon: 45.0},
		Note:      "European day-ahead bidding zones. No key required.",
	},
	{
		ID: "elprisetjustnu", Kind: KindPrice, Label: "Elpriset just nu",
		Area:      "Sweden",
		Countries: []string{"SE"},
		BBox:      &BBox{MinLat: 55.0, MinLon: 10.0, MaxLat: 69.5, MaxLon: 24.5},
		Note:      "Swedish bidding zones SE1-SE4 only. No key required.",
	},
	{
		ID: "entsoe", Kind: KindPrice, Label: "ENTSO-E Transparency",
		Area:        "Europe",
		Countries:   europeanPriceCountries,
		BBox:        &BBox{MinLat: 34.0, MinLon: -25.0, MaxLat: 72.0, MaxLon: 45.0},
		RequiresKey: true,
		Note:        "All ENTSO-E member bidding zones.",
	},
}

// europeanPriceCountries are the ENTSO-E member states whose day-ahead prices
// the European providers can serve. Shared by sourceful and entsoe because both
// resolve to the same underlying bidding zones.
var europeanPriceCountries = []string{
	"AT", "BE", "BG", "CH", "CZ", "DE", "DK", "EE", "ES", "FI",
	"FR", "GR", "HR", "HU", "IE", "IT", "LT", "LU", "LV", "NL",
	"NO", "PL", "PT", "RO", "RS", "SE", "SI", "SK",
}

// All returns every known source.
func All() []Source {
	out := make([]Source, len(sources))
	copy(out, sources)
	return out
}

// ByID returns the source with the given id.
func ByID(id string) (Source, bool) {
	for _, s := range sources {
		if s.ID == id {
			return s, true
		}
	}
	return Source{}, false
}

// ForKind returns every source of one kind, in registry order.
func ForKind(k Kind) []Source {
	var out []Source
	for _, s := range sources {
		if s.Kind == k {
			out = append(out, s)
		}
	}
	return out
}

// Covers reports whether the named source plausibly serves (lat, lon). An
// unknown id returns false: callers ask about a source they intend to use, and
// answering "sure" for a source we know nothing about is the wrong default.
func Covers(id string, lat, lon float64) bool {
	s, ok := ByID(id)
	if !ok {
		return false
	}
	return s.Covers(lat, lon)
}
