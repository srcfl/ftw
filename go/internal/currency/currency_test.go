package currency

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

const sampleXML = `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
  <gesmes:subject>Reference rates</gesmes:subject>
  <gesmes:Sender><gesmes:name>European Central Bank</gesmes:name></gesmes:Sender>
  <Cube>
    <Cube time="2026-04-14">
      <Cube currency="USD" rate="1.0742"/>
      <Cube currency="SEK" rate="11.4253"/>
      <Cube currency="NOK" rate="11.6841"/>
      <Cube currency="DKK" rate="7.4591"/>
      <Cube currency="GBP" rate="0.8612"/>
      <Cube currency="CHF" rate="0.9823"/>
      <Cube currency="PLN" rate="4.3217"/>
      <Cube currency="CZK" rate="25.233"/>
      <Cube currency="HUF" rate="396.47"/>
      <Cube currency="JPY" rate="164.81"/>
    </Cube>
  </Cube>
</gesmes:Envelope>`

func TestParseECBXML(t *testing.T) {
	var env ecbEnvelope
	if err := parseECB([]byte(sampleXML), &env); err != nil {
		t.Fatal(err)
	}
	if env.Cube.Cube.Time != "2026-04-14" {
		t.Errorf("time: %s", env.Cube.Cube.Time)
	}
	found := map[string]float64{}
	for _, r := range env.Cube.Cube.Rates {
		found[r.Currency] = r.Rate
	}
	if found["SEK"] != 11.4253 {
		t.Errorf("SEK rate: %f", found["SEK"])
	}
}

func TestConvert(t *testing.T) {
	s := &Service{rates: map[string]float64{"EUR": 1, "SEK": 11.4, "USD": 1.08}}
	// 100 EUR → 1140 SEK
	got, ok := s.Convert(100, "EUR", "SEK")
	if !ok || got != 1140 {
		t.Errorf("EUR→SEK: %f ok=%v", got, ok)
	}
	// 1140 SEK → ~100 EUR
	got, ok = s.Convert(1140, "SEK", "EUR")
	if !ok || got < 99.9 || got > 100.1 {
		t.Errorf("SEK→EUR: %f", got)
	}
	// SEK → USD via EUR: 1140 SEK ≈ 100 EUR ≈ 108 USD
	got, ok = s.Convert(1140, "SEK", "USD")
	if !ok || got < 107.5 || got > 108.5 {
		t.Errorf("SEK→USD: %f", got)
	}
	// Same currency: identity
	got, ok = s.Convert(100, "SEK", "SEK")
	if !ok || got != 100 {
		t.Errorf("same currency: %f", got)
	}
	// Unknown currency
	_, ok = s.Convert(100, "SEK", "XYZ")
	if ok {
		t.Errorf("unknown currency should return ok=false")
	}
}

func TestConvertRejectsInvalidRatesAndResults(t *testing.T) {
	s := &Service{rates: map[string]float64{
		"EUR":  1,
		"ZERO": 0,
		"NEG":  -1,
		"NAN":  math.NaN(),
		"INF":  math.Inf(1),
		"TINY": math.SmallestNonzeroFloat64,
		"HUGE": math.MaxFloat64,
	}}

	for _, code := range []string{"ZERO", "NEG", "NAN", "INF"} {
		if _, ok := s.Convert(100, code, "EUR"); ok {
			t.Errorf("invalid source rate %s was accepted", code)
		}
		if _, ok := s.Convert(100, "EUR", code); ok {
			t.Errorf("invalid target rate %s was accepted", code)
		}
	}
	if _, ok := s.Convert(math.NaN(), "EUR", "EUR"); ok {
		t.Error("non-finite amount was accepted for an identity conversion")
	}
	if _, ok := s.Convert(math.MaxFloat64, "TINY", "HUGE"); ok {
		t.Error("overflowing conversion result was accepted")
	}
}

func TestCachedBlobRejectsInvalidRates(t *testing.T) {
	s := New(nil)
	if err := s.parseCached("2026-04-14;SEK:11.4;ZERO:0;NEG:-1;NAN:NaN;INF:+Inf;EUR:2"); err != nil {
		t.Fatal(err)
	}

	if rate, ok := s.Rate("SEK"); !ok || rate != 11.4 {
		t.Fatalf("valid cached rate = %g, %v; want 11.4, true", rate, ok)
	}
	for _, code := range []string{"ZERO", "NEG", "NAN", "INF"} {
		if rate, ok := s.Rate(code); ok {
			t.Errorf("invalid cached rate %s = %g was restored", code, rate)
		}
	}
	if rate, ok := s.Rate("EUR"); !ok || rate != 1 {
		t.Errorf("cached EUR override changed fixed rate: got %g, %v", rate, ok)
	}
}

func TestCachedBlobNeedsOneUsableRate(t *testing.T) {
	s := New(nil)
	if err := s.parseCached("2026-04-14;SEK:0;USD:NaN"); err == nil {
		t.Fatal("cache with no usable non-EUR rates was accepted")
	}
}

func TestCachedBlobRoundtrip(t *testing.T) {
	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	s := New(st)
	s.rates = map[string]float64{"EUR": 1, "SEK": 11.4, "USD": 1.08}
	s.persist()
	// new service, restore from cache
	s2 := New(st)
	if js, ok := st.LoadConfig(stateKey); ok {
		if err := s2.parseCached(js); err != nil {
			t.Fatal(err)
		}
	}
	r, ok := s2.Rate("SEK")
	if !ok || r != 11.4 {
		t.Errorf("restored SEK: %f ok=%v", r, ok)
	}
}

func TestLiveNoStarts(t *testing.T) {
	// Start/Stop should not crash even with no state store.
	s := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately
	s.Start(ctx)
	s.Stop()
}
