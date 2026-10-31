package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
)

// zoneServer is a box whose host clock is UTC unless known says otherwise.
// It counts saves and restarts.
func zoneServer(t *testing.T, stored string, known bool) (srv *Server, cfg *config.Config, saves *int, restarts chan struct{}) {
	t.Helper()
	local := time.Local
	time.Local = time.UTC
	wasKnown := householdZoneKnown
	householdZoneKnown = func() bool { return known }
	t.Cleanup(func() { time.Local, householdZoneKnown = local, wasKnown })

	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	cfg = &config.Config{Site: config.Site{TimeZone: stored}}
	saves = new(int)
	restarts = make(chan struct{}, 1)
	srv = New(&Deps{
		Ctrl: control.NewState(0, 42, ""), CtrlMu: &ctrlMu,
		Cfg: cfg, CfgMu: &cfgMu,
		ConfigPath: t.TempDir() + "/config.yaml",
		SaveConfig: func(_ string, c *config.Config) error {
			*saves++
			return nil
		},
		Restart: func(context.Context) error {
			restarts <- struct{}{}
			return nil
		},
	})
	return srv, cfg, saves, restarts
}

// Erik's box: Docker on UTC, nothing stored. The phone's zone becomes the
// household clock, and Core restarts to run on it.
func TestABoxOnUTCTakesThePhonesZoneAndRestarts(t *testing.T) {
	srv, cfg, saves, restarts := zoneServer(t, "", false)
	answer, err := srv.ProposeTimeZone("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	if !answer.Taken || answer.Zone != "Europe/Stockholm" {
		t.Fatalf("answer = %+v, want Europe/Stockholm taken", answer)
	}
	if *saves != 1 || cfg.Site.TimeZone != "Europe/Stockholm" {
		t.Fatalf("saves=%d live zone %q, want one save of Europe/Stockholm", *saves, cfg.Site.TimeZone)
	}
	select {
	case <-restarts:
	case <-time.After(2 * time.Second):
		t.Fatal("Core did not restart onto the household clock")
	}
}

// A zone with UTC's rules needs no restart: the clock already reads it.
func TestAZoneWithUTCRulesIsStoredWithoutARestart(t *testing.T) {
	srv, _, saves, restarts := zoneServer(t, "", false)
	if answer, err := srv.ProposeTimeZone("Atlantic/Reykjavik"); err != nil || !answer.Taken {
		t.Fatalf("answer %+v err %v", answer, err)
	}
	if *saves != 1 {
		t.Fatalf("saves = %d, want 1", *saves)
	}
	select {
	case <-restarts:
		t.Fatal("restarted for a zone on the clock it already runs")
	case <-time.After(400 * time.Millisecond):
	}
}

// A phone abroad never moves a house's clock the site keeps.
func TestAStoredZoneIsKept(t *testing.T) {
	srv, cfg, saves, _ := zoneServer(t, "Europe/Stockholm", false)
	answer, err := srv.ProposeTimeZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	if answer.Taken || answer.Zone != "Europe/Stockholm" || *saves != 0 || cfg.Site.TimeZone != "Europe/Stockholm" {
		t.Fatalf("answer %+v saves %d zone %q, want Europe/Stockholm kept", answer, *saves, cfg.Site.TimeZone)
	}
}

// A native install keeps the host zone it plans on today.
func TestAHostZoneIsKept(t *testing.T) {
	srv, cfg, saves, _ := zoneServer(t, "", true)
	answer, err := srv.ProposeTimeZone("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	if answer.Taken || *saves != 0 || cfg.Site.TimeZone != "" {
		t.Fatalf("answer %+v saves %d zone %q, want the host zone kept", answer, *saves, cfg.Site.TimeZone)
	}
}

func TestAZoneNoClientCanLoadIsRefused(t *testing.T) {
	srv, _, saves, _ := zoneServer(t, "", false)
	for _, zone := range []string{"", "Local", "Mars/Olympus"} {
		if _, err := srv.ProposeTimeZone(zone); err == nil {
			t.Fatalf("took %q", zone)
		}
	}
	if *saves != 0 {
		t.Fatalf("saves = %d, want none", *saves)
	}
}
