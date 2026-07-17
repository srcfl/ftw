// Package prices fetches spot electricity prices from external providers
// and persists them to the state DB.
//
// Supported:
//   - sourceful — Default. Keyless European day-ahead prices through
//     Sourceful's cached ENTSO-E API. Resolution varies per bidding zone
//     (currently 15m in the Nordics).
//   - elprisetjustnu — Sweden, zones SE1-SE4, no API key. Since late 2025
//     NordPool publishes in 15-minute PTU (quarterly) resolution; this
//     package defaults to the quarterly endpoint and can fall back to
//     hourly if the provider returns that.
//   - entsoe — All EU, needs ENTSO-E transparency platform API key.
//     Resolution varies per bidding zone (15m or 60m).
//
// Consumer price = (spot + grid_tariff) × (1 + VAT/100). We store both
// pure spot AND the consumer total so the UI can surface either.
// Prices are in öre/kWh (1 SEK = 100 öre).
package prices

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// Provider is implemented by each concrete price source. Fetch returns
// spot-price slots in SEK/kWh (we convert to öre later).
type Provider interface {
	// Name returns a short identifier for logging / store.source column.
	Name() string
	// Fetch returns hourly day-ahead spot prices for the given zone +
	// calendar day (local time). Returns {} with nil error if no data
	// published yet for that day (day-ahead typically releases around 13:00 CET).
	Fetch(ctx context.Context, zone string, day time.Time) ([]RawPrice, error)
}

// RawPrice is one time slot's pure-spot price in SEK/kWh (before grid + VAT).
// SlotLenMin is typically 15 (NordPool PTU) or 60 (legacy hourly).
type RawPrice struct {
	SlotStart  time.Time
	SlotLenMin int
	SEKPerKWh  float64
}

// ---- Sourceful ----

// SourcefulProvider uses the same keyless, cached ENTSO-E price API as the
// Sourceful Energy app. The API returns the requested currency per MWh; FTW
// always requests SEK so the package-wide SEK/kWh invariant remains true.
type SourcefulProvider struct {
	Client  *http.Client
	BaseURL string // override in tests
}

// NewSourceful returns a provider pointed at Sourceful's production API.
func NewSourceful() *SourcefulProvider {
	return &SourcefulProvider{
		Client:  &http.Client{Timeout: 15 * time.Second},
		BaseURL: "https://novacore-mainnet.sourceful.dev/services/price/electricity",
	}
}

func (s *SourcefulProvider) Name() string { return "sourceful" }

