package ocpp

import (
	"encoding/json"
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
	url := wsURL(port)
	if version == Version201 {
		cp, stop, err := dialChargingStation201(t, url, "garage", func(cp ocpp201.ChargingStation) {
			cp.SetRemoteControlHandler(bootTrigger201{requested: requests, unsupported: unsupported})
		})
		if err != nil {
			t.Fatal(err)
		}
		return stop, func(serial string) error {
			_, err := cp.BootNotification(provisioning.BootReasonTriggered, "Home", "Easee", func(req *provisioning.BootNotificationRequest) { req.ChargingStation.SerialNumber = serial })
			return err
		}, requests
	}
	cp, stop, err := dialChargePoint16(t, url, "garage", nil, func(cp ocpp16.ChargePoint) {
		cp.SetRemoteTriggerHandler(bootTrigger16{requests, unsupported})
	})
	if err != nil {
		t.Fatal(err)
	}
	return stop, func(serial string) error {
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
	const interval = 50 * time.Millisecond
	// The first request waits an hour for its answer, so no timer can send
	// another before the reconnect below, however late this goroutine runs.
	h.identityProbeTiming = identityProbeTiming{time.Millisecond, time.Hour, interval}
	requests := make(chan time.Time, 10)
	h.identityProbe = func(_ string, _ Version, done func(error)) error { requests <- time.Now(); done(nil); return nil }
	// Each request may go no sooner than one interval after the one before,
	// so the kth after the first goes at least k intervals after the first
	// connect. Only that lower bound is checked: a loaded machine delays
	// requests, it cannot bring them forward.
	connected := time.Now()
	h.OnConnect("garage")
	select {
	case <-requests:
	case <-time.After(5 * time.Second):
		t.Fatal("initial identity request missing")
	}
	// A reconnect starts a fresh set of attempts but keeps the rate limit.
	// From here each request times out after 5 ms.
	h.mu.Lock()
	h.identityProbeTiming.timeout = 5 * time.Millisecond
	h.mu.Unlock()
	h.OnDisconnect("garage")
	h.OnConnect("garage")
	for k := 1; k <= maxIdentityProbeAttempts; k++ {
		select {
		case at := <-requests:
			if early := at.Sub(connected); early < time.Duration(k)*interval {
				t.Fatalf("request %d after the first went %v after connect, before the rate limit allowed", k, early)
			}
		case <-time.After(5 * time.Second):
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

	// Stopping cancels a pending probe and schedules no new one. The grace
	// is long so that the pending probe cannot fire before the stop.
	h.mu.Lock()
	h.identityProbeTiming.grace = time.Hour
	h.mu.Unlock()
	h.OnDisconnect("garage")
	h.OnConnect("garage")
	h.mu.Lock()
	pending, epoch := h.chargers["garage"].identityProbeTimer != nil, h.chargers["garage"].identityProbeEpoch
	h.mu.Unlock()
	if !pending {
		t.Fatal("reconnect scheduled no identity probe")
	}
	h.stopIdentityProbes()
	h.requestIdentity("garage", epoch) // a timer that fired as the server stopped
	h.OnDisconnect("garage")
	h.OnConnect("garage")
	h.mu.Lock()
	pending = h.chargers["garage"].identityProbeTimer != nil
	h.mu.Unlock()
	if pending {
		t.Fatal("probe scheduled after server stop")
	}
	select {
	case <-requests:
		t.Fatal("probe outlived server stop")
	default:
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
	// The grace is long so that the pending probe cannot fire before the
	// revocation, however late this goroutine runs.
	h.identityProbeTiming = identityProbeTiming{time.Hour, time.Second, time.Minute}
	requested := make(chan struct{}, 1)
	h.identityProbe = func(string, Version, func(error)) error { requested <- struct{}{}; return nil }
	h.SetApprovedIDs([]string{"garage"})
	h.OnConnect("garage")
	h.mu.Lock()
	pending, epoch := h.chargers["garage"].identityProbeTimer != nil, h.chargers["garage"].identityProbeEpoch
	h.mu.Unlock()
	if !pending {
		t.Fatal("connect scheduled no identity probe")
	}
	h.SetApprovedIDs(nil)
	h.mu.Lock()
	pending = h.chargers["garage"].identityProbeTimer != nil
	h.mu.Unlock()
	if pending {
		t.Fatal("revocation left the identity probe pending")
	}
	h.requestIdentity("garage", epoch) // a timer that fired as the charger was revoked
	select {
	case <-requested:
		t.Fatal("revoked charger received probe")
	default:
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
		Connected *bool  `json:"connected"`
		Unknown   bool   `json:"connection_unknown"`
		Online    *bool  `json:"is_online"`
		State     string `json:"state_label"`
		Reason    string `json:"reason_no_current_label"`
	}
	if err := json.Unmarshal(tel.Get("garage", telemetry.DerEV).Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Connected != nil || !data.Unknown || data.Online == nil || *data.Online {
		t.Fatalf("late status looked physically unplugged: %+v", data)
	}
	if data.State != "Waiting for charger connection status" || data.Reason != "" {
		t.Fatalf("connection wait must be a state, got %+v", data)
	}
}
