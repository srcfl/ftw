package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

func TestPriceColumnsRepeatLongSlotsAndLeaveGapsNull(t *testing.T) {
	t0 := int64(1_791_763_200_000)
	q := int64(15 * 60_000)
	p := priceColumns("SE3", "SEK", 25.5, []appproto.PricePoint{
		{StartMs: t0 + 4*q, LenMin: 60, SpotMinor: 40.4, TotalMinor: 110.6},
		{StartMs: t0, LenMin: 15, SpotMinor: 41.5, TotalMinor: 112},
		{StartMs: t0 + q, LenMin: 15, SpotMinor: -2.4, TotalMinor: 60},
	})
	if p.T0Ms != t0 || p.SlotMs != q || len(p.SpotMinor) != 8 || len(p.TotalMinor) != 8 {
		t.Fatalf("columns %+v", p)
	}
	if p.VATPermille != 255 {
		t.Fatalf("VAT 25.5 %% went out as %d permille, want 255", p.VATPermille)
	}
	want := []any{int64(42), int64(-2), nil, nil, int64(40), int64(40), int64(40), int64(40)}
	for i, w := range want {
		got := p.SpotMinor[i]
		if (w == nil) != (got == nil) || (got != nil && *got != w.(int64)) {
			t.Fatalf("slot %d = %v, want %v", i, got, w)
		}
	}
}

func TestEachPlugInIsANewSession(t *testing.T) {
	c := &cloudSite{}
	if s := c.session("garage", false); s != "" {
		t.Fatalf("unplugged session %q", s)
	}
	first := c.session("garage", true)
	if again := c.session("garage", true); again != first || first == "" {
		t.Fatalf("one plug-in gave %q then %q", first, again)
	}
	c.session("garage", false)
	if next := c.session("garage", true); next == first {
		t.Fatal("a second plug-in kept the first session")
	}
	if other := c.session("drive", true); other == "" {
		t.Fatal("another loadpoint has no session")
	}
}

// The wire shape has no room for limits, phases, battery size or the car's
// name, and an unplugged car has no level.
func TestCloudLoadpointLeavesCapabilitiesAndNamesBehind(t *testing.T) {
	c := &cloudSite{}
	st := loadpoint.State{
		ID: "garage", DriverName: "easee", PluggedIn: false, CurrentSoC: 0.5,
		MaxChargeW: 11_000, MinChargeW: 1_400, Phases: 3, VehicleCapacityWh: 77_000,
		VehicleName: "Familjebilen", VehicleDriver: "tesla-fleet",
	}
	lp := c.loadpoint(st, time.Now())
	if lp.SocPermille != nil || lp.Session != "" {
		t.Fatalf("unplugged loadpoint carries a level or a session: %+v", lp)
	}
	raw, err := cloudwire.Marshal(lp)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"Familjebilen", "tesla-fleet"} {
		if bytes.Contains(raw, []byte(leak)) {
			t.Fatalf("%q is in the loadpoint", leak)
		}
	}

	st.PluggedIn, st.SoCSource, st.VehicleSoCAgeS = true, "vehicle", 90
	lp = c.loadpoint(st, time.Now())
	if lp.SocPermille == nil || *lp.SocPermille != 500 || lp.SocAgeS == nil || *lp.SocAgeS != 90 {
		t.Fatalf("plugged loadpoint %+v", lp)
	}
}

type quietCloudSite struct{}

func (quietCloudSite) Live() cloudlink.Live {
	return cloudlink.Live{Sources: map[string]cloudlink.Source{"p1": {Kind: "driver"}}}
}
func (quietCloudSite) Loadpoints() (cloudwire.LP, bool) { return cloudwire.LP{}, false }
func (quietCloudSite) Prices() (cloudwire.Prices, bool) { return cloudwire.Prices{}, false }

type noPhones struct{}

func (noPhones) Devices() []cloudlink.RosterDevice { return nil }

func (noPhones) Pending() ([]byte, time.Time, bool) { return nil, time.Time{}, false }
func (noPhones) Noise() []byte                      { return make([]byte, 32) }

