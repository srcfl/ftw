package appuplink

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// countingModes is a mode controller that counts how often the box acted.
type countingModes struct {
	mu    sync.Mutex
	calls int
}

func (m *countingModes) SetMode(context.Context, control.Mode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return nil
}

func (m *countingModes) ObservedMode() (control.Mode, bool) {
	return control.ModeSelfConsumption, true
}

func (m *countingModes) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// handlerOver builds every session's handler over one box: one mode
// controller and one command log, as cmd/ftw wires them.
func handlerOver(modes appproto.ModeController, cmds *appproto.CmdLog) HandlerBuilder {
	return func(s appproto.Sender, caller apiauth.Caller, grants appproto.GrantReader) (*appproto.Handler, error) {
		return appproto.New(appproto.Config{
			Clock:      appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
			Site:       stubSite{},
			Info:       stubInfo{},
			Modes:      modes,
			Plans:      stubPlans{},
			Codec:      codec{},
			Sender:     s,
			Caller:     caller,
			Grants:     grants,
			Commands:   cmds,
			SrcGrid:    "meter",
			SrcPV:      "meter",
			SrcBattery: "meter",
			Logger:     slog.New(slog.DiscardHandler),
		})
	}
}

// A command retried after reconnect gets its earlier result and acts once.
func TestARetryInANewSessionActsOnce(t *testing.T) {
	r := newTunRig(t)
	app := r.pair(t)
	dev := cloudwire.DeviceID(app.staticPublic())
	cmd := setModeCmd("0192f2a0-7c1e-7000-8000-0123456789ab")
	r.open(t, app, dev, 1, nil)
	r.hello(t, app, dev, 1)
	r.send(t, app, dev, 1, appproto.MsgCmd, cmd)
	r.receive(t, app, appproto.MsgCmdAck)
	var first appproto.CmdResult
	if err := appproto.Unmarshal(r.receive(t, app, appproto.MsgCmdResult).Envelope.B, &first); err != nil {
		t.Fatal(err)
	}
	if first.State != appproto.CmdApplied || r.modes.count() != 1 {
		t.Fatalf("first command state %q, calls %d", first.State, r.modes.count())
	}
	r.receive(t, app, appproto.MsgPlan)
	r.tunnel.Drop()
	again := &appClient{static: app.static}
	r.open(t, again, dev, 2, nil)
	r.hello(t, again, dev, 2)
	r.send(t, again, dev, 2, appproto.MsgCmd, cmd)
	r.receive(t, again, appproto.MsgCmdAck)
	var retry appproto.CmdResult
	if err := appproto.Unmarshal(r.receive(t, again, appproto.MsgCmdResult).Envelope.B, &retry); err != nil {
		t.Fatal(err)
	}
	if retry.State != first.State || r.modes.count() != 1 {
		t.Fatalf("retry state %q, calls %d", retry.State, r.modes.count())
	}
}
