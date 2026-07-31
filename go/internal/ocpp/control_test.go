package ocpp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// fakeCharger is a charge point that records the charging profiles it is sent
// and answers with a configurable status.
type fakeCharger struct {
	mu       sync.Mutex
	status   smartcharging.ChargingProfileStatus
	profiles []*smartcharging.SetChargingProfileRequest
}

func newFakeCharger() *fakeCharger {
	return &fakeCharger{status: smartcharging.ChargingProfileStatusAccepted}
}

func (f *fakeCharger) OnSetChargingProfile(req *smartcharging.SetChargingProfileRequest) (*smartcharging.SetChargingProfileConfirmation, error) {
	f.mu.Lock()
	f.profiles = append(f.profiles, req)
	status := f.status
	f.mu.Unlock()
	return smartcharging.NewSetChargingProfileConfirmation(status), nil
}

func (f *fakeCharger) OnClearChargingProfile(*smartcharging.ClearChargingProfileRequest) (*smartcharging.ClearChargingProfileConfirmation, error) {
	return nil, errors.New("not used")
}

func (f *fakeCharger) OnGetCompositeSchedule(*smartcharging.GetCompositeScheduleRequest) (*smartcharging.GetCompositeScheduleConfirmation, error) {
	return nil, errors.New("not used")
}

func (f *fakeCharger) setStatus(s smartcharging.ChargingProfileStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
}

// lastLimit returns the amp limit from the most recent profile received.
func (f *fakeCharger) lastLimit(t *testing.T) float64 {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.profiles) == 0 {
		t.Fatal("charger received no charging profile")
	}
	p := f.profiles[len(f.profiles)-1]
	if p.ChargingProfile == nil || p.ChargingProfile.ChargingSchedule == nil {
		t.Fatal("charging profile had no schedule")
	}
	periods := p.ChargingProfile.ChargingSchedule.ChargingSchedulePeriod
	if len(periods) == 0 {
		t.Fatal("charging schedule had no periods")
	}
	return periods[0].Limit
}

// lastNumberPhases returns the declared phase count from the most recent
// profile, or nil when the charger was left to decide.
func (f *fakeCharger) lastNumberPhases(t *testing.T) *int {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.profiles) == 0 {
		t.Fatal("charger received no charging profile")
	}
	p := f.profiles[len(f.profiles)-1]
	periods := p.ChargingProfile.ChargingSchedule.ChargingSchedulePeriod
	if len(periods) == 0 {
		t.Fatal("charging schedule had no periods")
	}
	return periods[0].NumberPhases
}

func (f *fakeCharger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.profiles)
}

// connectCharger brings up a charge point against the server and waits until
// the server has registered the session.
//
// The returned stop is idempotent: ocpp-go panics on a second Stop, and the
// cleanup below would otherwise fire after a test that disconnects on purpose.
func connectCharger(t *testing.T, srv *Server, port int, id string) (ocpp16.ChargePoint, *fakeCharger, func()) {
	t.Helper()
	fake := newFakeCharger()
	cp := ocpp16.NewChargePoint(id, nil, nil)
	cp.SetSmartChargingHandler(fake)

	if err := cp.Start(fmt.Sprintf("ws://127.0.0.1:%d", port)); err != nil {
		t.Fatalf("charge point connect: %v", err)
	}
	var once sync.Once
	stop := func() { once.Do(cp.Stop) }
	t.Cleanup(stop)

	if _, err := cp.BootNotification("Dawn", "Charge Amps"); err != nil {
		t.Fatalf("boot: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Handler().IsOnline(id) {
			return cp, fake, stop
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never registered charger %s as online", id)
	return nil, nil, nil
}

func mustPayload(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// The whole point of Phase 2: a pause has to arrive as a 0 A limit, not as a
// RemoteStopTransaction, because Charge Amps units resume on their own after a
// remote stop.
func TestPauseSendsZeroAmpLimit(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, fake, _ := connectCharger(t, srv, port, "garage-left")

	err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
		"action": "ev_pause",
	}))
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := fake.lastLimit(t); got != 0 {
		t.Errorf("pause limit: got %v A, want 0 A", got)
	}
}

func TestSetCurrentConvertsPowerToAmps(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, fake, _ := connectCharger(t, srv, port, "garage-left")

	// 11040 W over 3 phases at 230 V = 16 A per phase.
	err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
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

// Below the IEC 61851 minimum the charger must be told zero, never a rounded-up
// 6 A the site fuse was not asked to carry.
func TestBelowMinimumBecomesZero(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, fake, _ := connectCharger(t, srv, port, "garage-left")

	// 2000 W over 3 phases at 230 V ≈ 2.9 A per phase — under the minimum.
	err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
		"action":      "ev_set_current",
		"power_w":     2000.0,
		"voltage":     230.0,
		"site_phases": 3,
	}))
	if err != nil {
		t.Fatalf("set current: %v", err)
	}
	if got := fake.lastLimit(t); got != 0 {
		t.Errorf("sub-minimum limit: got %v A, want 0 A", got)
	}
}

func TestMaxAmpsPerPhaseIsRespected(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, fake, _ := connectCharger(t, srv, port, "garage-left")

	// 22 kW would be 32 A per phase, but the loadpoint allows only 16 A.
	err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
		"action":             "ev_set_current",
		"power_w":            22080.0,
		"voltage":            230.0,
		"site_phases":        3,
		"max_amps_per_phase": 16.0,
	}))
	if err != nil {
		t.Fatalf("set current: %v", err)
	}
	if got := fake.lastLimit(t); got != 16 {
		t.Errorf("clamped limit: got %v A, want 16 A", got)
	}
}

