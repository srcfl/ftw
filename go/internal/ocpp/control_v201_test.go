package ocpp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	ocpp201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	smartcharging201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// fakeStationV201 is a 2.0.1 charging station that records the charging
// profiles it is sent.
type fakeStationV201 struct {
	mu       sync.Mutex
	status   smartcharging201.ChargingProfileStatus
	profiles []*smartcharging201.SetChargingProfileRequest
}

func newFakeStationV201() *fakeStationV201 {
	return &fakeStationV201{status: smartcharging201.ChargingProfileStatusAccepted}
}

func (f *fakeStationV201) OnSetChargingProfile(req *smartcharging201.SetChargingProfileRequest) (*smartcharging201.SetChargingProfileResponse, error) {
	f.mu.Lock()
	f.profiles = append(f.profiles, req)
	status := f.status
	f.mu.Unlock()
	return smartcharging201.NewSetChargingProfileResponse(status), nil
}

func (f *fakeStationV201) OnClearChargingProfile(*smartcharging201.ClearChargingProfileRequest) (*smartcharging201.ClearChargingProfileResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeStationV201) OnGetChargingProfiles(*smartcharging201.GetChargingProfilesRequest) (*smartcharging201.GetChargingProfilesResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeStationV201) OnGetCompositeSchedule(*smartcharging201.GetCompositeScheduleRequest) (*smartcharging201.GetCompositeScheduleResponse, error) {
	return nil, errors.New("not used")
}

func (f *fakeStationV201) lastLimit(t *testing.T) float64 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.profiles) == 0 {
		t.Fatal("station received no charging profile")
	}
	p := f.profiles[len(f.profiles)-1]
	if p.ChargingProfile == nil {
		t.Fatal("request carried no charging profile")
	}
	if len(p.ChargingProfile.ChargingSchedule) == 0 {
		t.Fatal("charging profile carried no schedule")
	}
	periods := p.ChargingProfile.ChargingSchedule[0].ChargingSchedulePeriod
	if len(periods) == 0 {
		t.Fatal("charging schedule had no periods")
	}
	return periods[0].Limit
}

