package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ws"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/ocpp"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestOCPPApprovedIDsSkipLuaLoadpointNames(t *testing.T) {
	cfg := &config.Config{
		Drivers: []config.Driver{{Name: "easee", Lua: "drivers/easee.lua"}},
		Loadpoints: []config.Loadpoint{
			{ID: "cloud", DriverName: "easee"},
			{ID: "garage", DriverName: "garage"},
		},
		OCPP: &config.OCPP{
			Enabled:  true,
			Username: "ftw",
			Password: "shared-secret",
			Chargers: []config.OCPPCharger{{ID: "wallbox", Password: "own"}},
		},
	}
	got := ocppApprovedIDs(cfg)
	want := map[string]bool{"garage": true, "wallbox": true}
	if len(got) != len(want) {
		t.Fatalf("approved %v, want garage and wallbox only", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("approved unexpected id %q", id)
		}
	}
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestEaseeNamedLoadpointIsNotAnApprovedOCPPID(t *testing.T) {
	cfg := &config.Config{
		Drivers:    []config.Driver{{Name: "easee", Lua: "drivers/easee.lua"}},
		Loadpoints: []config.Loadpoint{{ID: "garage", DriverName: "easee"}},
		OCPP:       &config.OCPP{Enabled: true, Username: "ftw", Password: "shared-secret"},
	}
	if containsID(ocppApprovedIDs(cfg), "easee") {
		t.Fatal("Lua loadpoint name easee must not be an approved OCPP identity")
	}
}

type recordingLuaSend struct {
	mu    sync.Mutex
	calls int
}

func (r *recordingLuaSend) Send(context.Context, string, []byte) error {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return errors.New(`driver "easee" not found`)
}

func (r *recordingLuaSend) SendWithOutcome(_ context.Context, _ string, _ []byte, outcome func(error)) error {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	err := errors.New(`driver "easee" not found`)
	if outcome != nil {
		outcome(err)
	}
	return err
}

func (r *recordingLuaSend) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

type profileRecorder struct {
	mu       sync.Mutex
	profiles int
}

func (f *profileRecorder) OnSetChargingProfile(*smartcharging.SetChargingProfileRequest) (*smartcharging.SetChargingProfileConfirmation, error) {
	f.mu.Lock()
	f.profiles++
	f.mu.Unlock()
	return smartcharging.NewSetChargingProfileConfirmation(smartcharging.ChargingProfileStatusAccepted), nil
}

func (f *profileRecorder) OnClearChargingProfile(*smartcharging.ClearChargingProfileRequest) (*smartcharging.ClearChargingProfileConfirmation, error) {
	return nil, errors.New("not used")
}

func (f *profileRecorder) OnGetCompositeSchedule(*smartcharging.GetCompositeScheduleRequest) (*smartcharging.GetCompositeScheduleConfirmation, error) {
	return nil, errors.New("not used")
}

func (f *profileRecorder) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.profiles
}

func startTestOCPP(t *testing.T, cfg *ocpp.Config) (int, *ocpp.Server) {
	t.Helper()
	if cfg.Bind == "" {
		cfg.Bind = "127.0.0.1"
	}
	if cfg.Port == 0 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		cfg.Port = l.Addr().(*net.TCPAddr).Port
		l.Close()
	}
	srv, err := ocpp.Start(context.Background(), cfg, telemetry.NewStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	deadline := time.Now().Add(2 * time.Second)
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			c.Close()
			return cfg.Port, srv
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("ocpp listener did not bind on %s", addr)
	return 0, nil
}

func connectTestCharger(t *testing.T, port int, id, user, pass string) (*profileRecorder, func()) {
	t.Helper()
	fake := &profileRecorder{}
	client := ws.NewClient()
	if user != "" || pass != "" {
		client.SetBasicAuth(user, pass)
	}
	cp := ocpp16.NewChargePoint(id, nil, client)
	cp.SetSmartChargingHandler(fake)
	if err := cp.Start(fmt.Sprintf("ws://127.0.0.1:%d", port)); err != nil {
		t.Fatalf("connect %s: %v", id, err)
	}
	var once sync.Once
	stop := func() { once.Do(cp.Stop) }
	t.Cleanup(stop)
	if _, err := cp.BootNotification("Home", "Easee"); err != nil {
		t.Fatalf("boot %s: %v", id, err)
	}
	return fake, stop
}

