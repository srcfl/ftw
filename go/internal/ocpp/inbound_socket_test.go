package ocpp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/availability"
	"github.com/lorenzodonini/ocpp-go/ocpp2.0.1/provisioning"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type delayedStatus16 struct {
	core.CentralSystemHandler
	entered, release, finished chan struct{}
}

func (h delayedStatus16) OnStatusNotification(id string, r *core.StatusNotificationRequest) (*core.StatusNotificationConfirmation, error) {
	if r.Status == core.ChargePointStatusAvailable && r.Info == "delay-old-socket" {
		select {
		case <-h.entered:
		default:
			close(h.entered)
			<-h.release
			defer close(h.finished)
		}
	}
	return h.CentralSystemHandler.OnStatusNotification(id, r)
}

type delayedStatus201 struct {
	availability.CSMSHandler
	entered, release, finished chan struct{}
}

func (h delayedStatus201) OnStatusNotification(id string, r *availability.StatusNotificationRequest) (*availability.StatusNotificationResponse, error) {
	if r.ConnectorStatus == availability.ConnectorStatusAvailable && r.Timestamp.Time.Year() == 2020 {
		select {
		case <-h.entered:
		default:
			close(h.entered)
			<-h.release
			defer close(h.finished)
		}
	}
	return h.CSMSHandler.OnStatusNotification(id, r)
}

func TestOldSocketStatusCannotClearPauseAfterReconnect(t *testing.T) {
	for _, version := range []Version{Version16, Version201} {
		t.Run(string(version), func(t *testing.T) {
			tel := telemetry.NewStore()
			p16, p201, srv := startDualServer(t, tel)
			h := srv.Handler()
			h.SetApprovedIDs([]string{"garage"})
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			// The real SDK starts a goroutine for each CALL. Delay its invocation of
			// the application handler, as a scheduler pause or a blocked callback can.
			srv.cs.SetCoreHandler(delayedStatus16{&boundHandler16{inner: h, sessions: srv.sockets}, entered, release, finished})
			srv.csms.SetAvailabilityHandler(delayedStatus201{&boundHandler201{inner: &handlerV201{h}, sessions: srv.sockets}, entered, release, finished})
			connect := func() (func(), func(bool) error) {
				t.Helper()
				if version == Version201 {
					cp, stop, e := dialChargingStation201(t, wsURL(p201), "garage", nil)
					if e != nil {
						t.Fatal(e)
					}
					if _, e := cp.BootNotification(provisioning.BootReasonPowerUp, "Home", "Easee"); e != nil {
						t.Fatal(e)
					}
					firstAvailable := true
					return stop, func(connected bool) error {
						s := availability.ConnectorStatusAvailable
						if connected {
							s = availability.ConnectorStatusOccupied
						}
						stamp := time.Now()
						if !connected && firstAvailable {
							stamp = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
							firstAvailable = false
						}
						_, e := cp.StatusNotification(types201.NewDateTime(stamp), s, 1, 1)
						return e
					}
				}
				cp, stop, e := dialChargePoint16(t, wsURL(p16), "garage", nil, nil)
				if e != nil {
					t.Fatal(e)
				}
				if _, e := cp.BootNotification("Home", "Easee"); e != nil {
					t.Fatal(e)
				}
				return stop, func(connected bool) error {
					s := core.ChargePointStatusAvailable
					if connected {
						s = core.ChargePointStatusSuspendedEVSE
					}
					_, e := cp.StatusNotification(1, core.NoError, s, func(r *core.StatusNotificationRequest) {
						if !connected {
							r.Info = "delay-old-socket"
						}
					})
					return e
				}
			}
			stop, status := connect()
			if e := status(true); e != nil {
				t.Fatal(e)
			}
			manager := loadpoint.NewManager()
			manager.Load([]loadpoint.Config{{ID: "garage", DriverName: "garage", MinChargeW: 1380, MaxChargeW: 11040}})
			ctrl := loadpoint.NewController(manager, func(time.Time) (loadpoint.Directive, bool) { return loadpoint.Directive{}, false }, func(string) (loadpoint.EVSample, bool) {
				var r struct {
					Connected bool `json:"connected"`
				}
				json.Unmarshal(tel.Get("garage", telemetry.DerEV).Data, &r)
				return loadpoint.EVSample{Connected: r.Connected, RequestActive: true}, true
			}, nil)
			ctrl.Tick(context.Background(), time.Now())
			ctrl.SetManualHold("garage", loadpoint.ManualHold{Persistent: true})
			go status(false)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("SDK did not dispatch old Available")
			}
			stop()
			awaitIdentityCondition(t, func() bool { return !h.IsOnline("garage") })
			stop, status = connect()
			defer stop()
			if e := status(true); e != nil {
				t.Fatal(e)
			}
			close(release)
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("old SDK handler did not finish")
			}
			ctrl.Tick(context.Background(), time.Now())
			var raw map[string]any
			json.Unmarshal(tel.Get("garage", telemetry.DerEV).Data, &raw)
			if hold, ok := ctrl.GetManualHold("garage", time.Now()); !ok || hold.PowerW != 0 {
				t.Fatalf("old socket cleared explicit Pause: reading=%v", raw)
			}
			if raw["connected"] != true {
				t.Fatalf("old socket replaced fresh connection: %v", raw)
			}
			if err := status(false); err != nil {
				t.Fatal(err)
			}
			unplugged := time.Now()
			ctrl.Tick(context.Background(), unplugged)
			// The cable stays out for UnplugGrace.
			ctrl.Tick(context.Background(), unplugged.Add(loadpoint.UnplugGrace))
			if _, ok := ctrl.GetManualHold("garage", time.Now()); ok {
				t.Fatal("fresh physical unplug did not clear Pause")
			}
		})
	}
}

