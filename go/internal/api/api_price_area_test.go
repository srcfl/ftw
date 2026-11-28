package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/prices"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A box whose price area was never chosen keeps its SE3 prices, and its
// status says so instead of letting the fallback pass unseen.
func TestStatusSaysWhenThePriceAreaIsNotSet(t *testing.T) {
	status := func(price *config.Price) map[string]any {
		t.Helper()
		srv := New(&Deps{
			Ctrl: control.NewState(0, 50, "meter"), CtrlMu: &sync.Mutex{}, Tel: telemetry.NewStore(),
			CapMu: &sync.RWMutex{}, CfgMu: &sync.RWMutex{}, Cfg: &config.Config{Price: price},
			Prices: prices.FromConfig(price, nil, nil),
		})
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	unset := status(&config.Price{Provider: "sourceful"})
	if unset["price_area_note"] != "Price area not set, using SE3" {
		t.Fatalf("price_area_note = %v", unset["price_area_note"])
	}
	for name, price := range map[string]*config.Price{
		"chosen area": {Provider: "sourceful", Zone: "SE4"},
		"prices off":  {Provider: "none"},
		"no section":  nil,
	} {
		if note, ok := status(price)["price_area_note"]; ok {
			t.Errorf("%s: unexpected note %v", name, note)
		}
	}
}