func newTestCloudLink(t *testing.T, keys []*ecdsa.PublicKey) *cloudlink.Link {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	link, err := cloudlink.New(cloudlink.Options{
		Site: quietCloudSite{}, Roster: noPhones{}, KV: st,
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: keys,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return link
}

// An app session hears cloud.link only from a build that can run the uplink,
// and the session information only while an owner has FTW Cloud on.
func TestAppSessionsHearOfFTWCloudOnlyWhenItCanRun(t *testing.T) {
	base := appproto.DefaultCaps()
	var none *cloudSession
	unbound := &cloudSession{}
	release := &cloudSession{}
	release.bind(newTestCloudLink(t, []*ecdsa.PublicKey{}))
	for name, c := range map[string]*cloudSession{"no link": none, "not bound yet": unbound, "release build": release} {
		if slices.Contains(c.caps(base), appproto.CapCloudLink) || c.info() != nil {
			t.Fatalf("%s: caps %v, info %+v", name, c.caps(base), c.info())
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	link := newTestCloudLink(t, []*ecdsa.PublicKey{&key.PublicKey})
	withService := &cloudSession{}
	withService.bind(link)
	caps := withService.caps(base)
	if !slices.Contains(caps, appproto.CapCloudLink) || slices.Contains(base, appproto.CapCloudLink) {
		t.Fatalf("caps %v from %v", caps, base)
	}
	if withService.info() != nil {
		t.Fatal("session information before a grant")
	}
	if err := link.Grant(cloudlink.GrantRequest{Scopes: []string{appproto.ScopeLiveRead}, Policy: "2026-10"}); err != nil {
		t.Fatal(err)
	}
	info := withService.info()
	if info == nil || info.Home != link.Status().Home || len(info.Sources) != 1 {
		t.Fatalf("session information %+v", info)
	}
	for id, name := range info.Sources {
		if name != "p1" || len(id) != 11 {
			t.Fatalf("source %q named %q", id, name)
		}
	}
	if slices.Contains(caps, appproto.CapCloudTun) {
		t.Fatalf("cloud.tun from a link that takes no tun: %v", caps)
	}
}

// A phone hears cloud.tun only from a box whose link carries its session, so
// a phone that knows the home id never waits on a tunnel the box cannot take.
func TestAppSessionsHearOfTheTunnelOnlyWhenTheLinkTakesIt(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keys := []*ecdsa.PublicKey{&key.PublicKey}
	open := func(tunnel cloudlink.Tunnel, keys []*ecdsa.PublicKey) []string {
		st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		link, err := cloudlink.New(cloudlink.Options{
			Site: quietCloudSite{}, Roster: noPhones{}, KV: st, Tunnel: tunnel,
			KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: keys,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		c := &cloudSession{}
		c.bind(link)
		return c.caps(appproto.DefaultCaps())
	}
	if caps := open(nil, keys); slices.Contains(caps, appproto.CapCloudTun) {
		t.Fatalf("no tunnel: %v", caps)
	}
	if caps := open(nopTunnel{}, []*ecdsa.PublicKey{}); slices.Contains(caps, appproto.CapCloudTun) || slices.Contains(caps, appproto.CapCloudLink) {
		t.Fatalf("a build that names no service: %v", caps)
	}
	if caps := open(nopTunnel{}, keys); !slices.Contains(caps, appproto.CapCloudTun) || !slices.Contains(caps, appproto.CapCloudLink) {
		t.Fatalf("a link with a tunnel: %v", caps)
	}
}

type nopTunnel struct{}

func (nopTunnel) Deliver(cloudwire.Tun) {}
func (nopTunnel) Drop()                 {}

// FTW Cloud's live body is the same field set the app session streams, and
// each phase follows the site meter's freshness there too. The fuse is
// config, so it names no driver.
func TestCloudLiveCarriesThePhasesUnderTheMeter(t *testing.T) {
	tel, ctrl := seedSite(t)
	tel.Update("meter", telemetry.DerMeter, 160, nil, json.RawMessage(
		`{"l1_w":-367,"l2_w":908,"l3_w":-381,"l1_a":2.13,"l2_a":4.76,"l3_a":1.61}`))
	ctrl.SiteFuseAmps, ctrl.SiteFusePhases = 20, 3
	c := &cloudSite{site: &appSite{
		tel: tel, ctrl: ctrl, ctrlMu: &sync.Mutex{},
		revision: &control.Revision{}, started: time.Now(),
		siteMeterStale: time.Minute,
	}}

	live := c.Live()

	want := map[appproto.Fid]int64{
		appproto.FidGridL1Da: -21, appproto.FidGridL2Da: 48, appproto.FidGridL3Da: -16, appproto.FidGridFuseDa: 200,
	}
	for fid, v := range want {
		key := strconv.Itoa(int(fid))
		if got, ok := live.Fields[key]; !ok || got != v {
			t.Fatalf("field %s = %d (present %v), want %d", key, got, ok, v)
		}
	}
	for _, fid := range []appproto.Fid{appproto.FidGridL1Da, appproto.FidGridL2Da, appproto.FidGridL3Da} {
		key := strconv.Itoa(int(fid))
		if got := live.FieldSources[key]; !slices.Equal(got, []string{"meter"}) {
			t.Fatalf("field %s follows %v, want the meter", key, got)
		}
	}
	if got, ok := live.FieldSources[strconv.Itoa(int(appproto.FidGridFuseDa))]; ok {
		t.Fatalf("the fuse follows %v; it is config", got)
	}
}

// FTW Cloud's lp.session is the plug-in's id from the cars runtime, the
// same across a restart, which a tap after car.which names.
func TestCloudSessionIsTheCarsRuntimes(t *testing.T) {
	c := &cloudSite{}
	st := loadpoint.State{ID: "garage", DriverName: "easee", PluggedIn: true, Session: "7d1f0c2a9b3e4f50"}
	if lp := c.loadpoint(st, time.Now()); lp.Session != "7d1f0c2a9b3e4f50" {
		t.Fatalf("session %q", lp.Session)
	}
	st.Session = ""
	if lp := c.loadpoint(st, time.Now()); lp.Session == "" {
		t.Fatal("without the cars runtime the plug-in count is the session")
	}
	// The session's car by the box's id: the link cuts it per grant.
	st.CarID, st.CarIdentifiedBy, st.Choose = "0123456789abcdef0123456789abcdef", "tap", nil
	if lp := c.loadpoint(st, time.Now()); lp.CarID != st.CarID || lp.CarIdentifiedBy != "tap" {
		t.Fatalf("car %q by %q", lp.CarID, lp.CarIdentifiedBy)
	}
	st.PluggedIn = false
	if lp := c.loadpoint(st, time.Now()); lp.CarID != "" {
		t.Fatal("an unplugged charger names a car")
	}
}
