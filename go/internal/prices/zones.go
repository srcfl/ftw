package prices

import (
	"sort"
	"strings"
)

// Zone is one day-ahead bidding zone: what to ask a provider for, and what
// the household in it pays with.
//
// Code is the area code both the Sourceful API and our config use. EIC is
// the ENTSO-E energy identification code for the same area, so the direct
// ENTSO-E provider needs no second table. Currency is the local currency of
// the country, which is what a new install should default to — the API
// itself only serves EUR and SEK, so anything else is converted here.
type Zone struct {
	Code     string
	Country  string
	Currency string
	EIC      string
	// Label distinguishes zones whose code doesn't say where it is
	// (IT-SARDINIA, NO2NSL). Empty when the code speaks for itself.
	Label string
}

// Name is what a picker shows: country plus the zone's own label when the
// country has more than one zone.
func (z Zone) Name() string {
	if z.Label != "" {
		return z.Country + " — " + z.Label
	}
	return z.Country
}

// zones lists every area the Sourceful price API publishes, generated from
// its /areas endpoint. ENTSO-E covers more areas than this; a zone that
// isn't here can still be reached with provider=entsoe and a hand-written
// config, but it won't appear in the picker.
//
// Serbia and Ukraine default to EUR: the ECB publishes no reference rate for
// RSD or UAH, so a local-currency price would be a guess. The price is real,
// only the currency isn't theirs.
var zones = []Zone{
	{Code: "AT", Country: "Austria", Currency: "EUR", EIC: "10YAT-APG------L", Label: ""},
	{Code: "BE", Country: "Belgium", Currency: "EUR", EIC: "10YBE----------2", Label: ""},
	{Code: "BG", Country: "Bulgaria", Currency: "EUR", EIC: "10YCA-BULGARIA-R", Label: ""},
	{Code: "HR", Country: "Croatia", Currency: "EUR", EIC: "10YHR-HEP------M", Label: ""},
	{Code: "CZ", Country: "Czech Republic", Currency: "CZK", EIC: "10YCZ-CEPS-----N", Label: ""},
	{Code: "DK1", Country: "Denmark", Currency: "DKK", EIC: "10YDK-1--------W", Label: "West"},
	{Code: "DK2", Country: "Denmark", Currency: "DKK", EIC: "10YDK-2--------M", Label: "East"},
	{Code: "EE", Country: "Estonia", Currency: "EUR", EIC: "10Y1001A1001A39I", Label: ""},
	{Code: "FI", Country: "Finland", Currency: "EUR", EIC: "10YFI-1--------U", Label: ""},
	{Code: "FR", Country: "France", Currency: "EUR", EIC: "10YFR-RTE------C", Label: ""},
	{Code: "DE", Country: "Germany", Currency: "EUR", EIC: "10Y1001A1001A82H", Label: ""},
	{Code: "GR", Country: "Greece", Currency: "EUR", EIC: "10YGR-HTSO-----Y", Label: ""},
	{Code: "HU", Country: "Hungary", Currency: "HUF", EIC: "10YHU-MAVIR----U", Label: ""},
	{Code: "IT-CALABRIA", Country: "Italy", Currency: "EUR", EIC: "10Y1001C--00096J", Label: "Calabria"},
	{Code: "IT-CENTRE-NORTH", Country: "Italy", Currency: "EUR", EIC: "10Y1001A1001A70O", Label: "Centre-North"},
	{Code: "IT-CENTRE-SOUTH", Country: "Italy", Currency: "EUR", EIC: "10Y1001A1001A71M", Label: "Centre-South"},
	{Code: "IT-NORTH", Country: "Italy", Currency: "EUR", EIC: "10Y1001A1001A73I", Label: "North"},
	{Code: "IT-SACOAC", Country: "Italy", Currency: "EUR", EIC: "10Y1001A1001A885", Label: "SACOAC"},
	{Code: "IT-SACODC", Country: "Italy", Currency: "EUR", EIC: "10Y1001A1001A893", Label: "SACODC"},
	{Code: "IT-SARDINIA", Country: "Italy", Currency: "EUR", EIC: "10Y1001A1001A74G", Label: "Sardinia"},
	{Code: "IT-SICILY", Country: "Italy", Currency: "EUR", EIC: "10Y1001A1001A75E", Label: "Sicily"},
	{Code: "IT-SOUTH", Country: "Italy", Currency: "EUR", EIC: "10Y1001A1001A788", Label: "South"},
	{Code: "LV", Country: "Latvia", Currency: "EUR", EIC: "10YLV-1001A00074", Label: ""},
	{Code: "LT", Country: "Lithuania", Currency: "EUR", EIC: "10YLT-1001A0008Q", Label: ""},
	{Code: "LU", Country: "Luxembourg", Currency: "EUR", EIC: "10Y1001A1001A82H", Label: ""},
	{Code: "ME", Country: "Montenegro", Currency: "EUR", EIC: "10YCS-CG-TSO---S", Label: ""},
	{Code: "NL", Country: "Netherlands", Currency: "EUR", EIC: "10YNL----------L", Label: ""},
	{Code: "NO1", Country: "Norway", Currency: "NOK", EIC: "10YNO-1--------2", Label: "Oslo"},
	{Code: "NO2", Country: "Norway", Currency: "NOK", EIC: "10YNO-2--------T", Label: "Kristiansand"},
	{Code: "NO2NSL", Country: "Norway", Currency: "NOK", EIC: "50Y0JVU59B4JWQCU", Label: "North Sea Link"},
	{Code: "NO3", Country: "Norway", Currency: "NOK", EIC: "10YNO-3--------J", Label: "Trondheim"},
	{Code: "NO4", Country: "Norway", Currency: "NOK", EIC: "10YNO-4--------9", Label: "Tromsø"},
	{Code: "NO5", Country: "Norway", Currency: "NOK", EIC: "10Y1001A1001A48H", Label: "Bergen"},
	{Code: "PL", Country: "Poland", Currency: "PLN", EIC: "10YPL-AREA-----S", Label: ""},
	{Code: "PT", Country: "Portugal", Currency: "EUR", EIC: "10YPT-REN------W", Label: ""},
	{Code: "RO", Country: "Romania", Currency: "RON", EIC: "10YRO-TEL------P", Label: ""},
	{Code: "RS", Country: "Serbia", Currency: "EUR", EIC: "10YCS-SERBIATSOV", Label: ""},
	{Code: "SK", Country: "Slovakia", Currency: "EUR", EIC: "10YSK-SEPS-----K", Label: ""},
	{Code: "SI", Country: "Slovenia", Currency: "EUR", EIC: "10YSI-ELES-----O", Label: ""},
	{Code: "ES", Country: "Spain", Currency: "EUR", EIC: "10YES-REE------0", Label: ""},
	{Code: "SE1", Country: "Sweden", Currency: "SEK", EIC: "10Y1001A1001A44P", Label: "Luleå"},
	{Code: "SE2", Country: "Sweden", Currency: "SEK", EIC: "10Y1001A1001A45N", Label: "Sundsvall"},
	{Code: "SE3", Country: "Sweden", Currency: "SEK", EIC: "10Y1001A1001A46L", Label: "Stockholm"},
	{Code: "SE4", Country: "Sweden", Currency: "SEK", EIC: "10Y1001A1001A47J", Label: "Malmö"},
	{Code: "CH", Country: "Switzerland", Currency: "CHF", EIC: "10YCH-SWISSGRIDZ", Label: ""},
	{Code: "UA-IPS", Country: "Ukraine", Currency: "EUR", EIC: "10Y1001C--000182", Label: "IPS"},
}

// Zones returns every known bidding zone, sorted by country then code, which
// is the order a picker wants.
func Zones() []Zone {
	out := make([]Zone, len(zones))
	copy(out, zones)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Country != out[j].Country {
			return out[i].Country < out[j].Country
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// LookupZone finds a zone by area code, case- and space-insensitive.
func LookupZone(code string) (Zone, bool) {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, z := range zones {
		if z.Code == code {
			return z, true
		}
	}
	return Zone{}, false
}

// ZoneCurrency returns the local currency for an area code, or "" when the
// code is unknown. Callers treat "" as "keep whatever is configured".
func ZoneCurrency(code string) string {
	if z, ok := LookupZone(code); ok {
		return z.Currency
	}
	return ""
}
