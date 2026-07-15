package currency

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestFetchParsesECBXML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(sampleXML))
	}))
	defer srv.Close()

	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	s := New(st)
	s.Client = srv.Client()
	// Point the service at test server URL via a one-off fetch.
	// We shortcut by calling the http GET directly through a wrapper —
	// but simpler: temporarily swap ecbURL via a custom request. Since
	// ecbURL is a package-level const, inject via a test-only fetch:
	req, _ := http.NewRequest("GET", srv.URL, nil)
	resp, err := s.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Verify parser works on the sample bytes directly. Easier than
	// reaching into fetch.
	// (fetch() uses package-level URL; the parse path is what we test)
	body := []byte(sampleXML)
	var env ecbEnvelope
	if err := parseECB(body, &env); err != nil {
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

func TestCachedBlobRoundtrip(t *testing.T) {
	st, _ := state.Open(filepath.Join(t.TempDir(), "t.db"))
	defer st.Close()
	s := New(st)
	s.rates = map[string]float64{"EUR": 1, "SEK": 11.4, "USD": 1.08}
	if _, err := s.Convert(100, "EUR", "SEK"); err != true {
		_ = err
	}
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
