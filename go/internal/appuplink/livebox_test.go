package appuplink

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// TestLiveBoxForTheiPhone is a box for the iPhone's live tests in
// srcfl/ftw-app (LiveTunnelTests, LiveLatencyTests): the real uplink, cloud
// link, tunnel table and handler, linked to a local ftwcloudd, with the
// relay unreachable. Off unless FTW_LIVE_BOX names a directory.
//
//	FTW_LIVE_BOX=<dir>   holds the service's ca.pem and service.pub; gets
//	                     pair.url, arrivals.log and tun.log
//	FTW_LIVE_BOX_URL     the service's /v1/box, wss://127.0.0.1:18843/v1/box
//	FTW_LIVE_BOX_DRIVERS how many pretend drivers read, 1 by default
//	FTW_LIVE_BOX_PERIOD_MS how often each reads, 1000 by default
//
// Each pretend driver reads at a random 0.8 to 1.2 times its period, so its
// readings land at every phase of any clock the box keeps. The grid reading is the
// wall-clock millisecond the newest arrived, so the phone can time it. arrivals.log has each
// arrival and tun.log each tun the box sent, in wall-clock microseconds.
// The box serves until <dir>/stop appears.
func TestLiveBoxForTheiPhone(t *testing.T) {
	dir := os.Getenv("FTW_LIVE_BOX")
	if dir == "" {
		t.Skip("set FTW_LIVE_BOX")
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	svcRaw, _ := base64.RawURLEncoding.DecodeString(read("service.pub"))
	svcPub, err := cloudwire.ParsePublicKey(svcRaw)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(read("ca.pem")))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logs := func(name string) (func(string, ...any), func()) {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		w := bufio.NewWriter(f)
		var mu sync.Mutex
		return func(format string, a ...any) {
				mu.Lock()
				fmt.Fprintf(w, format, a...)
				mu.Unlock()
			}, func() {
				mu.Lock()
				_ = w.Flush()
				_ = f.Close()
				mu.Unlock()
			}
	}

	enroll, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	site := &clockSite{}
	site.ms.Store(start.UnixMilli())
	modes := &countingModes{}
	cmds := appproto.NewCmdLog()
	var bound atomic.Pointer[cloudlink.Link]
	caps := append(appproto.DefaultCaps(), appproto.CapCloudLink, appproto.CapCloudTun)
	build := func(s appproto.Sender, caller apiauth.Caller, grants appproto.GrantReader) (*appproto.Handler, error) {
		return appproto.New(appproto.Config{
			Clock: appproto.SystemClock{StartedAt: start, Source: "ntp"},
			Site:  site, Info: stubInfo{}, Modes: modes, Plans: stubPlans{}, Codec: codec{},
			Sender: s, Caller: caller, Grants: grants, Commands: cmds, Caps: caps,
			Cloud: func() *appproto.CloudSession {
				if l := bound.Load(); l != nil {
					return l.SessionInfo()
				}
				return nil
			},
			SrcGrid: "meter", SrcPV: "meter", SrcBattery: "meter", Logger: logger,
		})
	}
	arrivals := make(chan struct{}, 1)
	uplink, err := New(Options{Enroll: enroll, Handler: build, Arrivals: arrivals, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	link, err := cloudlink.New(cloudlink.Options{
		Site: quietSite{}, Roster: CloudRoster{Enroll: enroll, Tunnel: true}, KV: &memState{m: map[string]string{}},
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), Build: "live", Tunnel: uplink.Tunnel(),
		Endpoint: os.Getenv("FTW_LIVE_BOX_URL"), ServiceKeys: []*ecdsa.PublicKey{svcPub}, RootCAs: pool,
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	tunLog, closeTun := logs("tun.log")
	defer closeTun()
	uplink.Tunnel().Attach(timedTun{inner: link, log: tunLog})
	bound.Store(link)
	enroll.SetHome(link.Home)
	enroll.OnChange(link.RosterChanged)
	enroll.OnPairingCode(link.RosterChanged)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = uplink.Run(ctx) }()
	go func() { _ = link.Run(ctx) }()

	arrivalLog, closeArrivals := logs("arrivals.log")
	defer closeArrivals()
	drivers, _ := strconv.Atoi(os.Getenv("FTW_LIVE_BOX_DRIVERS"))
	period, _ := strconv.Atoi(os.Getenv("FTW_LIVE_BOX_PERIOD_MS"))
	if period <= 0 {
		period = 1000
	}
	var arriving sync.Mutex
	for range max(drivers, 1) {
		go func() {
			for ctx.Err() == nil {
				time.Sleep(time.Duration(period*4/5+rand.IntN(period*2/5+1)) * time.Millisecond)
				arriving.Lock()
				now := time.Now()
				site.ms.Store(now.UnixMilli())
				site.okUpMs.Store(now.Sub(start).Milliseconds())
				arrivalLog("%d %d\n", now.UnixMicro(), now.UnixMilli())
				arriving.Unlock()
				select {
				case arrivals <- struct{}{}:
				default:
				}
			}
		}()
	}

	code, _, err := enroll.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !link.Status().Connected || link.Status().Home == "" {
		if time.Now().After(deadline) {
			t.Fatalf("the box never linked: %+v", link.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	link.RosterChanged()
	time.Sleep(300 * time.Millisecond)
	pairURL, err := enroll.EnrollmentURL(code, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pair.url"), []byte(pairURL), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("box linked as %s; pairing link written", link.Status().Home)

	stop := filepath.Join(dir, "stop")
	_ = os.Remove(stop)
	for end := time.Now().Add(30 * time.Minute); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if _, err := os.Stat(stop); err == nil {
			break
		}
	}
	t.Logf("done: mode set %d times", modes.count())
}

// clockSite is a box whose grid reading is the wall-clock millisecond its
// pretend driver last read, and whose meter last answered then.
type clockSite struct {
	ms     atomic.Int64
	okUpMs atomic.Int64
}

func (c *clockSite) Snapshot() appproto.Snapshot {
	return appproto.Snapshot{
		Mode: control.ModeSelfConsumption, ControlRev: 1,
		GridW: float64(c.ms.Load()), PVW: -3000, BatteryW: 800, LoadW: 1000,
		BatterySoC: 0.55, BatterySoCKnown: true,
		Sources: []appproto.Source{{ID: "meter", Kind: "driver", Name: "meter",
			LastOkUptimeMs: c.okUpMs.Load(), StaleAfterMs: 5000}},
	}
}

// timedTun logs each tun the box sends: when, how long and which lane.
type timedTun struct {
	inner TunSender
	log   func(string, ...any)
}

func (t timedTun) SendTun(m cloudwire.Tun, bulk bool) error {
	at := time.Now().UnixMicro()
	err := t.inner.SendTun(m, bulk)
	t.log("%d %d %v\n", at, len(m.D), bulk)
	return err
}