type delayedBoot16 struct {
	core.CentralSystemHandler
	entered, release, finished chan struct{}
}

func (h delayedBoot16) OnBootNotification(id string, r *core.BootNotificationRequest) (*core.BootNotificationConfirmation, error) {
	if r.ChargePointSerialNumber == "stale" {
		close(h.entered)
		<-h.release
		defer close(h.finished)
	}
	return h.CentralSystemHandler.OnBootNotification(id, r)
}

type delayedBoot201 struct {
	provisioning.CSMSHandler
	entered, release, finished chan struct{}
}

func (h delayedBoot201) OnBootNotification(id string, r *provisioning.BootNotificationRequest) (*provisioning.BootNotificationResponse, error) {
	if r.ChargingStation.SerialNumber == "stale" {
		close(h.entered)
		<-h.release
		defer close(h.finished)
	}
	return h.CSMSHandler.OnBootNotification(id, r)
}

func TestOldSocketBootCannotReplaceCurrentHardware(t *testing.T) {
	for _, first := range []Version{Version16, Version201} {
		for _, second := range []Version{Version16, Version201} {
			t.Run(string(first)+"->"+string(second), func(t *testing.T) {
				p16, p201, srv := startDualServer(t, telemetry.NewStore())
				h := srv.Handler()
				h.SetApprovedIDs([]string{"garage"})
				entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				srv.cs.SetCoreHandler(delayedBoot16{&boundHandler16{h, srv.sockets}, entered, release, finished})
				srv.csms.SetProvisioningHandler(delayedBoot201{&boundHandler201{&handlerV201{h}, srv.sockets}, entered, release, finished})
				port := func(v Version) int {
					if v == Version201 {
						return p201
					}
					return p16
				}
				stop, boot, _ := connectIdentityCharger(t, first, port(first), false)
				if err := boot("A"); err != nil {
					t.Fatal(err)
				}
				oldID, err := srv.sockets.currentID("garage")
				if err != nil {
					t.Fatal(err)
				}
				go boot("stale")
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("old Boot did not enter SDK goroutine")
				}
				stop()
				awaitIdentityCondition(t, func() bool { return !h.IsOnline("garage") })
				stop, boot, _ = connectIdentityCharger(t, second, port(second), false)
				defer stop()
				if err := boot("B"); err != nil {
					t.Fatal(err)
				}
				newID, err := srv.sockets.currentID("garage")
				if err != nil || newID == oldID {
					t.Fatal("socket reused SDK identity")
				}
				close(release)
				select {
				case <-finished:
				case <-time.After(time.Second):
					t.Fatal("old Boot did not complete")
				}
				if identity, ok := h.CurrentIdentity("garage"); !ok || identity.Serial != "B" {
					t.Fatalf("old Boot changed hardware: %+v %v", identity, ok)
				}
				// A late callback and response cannot resolve an old alias to its replacement.
				called := false
				_, err = boundCall(srv.sockets, oldID, func(string) (bool, error) { called = true; return true, nil })
				if err == nil || called {
					t.Fatal("old callback acquired replacement")
				}
			})
		}
	}
}

