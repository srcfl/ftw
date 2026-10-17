package appuplink

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
)

func enrolOwner(t *testing.T, enroll *appenroll.Identity) {
	t.Helper()
	code, _, err := enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newAppClient()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enroll.Authorise(app.staticPublic(), code); err != nil {
		t.Fatal(err)
	}
}

type stubSite struct{}

func (stubSite) Snapshot() appproto.Snapshot {
	return appproto.Snapshot{
		Mode:       control.ModeSelfConsumption,
		ControlRev: 1,
		GridW:      1200,
		// Negative while generating. FTW's convention is that positive watts
		// flow into the site, so PV is never positive.
		PVW:             -3000,
		BatteryW:        800,
		LoadW:           1000,
		BatterySoC:      0.55,
		BatterySoCKnown: true,
		Sources: []appproto.Source{
			{ID: "meter", Kind: "meter", Name: "P1", LastOkUptimeMs: 1000, StaleAfterMs: 5000},
		},
	}
}

type stubInfo struct{}

func (stubInfo) Identity() appproto.Identity {
	return appproto.Identity{ID: "box", Build: "test", TZ: "Europe/Stockholm"}
}
func (stubInfo) Boot() *appproto.BootProgress { return nil }

type stubModes struct{}

func (stubModes) SetMode(context.Context, control.Mode) error { return nil }
func (stubModes) ObservedMode() (control.Mode, bool)          { return control.ModeSelfConsumption, true }

type stubPlans struct{}

func (stubPlans) Latest() *mpc.Plan { return nil }
func (stubPlans) Rev() uint64       { return 0 }
func (stubPlans) CeilingW() *int64  { return nil }

// newHandler builds a handler for a test that sends no command, so each one
// gets an empty command log of its own. A test about commands builds its
// handlers over one log, as the box does.
func newHandler(
	s appproto.Sender, caller apiauth.Caller, grants appproto.GrantReader,
) (*appproto.Handler, error) {
	return appproto.New(appproto.Config{
		Clock:      appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
		Site:       stubSite{},
		Info:       stubInfo{},
		Modes:      stubModes{},
		Plans:      stubPlans{},
		Codec:      codec{},
		Sender:     s,
		Caller:     caller,
		Grants:     grants,
		Commands:   appproto.NewCmdLog(),
		SrcGrid:    "meter",
		SrcPV:      "meter",
		SrcBattery: "meter",
		Logger:     slog.New(slog.DiscardHandler),
	})
}