// A pause must not lose the previous rate: resuming without one restores it.
func TestResumeRestoresLastLimit(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, fake, _ := connectCharger(t, srv, port, "garage-left")

	ctx := context.Background()
	if err := srv.Command(ctx, "garage-left", mustPayload(t, map[string]any{
		"action":      "ev_set_current",
		"power_w":     6900.0, // 10 A over 3 phases at 230 V
		"voltage":     230.0,
		"site_phases": 3,
	})); err != nil {
		t.Fatalf("set current: %v", err)
	}
	if err := srv.Command(ctx, "garage-left", mustPayload(t, map[string]any{
		"action": "ev_pause",
	})); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := fake.lastLimit(t); got != 0 {
		t.Fatalf("pause limit: got %v A, want 0 A", got)
	}

	if err := srv.Command(ctx, "garage-left", mustPayload(t, map[string]any{
		"action": "ev_resume",
	})); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := fake.lastLimit(t); got != 10 {
		t.Errorf("resumed limit: got %v A, want the previous 10 A", got)
	}
}

func TestRejectedProfileIsAnError(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, fake, _ := connectCharger(t, srv, port, "garage-left")
	fake.setStatus(smartcharging.ChargingProfileStatusRejected)

	err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
		"action": "ev_pause",
	}))
	if err == nil {
		t.Fatal("expected an error when the charger rejects the profile")
	}
}

func TestUnknownChargerIsNotConnected(t *testing.T) {
	_, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()

	err := srv.Command(context.Background(), "nobody-home", mustPayload(t, map[string]any{
		"action": "ev_pause",
	}))
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("expected ErrNotConnected, got %v", err)
	}
}

// A charger that drops off must not silently swallow commands — control needs
// the error so it can fall back.
func TestDisconnectedChargerRejectsCommands(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, _, stop := connectCharger(t, srv, port, "garage-left")

	stop()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && srv.Handler().IsOnline("garage-left") {
		time.Sleep(20 * time.Millisecond)
	}

	err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
		"action": "ev_pause",
	}))
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("expected ErrNotConnected after disconnect, got %v", err)
	}
}

// init/deinit are Lua lifecycle hooks. They must be accepted and do nothing
// rather than reaching the charger or erroring.
func TestLifecycleActionsAreNoOps(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, fake, _ := connectCharger(t, srv, port, "garage-left")

	for _, action := range []string{"init", "deinit"} {
		if err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
			"action": action,
		})); err != nil {
			t.Errorf("%s: unexpected error %v", action, err)
		}
	}
	if n := fake.count(); n != 0 {
		t.Errorf("lifecycle actions sent %d profiles, want 0", n)
	}
}

func TestUnknownActionIsAnError(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, fake, _ := connectCharger(t, srv, port, "garage-left")

	err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
		"action": "self_destruct",
	}))
	if err == nil {
		t.Fatal("expected an error for an unknown action")
	}
	if n := fake.count(); n != 0 {
		t.Errorf("unknown action sent %d profiles, want 0", n)
	}
}

// Single-phase mode changes the conversion: the same watts land on one phase.
func TestSinglePhaseModeUsesOnePhase(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, fake, _ := connectCharger(t, srv, port, "garage-left")

	// 3680 W on one phase at 230 V = 16 A.
	err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
		"action":      "ev_set_current",
		"power_w":     3680.0,
		"voltage":     230.0,
		"site_phases": 3,
		"phase_mode":  "1p",
	}))
	if err != nil {
		t.Fatalf("set current: %v", err)
	}
	if got := fake.lastLimit(t); got != 16 {
		t.Errorf("1p limit: got %v A, want 16 A", got)
	}
	if got := fake.lastNumberPhases(t); got == nil || *got != 1 {
		t.Errorf("1p numberPhases: got %v, want 1", got)
	}
}

// With no phase mode pinned, the phase count is left unset so a charger that
// can switch phases keeps deciding for itself.
func TestThreePhaseLeavesPhaseCountUnset(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	_, fake, _ := connectCharger(t, srv, port, "garage-left")

	err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
		"action":      "ev_set_current",
		"power_w":     11040.0,
		"voltage":     230.0,
		"site_phases": 3,
	}))
	if err != nil {
		t.Fatalf("set current: %v", err)
	}
	if got := fake.lastNumberPhases(t); got != nil {
		t.Errorf("numberPhases: got %v, want unset", *got)
	}
}

// The status handler marks a charger connected; a suspended charger is still
// commandable, which is what lets a paused session be resumed.
func TestSuspendedChargerStillAcceptsCommands(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore())
	defer srv.Stop()
	cp, fake, _ := connectCharger(t, srv, port, "garage-left")

	if _, err := cp.StatusNotification(1, core.NoError, core.ChargePointStatusSuspendedEV); err != nil {
		t.Fatalf("status: %v", err)
	}

	if err := srv.Command(context.Background(), "garage-left", mustPayload(t, map[string]any{
		"action":      "ev_set_current",
		"power_w":     6900.0,
		"voltage":     230.0,
		"site_phases": 3,
	})); err != nil {
		t.Fatalf("set current on suspended charger: %v", err)
	}
	if got := fake.lastLimit(t); got != 10 {
		t.Errorf("limit: got %v A, want 10 A", got)
	}
}
