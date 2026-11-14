package ocpp

import (
	"context"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	ocpp201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// startManagedServer runs a 1.6 listener (and 2.0.1 when v201) where
// "garage" is named by a loadpoint and "metered" is listed only under
// ocpp.chargers. The status burst after a boot settles in testBootLimitSettle,
// well inside settle(), so a start limit that should not come has time to.
func startManagedServer(t *testing.T, v201 bool) *Server {
	t.Helper()
	srv := startOCPP(t, Config{
		Enabled: true, Bind: "127.0.0.1", HeartbeatIntervalS: 60,
		ApprovedIDs: []string{"garage", "metered"},
		ManagedIDs:  []string{"garage"},
	}, telemetry.NewStore(), v201)
	srv.handler.mu.Lock()
	srv.handler.bootLimitSettle = testBootLimitSettle
	srv.handler.mu.Unlock()
	return srv
}

const testBootLimitSettle = 50 * time.Millisecond

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

// reportStatus16 sends what a 1.6 charger sends after an accepted boot: the
// status of connector 0 and of its connector (OCPP 1.6 §4.9).
func reportStatus16(t *testing.T, cp ocpp16.ChargePoint, status core.ChargePointStatus) {
	t.Helper()
	for connector, st := range []core.ChargePointStatus{core.ChargePointStatusAvailable, status} {
		if _, err := cp.StatusNotification(connector, core.NoError, st); err != nil {
			t.Fatal(err)
		}
	}
}

// A managed charger that boots and reports no car gets a 0 A
// TxDefaultProfile, and FTW's first order uses the same id, purpose and stack
// level, so it replaces that profile instead of stacking under it.
func TestManagedChargerStartsAtZeroAmpsOnBoot(t *testing.T) {
	srv := startManagedServer(t, false)
	cp, fake, _ := connectCharger(t, srv, srv.cfg.Port, "garage")
	settle()
	if n := fake.count(); n != 0 {
		t.Fatalf("boot alone sent %d profiles; want none before the charger reports its connector", n)
	}
	reportStatus16(t, cp, core.ChargePointStatusAvailable)
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
	cpManaged, managed, _ := connectCharger(t, srv, srv.cfg.Port, "garage")
	cpMetered, metered, _ := connectCharger(t, srv, srv.cfg.Port, "metered")
	cpPending, pending, _ := connectCharger(t, srv, srv.cfg.Port, "stranger")
	for _, cp := range []ocpp16.ChargePoint{cpManaged, cpMetered, cpPending} {
		reportStatus16(t, cp, core.ChargePointStatusAvailable)
	}
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
	reportStatus16(t, cp, core.ChargePointStatusAvailable)
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
func reconnectAndBoot(t *testing.T, srv *Server, stop func()) (ocpp16.ChargePoint, *fakeCharger) {
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
	return cp, fake
}

// A Wi-Fi drop mid-session: the charger had reported Charging and taken
// FTW's 16 A. It reconnects and boots before any status. The car is most
// likely still charging, so FTW keeps its live order rather than pausing it.
func TestReconnectBootMidSessionKeepsFTWOrder(t *testing.T) {
	srv := startManagedServer(t, false)
	cp, fake, stop := connectCharger(t, srv, srv.cfg.Port, "garage")
	reportStatus16(t, cp, core.ChargePointStatusAvailable)
	awaitProfiles(t, fake.count, 1) // the real boot's 0 A
	if _, err := cp.StatusNotification(1, core.NoError, core.ChargePointStatusCharging); err != nil {
		t.Fatal(err)
	}
	if err := srv.Command(context.Background(), "garage", setCurrent11kW(t)); err != nil {
		t.Fatal(err)
	}

	cpAfter, after := reconnectAndBoot(t, srv, stop)
	reportStatus16(t, cpAfter, core.ChargePointStatusCharging)
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
	reportStatus16(t, cp, core.ChargePointStatusAvailable)
	awaitProfiles(t, fake.count, 1)

	cpAfter, after := reconnectAndBoot(t, srv, stop)
	reportStatus16(t, cpAfter, core.ChargePointStatusAvailable)
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
	reportStatus16(t, cp, core.ChargePointStatusAvailable)
	settle()
	if n := fake.count(); n != 0 {
		t.Fatalf("boot answering FTW's probe got %d profiles, want 0", n)
	}
}

// 2.0.1: the same 0 A TxDefaultProfile on EVSE 0, replaced by FTW's order.
func TestV201ManagedStationStartsAtZeroAmpsOnBoot(t *testing.T) {
	srv := startManagedServer(t, true)
	cs, fake := bootStationV201(t, srv)
	count := func() int { fake.mu.Lock(); defer fake.mu.Unlock(); return len(fake.profiles) }
	if _, err := cs.StatusNotification(types201.NewDateTime(time.Now()), availability.ConnectorStatusAvailable, 1, 1); err != nil {
		t.Fatal(err)
	}
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

// Rig s7s-restart-short-25A on 964643e9: Core restarts during Charge now. The
// charger reconnects to the new process and sends its own BootNotification at
// once, before FTW's identity probe, then reports connector 0 Available and
// connector 1 Charging. The new process holds no status and no transaction, so
// the boot alone looks like a fresh start. FTW sent 0 A, the car went to
// SuspendedEVSE for a second, and "Charge now never stops" failed. The status
// after the boot shows the car, so no start limit goes out.
func TestCoreRestartBootMidSessionGetsNoBootLimit(t *testing.T) {
	srv := startManagedServer(t, false)
	fake := newFakeCharger()
	cp, _, err := dialChargePoint16(t, wsURL(srv.cfg.Port), "garage", nil, func(cp ocpp16.ChargePoint) { cp.SetSmartChargingHandler(fake) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cp.BootNotification("Rig11", "RigCo"); err != nil {
		t.Fatal(err)
	}
	reportStatus16(t, cp, core.ChargePointStatusCharging)
	settle()
	if n := fake.count(); n != 0 {
		t.Fatalf("boot after a Core restart with the car charging got %d profile(s), last %v A; want none",
			n, fake.lastLimit(t))
	}
}

// bootStationV201 dials "garage" on 2.0.1 and sends a BootNotification.
func bootStationV201(t *testing.T, srv *Server) (ocpp201.ChargingStation, *fakeStationV201) {
	t.Helper()
	fake := newFakeStationV201()
	cs, _, err := dialChargingStation201(t, wsURL(srv.cfg.PortV201), "garage", func(cs ocpp201.ChargingStation) { cs.SetSmartChargingHandler(fake) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.BootNotification(provisioning.BootReasonPowerUp, "Dawn", "Charge Amps"); err != nil {
		t.Fatal(err)
	}
	return cs, fake
}

// The s7s shape on 2.0.1: after a Core restart the station boots on its own
// and reports its connector Occupied. No start limit.
func TestV201CoreRestartBootWithCarGetsNoBootLimit(t *testing.T) {
	srv := startManagedServer(t, true)
	cs, fake := bootStationV201(t, srv)
	if _, err := cs.StatusNotification(types201.NewDateTime(time.Now()), availability.ConnectorStatusOccupied, 1, 1); err != nil {
		t.Fatal(err)
	}
	settle()
	fake.mu.Lock()
	n := len(fake.profiles)
	fake.mu.Unlock()
	if n != 0 {
		t.Fatalf("boot after a Core restart with a car got %d profile(s); want none", n)
	}
}

// A Core restart while a car charges on socket 2 of a two-socket 1.6
// charger. The charger boots on its own and reports socket 1 Available
// before socket 2 Charging. The start limit goes to connector 0, the whole
// charger, so sending it on socket 1's status would pause the car on socket
// 2. FTW waits for the status burst to settle; the car on socket 2 cancels it.
func TestDualSocketCoreRestartCarOnSocket2GetsNoBootLimit(t *testing.T) {
	srv := startManagedServer(t, false)
	fake := newFakeCharger()
	cp, _, err := dialChargePoint16(t, wsURL(srv.cfg.Port), "garage", nil, func(cp ocpp16.ChargePoint) { cp.SetSmartChargingHandler(fake) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cp.BootNotification("Twin", "RigCo"); err != nil {
		t.Fatal(err)
	}
	for connector, st := range []core.ChargePointStatus{core.ChargePointStatusAvailable, core.ChargePointStatusAvailable, core.ChargePointStatusCharging} {
		if _, err := cp.StatusNotification(connector, core.NoError, st); err != nil {
			t.Fatal(err)
		}
	}
	settle()
	if n := fake.count(); n != 0 {
		t.Fatalf("two-socket restart with a car on socket 2 got %d profile(s), last %v A; want none", n, fake.lastLimit(t))
	}
}

// The same charger reporting socket 2 first. FTW keeps one connection state
// per charger and the last status wins, so socket 1's later Available must
// not undo the car socket 2 showed during the burst.
func TestDualSocketCarReportedFirstStillCancelsBootLimit(t *testing.T) {
	srv := startManagedServer(t, false)
	fake := newFakeCharger()
	cp, _, err := dialChargePoint16(t, wsURL(srv.cfg.Port), "garage", nil, func(cp ocpp16.ChargePoint) { cp.SetSmartChargingHandler(fake) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cp.BootNotification("Twin", "RigCo"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		connector int
		status    core.ChargePointStatus
	}{{0, core.ChargePointStatusAvailable}, {2, core.ChargePointStatusCharging}, {1, core.ChargePointStatusAvailable}} {
		if _, err := cp.StatusNotification(c.connector, core.NoError, c.status); err != nil {
			t.Fatal(err)
		}
	}
	settle()
	if n := fake.count(); n != 0 {
		t.Fatalf("two-socket restart with a car on socket 2 reported first got %d profile(s); want none", n)
	}
}

// FTW orders the charger while the start limit waits for the statuses to
// settle. The order is newer, so the 0 A must not land after it.
func TestOrderDuringSettleWaitCancelsBootLimit(t *testing.T) {
	srv := startManagedServer(t, false)
	cp, fake, _ := connectCharger(t, srv, srv.cfg.Port, "garage")
	reportStatus16(t, cp, core.ChargePointStatusAvailable)
	if err := srv.Command(context.Background(), "garage", setCurrent11kW(t)); err != nil {
		t.Fatal(err)
	}
	settle()
	if n, got := fake.count(), fake.lastLimit(t); n != 1 || got != 16 {
		t.Fatalf("order during the settle wait: %d profile(s), last %v A; want FTW's 16 A alone", n, got)
	}
}

// 2.0.1 with two EVSEs: EVSE 1 Available, then EVSE 2 Occupied. The start
// limit goes to EVSE 0, the whole station, so it must not go out.
func TestV201TwoEVSECoreRestartCarOnEVSE2GetsNoBootLimit(t *testing.T) {
	srv := startManagedServer(t, true)
	cs, fake := bootStationV201(t, srv)
	if _, err := cs.StatusNotification(types201.NewDateTime(time.Now()), availability.ConnectorStatusAvailable, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.StatusNotification(types201.NewDateTime(time.Now()), availability.ConnectorStatusOccupied, 2, 1); err != nil {
		t.Fatal(err)
	}
	settle()
	fake.mu.Lock()
	n := len(fake.profiles)
	fake.mu.Unlock()
	if n != 0 {
		t.Fatalf("two-EVSE restart with a car on EVSE 2 got %d profile(s); want none", n)
	}
}

// A Core restart while a car charges on socket 2 of a two-socket 1.6
// charger, where socket 2's status comes only after the wait. Socket 1
// reports Available, then meter values for socket 2's car arrive inside the
// wait. They show the car, so the start limit must not go out to the whole
// charger. The meter values may name the transaction, or carry only power.
func TestMeterValuesShowingACarDuringWaitCancelBootLimit(t *testing.T) {
	tx := 7
	for _, c := range []struct {
		name  string
		txID  *int
		power string
	}{
		{"transaction and 7 kW", &tx, "7000"},
		{"transaction at 0 W", &tx, "0"},
		{"7 kW without transaction", nil, "7000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := startManagedServer(t, false)
			srv.handler.mu.Lock()
			srv.handler.bootLimitSettle = 200 * time.Millisecond
			srv.handler.mu.Unlock()
			cp, fake, _ := connectCharger(t, srv, srv.cfg.Port, "garage")
			reportStatus16(t, cp, core.ChargePointStatusAvailable)
			if _, err := cp.MeterValues(2, meterValues16(c.power), func(r *core.MeterValuesRequest) { r.TransactionId = c.txID }); err != nil {
				t.Fatal(err)
			}
			time.Sleep(400 * time.Millisecond)
			if n := fake.count(); n != 0 {
				t.Fatalf("car on socket 2 shown by meter values got %d profile(s), last %v A; want none", n, fake.lastLimit(t))
			}
		})
	}
}

// Meter values with no transaction and 0 W show no car: the start limit
// still goes out.
func TestIdleMeterValuesDuringWaitKeepBootLimit(t *testing.T) {
	srv := startManagedServer(t, false)
	cp, fake, _ := connectCharger(t, srv, srv.cfg.Port, "garage")
	reportStatus16(t, cp, core.ChargePointStatusAvailable)
	if _, err := cp.MeterValues(1, meterValues16("0")); err != nil {
		t.Fatal(err)
	}
	awaitProfiles(t, fake.count, 1)
	if got := fake.lastLimit(t); got != 0 {
		t.Fatalf("start limit after idle meter values = %v A; want 0", got)
	}
}

func meterValues16(powerW string) []types.MeterValue {
	return []types.MeterValue{{
		Timestamp: types.NewDateTime(time.Now()),
		SampledValue: []types.SampledValue{
			{Value: powerW, Measurand: types.MeasurandPowerActiveImport, Unit: types.UnitOfMeasureW},
			{Value: "12345", Measurand: types.MeasurandEnergyActiveImportRegister, Unit: types.UnitOfMeasureWh},
		},
	}}
}
