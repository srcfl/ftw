// Package appuplink serves the encrypted app sessions FTW Cloud carries: a
// phone's stream through the tunnel and a watch's exchange.
//
// FTW Cloud moves opaque bytes. Everything that makes them meaningful is in
// go/protocol/appwire (the Noise session) and go/protocol (the messages).
package appuplink

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
)

// Options wires the box's app sessions to the FTW Cloud tunnel.
type Options struct {
	// Enroll owns the static key and the record of which phones are
	// allowed in.
	Enroll *appenroll.Identity

	// Handler builds one message-layer handler per app session. The caller
	// supplies it so this package needs to know nothing about telemetry,
	// control or the planner.
	Handler HandlerBuilder

	// ExchangeHandler builds one handler per watch session, with
	// appproto.Config.Exchange set (ADR 0009). Nil opens no exchange: the
	// box takes no x from FTW Cloud, and nothing else changes.
	ExchangeHandler HandlerBuilder

	Logger *slog.Logger

	// Arrivals hears each finished driver poll (telemetry.Store.Arrivals).
	// Sessions send on it; nil leaves them on the beat alone, one frame per
	// subscribed period.
	Arrivals <-chan struct{}
}

// HandlerBuilder makes one message-layer handler for one app session.
//
// The caller is passed in rather than looked up later, because the handshake
// has just authenticated the device and that is the only moment the answer is
// certain. The grant reader is passed beside it because the caller is a
// snapshot and a revoke can land a second afterwards: the handler re-reads
// the enrolment on every privileged request, and refuses one whose epoch has
// moved.
type HandlerBuilder func(appproto.Sender, apiauth.Caller, appproto.GrantReader) (*appproto.Handler, error)

// Uplink holds the sessions FTW Cloud carries.
type Uplink struct {
	// exchange is the watches' door through FTW Cloud and their table of
	// sessions, nil without an ExchangeHandler.
	exchange *Exchange
	// tunnel is the table of phone sessions FTW Cloud carries.
	tunnel *Tunnel
}

// DropSessionsByAppKey tears down every live session a revoked key holds.
// Returns how many went; zero is normal when the phone is not connected.
func (u *Uplink) DropSessionsByAppKey(pub []byte) int {
	dropped := 0
	if u.exchange != nil {
		dropped += u.exchange.table.dropByAppKey(pub)
	}
	dropped += u.tunnel.dropByAppKey(pub)
	return dropped
}

// New builds an uplink.
func New(opts Options) (*Uplink, error) {
	switch {
	case opts.Enroll == nil:
		return nil, errors.New("appuplink: Enroll is required")
	case opts.Handler == nil:
		return nil, errors.New("appuplink: Handler is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	log := opts.Logger.With("component", "appuplink")

	u := &Uplink{tunnel: newTunnel(opts.Handler, opts.Enroll, &slots{}, opts.Arrivals, log)}
	if opts.ExchangeHandler != nil {
		u.exchange = newExchange(&exchanges{
			build:    opts.ExchangeHandler,
			enroll:   opts.Enroll,
			static:   opts.Enroll.StaticKey(),
			log:      log,
			now:      time.Now,
			cmdFloor: ExchangeCmdFloor,
		}, log)
	}
	return u, nil
}

// Run serves phone sessions and watch exchanges carried by FTW Cloud until
// the context ends.
func (u *Uplink) Run(ctx context.Context) error {
	if u.exchange != nil {
		go u.exchange.Run(ctx)
	}
	u.tunnel.Run(ctx)
	return ctx.Err()
}
