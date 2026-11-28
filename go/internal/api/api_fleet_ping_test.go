package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/energyreport"
	"github.com/srcfl/ftw/go/internal/fleetping"
	"github.com/srcfl/ftw/go/internal/state"
)

type silentProvider string

func (silentProvider) Send(context.Context, fleetping.Payload) error { return nil }
func (p silentProvider) EndpointURL() string                         { return string(p) }

// fleetPingServer is a box with FTW Cloud on under the current terms, so it
// shares statistics.
func fleetPingServer(t *testing.T, cfg *config.Config) *Server {
	return fleetPingServerWith(t, cfg, config.DefaultFleetPingEndpoint, true)
}

func fleetPingServerAt(t *testing.T, cfg *config.Config, endpoint string) *Server {
	return fleetPingServerWith(t, cfg, endpoint, true)
}

// fleetPingServerWith is a box whose one statistics answer, which only FTW
// Cloud under the current terms makes true, is shares.
func fleetPingServerWith(t *testing.T, cfg *config.Config, endpoint string, shares bool) *Server {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pinger, err := fleetping.New(fleetping.Options{
		Enabled:  func() bool { return shares },
		Provider: silentProvider(endpoint),
		Facts: func() fleetping.Facts {
			return fleetping.FactsFromConfig(cfg, "v1.4.0",
				[]string{"sungrow", "easee_cloud"},
				time.Now().AddDate(0, -2, 0))
		},
	})
	if err != nil {
		t.Fatalf("fleetping.New: %v", err)
	}
	return New(&Deps{CfgMu: &sync.RWMutex{}, Cfg: cfg, FleetPing: pinger, State: st})
}

func TestFleetPingShowsTheRealPayload(t *testing.T) {
	// The Settings screen makes a claim about what leaves the house. It is
	// only checkable if this endpoint renders the message itself, built from
	// this box's own values.
	cfg := &config.Config{
		FleetPing: &config.FleetPing{Enabled: true, Endpoint: config.DefaultFleetPingEndpoint},
		Drivers: []config.Driver{
			{Name: "sungrow-vasagatan-12", Lua: "drivers/sungrow.lua", BatteryCapacityWh: 9600},
		},
		Price: &config.Price{Zone: "SE3"},
	}

	w := httptest.NewRecorder()
	fleetPingServer(t, cfg).Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fleet-ping", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Enabled  bool           `json:"enabled"`
		Endpoint string         `json:"endpoint"`
		FTWRelay bool           `json:"ftw_relay"`
		Payload  map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled || got.Endpoint != config.DefaultFleetPingEndpoint || !got.FTWRelay {
		t.Errorf("enabled=%v endpoint=%q ftw_relay=%v", got.Enabled, got.Endpoint, got.FTWRelay)
	}
	if got.Payload["price_zone"] != "SE3" || got.Payload["battery_kwh"] != "10" {
		t.Errorf("payload = %v, want this box's own values", got.Payload)
	}
	drivers, ok := got.Payload["drivers"].([]any)
	if !ok || len(drivers) != 1 || drivers[0] != "sungrow" {
		t.Errorf("drivers = %v, want the catalogue name, never the operator's", got.Payload["drivers"])
	}
}

func TestFleetPingStillShowsThePayloadWithoutFTWCloud(t *testing.T) {
	// Seeing the message while nothing is sent is most of the point:
	// somebody deciding whether to turn FTW Cloud on needs to read it first.
	cfg := &config.Config{
		FleetPing: &config.FleetPing{Enabled: true, Endpoint: config.DefaultFleetPingEndpoint},
		Price:     &config.Price{Zone: "SE3"},
	}

	w := httptest.NewRecorder()
	fleetPingServerWith(t, cfg, config.DefaultFleetPingEndpoint, false).Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fleet-ping", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Enabled bool           `json:"enabled"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled {
		t.Error("reported enabled on a box without FTW Cloud")
	}
	if got.Payload["schema"] != fleetping.Schema {
		t.Errorf("payload = %v, want the message it would send", got.Payload)
	}
}

