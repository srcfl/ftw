// Package appuplink serves encrypted app sessions through FTW Cloud.
package appuplink

import (
	"context"
	"errors"
	"log/slog"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
)

// Options wires the box's app sessions to the Cloud tunnel.
type Options struct {
	Enroll  *appenroll.Identity
	Handler HandlerBuilder
	Logger  *slog.Logger
	// Arrivals hears each finished driver poll. Nil sends on the beat alone.
	Arrivals <-chan struct{}
}

// HandlerBuilder builds one message handler after the handshake proves the
// caller's key. The grant reader checks that the caller still has access.
type HandlerBuilder func(appproto.Sender, apiauth.Caller, appproto.GrantReader) (*appproto.Handler, error)

// Uplink serves app sessions on the box's existing FTW Cloud connection.
type Uplink struct {
	tunnel *Tunnel
}

// DropSessionsByAppKey ends every session a revoked key holds.
func (u *Uplink) DropSessionsByAppKey(pub []byte) int {
	return u.tunnel.dropByAppKey(pub)
}

// New builds the Cloud tunnel's app session table.
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
	return &Uplink{tunnel: newTunnel(opts.Handler, opts.Enroll, &slots{}, opts.Arrivals,
		opts.Logger.With("component", "appuplink"))}, nil
}

// Run serves phone sessions carried by FTW Cloud until the context ends.
func (u *Uplink) Run(ctx context.Context) error {
	u.tunnel.Run(ctx)
	return ctx.Err()
}
