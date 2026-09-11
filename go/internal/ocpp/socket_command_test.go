package ocpp

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/smartcharging"
	"github.com/lorenzodonini/ocpp-go/ocpp1.6/types"
	ocpp201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	smart201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/smartcharging"
	types201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1/types"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type callbackGate struct {
	once             sync.Once
	entered, release chan struct{}
}

func (g *callbackGate) wait() {
	first := false
	g.once.Do(func() { first = true })
	if first {
		close(g.entered)
		<-g.release
	}
}

type gatedProfile16 struct {
	ocpp16.CentralSystem
	gate *callbackGate
}

func (g *gatedProfile16) SetChargingProfile(id string, cb func(*smartcharging.SetChargingProfileConfirmation, error), connector int, p *types.ChargingProfile, opts ...func(*smartcharging.SetChargingProfileRequest)) error {
	return g.CentralSystem.SetChargingProfile(id, func(r *smartcharging.SetChargingProfileConfirmation, e error) { g.gate.wait(); cb(r, e) }, connector, p, opts...)
}

type gatedProfile201 struct {
	ocpp201.CSMS
	gate *callbackGate
}

func (g *gatedProfile201) SetChargingProfile(id string, cb func(*smart201.SetChargingProfileResponse, error), evse int, p *types201.ChargingProfile, opts ...func(*smart201.SetChargingProfileRequest)) error {
	return g.CSMS.SetChargingProfile(id, func(r *smart201.SetChargingProfileResponse, e error) { g.gate.wait(); cb(r, e) }, evse, p, opts...)
}

func TestOldSocketAcceptedCommandCannotAcknowledgeReplacement(t *testing.T) {
	for _, version := range []Version{Version16, Version201} {
		t.Run(string(version), func(t *testing.T) {
			p16, p201, srv := startDualServer(t, telemetry.NewStore())
			srv.Handler().SetApprovedIDs([]string{"garage"})
			gate := &callbackGate{entered: make(chan struct{}), release: make(chan struct{})}
			// The charger answers on the real wire. Delay only the SDK's asynchronous
			// callback, then reconnect before it reaches the caller waiting for ACK.
			srv.cs = &gatedProfile16{srv.cs, gate}
			srv.csms = &gatedProfile201{srv.csms, gate}
			connect := func() (func(), func() int) {
				if version == Version201 {
					fake, stop := connectStationV201(t, srv, p201, "garage")
					return stop, func() int { fake.mu.Lock(); defer fake.mu.Unlock(); return len(fake.profiles) }
				}
				_, fake, stop := connectCharger(t, srv, p16, "garage")
				return stop, fake.count
			}
			stop, _ := connect()
			result := make(chan error, 1)
			go func() {
				result <- srv.Command(context.Background(), "garage", []byte(`{"action":"ev_set_current","power_w":4140}`))
			}()
			select {
			case <-gate.entered:
			case <-time.After(time.Second):
				t.Fatal("wire ACK did not reach SDK callback")
			}
			stop()
			awaitIdentityCondition(t, func() bool { return !srv.Handler().IsOnline("garage") })
			_, count := connect()
			close(gate.release)
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("old wire ACK acknowledged replacement")
				}
			case <-time.After(time.Second):
				t.Fatal("old callback did not settle")
			}
			if count() != 0 {
				t.Fatal("old command or retry reached replacement")
			}
			if got := srv.Handler().LastAmps("garage", 7); got != 7 {
				t.Fatalf("old ACK stored %v A on replacement", got)
			}
			if err := srv.Command(context.Background(), "garage", []byte(`{"action":"ev_set_current","power_w":4140}`)); err != nil {
				t.Fatal(err)
			}
			if count() != 1 {
				t.Fatal("new command did not reach replacement exactly once")
			}
		})
	}
}

func TestReconnectProbesCapabilitiesAgain(t *testing.T) {
	port, srv := startServer(t, telemetry.NewStore(), "garage")
	first := ocpp16.NewChargePoint("garage", nil, nil)
	first.SetCoreHandler(&capabilityCP{profiles: "Core,SmartCharging"})
	if err := first.Start(fmt.Sprintf("ws://127.0.0.1:%d", port)); err != nil {
		t.Fatal(err)
	}
	if _, err := first.BootNotification("Home", "Easee"); err != nil {
		t.Fatal(err)
	}
	if view := waitSteerable(t, srv, "garage"); view.Steerable == nil || !*view.Steerable {
		t.Fatal("first capabilities missing")
	}
	first.Stop()
	awaitIdentityCondition(t, func() bool { return !srv.Handler().IsOnline("garage") })
	next := &capabilityCP{profiles: "Core"}
	connectCapabilityCP(t, port, "garage", next)
	if view := waitSteerable(t, srv, "garage"); view.Steerable == nil || *view.Steerable || view.FeatureProfiles != "Core" {
		t.Fatalf("replacement retained old capabilities: %+v", view)
	}
}