func (s *SourcefulProvider) Fetch(ctx context.Context, zone string, day time.Time) ([]RawPrice, error) {
	zone = strings.ToUpper(strings.TrimSpace(zone))
	endpoint := fmt.Sprintf("%s/%s?currency=SEK&date=%s&days=1",
		strings.TrimRight(s.BaseURL, "/"), url.PathEscape(zone), day.Format("2006-01-02"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("sourceful: status %d: %s", resp.StatusCode, string(body))
	}

	var payload struct {
		Area       string `json:"area"`
		Currency   string `json:"currency"`
		Unit       string `json:"unit"`
		Resolution string `json:"resolution"`
		Prices     []struct {
			Datetime string  `json:"datetime"`
			Price    float64 `json:"price"`
		} `json:"prices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("sourceful: decode: %w", err)
	}
	if payload.Area != "" && !strings.EqualFold(payload.Area, zone) {
		return nil, fmt.Errorf("sourceful: response area %q does not match %q", payload.Area, zone)
	}
	if !strings.EqualFold(payload.Currency, "SEK") {
		return nil, fmt.Errorf("sourceful: unexpected currency %q", payload.Currency)
	}
	if !strings.HasSuffix(strings.ToUpper(strings.TrimSpace(payload.Unit)), "MWH") {
		return nil, fmt.Errorf("sourceful: unexpected unit %q", payload.Unit)
	}
	slotMin := resolutionMinutes(payload.Resolution)
	if slotMin <= 0 {
		return nil, fmt.Errorf("sourceful: bad resolution %q", payload.Resolution)
	}

	out := make([]RawPrice, 0, len(payload.Prices))
	for _, point := range payload.Prices {
		t, err := time.Parse(time.RFC3339, point.Datetime)
		if err != nil {
			return nil, fmt.Errorf("sourceful: datetime %q: %w", point.Datetime, err)
		}
		out = append(out, RawPrice{
			SlotStart:  t,
			SlotLenMin: slotMin,
			SEKPerKWh:  point.Price / 1000.0,
		})
	}
	return out, nil
}

// ---- elprisetjustnu ----

// ElpriserProvider is the legacy Sweden-only provider — keyless.
//
// Since NordPool's transition to 15-min PTU in late 2025, the single
// endpoint /api/v1/prices/YYYY/MM-DD_SEZ.json returns 96 rows × 15 min
// for current days, and 24 rows × 60 min for older days. We auto-detect
// the resolution from the spacing between consecutive time_start values
// so we don't have to guess per-endpoint.
type ElpriserProvider struct {
	Client  *http.Client
	BaseURL string // override in tests
}

// NewElpriser returns a provider with default HTTP client.
func NewElpriser() *ElpriserProvider {
	return &ElpriserProvider{
		Client:  &http.Client{Timeout: 15 * time.Second},
		BaseURL: "https://www.elprisetjustnu.se/api/v1/prices",
	}
}

func (e *ElpriserProvider) Name() string { return "elprisetjustnu" }

func (e *ElpriserProvider) Fetch(ctx context.Context, zone string, day time.Time) ([]RawPrice, error) {
	url := fmt.Sprintf("%s/%d/%02d-%02d_%s.json",
		e.BaseURL, day.Year(), day.Month(), day.Day(), zone)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("elprisetjustnu: status %d: %s", resp.StatusCode, string(body))
	}
	var rows []struct {
		SEKPerKWh float64 `json:"SEK_per_kWh"`
		TimeStart string  `json:"time_start"`
		TimeEnd   string  `json:"time_end"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("elprisetjustnu: decode: %w", err)
	}
	out := make([]RawPrice, 0, len(rows))
	for _, r := range rows {
		t, err := time.Parse(time.RFC3339, r.TimeStart)
		if err != nil {
			continue
		}
		slotMin := 60
		if r.TimeEnd != "" {
			if te, err := time.Parse(time.RFC3339, r.TimeEnd); err == nil {
				d := int(te.Sub(t).Minutes())
				if d >= 5 && d <= 120 {
					slotMin = d
				}
			}
		}
		out = append(out, RawPrice{SlotStart: t, SlotLenMin: slotMin, SEKPerKWh: r.SEKPerKWh})
	}
	// Back-fill slot length from spacing if time_end wasn't present
	if len(out) >= 2 && out[0].SlotLenMin == 60 {
		delta := int(out[1].SlotStart.Sub(out[0].SlotStart).Minutes())
		if delta > 0 && delta < 60 {
			for i := range out {
				out[i].SlotLenMin = delta
			}
		}
	}
	return out, nil
}

// ---- ENTSOE ----

// ENTSOEProvider uses the EU transparency platform. Needs an API key
// ("security token") — free to request at
// https://transparency.entsoe.eu/ then email for activation (~1 day).
//
// Fetches the A44 day-ahead Publication_MarketDocument for a bidding zone
// (EIC codes below), decodes its TimeSeries > Period > Point structure
// (handling both PT60M and PT15M resolutions and the sparse carry-forward
// representation), and converts EUR/MWh to native currency per kWh via
// EURToNative. Returns {} for a day not yet published, like elprisetjustnu.
type ENTSOEProvider struct {
	Client  *http.Client
	APIKey  string
	BaseURL string

	// Currency is the ISO code the caller wants prices in (default SEK).
	// ENTSOE publishes EUR/MWh; we convert via EURToNative if non-nil.
	Currency    string
	EURToNative func(eur float64) float64 // returns amount in native currency
}

// NewENTSOE returns a provider — caller must set APIKey.
func NewENTSOE(apiKey string) *ENTSOEProvider {
	return &ENTSOEProvider{
		Client:   &http.Client{Timeout: 30 * time.Second},
		APIKey:   apiKey,
		BaseURL:  "https://web-api.tp.entsoe.eu/api",
		Currency: "SEK",
	}
}

func (e *ENTSOEProvider) Name() string { return "entsoe" }

// EIC codes for common zones. Full list at
// https://eepublicdownloads.entsoe.eu/clean-documents/EDI/Library/Y_codes_list.pdf
var entsoeZoneEIC = map[string]string{
	"SE1": "10Y1001A1001A44P",
	"SE2": "10Y1001A1001A45N",
	"SE3": "10Y1001A1001A46L",
	"SE4": "10Y1001A1001A47J",
	"NO1": "10YNO-1--------2",
	"NO2": "10YNO-2--------T",
	"NO3": "10YNO-3--------J",
	"NO4": "10YNO-4--------9",
	"DK1": "10YDK-1--------W",
	"DK2": "10YDK-2--------M",
	"FI":  "10YFI-1--------U",
	"DE":  "10Y1001A1001A83F",
}

func (e *ENTSOEProvider) Fetch(ctx context.Context, zone string, day time.Time) ([]RawPrice, error) {
	if e.APIKey == "" {
		return nil, errors.New("entsoe: API key required")
	}
	eic, ok := entsoeZoneEIC[zone]
	if !ok {
		return nil, fmt.Errorf("entsoe: unknown zone %q", zone)
	}
	periodStart := day.UTC().Format("200601021504")
	periodEnd := day.Add(24 * time.Hour).UTC().Format("200601021504")
	url := fmt.Sprintf("%s?documentType=A44&in_Domain=%s&out_Domain=%s&periodStart=%s&periodEnd=%s&securityToken=%s",
		e.BaseURL, eic, eic, periodStart, periodEnd, e.APIKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("entsoe: status %d: %s", resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return e.parseXML(body)
}

// ---- ENTSOE XML decode ----
//
// The transparency platform returns a Publication_MarketDocument with
// nested TimeSeries > Period > Point entries. Prices are EUR/MWh. We
// convert to native currency per kWh via EURToNative (set from config FX;
// falls back to a ballpark 11.5 SEK/EUR when unwired).
//
// The struct tags carry no namespace, which encoding/xml matches by local
// name regardless of the document's default xmlns — so the dotted element
// names (price.amount, currency_Unit.name) bind directly.
type entsoePublication struct {
	XMLName    xml.Name           `xml:"Publication_MarketDocument"`
	TimeSeries []entsoeTimeSeries `xml:"TimeSeries"`
}

type entsoeTimeSeries struct {
	Periods []entsoePeriod `xml:"Period"`
}

type entsoePeriod struct {
	Start      string        `xml:"timeInterval>start"`
	End        string        `xml:"timeInterval>end"`
	Resolution string        `xml:"resolution"`
	Points     []entsoePoint `xml:"Point"`
}

type entsoePoint struct {
	Position int     `xml:"position"`
	Amount   float64 `xml:"price.amount"`
}

// eurMWhToNative converts an EUR/MWh figure to native currency per kWh,
// using the provider's configured converter or the ballpark fallback.
func (e *ENTSOEProvider) eurMWhToNative(eurPerMWh float64) float64 {
	eurPerKWh := eurPerMWh / 1000.0
	if e.EURToNative != nil {
		return e.EURToNative(eurPerKWh)
	}
	return eurPerKWh * 11.5
}

// parseXML decodes a day-ahead Publication_MarketDocument into raw slots.
// Returns ({}, nil) when the document carries no price data (e.g. an
// Acknowledgement returned for a day the auction hasn't published yet),
// mirroring the elprisetjustnu 404 path so the scheduler just retries.
func (e *ENTSOEProvider) parseXML(body []byte) ([]RawPrice, error) {
	var doc entsoePublication
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("entsoe: decode: %w", err)
	}
	var out []RawPrice
	for _, ts := range doc.TimeSeries {
		for _, pd := range ts.Periods {
			rows, err := e.expandPeriod(pd)
			if err != nil {
				return nil, err
			}
			out = append(out, rows...)
		}
	}
	return out, nil
}

// expandPeriod turns one Period into per-slot prices. ENTSOE uses a sparse
// A44 representation: a Point is omitted when its price equals the previous
// position's, so we carry the last seen price forward to fill the period's
// full slot count (derived from the timeInterval, not the Point count).
func (e *ENTSOEProvider) expandPeriod(pd entsoePeriod) ([]RawPrice, error) {
	slotMin := resolutionMinutes(pd.Resolution)
	if slotMin <= 0 {
		return nil, fmt.Errorf("entsoe: bad resolution %q", pd.Resolution)
	}
	start, err := parseENTSOETime(pd.Start)
	if err != nil {
		return nil, fmt.Errorf("entsoe: period start %q: %w", pd.Start, err)
	}
	// Slot count from the interval if the end is parseable, else from the
	// highest position present. The interval form is what lets carry-forward
	// extend a trailing price to the end of the day.
	nSlots := 0
	if end, err := parseENTSOETime(pd.End); err == nil && end.After(start) {
		nSlots = int(end.Sub(start) / (time.Duration(slotMin) * time.Minute))
	}
	priceAt := make(map[int]float64, len(pd.Points))
	maxPos := 0
	for _, p := range pd.Points {
		priceAt[p.Position] = p.Amount
		if p.Position > maxPos {
			maxPos = p.Position
		}
	}
	if nSlots < maxPos {
		nSlots = maxPos
	}
	if nSlots <= 0 || nSlots > 4*24*4 { // guard malformed intervals (≤ 4 days @ 15min)
		return nil, fmt.Errorf("entsoe: implausible slot count %d", nSlots)
	}
	out := make([]RawPrice, 0, nSlots)
	last, have := 0.0, false
	for pos := 1; pos <= nSlots; pos++ {
		if v, ok := priceAt[pos]; ok {
			last, have = v, true
		}
		if !have {
			continue // no leading price to carry yet
		}
		out = append(out, RawPrice{
			SlotStart:  start.Add(time.Duration(pos-1) * time.Duration(slotMin) * time.Minute),
			SlotLenMin: slotMin,
			SEKPerKWh:  e.eurMWhToNative(last),
		})
	}
	return out, nil
}

// resolutionMinutes maps an ISO-8601 duration (PT60M, PT15M, PT30M, PT1H)
// to minutes. Returns 0 for anything it can't read.
func resolutionMinutes(res string) int {
	s := strings.TrimPrefix(res, "PT")
	switch {
	case strings.HasSuffix(s, "M"):
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "M")); err == nil {
			return n
		}
	case strings.HasSuffix(s, "H"):
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "H")); err == nil {
			return n * 60
		}
	}
	return 0
}

