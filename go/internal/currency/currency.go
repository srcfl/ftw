// Package currency fetches daily FX reference rates from the European
// Central Bank and caches them in state for offline lookup.
//
// ECB publishes once per business day at ~16:00 CET. We refresh every
// 6 hours; between fetches we use the cached value — so a network blip
// doesn't break pricing.
//
// The rates file is a tiny XML document (~2KB) at a stable public URL:
// https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml
//
// All rates are EUR-based. To convert from e.g. NOK to SEK we go via
// EUR: sek_amount = nok_amount × (rate[SEK] / rate[NOK]).
//
// If a currency is missing from ECB (e.g. some smaller countries),
// Rate returns (0, false) and callers must fall back to configured
// defaults.
package currency

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

const (
	ecbURL     = "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml"
	stateKey   = "fx/ecb_rates"
	defaultTTL = 6 * time.Hour
)

// Service fetches + caches FX rates. Safe for concurrent use.
type Service struct {
	Store  *state.Store
	Client *http.Client
	TTL    time.Duration

	mu    sync.RWMutex
	rates map[string]float64 // currency code → units per EUR
	asOf  time.Time

	stop chan struct{}
	done chan struct{}
}

// New constructs a service. Call Start to begin the refresh loop.
func New(st *state.Store) *Service {
	return &Service{
		Store:  st,
		Client: &http.Client{Timeout: 15 * time.Second},
		TTL:    defaultTTL,
		rates:  map[string]float64{"EUR": 1.0},
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// Rate returns units-per-EUR for a currency code (e.g. "SEK" → 11.42).
// Case-insensitive. Returns (1, true) for EUR.
func (s *Service) Rate(code string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	code = strings.ToUpper(code)
	if code == "EUR" {
		return 1.0, true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.rates[code]
	return v, ok
}

// Convert transforms an amount from fromCode to toCode via EUR. Returns
// (converted_amount, true) on success, (0, false) if either currency is
// unknown.
func (s *Service) Convert(amount float64, fromCode, toCode string) (float64, bool) {
	if strings.EqualFold(fromCode, toCode) {
		return amount, true
	}
	from, ok1 := s.Rate(fromCode)
	to, ok2 := s.Rate(toCode)
	if !ok1 || !ok2 {
		return 0, false
	}
	return amount / from * to, true
}

// AsOf returns when the current rates were published by ECB.
func (s *Service) AsOf() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.asOf
}

// Start runs the fetch loop. Does an initial fetch immediately, then
// every TTL. Restores from state on boot so we have rates before the
// first successful HTTP call.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	if s.Store != nil {
		if js, ok := s.Store.LoadConfig(stateKey); ok && js != "" {
			if err := s.parseCached(js); err == nil {
				slog.Info("currency rates restored from cache", "asof", s.asOf, "codes", len(s.rates))
			}
		}
	}
	go s.loop(ctx)
}

// Stop terminates the fetcher.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	close(s.stop)
	<-s.done
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.done)
	s.fetch(ctx)
	t := time.NewTicker(s.TTL)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			s.fetch(ctx)
		}
	}
}

// parseECB decodes the ECB XML into the envelope struct. Exposed so
// tests don't have to go through the fetch loop.
func parseECB(body []byte, env *ecbEnvelope) error {
	return xml.Unmarshal(body, env)
}

// ecbEnvelope mirrors the small subset of the ECB daily XML we parse.
type ecbEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Cube    struct {
		Cube struct {
			Time  string `xml:"time,attr"`
			Rates []struct {
				Currency string  `xml:"currency,attr"`
				Rate     float64 `xml:"rate,attr"`
			} `xml:"Cube"`
		} `xml:"Cube"`
	} `xml:"Cube"`
}

func (s *Service) fetch(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ecbURL, nil)
	if err != nil {
		slog.Warn("ecb fx fetch", "err", err)
		return
	}
	req.Header.Set("User-Agent", "FTW/1.0")
	resp, err := s.Client.Do(req)
	if err != nil {
		slog.Warn("ecb fx fetch", "err", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		slog.Warn("ecb fx fetch non-200", "status", resp.StatusCode, "body", string(body))
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Warn("ecb fx read", "err", err)
		return
	}
	var env ecbEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		slog.Warn("ecb fx parse", "err", err)
		return
	}
	asOf, _ := time.Parse("2006-01-02", env.Cube.Cube.Time)
	rates := map[string]float64{"EUR": 1.0}
	for _, r := range env.Cube.Cube.Rates {
		if r.Rate > 0 {
			rates[strings.ToUpper(r.Currency)] = r.Rate
		}
	}
	if len(rates) < 10 {
		slog.Warn("ecb fx parse yielded too few rates", "count", len(rates))
		return
	}
	s.mu.Lock()
	s.rates = rates
	s.asOf = asOf
	s.mu.Unlock()
	s.persist()
	slog.Info("ecb fx rates updated", "asof", asOf, "codes", len(rates))
}

// persist writes the current rates to state as a simple flat string:
//
//   "YYYY-MM-DD;CODE:RATE;CODE:RATE;..."
//
// Keeps parsing trivial + human-debuggable.
func (s *Service) persist() {
	if s.Store == nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sb strings.Builder
	sb.WriteString(s.asOf.Format("2006-01-02"))
	for code, rate := range s.rates {
		sb.WriteString(";")
		sb.WriteString(code)
		sb.WriteString(":")
		sb.WriteString(strconv.FormatFloat(rate, 'f', 6, 64))
	}
	_ = s.Store.SaveConfig(stateKey, sb.String())
}

func (s *Service) parseCached(blob string) error {
	parts := strings.Split(blob, ";")
	if len(parts) < 2 {
		return fmt.Errorf("invalid cached rates blob")
	}
	asOf, err := time.Parse("2006-01-02", parts[0])
	if err != nil {
		return err
	}
	rates := map[string]float64{"EUR": 1.0}
	for _, p := range parts[1:] {
		kv := strings.SplitN(p, ":", 2)
		if len(kv) != 2 {
			continue
		}
		rate, err := strconv.ParseFloat(kv[1], 64)
		if err != nil {
			continue
		}
		rates[strings.ToUpper(kv[0])] = rate
	}
	s.mu.Lock()
	s.rates = rates
	s.asOf = asOf
	s.mu.Unlock()
	return nil
}