func TestFleetPingShowsTheRunningEndpointUntilRestart(t *testing.T) {
	cfg := &config.Config{
		FleetPing: &config.FleetPing{Enabled: true, Endpoint: "https://saved.example.test/fleet"},
	}

	w := httptest.NewRecorder()
	fleetPingServer(t, cfg).Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fleet-ping", nil))

	var got fleetPingView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Endpoint != config.DefaultFleetPingEndpoint {
		t.Errorf("endpoint = %q, want the running provider %q", got.Endpoint, config.DefaultFleetPingEndpoint)
	}
	if !got.FTWRelay {
		t.Error("the active FTW relay endpoint was not marked as such")
	}
}

func TestFleetPingDoesNotApplyRelayPromisesToACustomCollector(t *testing.T) {
	cfg := &config.Config{FleetPing: &config.FleetPing{Enabled: true}}
	w := httptest.NewRecorder()
	fleetPingServerAt(t, cfg, "https://collector.example.test/fleet").Handler().ServeHTTP(
		w, httptest.NewRequest(http.MethodGet, "/api/fleet-ping", nil))

	var got fleetPingView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Endpoint != "https://collector.example.test/fleet" || got.FTWRelay {
		t.Errorf("endpoint=%q ftw_relay=%v", got.Endpoint, got.FTWRelay)
	}
}

func TestFleetPingSaysSoWhenItIsNotRunning(t *testing.T) {
	s := New(&Deps{CfgMu: &sync.RWMutex{}, Cfg: &config.Config{}})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fleet-ping", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
}

// dayLedger is one site meter that recorded every bucket of every day.
type dayLedger struct{}

func (dayLedger) EnergyAssets() ([]state.EnergyAsset, error) {
	return []state.EnergyAsset{{AssetID: "m/grid_meter", Kind: state.AssetGridMeter, FirstSeenMS: 0, LastSeenMS: 1 << 62}}, nil
}

func (dayLedger) LoadEnergyHistoryContext(_ context.Context, q state.EnergyHistoryQuery) ([]state.EnergyLedgerPoint, bool, error) {
	var out []state.EnergyLedgerPoint
	for at := q.SinceMS; at < q.UntilMS; at += state.EnergyLedgerBucketMS {
		out = append(out, state.EnergyLedgerPoint{Flow: state.FlowGridImport, BucketStartMS: at, BucketLenMS: state.EnergyLedgerBucketMS, EnergyWh: 50, Quality: "measured"})
	}
	return out, false, nil
}

// Settings shows the daily energy report beside the fleet report, built by
// the call its sender uses, and says it goes straight to FTW Cloud.
func TestFleetPingShowsTheEnergyReportItWouldSend(t *testing.T) {
	cfg := &config.Config{FleetPing: &config.FleetPing{Enabled: true}}
	srv := fleetPingServer(t, cfg)
	sender, err := energyreport.New(energyreport.Options{Ledger: dayLedger{}, Enabled: func() bool { return true },
		Now: func() time.Time { return time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	srv.deps.EnergyReport = sender

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fleet-ping", nil))
	var got struct {
		Energy struct {
			Endpoint string         `json:"endpoint"`
			Sending  bool           `json:"sending"`
			Day      string         `json:"day"`
			Payload  map[string]any `json:"payload"`
		} `json:"energy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var keys struct {
		Energy map[string]json.RawMessage `json:"energy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil || len(keys.Energy) != 4 {
		t.Fatalf("energy keys %v: %v", keys.Energy, err)
	}
	e := got.Energy
	if e.Endpoint != "https://cloud.ftw.energy/v1/energy" || !e.Sending || e.Day != "2026-10-23" || e.Payload["grid_import_kwh"] != 14.0 || e.Payload["date"] != "2026-10-23" || len(e.Payload) != 8 {
		t.Fatalf("energy = %+v", e)
	}
}
