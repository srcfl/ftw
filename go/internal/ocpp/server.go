// Package ocpp is the OCPP 1.6J Central System for FTW.
//
// EV chargers connect to us via WebSocket. We translate every BootNotification,
// MeterValues, and StatusNotification into a DerEV reading in telemetry.Store,
// keyed by the chargePointId from the URL path. The dispatch layer
// (control/dispatch.go:199-216) sums DerEV readings and prevents home batteries
// from discharging into an active EV charge.
//
// Phase 1 is read-only — handlers below ack everything but do not push remote
// commands. Phase 2 will add RemoteStartTransaction / SetChargingProfile.
//
// The library backing this is github.com/lorenzodonini/ocpp-go (MIT, also used
// by SteVe) — it owns the WebSocket + JSON layer; we own the message handlers
// and the telemetry mapping.
package ocpp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	ocpp16 "github.com/lorenzodonini/ocpp-go/ocpp1.6"
	"github.com/lorenzodonini/ocpp-go/ws"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Server is a running OCPP 1.6J Central System.
type Server struct {
	cfg      *Config
	cs       ocpp16.CentralSystem
	handler  *Handler
	done     chan struct{}
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
	cs.SetCoreHandler(h)
	cs.SetNewChargePointHandler(func(cp ocpp16.ChargePointConnection) {
		h.OnConnect(cp.ID())
	})
	cs.SetChargePointDisconnectedHandler(func(cp ocpp16.ChargePointConnection) {
		h.OnDisconnect(cp.ID())
	})

	s := &Server{cfg: cfg, cs: cs, handler: h, done: make(chan struct{})}
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
	s.stopOnce.Do(func() { s.cs.Stop() })
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		slog.Warn("ocpp: shutdown timeout — forcing close")
	}
}

// Handler exposes per-charger state for tests + introspection.
func (s *Server) Handler() *Handler { return s.handler }