func TestFailedAndDuplicateHandshakesPreserveCurrentSocket(t *testing.T) {
	p16, p201, srv := startDualServer(t, telemetry.NewStore())
	h := srv.Handler()
	h.SetApprovedIDs([]string{"garage"})
	// This reaches the connection check but fails WebSocket Upgrade. Request
	// completion must release its reservation so a real charger can connect.
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/garage", p16), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-WebSocket-Protocol", "ocpp1.6")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode == http.StatusSwitchingProtocols {
		t.Fatal("malformed upgrade unexpectedly succeeded")
	}
	awaitIdentityCondition(t, func() bool {
		slot := srv.sockets.slot("garage")
		slot.mu.Lock()
		defer slot.mu.Unlock()
		return slot.pending == nil
	})
	stop, boot, _ := connectIdentityCharger(t, Version16, p16, false)
	defer stop()
	if err := boot("A"); err != nil {
		t.Fatal(err)
	}
	oldID, err := srv.sockets.currentID("garage")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := dialChargingStation201(t, wsURL(p201), "garage", nil); err == nil {
		t.Fatal("other protocol took an active public charger ID")
	}
	if current, err := srv.sockets.currentID("garage"); err != nil || current != oldID {
		t.Fatal("duplicate handshake removed active owner")
	}
	if err := boot("A"); err != nil {
		t.Fatal("duplicate handshake stopped active connection:", err)
	}
	if identity, ok := h.CurrentIdentity("garage"); !ok || identity.Serial != "A" {
		t.Fatal("duplicate changed hardware")
	}
	// Neither the failed nor the refused handshake may hold up a stop.
	if !srv.sockets.refuseHandshakes(5 * time.Second) {
		t.Fatal("a finished handshake still counts as in flight")
	}
}

// The library closes the sockets in its table when it stops, but a handshake
// past the connection check joins that table only after its 101 is written.
// Stop must wait for it and refuse any later one; otherwise the socket stays
// open on a stopped server.
func TestStopWaitsForAHandshakePastTheCheck(t *testing.T) {
	s := newSocketSessions()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !s.reserveConnection("garage", httptest.NewRequest(http.MethodGet, "/garage", nil).WithContext(ctx)) {
		t.Fatal("handshake refused before stop")
	}
	stopped := make(chan bool, 1)
	go func() { stopped <- s.refuseHandshakes(time.Minute) }()
	awaitIdentityCondition(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed })
	if s.reserveConnection("carport", httptest.NewRequest(http.MethodGet, "/carport", nil)) {
		t.Fatal("a handshake was admitted after stop began")
	}
	select {
	case <-stopped:
		t.Fatal("stop went ahead while a handshake was past the check")
	case <-time.After(50 * time.Millisecond):
	}
	cancel() // the handshake ends without joining the table
	select {
	case ok := <-stopped:
		if !ok {
			t.Fatal("stop reported a handshake still in flight")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop still waiting after the handshake ended")
	}
}

// A charger that connected has joined the table, so a stop does not wait for it.
func TestStopDoesNotWaitForAConnectedCharger(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "garage")
	if _, _, err := dialChargePoint16(t, wsURL(port), "garage", nil, nil); err != nil {
		t.Fatal(err)
	}
	awaitIdentityCondition(t, func() bool { return srv.Handler().IsOnline("garage") })
	if !srv.sockets.refuseHandshakes(5 * time.Second) {
		t.Fatal("a connected charger still counts as a handshake in flight")
	}
}