// parseENTSOETime reads the platform's timestamps, which come minute-
// precision UTC ("2026-06-02T22:00Z") but occasionally second-precision.
func parseENTSOETime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04Z07:00", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q", s)
}

// ---- Applier: turns raw SEK/kWh into consumer öre/kWh ----

// Applier applies grid tariff + VAT to raw spot prices.
type Applier struct {
	// GridTariffOreKwh is the fixed per-kWh transport fee added to spot
	GridTariffOreKwh float64
	// VATPercent is Swedish default 25.0
	VATPercent float64
}

// Apply computes total öre/kWh the consumer pays (spot + grid tariff) × (1 + VAT).
// Returns (spot_ore, total_ore).
func (a Applier) Apply(sekPerKwh float64) (spotOre, totalOre float64) {
	spotOre = sekPerKwh * 100 // SEK/kWh → öre/kWh
	// Consumer cost: (spot + grid tariff) * (1 + VAT/100)
	totalOre = (spotOre + a.GridTariffOreKwh) * (1 + a.VATPercent/100)
	return
}

// ---- Service: coordinator that fetches on a schedule + exposes read API ----

// Service wraps a provider + store + scheduler.
type Service struct {
	Provider Provider
	Store    *state.Store
	Applier  Applier
	Zone     string

	stop chan struct{}
	done chan struct{}
}

