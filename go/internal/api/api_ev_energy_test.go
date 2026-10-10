package api

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestEVEnergyAPIReportsBothWindowsCostAndPartialPrices(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Both hours sit in the past so a bucket cannot fall outside "now".
	end := time.Now().Add(-3 * time.Hour).UnixMilli()
	end = end / state.EnergyLedgerBucketMS * state.EnergyLedgerBucketMS
	start := end - time.Hour.Milliseconds()
	later := end + time.Hour.Milliseconds()
	tick := func(at int64, obs []state.EnergyObservation) {
		t.Helper()
		if err := st.RecordTickWithEnergy(state.HistoryPoint{TsMs: at, JSON: "{}"}, nil, obs); err != nil {
			t.Fatal(err)
		}
	}
	obs := func(asset, label string, kind state.EnergyAssetKind, flow state.EnergyFlow, at int64, counter float64) state.EnergyObservation {
		c := counter
		return state.EnergyObservation{
			AssetID: asset, DeviceID: asset, AssetKind: kind, Label: label,
			Flow: flow, AtMs: at, CounterWh: &c,
		}
	}
	tick(start, []state.EnergyObservation{
		obs("charger/garage", "easee", state.AssetVehicleCharger, state.FlowVehicleCharge, start, 0),
		obs("charger/driveway", "zaptec", state.AssetVehicleCharger, state.FlowVehicleCharge, start, 0),
		obs("site/meter", "meter", state.AssetGridMeter, state.FlowGridImport, start, 0),
		obs("site/load", "load", state.AssetObservedConsumer, state.FlowConsumerUse, start, 0),
	})
	tick(end, []state.EnergyObservation{
		obs("charger/garage", "easee", state.AssetVehicleCharger, state.FlowVehicleCharge, end, 6000),
		obs("charger/driveway", "zaptec", state.AssetVehicleCharger, state.FlowVehicleCharge, end, 2000),
		obs("site/meter", "meter", state.AssetGridMeter, state.FlowGridImport, end, 4000),
		obs("site/load", "load", state.AssetObservedConsumer, state.FlowConsumerUse, end, 2000),
	})
	// A later hour of garage energy with no stored price.
	tick(later, []state.EnergyObservation{
		obs("charger/garage", "easee", state.AssetVehicleCharger, state.FlowVehicleCharge, later, 7000),
	})
	if err := st.SavePrices([]state.PricePoint{{
		Zone: "SE3", SlotTsMs: start, SlotLenMin: 60,
		SpotOreKwh: 80, TotalOreKwh: 200, Source: "test", FetchedAtMs: start,
	}}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Price:      &config.Price{Zone: "SE3", Currency: "EUR"},
		Loadpoints: []config.Loadpoint{{ID: "garage", DriverName: "easee"}},
	}
	var mu sync.RWMutex
	srv := New(&Deps{State: st, Cfg: cfg, CfgMu: &mu})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/ev/energy", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Currency string `json:"currency"`
		Window   string `json:"window"`
		Windows  []struct {
			ID          string  `json:"id"`
			EnergyWh    float64 `json:"energy_wh"`
			CostOre     float64 `json:"cost_ore"`
			UnpricedWh  float64 `json:"unpriced_wh"`
			CostPartial bool    `json:"cost_partial"`
			Attribution string  `json:"attribution"`
			Daily       []struct {
				Day string `json:"day"`
			} `json:"daily"`
			Chargers []struct {
				Label       string  `json:"label"`
				LoadpointID string  `json:"loadpoint_id"`
				EnergyWh    float64 `json:"energy_wh"`
			} `json:"chargers"`
		} `json:"windows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Currency != "EUR" || body.Window != "rolling_local_days" || len(body.Windows) != 2 {
		t.Fatalf("header %+v", body)
	}
	if body.Windows[0].ID != "7d" || body.Windows[1].ID != "30d" {
		t.Fatalf("windows %s %s", body.Windows[0].ID, body.Windows[1].ID)
	}
	if len(body.Windows[0].Daily) != 7 || len(body.Windows[1].Daily) != 30 {
		t.Fatalf("daily lengths %d %d", len(body.Windows[0].Daily), len(body.Windows[1].Daily))
	}
	for _, w := range body.Windows {
		if math.Abs(w.EnergyWh-9000) > 1e-3 {
			t.Fatalf("%s energy %v", w.ID, w.EnergyWh)
		}
		if math.Abs(w.CostOre-640) > 1e-2 {
			t.Fatalf("%s cost %v", w.ID, w.CostOre)
		}
		if math.Abs(w.UnpricedWh-1000) > 1e-2 || !w.CostPartial {
			t.Fatalf("%s partial %+v", w.ID, w)
		}
		if w.Attribution != state.EVAttributionMixed {
			t.Fatalf("%s attribution %s", w.ID, w.Attribution)
		}
		if len(w.Chargers) != 2 {
			t.Fatalf("%s chargers %+v", w.ID, w.Chargers)
		}
		var garage, driveway bool
		for _, c := range w.Chargers {
			switch c.Label {
			case "easee":
				garage = c.LoadpointID == "garage" && math.Abs(c.EnergyWh-7000) < 1e-3
			case "zaptec":
				driveway = c.LoadpointID == "" && math.Abs(c.EnergyWh-2000) < 1e-3
			}
		}
		if !garage || !driveway {
			t.Fatalf("%s charger binding %+v", w.ID, w.Chargers)
		}
	}
}

func TestEVEnergyAPIWithoutStateIsEmpty(t *testing.T) {
	srv := New(&Deps{})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/ev/energy", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d", rr.Code)
	}
	var body struct {
		Currency string `json:"currency"`
		Windows  []any  `json:"windows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Currency != "SEK" || len(body.Windows) != 0 {
		t.Fatalf("body %+v", body)
	}
}
