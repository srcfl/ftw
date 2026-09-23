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
	"net/http"
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
	sockets *socketSessions
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

	auth := newAuthorizer(cfg)
	sockets := newSocketSessions()
	wsServer, err := newListener(cfg, auth, sockets)
	if err != nil {
		return nil, err
	}

	cs := ocpp16.NewCentralSystem(nil, wsServer)
	h := NewHandler(tel, cfg.HeartbeatIntervalS)
	h.SetApprovedIDs(cfg.ApprovedIDs)
	cs.SetCoreHandler(&boundHandler16{inner: h, sessions: sockets})
	cs.SetNewChargePointHandler(func(cp ocpp16.ChargePointConnection) {
		// Which listener a charger reached is what identifies its dialect, so
		// record it here rather than inferring it from a later message — and
		// before OnConnect, whose capability probe dispatches on it.
		_, _ = boundCall(sockets, cp.ID(), func(id string) (bool, error) { h.setVersion(id, Version16); h.OnConnect(id); return true, nil })
	})
	cs.SetChargePointDisconnectedHandler(func(cp ocpp16.ChargePointConnection) {
		sockets.disconnected(cp.ID(), h.OnDisconnect)
	})

	s := &Server{cfg: cfg, cs: cs, handler: h, sockets: sockets, done: make(chan struct{})}
	h.identityProbe = s.requestBootNotification

	// OCPP 2.0.1 on its own port, when configured. Same handler and therefore
	// the same charger state and telemetry — only the message encoding differs.
	if cfg.PortV201 > 0 {
		wsServer201, err := newListener(cfg, auth, sockets)
		if err != nil {
			return nil, err
		}
		h201 := &boundHandler201{inner: &handlerV201{Handler: h}, sessions: sockets}
		csms := ocpp201.NewCSMS(nil, wsServer201)
		csms.SetProvisioningHandler(h201)
		csms.SetAvailabilityHandler(h201)
		csms.SetTransactionsHandler(h201)
		csms.SetMeterHandler(h201)
		csms.SetAuthorizationHandler(h201)
		// Smart charging is registered for what the car reports, not for
		// what we send: charging profiles go out through control.go. The
		// one message here that changes behaviour is
		// NotifyEVChargingNeeds; the rest of the profile is acknowledged
		// and dropped. See charging_needs.go.
		csms.SetSmartChargingHandler(h201)
		csms.SetNewChargingStationHandler(func(cs ocpp201.ChargingStationConnection) {
			_, _ = boundCall(sockets, cs.ID(), func(id string) (bool, error) { h.setVersion(id, Version201); h.OnConnect(id); return true, nil })
		})
		csms.SetChargingStationDisconnectedHandler(func(cs ocpp201.ChargingStationConnection) {
			sockets.disconnected(cs.ID(), h.OnDisconnect)
		})

		s.csms = csms
		s.doneV201 = make(chan struct{})
		go func() {
			defer close(s.doneV201)
			slog.Info("OCPP central system listening",
				"version", Version201, "scheme", cfg.Scheme(),
				"bind", cfg.Bind, "port", cfg.PortV201, "path", cfg.Path,
				"basic_auth", auth.requiresCredential(),
				"per_charger_credentials", len(cfg.ChargerSecrets),
				"client_certs", cfg.TLS != nil && cfg.TLS.ClientCAFile != "")
			csms.Start(cfg.PortV201, fmt.Sprintf("%s{ws}", cfg.Path))
		}()
	}

	// Capability probe: whether a charger can be steered (SmartCharging /
	// SmartChargingCtrlr) or only meters. Fired by the Handler from connect
	// and boot until the charger answers; dispatched here by dialect. The
	// closure reads s.csms at call time, so a 2.0.1 charger probes correctly
	// even though the CSMS is wired after the 1.6 server.
	h.capabilityProbe = func(id string) {
		h.mu.Lock()
		ver := h.chargersLocked(id).version
		h.mu.Unlock()
		if ver == Version201 && s.csms != nil {
			probeSmartChargingV201(s.csms, h, id, sockets)
			return
		}
		probeFeatureProfiles16(cs, h, id, sockets)
	}

	go func() {
		defer close(s.done)
		slog.Info("OCPP central system listening",
			"version", Version16, "scheme", cfg.Scheme(),
			"bind", cfg.Bind, "port", cfg.Port, "path", cfg.Path,
			"basic_auth", auth.requiresCredential(),
			"per_charger_credentials", len(cfg.ChargerSecrets),
			"client_certs", cfg.TLS != nil && cfg.TLS.ClientCAFile != "")
		// The socket itself is opened on every interface — ws.Server.Start
		// builds its address from the port alone. cfg.Bind is enforced one
		// layer up, in authorizer.checkClient, which refuses the handshake
		// for a connection that arrived somewhere else.
		// cs.Start blocks until cs.Stop is called.
		cs.Start(cfg.Port, fmt.Sprintf("%s{ws}", cfg.Path))
	}()
	go func() {
		<-ctx.Done()
		s.Stop()
	}()
	return s, nil
}

