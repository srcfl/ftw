package ocpp

import (
	"context"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// startManagedServer runs a 1.6 listener (and 2.0.1 when v201) where
// "garage" is named by a loadpoint and "metered" is listed only under
// ocpp.chargers.
func startManagedServer(t *testing.T, v201 bool) *Server {
	t.Helper()
	return startOCPP(t, Config{
		Enabled: true, Bind: "127.0.0.1", HeartbeatIntervalS: 60,
		ApprovedIDs: []string{"garage", "metered"},
		ManagedIDs:  []string{"garage"},
	}, telemetry.NewStore(), v201)
}

func awaitProfiles(t *testing.T, count func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for count() < want {
		if time.Now().After(deadline) {
			t.Fatalf("got %d charging profiles, want %d", count(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// settle gives a profile that should not come time to arrive.
func settle() { time.Sleep(300 * time.Millisecond) }

// A managed charger gets a 0 A TxDefaultProfile at boot, before any car, and
// FTW's first order uses the same id, purpose and stack level, so it replaces
// that profile instead of stacking under it.
func TestManagedChargerStartsAtZeroAmpsOnBoot(t *testing.T) {
	srv := startManagedServer(t, false)
	_, fake, _ := connectCharger(t, srv, srv.cfg.Port, "garage")
	awaitProfiles(t, fake.count, 1)

	fake.mu.Lock()
	boot := fake.profiles[0]
	fake.mu.Unlock()
	p := boot.ChargingProfile
	if boot.ConnectorId != allConnectors || p.ChargingProfilePurpose != types.ChargingProfilePurposeTxDefaultProfile ||
		p.ChargingProfileId != ftwProfileID || p.StackLevel != ftwStackLevel {
		t.Fatalf("boot profile: connector %d purpose %s id %d stack %d, want 0 TxDefaultProfile %d %d",
			boot.ConnectorId, p.ChargingProfilePurpose, p.ChargingProfileId, p.StackLevel, ftwProfileID, ftwStackLevel)
	}
	if got := fake.lastLimit(t); got != 0 {
		t.Fatalf("boot limit %v A, want 0 A", got)
	}

	if err := srv.Command(context.Background(), "garage", setCurrent11kW(t)); err != nil {
		t.Fatalf("order: %v", err)
	}
	fake.mu.Lock()
	order := fake.profiles[len(fake.profiles)-1]
	fake.mu.Unlock()
	o := order.ChargingProfile
	if order.ConnectorId != boot.ConnectorId || o.ChargingProfilePurpose != p.ChargingProfilePurpose ||
		o.ChargingProfileId != p.ChargingProfileId || o.StackLevel != p.StackLevel {
		t.Fatalf("FTW's order does not replace the boot profile: connector %d purpose %s id %d stack %d",
			order.ConnectorId, o.ChargingProfilePurpose, o.ChargingProfileId, o.StackLevel)
	}
	if got := fake.lastLimit(t); got != 16 {
		t.Fatalf("order limit %v A, want 16 A", got)
	}
}

// FTW never orders a charger no loadpoint names, so a 0 A start would hold
// it at 0 A for good. A pending charger is not FTW's to steer at all.
func TestUnmanagedChargersGetNoBootLimit(t *testing.T) {
	srv := startManagedServer(t, false)
	_, managed, _ := connectCharger(t, srv, srv.cfg.Port, "garage")
	_, metered, _ := connectCharger(t, srv, srv.cfg.Port, "metered")
	_, pending, _ := connectCharger(t, srv, srv.cfg.Port, "stranger")
	awaitProfiles(t, managed.count, 1)
	settle()
	if n := metered.count(); n != 0 {
		t.Errorf("charger listed only under ocpp.chargers got %d profiles, want 0", n)
	}
	if n := pending.count(); n != 0 {
		t.Errorf("pending charger got %d profiles, want 0", n)
	}
}

// A charger that refuses the start limit gets the connector-1 retry every
// order gets, and then nothing more until it boots again.
func TestRefusedBootLimitIsNotRetried(t *testing.T) {
	srv := startManagedServer(t, false)
	fake := newFakeCharger()
	fake.setStatus(smartcharging.ChargingProfileStatusRejected)
	cp, _, err := dialChargePoint16(t, wsURL(srv.cfg.Port), "garage", nil, func(cp ocpp16.ChargePoint) { cp.SetSmartChargingHandler(fake) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cp.BootNotification("Dawn", "Charge Amps"); err != nil {
		t.Fatal(err)
	}
	awaitProfiles(t, fake.count, 2)
	settle()
	if n := fake.count(); n != 2 {
		t.Fatalf("refused start limit sent %d times, want 2 (connector 0, then 1)", n)
	}
}

// A boot on a socket that already reported a car is not a fresh start: FTW
// orders that car every tick, and 0 A would only pause it.
func TestBootWithCarAlreadyReportedGetsNoBootLimit(t *testing.T) {
	srv := startManagedServer(t, false)
	fake := newFakeCharger()
	cp, _, err := dialChargePoint16(t, wsURL(srv.cfg.Port), "garage", nil, func(cp ocpp16.ChargePoint) { cp.SetSmartChargingHandler(fake) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cp.StatusNotification(1, core.NoError, core.ChargePointStatusCharging); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.BootNotification("Dawn", "Charge Amps"); err != nil {
		t.Fatal(err)
	}
	settle()
	if n := fake.count(); n != 0 {
		t.Fatalf("boot with a car on the socket got %d profiles, want 0", n)
	}
}

// reconnectAndBoot drops the charger's socket, waits for FTW to see it go,
// then dials again and sends a BootNotification before any status, as much
// firmware does after a Wi-Fi drop. It returns the new socket's charger.
func reconnectAndBoot(t *testing.T, srv *Server, stop func()) *fakeCharger {
	t.Helper()
	stop()
	deadline := time.Now().Add(2 * time.Second)
	for srv.Handler().IsOnline("garage") {
		if time.Now().After(deadline) {
			t.Fatal("FTW never saw the socket drop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	fake := newFakeCharger()
	cp, _, err := dialChargePoint16(t, wsURL(srv.cfg.Port), "garage", nil, func(cp ocpp16.ChargePoint) { cp.SetSmartChargingHandler(fake) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cp.BootNotification("Dawn", "Charge Amps"); err != nil {
		t.Fatal(err)
	}
	return fake
}

// A Wi-Fi drop mid-session: the charger had reported Charging and taken
// FTW's 16 A. It reconnects and boots before any status. The car is most
// likely still charging, so FTW keeps its live order rather than pausing it.
func TestReconnectBootMidSessionKeepsFTWOrder(t *testing.T) {
	srv := startManagedServer(t, false)
	cp, fake, stop := connectCharger(t, srv, srv.cfg.Port, "garage")
	awaitProfiles(t, fake.count, 1) // the real boot's 0 A
	if _, err := cp.StatusNotification(1, core.NoError, core.ChargePointStatusCharging); err != nil {
		t.Fatal(err)
	}
	if err := srv.Command(context.Background(), "garage", setCurrent11kW(t)); err != nil {
		t.Fatal(err)
	}

	after := reconnectAndBoot(t, srv, stop)
	settle()
	if n := after.count(); n != 0 {
		t.Fatalf("reconnect boot mid-session got %d profile(s), last %v A; want FTW's live 16 A kept", n, after.lastLimit(t))
	}
}

// The same reconnect after the car left: no car to pause, so the charger
// gets its start limit again.
func TestReconnectBootWithNoCarGetsBootLimit(t *testing.T) {
	srv := startManagedServer(t, false)
	cp, fake, stop := connectCharger(t, srv, srv.cfg.Port, "garage")
	awaitProfiles(t, fake.count, 1)
	if _, err := cp.StatusNotification(1, core.NoError, core.ChargePointStatusAvailable); err != nil {
		t.Fatal(err)
	}

	after := reconnectAndBoot(t, srv, stop)
	awaitProfiles(t, after.count, 1)
	if got := after.lastLimit(t); got != 0 {
		t.Fatalf("reconnect boot with no car: limit %v A, want 0 A", got)
	}
}

// After a Core restart the charger reconnects without booting, and FTW asks
// it for a BootNotification. That boot is FTW's own request, not a charger
// restart; the car may be charging on FTW's last order.
func TestBootAnsweringIdentityProbeGetsNoBootLimit(t *testing.T) {
	srv := startManagedServer(t, false)
	fake := newFakeCharger()
	requests := make(chan string, 10)
	cp, _, err := dialChargePoint16(t, wsURL(srv.cfg.Port), "garage", nil, func(cp ocpp16.ChargePoint) {
		cp.SetSmartChargingHandler(fake)
		cp.SetRemoteTriggerHandler(bootTrigger16{requested: requests})
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-requests:
	case <-time.After(3 * time.Second):
		t.Fatal("FTW never asked for a BootNotification")
	}
	if _, err := cp.BootNotification("Dawn", "Charge Amps"); err != nil {
		t.Fatal(err)
	}
	settle()
	if n := fake.count(); n != 0 {
		t.Fatalf("boot answering FTW's probe got %d profiles, want 0", n)
	}
}

// 2.0.1: the same 0 A TxDefaultProfile on EVSE 0, replaced by FTW's order.
func TestV201ManagedStationStartsAtZeroAmpsOnBoot(t *testing.T) {
	srv := startManagedServer(t, true)
	fake, _ := connectStationV201(t, srv, srv.cfg.PortV201, "garage")
	count := func() int { fake.mu.Lock(); defer fake.mu.Unlock(); return len(fake.profiles) }
	awaitProfiles(t, count, 1)

	fake.mu.Lock()
	boot := fake.profiles[0]
	fake.mu.Unlock()
	p := boot.ChargingProfile
	if boot.EvseID != allEVSEs || p.ChargingProfilePurpose != types201.ChargingProfilePurposeTxDefaultProfile ||
		p.ID != ftwProfileID || p.StackLevel != ftwStackLevel {
		t.Fatalf("boot profile: evse %d purpose %s id %d stack %d, want 0 TxDefaultProfile %d %d",
			boot.EvseID, p.ChargingProfilePurpose, p.ID, p.StackLevel, ftwProfileID, ftwStackLevel)
	}
	if got := fake.lastLimit(t); got != 0 {
		t.Fatalf("boot limit %v A, want 0 A", got)
	}
	if err := srv.Command(context.Background(), "garage", setCurrent11kW(t)); err != nil {
		t.Fatalf("order: %v", err)
	}
	if got := fake.lastLimit(t); got != 16 {
		t.Fatalf("order limit %v A, want 16 A", got)
	}
}
