// Package prices fetches spot electricity prices from external providers
// and persists them to the state DB.
//
// Supported:
//   - sourceful — Default. Keyless European day-ahead prices through
//     Sourceful's cached ENTSO-E API, for every zone in zones.go.
//     Resolution varies per bidding zone (currently 15m in most of Europe).
//   - elprisetjustnu — Sweden, zones SE1-SE4, no API key. Since late 2025
//     NordPool publishes in 15-minute PTU (quarterly) resolution; this
//     package defaults to the quarterly endpoint and can fall back to
//     hourly if the provider returns that.
//   - entsoe — All EU, needs ENTSO-E transparency platform API key.
//     Resolution varies per bidding zone (15m or 60m).
//
// Consumer price = (spot + grid_tariff) × (1 + VAT/100). We store both
// pure spot AND the consumer total so the UI can surface either.
//
// Stored prices are in minor units of the configured currency per kWh —
// öre for SEK, cent for EUR, øre for NOK and DKK. One install holds one
// currency: the cache is cleared when the currency changes, because rows
// carry no currency of their own and mixed units would silently corrupt
// cost history.
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

// RawPrice is one time slot's pure-spot price in major units of the
// configured currency per kWh (SEK/kWh, EUR/kWh, …), before grid + VAT.
// SlotLenMin is typically 15 (NordPool PTU) or 60 (legacy hourly).
type RawPrice struct {
	SlotStart  time.Time
	SlotLenMin int
	SEKPerKWh  float64
}

// ---- Sourceful ----

// SourcefulProvider uses the same keyless, cached ENTSO-E price API as the
// Sourceful Energy app.
//
// The API converts to SEK on request but serves nothing else — every other
// currency code silently returns EUR. So we ask it only for what it really
// has (see sourcefulNative), and convert the rest ourselves from EUR with
// the same ECB rates the API uses.
type SourcefulProvider struct {
	Client  *http.Client
	BaseURL string // override in tests

	// Currency is the ISO code the caller wants prices in. Empty means SEK.
	Currency string
	// FX converts the EUR the API falls back to into Currency. Required
	// when Currency is neither EUR nor SEK.
	FX FXConverter
}

// sourcefulNative lists the currencies the price API converts to itself.
// Anything else has to go through EURToNative.
var sourcefulNative = map[string]bool{"EUR": true, "SEK": true}

// NewSourceful returns a provider pointed at Sourceful's production API.
func NewSourceful() *SourcefulProvider {
	return &SourcefulProvider{
		Client:   &http.Client{Timeout: 15 * time.Second},
		BaseURL:  "https://novacore-mainnet.sourceful.dev/services/price/electricity",
		Currency: "SEK",
	}
}

func (s *SourcefulProvider) Name() string { return "sourceful" }

// apiCurrency is what we ask the API for: the wanted currency when it can
// serve it, EUR otherwise.
func (s *SourcefulProvider) apiCurrency() string {
	want := strings.ToUpper(strings.TrimSpace(s.Currency))
	if want == "" {
		return "SEK"
	}
	if sourcefulNative[want] {
		return want
	}
	return "EUR"
}