// startDualServer brings up both listeners on free ports.
func startDualServer(t *testing.T, tel *telemetry.Store) (portV16, portV201 int, srv *Server) {
	t.Helper()
	portV16 = freePort(t)
	portV201 = freePort(t)
	cfg := &Config{
		Enabled:            true,
		Bind:               "127.0.0.1",
		Port:               portV16,
		PortV201:           portV201,
		HeartbeatIntervalS: 60,
	}
	srv, err := Start(context.Background(), cfg, tel)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(srv.Stop)

	// Both listeners must be reachable before a client tries to connect.
	for _, port := range []int{portV16, portV201} {
		deadline := time.Now().Add(2 * time.Second)
		bound := false
		for time.Now().Before(deadline) {
			c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
			if err == nil {
				c.Close()
				bound = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !bound {
			t.Fatalf("listener never bound on port %d", port)
		}
	}
	return portV16, portV201, srv
}

// connectStationV201 brings up a 2.0.1 charging station and waits for the
// server to register it.
func connectStationV201(t *testing.T, srv *Server, port int, id string) (*fakeStationV201, func()) {
	t.Helper()
	fake := newFakeStationV201()
	cs := ocpp201.NewChargingStation(id, nil, nil)
	cs.SetSmartChargingHandler(fake)

	if err := cs.Start(fmt.Sprintf("ws://127.0.0.1:%d", port)); err != nil {
		t.Fatalf("charging station connect: %v", err)
	}
	var once sync.Once
	stop := func() { once.Do(cs.Stop) }
	t.Cleanup(stop)

	station := provisioning.ChargingStationType{Model: "Dawn", VendorName: "Charge Amps"}
	if _, err := cs.BootNotification(provisioning.BootReasonPowerUp, station.Model, station.VendorName); err != nil {
		t.Fatalf("boot: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Handler().IsOnline(id) {
			return fake, stop
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never registered station %s as online", id)
	return nil, nil
}

// A 2.0.1 station must be steerable exactly like a 1.6 one, and the server has
// to answer it in its own dialect.
func TestV201ChargerAcceptsCurrentLimit(t *testing.T) {
	_, portV201, srv := startDualServer(t, telemetry.NewStore())
	fake, _ := connectStationV201(t, srv, portV201, "garage-v201")

	if v, ok := srv.Handler().Version("garage-v201"); !ok || v != Version201 {
		t.Fatalf("version: got %q ok=%v, want %q", v, ok, Version201)
	}

	// 11040 W over 3 phases at 230 V = 16 A per phase.
	err := srv.Command(context.Background(), "garage-v201", mustPayload(t, map[string]any{
		"action":      "ev_set_current",
		"power_w":     11040.0,
		"voltage":     230.0,
		"site_phases": 3,
	}))
	if err != nil {
		t.Fatalf("set current: %v", err)
	}
	if got := fake.lastLimit(t); got != 16 {
		t.Errorf("limit: got %v A, want 16 A", got)
	}
}

// Pause is a 0 A limit on 2.0.1 too, not a remote stop.
func TestV201PauseSendsZeroAmpLimit(t *testing.T) {
	_, portV201, srv := startDualServer(t, telemetry.NewStore())
	fake, _ := connectStationV201(t, srv, portV201, "garage-v201")

	if err := srv.Command(context.Background(), "garage-v201", mustPayload(t, map[string]any{
		"action": "ev_pause",
	})); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := fake.lastLimit(t); got != 0 {
		t.Errorf("pause limit: got %v A, want 0 A", got)
	}
}

// Both dialects served at once, each charger steered in its own, sharing one
// charger map and one telemetry store.
func TestBothVersionsServedSimultaneously(t *testing.T) {
	portV16, portV201, srv := startDualServer(t, telemetry.NewStore())

	_, fake16, _ := connectCharger(t, srv, portV16, "garage-v16")
	fake201, _ := connectStationV201(t, srv, portV201, "garage-v201")

	if v, _ := srv.Handler().Version("garage-v16"); v != Version16 {
		t.Errorf("v16 charger version: got %q, want %q", v, Version16)
	}
	if v, _ := srv.Handler().Version("garage-v201"); v != Version201 {
		t.Errorf("v201 charger version: got %q, want %q", v, Version201)
	}

	ctx := context.Background()
	payload := mustPayload(t, map[string]any{
		"action":      "ev_set_current",
		"power_w":     6900.0, // 10 A over 3 phases at 230 V
		"voltage":     230.0,
		"site_phases": 3,
	})
	if err := srv.Command(ctx, "garage-v16", payload); err != nil {
		t.Fatalf("v16 set current: %v", err)
	}
	if err := srv.Command(ctx, "garage-v201", payload); err != nil {
		t.Fatalf("v201 set current: %v", err)
	}

	if got := fake16.lastLimit(t); got != 10 {
		t.Errorf("v16 limit: got %v A, want 10 A", got)
	}
	if got := fake201.lastLimit(t); got != 10 {
		t.Errorf("v201 limit: got %v A, want 10 A", got)
	}

	// The two must not have been confused for one another.
	snap := srv.Handler().Snapshot()
	if len(snap) != 2 {
		t.Errorf("expected 2 chargers in the snapshot, got %d: %+v", len(snap), snap)
	}
}

// A rejection from a 2.0.1 station has to surface as an error, same as 1.6.
func TestV201RejectedProfileIsAnError(t *testing.T) {
	_, portV201, srv := startDualServer(t, telemetry.NewStore())
	fake, _ := connectStationV201(t, srv, portV201, "garage-v201")

	fake.mu.Lock()
	fake.status = smartcharging201.ChargingProfileStatusRejected
	fake.mu.Unlock()

	err := srv.Command(context.Background(), "garage-v201", mustPayload(t, map[string]any{
		"action": "ev_pause",
	}))
	if err == nil {
		t.Fatal("expected an error when the station rejects the profile")
	}
}

// Guard the version-neutral core: a 2.0.1 profile must be a single-entry
// schedule list with amps as the rate unit, matching the 1.6 request's meaning.
func TestV201ProfileShape(t *testing.T) {
	_, portV201, srv := startDualServer(t, telemetry.NewStore())
	fake, _ := connectStationV201(t, srv, portV201, "garage-v201")

	if err := srv.Command(context.Background(), "garage-v201", mustPayload(t, map[string]any{
		"action":      "ev_set_current",
		"power_w":     11040.0,
		"voltage":     230.0,
		"site_phases": 3,
	})); err != nil {
		t.Fatalf("set current: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	p := fake.profiles[len(fake.profiles)-1].ChargingProfile
	if len(p.ChargingSchedule) != 1 {
		t.Fatalf("schedules: got %d, want exactly 1", len(p.ChargingSchedule))
	}
	if unit := p.ChargingSchedule[0].ChargingRateUnit; unit != types201.ChargingRateUnitAmperes {
		t.Errorf("rate unit: got %q, want %q", unit, types201.ChargingRateUnitAmperes)
	}
	if p.ChargingProfilePurpose != types201.ChargingProfilePurposeTxDefaultProfile {
		t.Errorf("purpose: got %q, want TxDefaultProfile", p.ChargingProfilePurpose)
	}
}
