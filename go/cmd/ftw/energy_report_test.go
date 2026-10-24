package main

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/fleetping"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/statsrelay"
	"github.com/srcfl/ftw/go/internal/statsrelay/statsrelaytest"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// Both reports read one answer: part of FTW Cloud, on for a box without it
// until the owner turns it off. The energy report also leaves out a site a
// partner controls.
func TestBothReportsReadTheOneStatisticsAnswer(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{FleetPing: &config.FleetPing{Enabled: true}}
	cfgMu := &sync.RWMutex{}
	answer := statsAnswer{st: st}
	energy, fleet := energyReportAllowed(cfg, cfgMu, answer), answer.shares
	check := func(name string, wantEnergy, wantFleet bool) {
		t.Helper()
		if energy() != wantEnergy || fleet() != wantFleet {
			t.Fatalf("%s: energy %v fleet %v, want %v %v", name, energy(), fleet(), wantEnergy, wantFleet)
		}
	}

	check("before the stored state is read", false, false)
	migrateStatistics(cfg, cfgMu, st, nil)
	check("a box without Cloud, never asked", true, true)

	// The old switch is read once and is no longer a second setting.
	cfg.FleetPing.Enabled = false
	check("the retired fleet switch", true, true)

	cfg.Nova = &config.Nova{Enabled: true}
	check("a partner controls the site", false, true)
	cfg.Nova = nil

	if err := st.SaveConfig("statistics/share_v1", "off"); err != nil {
		t.Fatal(err)
	}
	check("the owner turned statistics off", false, false)

	// A stored Cloud grant counts only through a running link, which honours
	// it only with its key (cloudlink's tests). Without one, the box's own
	// answer stands.
	grant, _ := json.Marshal(map[string]any{"policy": cloudwire.CloudTermsPolicy, "purposes": []string{"serve", "stats"}})
	if err := st.SaveConfig("cloud/grant_v1", string(grant)); err != nil {
		t.Fatal(err)
	}
	check("a stored grant with no link", false, false)
}

// A box that shares only by default posts nothing straight to FTW Cloud: its
// fleet report waits for the relay. A yes, the relay or the household's own
// collector lets it go.
func TestADefaultBoxSendsNothingStraightToFTWCloud(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	migrateStatistics(&config.Config{}, &sync.RWMutex{}, st, nil)
	answer := statsAnswer{st: st}
	client, _, _ := statsrelaytest.New(t, time.Now())
	direct, _ := fleetProvider(config.DefaultFleetPingEndpoint, nil)
	relayed, _ := fleetProvider(config.DefaultFleetPingEndpoint, client)
	own, _ := fleetProvider("https://collector.example.test/fleet", nil)
	if fleetReportAllowed(answer, config.DefaultFleetPingEndpoint, direct)() {
		t.Fatal("a box that never said yes posted straight to FTW Cloud")
	}
	if !fleetReportAllowed(answer, config.DefaultFleetPingEndpoint, relayed)() || !fleetReportAllowed(answer, "https://collector.example.test/fleet", own)() {
		t.Fatal("the relay or the household's collector was held back")
	}
	if err := st.SaveConfig("statistics/share_v1", "on"); err != nil {
		t.Fatal(err)
	}
	if !fleetReportAllowed(answer, config.DefaultFleetPingEndpoint, direct)() {
		t.Fatal("a yes did not send")
	}
}

// A fleet report switched off before this release stays off.
func TestASwitchedOffFleetReportStaysOff(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{FleetPing: &config.FleetPing{Enabled: false}}
	migrateStatistics(cfg, &sync.RWMutex{}, st, nil)
	if (statsAnswer{st: st}).shares() {
		t.Fatal("an earlier off was turned on")
	}
}

// A build with a relay sends the fleet report to FTW Cloud only through it.
// A collector the household chose is posted to directly, and a build without
// a relay keeps the direct path.
func TestTheFleetReportTakesTheRelayToFTWCloud(t *testing.T) {
	client, _, _ := statsrelaytest.New(t, time.Now())
	for name, c := range map[string]struct {
		endpoint string
		relay    bool
		want     bool
	}{
		"FTW Cloud with a relay":    {config.DefaultFleetPingEndpoint, true, true},
		"FTW Cloud without a relay": {config.DefaultFleetPingEndpoint, false, false},
		"a household's collector":   {"https://collector.example.test/fleet", true, false},
	} {
		var relay *statsrelay.Client
		if c.relay {
			relay = client
		}
		p, err := fleetProvider(c.endpoint, relay)
		if err != nil {
			t.Fatal(err)
		}
		if _, through := p.(*fleetping.RelayProvider); through != c.want {
			t.Fatalf("%s: through the relay %v", name, through)
		}
	}
}