func (s *SourcefulProvider) Fetch(ctx context.Context, zone string, day time.Time) ([]RawPrice, error) {
	zone = strings.ToUpper(strings.TrimSpace(zone))
	apiCur := s.apiCurrency()
	endpoint := fmt.Sprintf("%s/%s?currency=%s&date=%s&days=1",
		strings.TrimRight(s.BaseURL, "/"), url.PathEscape(zone), apiCur, day.Format("2006-01-02"))
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
	// The API answers an unsupported currency code with EUR rather than an
	// error, so this check is what stops a NOK install from storing EUR
	// numbers labelled NOK.
	if !strings.EqualFold(payload.Currency, apiCur) {
		return nil, fmt.Errorf("sourceful: asked for %s, got %q", apiCur, payload.Currency)
	}
	if !strings.HasSuffix(strings.ToUpper(strings.TrimSpace(payload.Unit)), "MWH") {
		return nil, fmt.Errorf("sourceful: unexpected unit %q", payload.Unit)
	}
	slotMin := resolutionMinutes(payload.Resolution)
	if slotMin <= 0 {
		return nil, fmt.Errorf("sourceful: bad resolution %q", payload.Resolution)
	}
	// Currencies the API can't serve arrive as EUR and are converted here.
	want := strings.ToUpper(strings.TrimSpace(s.Currency))
	convert := want != "" && want != apiCur
	if convert && s.FX == nil {
		return nil, fmt.Errorf("sourceful: no exchange rate source for %s→%s", apiCur, want)
	}

	out := make([]RawPrice, 0, len(payload.Prices))
	for _, point := range payload.Prices {
		t, err := time.Parse(time.RFC3339, point.Datetime)
		if err != nil {
			return nil, fmt.Errorf("sourceful: datetime %q: %w", point.Datetime, err)
		}
		perKWh := point.Price / 1000.0
		if convert {
			native, ok := s.FX.Convert(perKWh, apiCur, want)
			if !ok {
				return nil, fmt.Errorf("sourceful: no %s→%s rate yet", apiCur, want)
			}
			perKWh = native
		}
		out = append(out, RawPrice{
			SlotStart:  t,
			SlotLenMin: slotMin,
			SEKPerKWh:  perKWh,
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
// (EIC codes come from the zone catalog in zones.go), decodes its
// TimeSeries > Period > Point structure (handling both PT60M and PT15M
// resolutions and the sparse carry-forward representation), and converts
// EUR/MWh to the configured currency per kWh via EURToNative. Returns {}
// for a day not yet published, like elprisetjustnu.
type ENTSOEProvider struct {
	Client  *http.Client
	APIKey  string
	BaseURL string

	// Currency is the ISO code the caller wants prices in (default SEK).
	// Most zones publish EUR/MWh; FX converts from whatever the document
	// says to Currency, and is only needed when the two differ.
	Currency string
	FX       FXConverter
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

func (e *ENTSOEProvider) Fetch(ctx context.Context, zone string, day time.Time) ([]RawPrice, error) {
	if e.APIKey == "" {
		return nil, errors.New("entsoe: API key required")
	}
	z, ok := LookupZone(zone)
	if !ok {
		return nil, fmt.Errorf("entsoe: unknown zone %q", zone)
	}
	eic := z.EIC
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
// nested TimeSeries > Period > Point entries. Most zones publish EUR/MWh,
// but not all — Poland and Hungary among others publish in their own
// currency — so each TimeSeries carries its own currency_Unit.name and we
// convert from that, not from an assumed EUR.
//
// The struct tags carry no namespace, which encoding/xml matches by local
// name regardless of the document's default xmlns — so the dotted element
// names (price.amount, currency_Unit.name) bind directly.
type entsoePublication struct {
	XMLName    xml.Name           `xml:"Publication_MarketDocument"`
	TimeSeries []entsoeTimeSeries `xml:"TimeSeries"`
}

type entsoeTimeSeries struct {
	Currency string         `xml:"currency_Unit.name"`
	Periods  []entsoePeriod `xml:"Period"`
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

// wantCurrency is the ISO code prices are stored in; empty config means SEK,
// which is what NewENTSOE and FromConfig already default to.
func (e *ENTSOEProvider) wantCurrency() string {
	if c := strings.ToUpper(strings.TrimSpace(e.Currency)); c != "" {
		return c
	}
	return "SEK"
}

// perMWhToNative converts a published price per MWh into the configured
// currency per kWh. from is the document's own currency; a document that
// already publishes in the wanted currency needs no rate at all.
//
// A missing rate is an error rather than a guess: a price off by an
// exchange rate steers dispatch and lands in cost history.
func (e *ENTSOEProvider) perMWhToNative(amountPerMWh float64, from string) (float64, error) {
	perKWh := amountPerMWh / 1000.0
	want := e.wantCurrency()
	if from == "" {
		from = "EUR" // the platform's default, and what it omits
	}
	if strings.EqualFold(from, want) {
		return perKWh, nil
	}
	if e.FX == nil {
		return 0, fmt.Errorf("entsoe: no exchange rate source for %s→%s", from, want)
	}
	v, ok := e.FX.Convert(perKWh, from, want)
	if !ok {
		return 0, fmt.Errorf("entsoe: no %s→%s rate yet", from, want)
	}
	return v, nil
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
			rows, err := e.expandPeriod(pd, ts.Currency)
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
func (e *ENTSOEProvider) expandPeriod(pd entsoePeriod, docCurrency string) ([]RawPrice, error) {
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
		native, err := e.perMWhToNative(last, docCurrency)
		if err != nil {
			return nil, err
		}
		out = append(out, RawPrice{
			SlotStart:  start.Add(time.Duration(pos-1) * time.Duration(slotMin) * time.Minute),
			SlotLenMin: slotMin,
			SEKPerKWh:  native,
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

// Apply computes the total the consumer pays, (spot + grid tariff) ×
// (1 + VAT), in minor units per kWh. Returns (spot, total).
func (a Applier) Apply(sekPerKwh float64) (spotOre, totalOre float64) {
	spotOre = sekPerKwh * 100 // major → minor units (SEK→öre, EUR→cent)
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
	// Currency the stored minor units are in. Read by the API so the UI
	// can label them.
	Currency string

	stop chan struct{}
	done chan struct{}
}

// FXConverter abstracts currency conversion so the prices package
// doesn't need to import currency/ directly (and FromConfig callers
// can pass a test stub). When it is nil, or has no rate for the pair, a
// provider that needs conversion fails its fetch instead of guessing.
type FXConverter interface {
	Convert(amount float64, from, to string) (float64, bool)
}

// FromConfig builds a Service from the runtime config. Returns nil + nil if
// prices are disabled (provider=none or missing section).
func FromConfig(cfg *config.Price, st *state.Store, fx FXConverter) *Service {
	if cfg == nil || cfg.Provider == "" || cfg.Provider == "none" {
		return nil
	}
	zone := cfg.Zone
	if zone == "" {
		zone = "SE3"
	}
	// An unset currency follows the zone: picking BE should not leave a
	// Belgian household paying in öre. Unknown zones keep the old default.
	currency := strings.ToUpper(strings.TrimSpace(cfg.Currency))
	if currency == "" {
		currency = ZoneCurrency(zone)
	}
	if currency == "" {
		currency = "SEK"
	}
	var p Provider
	switch cfg.Provider {
	case "sourceful":
		sp := NewSourceful()
		sp.Currency = currency
		sp.FX = fx
		p = sp
	case "elprisetjustnu":
		p = NewElpriser()
	case "entsoe":
		ep := NewENTSOE(cfg.APIKey)
		ep.Currency = currency
		ep.FX = fx
		p = ep
	default:
		return nil
	}
	vat := cfg.VATPercent
	if vat == 0 {
		vat = 25
	}
	return &Service{
		Provider: p,
		Store:    st,
		Zone:     zone,
		Currency: currency,
		Applier:  Applier{GridTariffOreKwh: cfg.GridTariffOreKwh, VATPercent: vat},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// priceCurrencyKey records the currency the cached price rows are in.
const priceCurrencyKey = "prices/currency"

// Start begins the fetch-on-schedule goroutine. Does an initial fetch
// immediately + every hour (plus specifically at 13:05 CET for day-ahead release).
func (s *Service) Start(ctx context.Context) {
	s.syncCachedCurrency()
	go s.loop(ctx)
}

// syncCachedCurrency empties the price cache when the configured currency
// no longer matches the currency the cached rows were fetched in. Runs
// before the first fetch so no reader ever sees two currencies at once.
func (s *Service) syncCachedCurrency() {
	if s.Store == nil || s.Currency == "" {
		return
	}
	prev, ok := s.Store.LoadConfig(priceCurrencyKey)
	if ok && prev == s.Currency {
		return
	}
	if ok && prev != "" {
		n, err := s.Store.ClearPrices()
		if err != nil {
			slog.Warn("price cache clear failed after currency change",
				"from", prev, "to", s.Currency, "err", err)
			return
		}
		slog.Warn("price currency changed — cached prices cleared",
			"from", prev, "to", s.Currency, "rows", n)
	}
	if err := s.Store.SaveConfig(priceCurrencyKey, s.Currency); err != nil {
		slog.Warn("could not record price currency", "err", err)
	}
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