// newListener builds one version's WebSocket server — TLS when configured —
// with both authorization gates wired.
//
// The basic-auth handler is registered only when a credential exists: the
// library reads a registered handler as "credentials are mandatory" and
// answers 401 to a charger that sends none, so registering it unconditionally
// would lock out every charger on a server with no username instead of
// admitting them all. checkClient is always safe to register; it authorizes
// everything when nothing is configured.
func newListener(cfg *Config, auth *authorizer, sockets *socketSessions) (ws.WsServer, error) {
	var srv *ws.Server
	if cfg.TLS.configured() {
		// Half a TLS section is an error, not a reason to serve plaintext:
		// an operator who asked for wss:// and silently got ws:// would
		// have no way to tell the link was never encrypted.
		tlsCfg, err := cfg.TLS.serverTLS()
		if err != nil {
			return nil, err
		}
		srv = ws.NewTLSServer(cfg.TLS.CertFile, cfg.TLS.KeyFile, tlsCfg)
	} else {
		srv = ws.NewServer()
	}
	if auth.requiresCredential() {
		srv.SetBasicAuthHandler(auth.basicAuth)
	}
	srv.SetCheckClientHandler(auth.checkClient)
	return &guardedServer{Server: srv, check: auth.checkClient, sessions: sockets}, nil
}

// guardedServer keeps our connection check installed.
//
// ocppj.Server.Start unconditionally calls SetCheckClientHandler with its own
// handler, which is nil unless the caller reached past the 1.6/2.0.1 facade to
// set one. Handing the raw ws.Server to NewCentralSystem therefore discards
// the bind and identity gates silently at startup — the listener comes up, the
// logs say the gates are configured, and every impersonation attempt is
// accepted. This wrapper chains instead of replacing, so ours runs first and
// the library's own check still runs after.
type guardedServer struct {
	*ws.Server
	check    func(id string, r *http.Request) bool
	sessions *socketSessions
}

func (g *guardedServer) SetCheckClientHandler(handler func(id string, r *http.Request) bool) {
	g.Server.SetCheckClientHandler(func(id string, r *http.Request) bool {
		if !g.check(id, r) || (handler != nil && !handler(id, r)) {
			return false
		}
		return g.sessions.reserveConnection(id, r)
	})
}

// Stop closes the WebSocket server and waits for the listener goroutine to exit.
// A 5-second timeout prevents deadlock if the listener goroutine is stuck.
func (s *Server) Stop() {
	if s == nil || s.cs == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.handler.stopIdentityProbes()
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

// The credential comparison that used to live here as basicAuthCheck now
// belongs to the authorizer in auth.go, which has to weigh a per-charger
// secret against the shared one before it can answer. Its constant-time
// comparison came from here and is kept: this is still the gate in front of
// a listener the socket layer will not pin, so a timing oracle on the string
// compare is worth closing even though a shared secret without TLS is a soft
// boundary to begin with.
