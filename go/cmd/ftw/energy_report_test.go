package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/fleetping"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/statsrelay"
	"github.com/srcfl/ftw/go/internal/statsrelay/statsrelaytest"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// quietSite and quietRoster let a Cloud link be built that never connects.
type quietSite struct{}

func (quietSite) Live() cloudlink.Live             { return cloudlink.Live{} }
func (quietSite) Loadpoints() (cloudwire.LP, bool) { return cloudwire.LP{}, false }
func (quietSite) Prices() (cloudwire.Prices, bool) { return cloudwire.Prices{}, false }

type quietRoster struct{}

func (quietRoster) Devices() []cloudlink.RosterDevice  { return nil }
func (quietRoster) Pending() ([]byte, time.Time, bool) { return nil, time.Time{}, false }
func (quietRoster) Noise() []byte                      { return nil }

// statsLink is a Cloud link on st that never connects, for the answer alone.
func statsLink(t *testing.T, st *state.Store) *cloudlink.Link {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	link, err := cloudlink.New(cloudlink.Options{Site: quietSite{}, Roster: quietRoster{}, KV: st,
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: []*ecdsa.PublicKey{&key.PublicKey},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return link
}

func openStatsStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// Both reports read one answer: FTW Cloud under the current terms, and
// nothing else (Fredrik, 28 November 2026). The energy report also leaves
// out a site a partner controls.
func TestBothReportsReadTheOneStatisticsAnswer(t *testing.T) {
	st := openStatsStore(t)
	cfg := &config.Config{FleetPing: &config.FleetPing{Enabled: true}}
	cfgMu := &sync.RWMutex{}
	link := statsLink(t, st)
	retireLocalStatistics(st, link)
	answer := statsAnswer{link: link}
	energy, fleet := energyReportAllowed(cfg, cfgMu, answer), answer.shares
	check := func(name string, wantEnergy, wantFleet bool) {
		t.Helper()
		if energy() != wantEnergy || fleet() != wantFleet {
			t.Fatalf("%s: energy %v fleet %v, want %v %v", name, energy(), fleet(), wantEnergy, wantFleet)
		}
	}

	check("a box without Cloud", false, false)
	// An earlier release's local yes sends nothing either.
	if err := st.SaveConfig("statistics/share_v1", "on"); err != nil {
		t.Fatal(err)
	}
	check("an old local yes", false, false)

	if err := link.Grant(cloudlink.GrantRequest{Scopes: []string{protocol.ScopeLiveRead}, Policy: cloudwire.CloudTermsPolicy, By: "owner"}); err != nil {
		t.Fatal(err)
	}
	check("FTW Cloud under the current terms", true, true)

	// The old fleet switch is not a second setting.
	cfg.FleetPing.Enabled = false
	check("the retired fleet switch", true, true)

	cfg.Nova = &config.Nova{Enabled: true}
	check("a partner controls the site", false, true)
	cfg.Nova = nil

	if err := link.End("owner"); err != nil {
		t.Fatal(err)
	}
	check("FTW Cloud turned off", false, false)
}

// A box whose Cloud link did not start sends nothing, whatever an earlier
// release stored, and its old local answer is written "off".
func TestWithoutALinkNothingIsSent(t *testing.T) {
	st := openStatsStore(t)
	if err := st.SaveConfig("statistics/share_v1", "default"); err != nil {
		t.Fatal(err)
	}
	grant, _ := json.Marshal(map[string]any{"policy": cloudwire.CloudTermsPolicy, "purposes": []string{"serve", "stats"}})
	if err := st.SaveConfig("cloud/grant_v1", string(grant)); err != nil {
		t.Fatal(err)
	}
	retireLocalStatistics(st, nil)
	if (statsAnswer{}).shares() {
		t.Fatal("a box with no Cloud link shares")
	}
	if v, _ := st.LoadConfig("statistics/share_v1"); v != "off" {
		t.Fatalf("the old local answer is %q, want off", v)
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
