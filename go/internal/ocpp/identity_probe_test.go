package ocpp

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/remotetrigger"
	ocpp201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/remotecontrol"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

type bootTrigger16 struct {
	requested   chan string
	unsupported bool
}

func (h bootTrigger16) OnTriggerMessage(req *remotetrigger.TriggerMessageRequest) (*remotetrigger.TriggerMessageConfirmation, error) {
	h.requested <- string(req.RequestedMessage)
	status := remotetrigger.TriggerMessageStatusAccepted
	if h.unsupported {
		status = remotetrigger.TriggerMessageStatusNotImplemented
	}
	return remotetrigger.NewTriggerMessageConfirmation(status), nil
}

type bootTrigger201 struct {
	// Other remote-control requests are not used by this charger fixture.
	remotecontrol.ChargingStationHandler
	requested   chan string
	unsupported bool
}

func (h bootTrigger201) OnTriggerMessage(req *remotecontrol.TriggerMessageRequest) (*remotecontrol.TriggerMessageResponse, error) {
	h.requested <- string(req.RequestedMessage)
	status := remotecontrol.TriggerMessageStatusAccepted
	if h.unsupported {
		status = remotecontrol.TriggerMessageStatusNotImplemented
	}
	return remotecontrol.NewTriggerMessageResponse(status), nil
}

func connectIdentityCharger(t *testing.T, version Version, port int, unsupported bool) (stop func(), boot func(string) error, requested <-chan string) {
	t.Helper()
	requests := make(chan string, 10)
	url := fmt.Sprintf("ws://127.0.0.1:%d", port)
	if version == Version201 {
		cp := ocpp201.NewChargingStation("garage", nil, nil)
		cp.SetRemoteControlHandler(bootTrigger201{requested: requests, unsupported: unsupported})
		if err := cp.Start(url); err != nil {
			t.Fatal(err)
		}
		return cp.Stop, func(serial string) error {
			_, err := cp.BootNotification(provisioning.BootReasonTriggered, "Home", "Easee", func(req *provisioning.BootNotificationRequest) { req.ChargingStation.SerialNumber = serial })
			return err
		}, requests
	}
	cp := ocpp16.NewChargePoint("garage", nil, nil)
	cp.SetRemoteTriggerHandler(bootTrigger16{requests, unsupported})
	if err := cp.Start(url); err != nil {
		t.Fatal(err)
	}
	return cp.Stop, func(serial string) error {
		_, err := cp.BootNotification("Home", "Easee", func(req *core.BootNotificationRequest) { req.ChargePointSerialNumber = serial })
		return err
	}, requests
}

func awaitIdentityCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("identity condition did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestReconnectRequestsFreshBootOverBothOCPPProtocols(t *testing.T) {
	for _, version := range []Version{Version16, Version201} {
		t.Run(string(version), func(t *testing.T) {
			p16, p201, srv := startDualServer(t, telemetry.NewStore())
			h := srv.Handler()
			h.SetApprovedIDs([]string{"garage"})
			h.mu.Lock()
			h.identityProbeTiming = identityProbeTiming{20 * time.Millisecond, time.Second, 100 * time.Millisecond}
			h.mu.Unlock()
			port := p16
			if version == Version201 {
				port = p201
			}
			stop, boot, _ := connectIdentityCharger(t, version, port, false)
			if err := boot("A"); err != nil {
				stop()
				t.Fatal(err)
			}
			stop()
			awaitIdentityCondition(t, func() bool { return !h.IsOnline("garage") })
			stop, boot, requested := connectIdentityCharger(t, version, port, false)
			defer stop()
			select {
			case got := <-requested:
				if got != "BootNotification" {
					t.Fatalf("requested %s", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("reconnected charger received no BootNotification trigger")
			}
			if _, ok := h.CurrentIdentity("garage"); ok {
				t.Fatal("Accepted trigger trusted historical hardware")
			}
			if err := boot("B"); err != nil {
				t.Fatal(err)
			}
			if got, ok := h.CurrentIdentity("garage"); !ok || got.Serial != "B" {
				t.Fatalf("fresh Boot identity=%+v %v", got, ok)
			}
		})
	}
}

func TestUnsupportedBootTriggerLeavesIdentityUnknown(t *testing.T) {
	for _, version := range []Version{Version16, Version201} {
		t.Run(string(version), func(t *testing.T) {
			p16, p201, srv := startDualServer(t, telemetry.NewStore())
			h := srv.Handler()
			h.SetApprovedIDs([]string{"garage"})
			h.mu.Lock()
			h.identityProbeTiming = identityProbeTiming{time.Millisecond, time.Second, 5 * time.Millisecond}
			h.mu.Unlock()
			port := p16
			if version == Version201 {
				port = p201
			}
			stop, _, requested := connectIdentityCharger(t, version, port, true)
			defer stop()
			select {
			case <-requested:
			case <-time.After(2 * time.Second):
				t.Fatal("no Boot trigger")
			}
			awaitIdentityCondition(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.chargers["garage"].identityProbeTimer == nil })
			if _, ok := h.CurrentIdentity("garage"); ok {
				t.Fatal("unsupported trigger invented identity")
			}
			select {
			case <-requested:
				t.Fatal("unsupported trigger retried")
			case <-time.After(25 * time.Millisecond):
			}
		})
	}
}

func TestIdentityProbeTimeoutAndReconnectAreBounded(t *testing.T) {
	h := NewHandler(telemetry.NewStore(), 60)
	t.Cleanup(h.stopIdentityProbes)
	h.SetApprovedIDs([]string{"garage"})
	h.identityProbeTiming = identityProbeTiming{time.Millisecond, 5 * time.Millisecond, 30 * time.Millisecond}
	requests := make(chan time.Time, 10)
	h.identityProbe = func(_ string, _ Version, done func(error)) error { requests <- time.Now(); done(nil); return nil }
	h.OnConnect("garage")
	var first time.Time
	select {
	case first = <-requests:
	case <-time.After(time.Second):
		t.Fatal("initial identity request missing")
	}
	h.OnDisconnect("garage")
	h.OnConnect("garage")
	select {
	case <-requests:
		t.Fatal("reconnect bypassed rate limit")
	case <-time.After(10 * time.Millisecond):
	}
	for i := 0; i < maxIdentityProbeAttempts; i++ {
		select {
		case next := <-requests:
			if next.Sub(first) < 25*time.Millisecond {
				t.Fatal("retry bypassed interval")
			}
			first = next
		case <-time.After(time.Second):
			t.Fatal("bounded retry missing")
		}
	}
	awaitIdentityCondition(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.chargers["garage"].identityProbeTimer == nil })
	if _, ok := h.CurrentIdentity("garage"); ok {
		t.Fatal("timeout invented identity")
	}
	select {
	case <-requests:
		t.Fatal("retried beyond limit")
	case <-time.After(45 * time.Millisecond):
	}
	h.OnDisconnect("garage")
	h.OnConnect("garage")
	h.stopIdentityProbes()
	select {
	case <-requests:
		t.Fatal("probe outlived server stop")
	case <-time.After(45 * time.Millisecond):
	}
}

