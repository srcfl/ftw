// Package ocpp is the OCPP Central System for FTW, speaking 1.6J and 2.0.1.
//
// EV chargers connect to us via WebSocket. For a charger a loadpoint names in
// config, every BootNotification, MeterValues, StatusNotification and
// transaction message becomes a DerEV reading in telemetry.Store, keyed by the
// charge point identity from the URL path. The dispatch layer sums DerEV
// readings and stops home batteries discharging into an active EV charge.
// Control goes the other way as charging profiles; see control.go for why
// never as a remote stop.
//
// A charger no loadpoint names is quarantined as "pending": it may stay
// connected and is visible in Snapshot so the UI can offer it for adoption,
// but none of its messages reach telemetry — an unknown device that merely
// knows the shared basic-auth secret cannot fabricate EV load and steer
// dispatch. See Handler.SetApprovedIDs.
//
// # Provenance
//
// The protocol layer is github.com/lorenzodonini/ocpp-go v0.19.0 (MIT). It is a
// third-party dependency resolved through go.mod like any other — nothing in
// this package is copied or forked from it. It owns the WebSocket transport,
// OCPP-J framing, message types and schema validation. This package owns the
// handlers, the telemetry mapping, the control semantics and the safety clamps.
//
// The split matters when reading a bug: a malformed-message or transport
// failure is upstream, a wrong power figure or a wrong current limit is ours.
//
// Upstream describes its own 2.0.1 support as "examples working, but will need
// more real-world testing", so treat the 2.0.1 path here as less proven than
// 1.6J regardless of the tests in this package. Upstream has cut no release
// since August 2025 and implements no OCPP 2.1.
package ocpp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	ocpp201 "github.com/lorenzodonini/ocpp-go/ocpp2.0.1"
	"github.com/lorenzodonini/ocpp-go/ws"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Server is a running OCPP Central System, serving one listener per enabled
// protocol version.
//
// Each version needs its own port. The OCPP library's ws.Server keeps a single
// message handler, so one listener cannot dispatch both dialects, and a charger
// picks its dialect in the WebSocket handshake before any message is sent.
type Server struct {
	cfg     *Config
	cs      ocpp16.CentralSystem
	csms    ocpp201.CSMS
	handler *Handler
	// done closes when the 1.6 listener goroutine exits; doneV201 likewise for
	// 2.0.1. A nil channel means that version was not enabled.
	done     chan struct{}
	doneV201 chan struct{}
	stopOnce sync.Once
}