// FXConverter abstracts currency conversion so the prices package
// doesn't need to import currency/ directly (and FromConfig callers
// can pass a test stub). If nil, ENTSOE assumes 1 EUR = 11.5 SEK — a
// ballpark used only until live rates land.
type FXConverter interface {
	Convert(amount float64, from, to string) (float64, bool)
}

// FromConfig builds a Service from the runtime config. Returns nil + nil if
// prices are disabled (provider=none or missing section).
func FromConfig(cfg *config.Price, st *state.Store, fx FXConverter) *Service {
	if cfg == nil || cfg.Provider == "" || cfg.Provider == "none" {
		return nil
	}
	currency := cfg.Currency
	if currency == "" {
		currency = "SEK"
	}
	var p Provider
	switch cfg.Provider {
	case "sourceful":
		p = NewSourceful()
	case "elprisetjustnu":
		p = NewElpriser()
	case "entsoe":
		ep := NewENTSOE(cfg.APIKey)
		ep.Currency = currency
		if fx != nil {
			ep.EURToNative = func(eur float64) float64 {
				v, ok := fx.Convert(eur, "EUR", currency)
				if !ok {
					return eur * 11.5 // fallback until rates land
				}
				return v
			}
		} else {
			ep.EURToNative = func(eur float64) float64 { return eur * 11.5 }
		}
		p = ep
	default:
		return nil
	}
	zone := cfg.Zone
	if zone == "" {
		zone = "SE3"
	}
	vat := cfg.VATPercent
	if vat == 0 {
		vat = 25
	}
	return &Service{
		Provider: p,
		Store:    st,
		Zone:     zone,
		Applier:  Applier{GridTariffOreKwh: cfg.GridTariffOreKwh, VATPercent: vat},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins the fetch-on-schedule goroutine. Does an initial fetch
// immediately + every hour (plus specifically at 13:05 CET for day-ahead release).
func (s *Service) Start(ctx context.Context) {
	go s.loop(ctx)
}

// Stop terminates the fetcher.
func (s *Service) Stop() {
	close(s.stop)
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	// Initial fetch (today + tomorrow in case day-ahead is already published)
	s.fetchAndStore(ctx)
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			s.fetchAndStore(ctx)
		}
	}
}