func TestAdoptionStartsIdentityProbeWithoutReconnect(t *testing.T) {
	for _, version := range []Version{Version16, Version201} {
		t.Run(string(version), func(t *testing.T) {
			p16, p201, srv := startDualServer(t, telemetry.NewStore())
			h := srv.Handler()
			h.mu.Lock()
			h.identityProbeTiming = identityProbeTiming{time.Millisecond, time.Second, 100 * time.Millisecond}
			h.mu.Unlock()
			port := p16
			if version == Version201 {
				port = p201
			}
			stop, boot, requested := connectIdentityCharger(t, version, port, false)
			defer stop()
			awaitIdentityCondition(t, func() bool { return h.IsOnline("garage") })
			select {
			case <-requested:
				t.Fatal("quarantined charger received identity probe")
			case <-time.After(10 * time.Millisecond):
			}
			h.SetApprovedIDs([]string{"garage"})
			select {
			case <-requested:
			case <-time.After(time.Second):
				t.Fatal("adoption did not request fresh identity")
			}
			if err := boot("A"); err != nil {
				t.Fatal(err)
			}
			if got, ok := h.CurrentIdentity("garage"); !ok || got.Serial != "A" {
				t.Fatalf("adopted identity=%+v %v", got, ok)
			}
		})
	}
}

func TestIdentityProbeRevocationCancelsPendingRequest(t *testing.T) {
	h := NewHandler(telemetry.NewStore(), 60)
	t.Cleanup(h.stopIdentityProbes)
	h.identityProbeTiming = identityProbeTiming{20 * time.Millisecond, time.Second, time.Minute}
	requested := make(chan struct{}, 1)
	h.identityProbe = func(string, Version, func(error)) error { requested <- struct{}{}; return nil }
	h.SetApprovedIDs([]string{"garage"})
	h.OnConnect("garage")
	h.SetApprovedIDs(nil)
	select {
	case <-requested:
		t.Fatal("revoked charger received probe")
	case <-time.After(40 * time.Millisecond):
	}
}

func TestDelayedStatusAfterDisconnectStaysUnknown(t *testing.T) {
	tel := telemetry.NewStore()
	h := NewHandler(tel, 60)
	h.SetApprovedIDs([]string{"garage"})
	h.OnConnect("garage")
	h.OnDisconnect("garage")
	h.OnStatusNotification("garage", &core.StatusNotificationRequest{ConnectorId: 1, Status: core.ChargePointStatusAvailable, ErrorCode: core.NoError})
	var data struct {
		Connected *bool `json:"connected"`
		Unknown   bool  `json:"connection_unknown"`
		Online    *bool `json:"is_online"`
	}
	if err := json.Unmarshal(tel.Get("garage", telemetry.DerEV).Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Connected != nil || !data.Unknown || data.Online == nil || *data.Online {
		t.Fatalf("late status looked physically unplugged: %+v", data)
	}
}