// Start brings up the OCPP CS on the configured bind:port. Returns
// immediately once the listener is up; the WebSocket loop runs in its own
// goroutine until ctx is cancelled or Stop() is called.
//
// The returned Server is the handle for shutdown — main.go is expected to
// call Stop() during graceful drain.
func Start(ctx context.Context, cfg *Config, tel *telemetry.Store) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("ocpp: nil config")
	}
	if tel == nil {
		return nil, errors.New("ocpp: nil telemetry store")
	}
	cfg.Defaults()

	wsServer := ws.NewServer()
	if cfg.Username != "" || cfg.Password != "" {
		u, p := cfg.Username, cfg.Password
		wsServer.SetBasicAuthHandler(func(user, pass string) bool {
			return user == u && pass == p
		})
	}

	cs := ocpp16.NewCentralSystem(nil, wsServer)
	h := NewHandler(tel, cfg.HeartbeatIntervalS)
	h.SetApprovedIDs(cfg.ApprovedIDs)
	cs.SetCoreHandler(h)
	cs.SetNewChargePointHandler(func(cp ocpp16.ChargePointConnection) {
		h.OnConnect(cp.ID())
		// Which listener a charger reached is what identifies its dialect, so
		// record it here rather than inferring it from a later message.
		h.setVersion(cp.ID(), Version16)
	})
	cs.SetChargePointDisconnectedHandler(func(cp ocpp16.ChargePointConnection) {
		h.OnDisconnect(cp.ID())
	})

	s := &Server{cfg: cfg, cs: cs, handler: h, done: make(chan struct{})}

	// OCPP 2.0.1 on its own port, when configured. Same handler and therefore
	// the same charger state and telemetry — only the message encoding differs.
	if cfg.PortV201 > 0 {
		wsServer201 := ws.NewServer()
		if cfg.Username != "" || cfg.Password != "" {
			u, p := cfg.Username, cfg.Password
			wsServer201.SetBasicAuthHandler(func(user, pass string) bool {
				return user == u && pass == p
			})
		}
		h201 := &handlerV201{Handler: h}
		csms := ocpp201.NewCSMS(nil, wsServer201)
		csms.SetProvisioningHandler(h201)
		csms.SetAvailabilityHandler(h201)
		csms.SetTransactionsHandler(h201)
		csms.SetMeterHandler(h201)
		csms.SetAuthorizationHandler(h201)
		csms.SetNewChargingStationHandler(func(cs ocpp201.ChargingStationConnection) {
			h.OnConnect(cs.ID())
			h.setVersion(cs.ID(), Version201)
		})
		csms.SetChargingStationDisconnectedHandler(func(cs ocpp201.ChargingStationConnection) {
			h.OnDisconnect(cs.ID())
		})

		s.csms = csms
		s.doneV201 = make(chan struct{})
		go func() {
			defer close(s.doneV201)
			slog.Info("OCPP central system listening",
				"version", Version201, "port", cfg.PortV201, "path", cfg.Path,
				"basic_auth", cfg.Username != "")
			csms.Start(cfg.PortV201, fmt.Sprintf("%s{ws}", cfg.Path))
		}()
	}

	go func() {
		defer close(s.done)
		slog.Info("OCPP central system listening",
			"bind", cfg.Bind, "port", cfg.Port, "path", cfg.Path,
			"basic_auth", cfg.Username != "")
		// TODO: cfg.Bind is not honored here. The ocpp-go library's
		// CentralSystem.Start(port, path) and ws.Server.Start(port, path)
		// only accept a port — there is no SetAddr or bind-address parameter.
		// To support bind-address natively we would need to either:
		//   (a) upstream a PR to ocpp-go adding a SetListenAddr method, or
		//   (b) create our own net.Listener bound to cfg.Bind:cfg.Port and
		//       serve the ws.Server's http.Handler on it.
		// For now cfg.Bind is advisory-only (documented in Config).
		// cs.Start blocks until cs.Stop is called.
		s.cs.Start(cfg.Port, fmt.Sprintf("%s{ws}", cfg.Path))
	}()
	go func() {
		<-ctx.Done()
		s.Stop()
	}()
	return s, nil
}

// Stop closes the WebSocket server and waits for the listener goroutine to exit.
// A 5-second timeout prevents deadlock if the listener goroutine is stuck.
func (s *Server) Stop() {
	if s == nil || s.cs == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.cs.Stop()
		if s.csms != nil {
			s.csms.Stop()
		}
	})
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		slog.Warn("ocpp: shutdown timeout — forcing close", "version", Version16)
	}
	if s.doneV201 != nil {
		select {
		case <-s.doneV201:
		case <-time.After(5 * time.Second):
			slog.Warn("ocpp: shutdown timeout — forcing close", "version", Version201)
		}
	}
}

// Handler exposes per-charger state for tests + introspection.
func (s *Server) Handler() *Handler { return s.handler }

// Port is the port the listener actually took, after defaults were applied.
// Callers configuring an unset port need this to log or display the real value.
func (s *Server) Port() int {
	if s == nil || s.cfg == nil {
		return 0
	}
	return s.cfg.Port
}

// Path is the URL prefix charge points connect to, after defaults were applied.
// A charger dials <path><identity>, and that identity becomes its device key.
func (s *Server) Path() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	return s.cfg.Path
}