func awaitOnline(t *testing.T, srv *ocpp.Server, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Handler().IsOnline(id) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never came online", id)
}

func TestLuaEaseeWebsocketStaysPendingAndDoesNotReceiveCurrent(t *testing.T) {
	site := &config.Config{
		Drivers:    []config.Driver{{Name: "easee", Lua: "drivers/easee.lua"}},
		Loadpoints: []config.Loadpoint{{ID: "garage", DriverName: "easee"}},
		OCPP:       &config.OCPP{Enabled: true, Username: "ftw", Password: "shared-secret"},
	}
	approved := ocppApprovedIDs(site)
	if containsID(approved, "easee") {
		t.Fatal("easee must not be approved")
	}
	port, srv := startTestOCPP(t, &ocpp.Config{
		Enabled:     true,
		Username:    "ftw",
		Password:    "shared-secret",
		ApprovedIDs: approved,
	})
	fake, _ := connectTestCharger(t, port, "easee", "ftw", "shared-secret")
	awaitOnline(t, srv, "easee")
	if !srv.Handler().Snapshot()["easee"].Pending {
		t.Fatal("websocket to /easee must stay pending")
	}

	lua := &recordingLuaSend{}
	router := newEVCommandRouter(srv, lua.Send, lua.SendWithOutcome, nil)
	payload, _ := json.Marshal(map[string]any{"action": "ev_set_current", "power_w": 4140, "voltage": 230.0, "site_phases": 3})
	if err := router.SendWithOutcome(context.Background(), "easee", payload, func(error) {}); err == nil {
		t.Fatal("impostor path must not succeed as OCPP")
	}
	if fake.count() != 0 {
		t.Fatal("pending /easee received ev_set_current")
	}
	if lua.n() == 0 {
		t.Fatal("command did not fall through to the Lua registry")
	}
}

func TestPeriodicDispatchWithOutcomeSenderCallsOCPPCommand(t *testing.T) {
	port, srv := startTestOCPP(t, &ocpp.Config{Enabled: true, ApprovedIDs: []string{"garage"}})
	fake, _ := connectTestCharger(t, port, "garage", "", "")
	awaitOnline(t, srv, "garage")

	lua := &recordingLuaSend{}
	router := newEVCommandRouter(srv, lua.Send, lua.SendWithOutcome, nil)

	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	cfg := loadpoint.Config{
		ID:            "garage",
		DriverName:    "garage",
		MinChargeW:    1400,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1400, 4140, 11000},
	}
	mgr := loadpoint.NewManager()
	mgr.Load([]loadpoint.Config{cfg})
	plan := loadpoint.PlanFunc(func(time.Time) (loadpoint.Directive, bool) {
		return loadpoint.Directive{
			SlotStart:         now.Add(-time.Second),
			SlotEnd:           now.Add(15 * time.Minute),
			LoadpointEnergyWh: map[string]float64{cfg.ID: 2750},
		}, true
	})
	tel := loadpoint.TelemetryFunc(func(string) (loadpoint.EVSample, bool) {
		return loadpoint.EVSample{Connected: true, RequestActive: true}, true
	})
	c := loadpoint.NewController(mgr, plan, tel, router.Send)
	c.SetOutcomeSender(router.SendWithOutcome)
	c.SetDriverOnline(func(name string) bool {
		return srv.Handler().IsOnline(name) && srv.Handler().IsApproved(name)
	})
	c.Tick(context.Background(), now)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fake.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if lua.n() != 0 {
		t.Fatal("periodic dispatch used Registry.Send instead of ocpp.Command")
	}
	if fake.count() == 0 {
		t.Fatal("controller with SetOutcomeSender did not call ocpp.Command")
	}

	profiles := fake.count()
	if err := router.SendCycle(context.Background(), "garage", []byte(`{"action":"ev_pause"}`), 1); err != nil {
		t.Fatal(err)
	}
	if lua.n() != 0 {
		t.Fatal("cycle sender used Registry.Send")
	}
	if fake.count() == profiles {
		t.Fatal("cycle sender did not call ocpp.Command")
	}
}