func (s *Service) fetchAndStore(ctx context.Context) {
	now := time.Now()
	for _, offset := range []int{0, 1} { // today + tomorrow
		day := now.AddDate(0, 0, offset)
		rows, err := s.Provider.Fetch(ctx, s.Zone, day)
		if err != nil {
			slog.Warn("price fetch failed", "zone", s.Zone, "day", day.Format("2006-01-02"), "err", err)
			continue
		}
		if len(rows) == 0 {
			continue
		}
		points := make([]state.PricePoint, 0, len(rows))
		nowMs := time.Now().UnixMilli()
		for _, r := range rows {
			spotOre, totalOre := s.Applier.Apply(r.SEKPerKWh)
			slot := r.SlotLenMin
			if slot <= 0 {
				slot = 60
			}
			points = append(points, state.PricePoint{
				Zone:        s.Zone,
				SlotTsMs:    r.SlotStart.UnixMilli(),
				SlotLenMin:  slot,
				SpotOreKwh:  spotOre,
				TotalOreKwh: totalOre,
				Source:      s.Provider.Name(),
				FetchedAtMs: nowMs,
			})
		}
		if err := s.Store.SavePrices(points); err != nil {
			slog.Warn("price save failed", "err", err)
			continue
		}
		slog.Info("prices fetched", "zone", s.Zone, "day", day.Format("2006-01-02"),
			"count", len(points), "source", s.Provider.Name())
	}
}

// Load is a convenience wrapper around store.LoadPrices using the service's zone.
func (s *Service) Load(sinceMs, untilMs int64) ([]state.PricePoint, error) {
	return s.Store.LoadPrices(s.Zone, sinceMs, untilMs)
}
