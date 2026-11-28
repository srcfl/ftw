package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/srcfl/ftw/go/internal/config"
)

func setHABundleEnv(t *testing.T, bundle, host, port string) {
	t.Helper()
	t.Setenv("FTW_BUNDLE", bundle)
	t.Setenv("FTW_HA_MQTT_HOST", host)
	t.Setenv("FTW_HA_MQTT_PORT", port)
	t.Setenv("FTW_HA_MQTT_USERNAME", "addons")
	t.Setenv("FTW_HA_MQTT_PASSWORD", "secret")
}

func TestHABundleBrokerComesOnlyFromTheHomeAssistantApp(t *testing.T) {
	setHABundleEnv(t, "home_assistant_addon", "core-mosquitto", "1884")
	got := haBundleBrokerFromEnv()
	want := haBundleBroker{Host: "core-mosquitto", Port: 1884, Username: "addons", Password: "secret"}
	if got == nil || *got != want {
		t.Fatalf("broker = %+v, want %+v", got, want)
	}

	setHABundleEnv(t, "home_assistant_addon", "core-mosquitto", "")
	if got := haBundleBrokerFromEnv(); got == nil || got.Port != 1883 {
		t.Fatalf("an unset port must mean 1883, got %+v", got)
	}

	for _, tc := range []struct{ name, bundle, host, port string }{
		{"native install", "", "core-mosquitto", "1883"},
		{"another bundle", "balena", "core-mosquitto", "1883"},
		{"Home Assistant has no broker", "home_assistant_addon", "", ""},
		{"port is not a number", "home_assistant_addon", "core-mosquitto", "mqtt"},
		{"port out of range", "home_assistant_addon", "core-mosquitto", "70000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHABundleEnv(t, tc.bundle, tc.host, tc.port)
			if got := haBundleBrokerFromEnv(); got != nil {
				t.Fatalf("broker = %+v, want none", got)
			}
		})
	}
}

func knownHABroker(b haBundleBroker) *haAppBroker { return &haAppBroker{known: &b} }

func TestHABundleBrokerFillsOnlyAnEmptyBroker(t *testing.T) {
	b := knownHABroker(haBundleBroker{Host: "core-mosquitto", Port: 1883, Username: "addons", Password: "secret"})

	saved := &config.HomeAssistant{Enabled: true, Broker: " ", Port: 1999, Username: "old", PublishIntervalS: 7}
	got, ready := b.fill(saved)
	if !ready || got.Broker != "core-mosquitto" || got.Port != 1883 || got.Username != "addons" || got.Password != "secret" {
		t.Fatalf("empty broker not filled: %+v", got)
	}
	if !got.Enabled || got.PublishIntervalS != 7 {
		t.Fatalf("the owner's own choices changed: %+v", got)
	}
	if saved.Broker != " " || saved.Username != "old" {
		t.Fatalf("fill changed the saved settings: %+v", saved)
	}

	own := &config.HomeAssistant{Enabled: true, Broker: "192.168.1.10", Port: 1883, Username: "ftw"}
	off := &config.HomeAssistant{Enabled: false}
	for name, cfg := range map[string]*config.HomeAssistant{"own broker": own, "turned off": off, "no settings": nil} {
		if got, ready := b.fill(cfg); got != cfg || !ready {
			t.Errorf("%s: fill = %+v, want the saved settings unchanged", name, got)
		}
	}
	if got, ready := (*haAppBroker)(nil).fill(&config.HomeAssistant{Enabled: true}); got.Broker != "" || !ready {
		t.Errorf("outside the app an empty broker must stay empty, got %q", got.Broker)
	}
	// In the app, before Home Assistant has a broker, the bridge must wait.
	if _, ready := (&haAppBroker{}).fill(&config.HomeAssistant{Enabled: true}); ready {
		t.Error("an empty broker with no broker known yet must not be ready")
	}
}

func TestHABundleBrokerTurnsOnABridgeThatWasNeverSetUp(t *testing.T) {
	b := knownHABroker(haBundleBroker{Host: "core-mosquitto", Port: 1883})
	got := b.defaultSettings(nil)
	if got == nil || !got.Enabled || got.Broker != "" || got.Password != "" {
		t.Fatalf("default = %+v, want enabled with no saved broker or password", got)
	}
	off := &config.HomeAssistant{Enabled: false}
	if b.defaultSettings(off) != off {
		t.Fatal("a bridge the owner turned off must stay off")
	}
	if (*haAppBroker)(nil).defaultSettings(nil) != nil {
		t.Fatal("outside the app the bridge must stay unset")
	}
}

// Mosquitto may come after FTW. With a Supervisor token, the app's broker
// source exists even when the app passed no broker, so the bridge defaults on
// and waits for one.
func TestHAAppBrokerAsksSupervisorWhenTheAppPassedNone(t *testing.T) {
	setHABundleEnv(t, "home_assistant_addon", "", "")
	t.Setenv("SUPERVISOR_TOKEN", "")
	if got := haAppBrokerFromEnv(); got != nil {
		t.Fatalf("without a broker or a token there is nothing to wait for, got %+v", got)
	}

	t.Setenv("SUPERVISOR_TOKEN", "token")
	a := haAppBrokerFromEnv()
	if a == nil || a.lookup == nil || a.current() != nil {
		t.Fatalf("app broker = %+v, want a Supervisor lookup and no broker yet", a)
	}
	if cfg := a.defaultSettings(nil); cfg == nil || !cfg.Enabled {
		t.Fatal("the bridge must default on so it can connect when Mosquitto comes")
	}

	setHABundleEnv(t, "", "core-mosquitto", "1883")
	if haAppBrokerFromEnv() != nil {
		t.Fatal("outside the Home Assistant app Core must not ask Supervisor")
	}
}

func supervisorStub(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/services/mqtt" || r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, "unexpected request", http.StatusForbidden)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestSupervisorMQTTFollowsTheAppsRules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   *haBundleBroker
		noneOK bool // errNoHABroker expected
	}{
		{"broker", 200, `{"result":"ok","data":{"host":"core-mosquitto","port":1884,"ssl":false,"username":"addons","password":"pw"}}`,
			&haBundleBroker{Host: "core-mosquitto", Port: 1884, Username: "addons", Password: "pw"}, false},
		{"no port means 1883", 200, `{"result":"ok","data":{"host":"core-mosquitto"}}`,
			&haBundleBroker{Host: "core-mosquitto", Port: 1883}, false},
		{"no service", 400, `{"result":"error","message":"Service not enabled"}`, nil, true},
		{"empty host", 200, `{"result":"ok","data":{"host":""}}`, nil, true},
		{"bad port", 200, `{"result":"ok","data":{"host":"h","port":70000}}`, nil, false},
		{"fractional port", 200, `{"result":"ok","data":{"host":"h","port":1883.5}}`, nil, false},
		{"needs TLS", 200, `{"result":"ok","data":{"host":"h","port":8883,"ssl":true}}`, nil, false},
		{"Supervisor fails", 500, `oops`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := supervisorStub(t, tc.status, tc.body)
			got, err := supervisorMQTT(srv.URL, "token", srv.Client())(context.Background())
			if tc.want != nil {
				if err != nil || got == nil || *got != *tc.want {
					t.Fatalf("broker = %+v, %v; want %+v", got, err, *tc.want)
				}
				return
			}
			if got != nil || err == nil {
				t.Fatalf("broker = %+v, %v; want an error", got, err)
			}
			if errors.Is(err, errNoHABroker) != tc.noneOK {
				t.Fatalf("err = %v; errNoHABroker expected: %v", err, tc.noneOK)
			}
		})
	}
}

func TestHAAppBrokerRefreshKeepsTheLastBrokerWhenSupervisorFails(t *testing.T) {
	old := &haBundleBroker{Host: "core-mosquitto", Port: 1883}
	a := &haAppBroker{known: old, lookup: func(context.Context) (*haBundleBroker, error) {
		return nil, errors.New("dial tcp: connection refused")
	}}
	if err := a.refresh(context.Background()); err == nil || a.current() != old {
		t.Fatalf("refresh = %v, broker %+v; want the error and the old broker", err, a.current())
	}

	a.lookup = func(context.Context) (*haBundleBroker, error) { return nil, errNoHABroker }
	if err := a.refresh(context.Background()); !errors.Is(err, errNoHABroker) || a.current() != nil {
		t.Fatalf("Supervisor says Mosquitto is gone; broker = %+v, err %v", a.current(), err)
	}
}

func TestHABridgeOwnerUsesHomeAssistantsBrokerForAnEmptyBroker(t *testing.T) {
	f := newFakeHAStarter()
	o := newFakeHAOwner(f)
	o.broker = knownHABroker(haBundleBroker{Host: "core-mosquitto", Port: 1883})
	defer o.Stop()

	o.Apply(&config.HomeAssistant{Enabled: true})
	if f.callCount("core-mosquitto") != 1 {
		t.Fatal("the bridge did not start with Home Assistant's broker")
	}
	o.Apply(haCfg("192.168.1.10"))
	if f.callCount("192.168.1.10") != 0 || len(f.bridges) != 1 || f.bridges[0].cfg.Broker != "192.168.1.10" {
		t.Fatal("a saved broker must replace Home Assistant's broker")
	}
}

// Mosquitto is added after FTW started: Supervisor has no broker at first,
// then names one. The bridge must start on it without a restart, and no start
// may dial an empty address while it waits.
func TestHABridgeOwnerStartsWhenMosquittoComesLater(t *testing.T) {
	f := newFakeHAStarter()
	o := newFakeHAOwner(f)
	srv, calls := supervisorStub(t, 400, `{"result":"error","message":"Service not enabled"}`)
	var mosquitto atomic.Bool
	lookup := supervisorMQTT(srv.URL, "token", srv.Client())
	o.broker = &haAppBroker{lookup: func(ctx context.Context) (*haBundleBroker, error) {
		if mosquitto.Load() {
			return &haBundleBroker{Host: "core-mosquitto", Port: 1883, Username: "addons", Password: "pw"}, nil
		}
		return lookup(ctx)
	}}
	defer o.Stop()

	o.Apply(o.broker.defaultSettings(nil))
	waitFor(t, "the owner never asked Supervisor again", func() bool { return calls.Load() >= 2 })
	if f.callCount("") != 0 || o.Bridge() != nil {
		t.Fatal("the bridge dialled an empty broker while Home Assistant had none")
	}

	mosquitto.Store(true)
	waitFor(t, "the bridge did not start once Mosquitto was there", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.bridges) == 1
	})
	f.mu.Lock()
	got := f.bridges[0].cfg
	f.mu.Unlock()
	if got.Broker != "core-mosquitto" || got.Username != "addons" || got.Password != "pw" {
		t.Fatalf("bridge settings = %+v, want Home Assistant's broker and login", got)
	}
}

// A settings save while the bridge waits must not dial an empty address
// either, and Stop must end the wait at once.
func TestHABridgeOwnerWaitingForMosquittoEndsOnStop(t *testing.T) {
	f := newFakeHAStarter()
	o := newFakeHAOwner(f)
	o.broker = &haAppBroker{lookup: func(context.Context) (*haBundleBroker, error) { return nil, errNoHABroker }}
	o.Apply(&config.HomeAssistant{Enabled: true})
	o.Apply(&config.HomeAssistant{Enabled: true, PublishIntervalS: 10})
	done := pendingRetryDone(o)
	if done == nil {
		t.Fatal("no retry is waiting for Home Assistant's broker")
	}
	o.Stop()
	assertClosed(t, done)
	if f.callCount("") != 0 {
		t.Fatal("a start dialled an empty broker")
	}
}

// The whole path on a real broker: no saved settings, the app passes Home
// Assistant's broker and login, and Home Assistant receives discovery.
func TestHABridgeOwnerAnnouncesEntitiesWithoutSettings(t *testing.T) {
	t.Parallel()
	port := freeTCPPort(t)
	login := &switchableAuth{}
	login.allow.Store(true)
	mb := startTestBroker(t, port, login)
	discovery := make(chan string, 1)
	err := mb.Subscribe("homeassistant/#", 1, func(_ *mqttserver.Client, _ packets.Subscription, pk packets.Packet) {
		if strings.HasSuffix(pk.TopicName, "/config") {
			select {
			case discovery <- pk.TopicName:
			default:
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	owner := newRealHAOwner(make(chan string, 4))
	owner.broker = knownHABroker(haBundleBroker{Host: "127.0.0.1", Port: port, Username: "ha", Password: "pw"})
	defer owner.Stop()
	owner.Apply(owner.broker.defaultSettings(nil))
	waitConnected(t, owner)

	select {
	case topic := <-discovery:
		t.Logf("first discovery topic: %s", topic)
	case <-time.After(5 * time.Second):
		t.Fatal("Home Assistant received no discovery message")
	}
	if login.refused.Load() != 0 {
		t.Fatal("the broker refused the app's login")
	}

	owner.Stop()
	closeTestBroker(t, mb)
}

// The whole path on a real broker when Mosquitto comes after FTW: Supervisor
// first has no MQTT service, then names a broker that is up. Home Assistant
// must receive discovery without a restart or a settings save.
func TestHABridgeOwnerAnnouncesEntitiesWhenMosquittoComesLater(t *testing.T) {
	t.Parallel()
	port := freeTCPPort(t)
	var mosquitto atomic.Bool
	supervisor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !mosquitto.Load() {
			http.Error(w, `{"result":"error","message":"Service not enabled"}`, http.StatusBadRequest)
			return
		}
		_, _ = fmt.Fprintf(w, `{"result":"ok","data":{"host":"127.0.0.1","port":%d,"ssl":false,"username":"ha","password":"pw"}}`, port)
	}))
	defer supervisor.Close()

	owner := newRealHAOwner(make(chan string, 4))
	owner.broker = &haAppBroker{lookup: supervisorMQTT(supervisor.URL, "token", supervisor.Client())}
	defer owner.Stop()
	owner.Apply(owner.broker.defaultSettings(nil))
	if owner.Bridge() != nil {
		t.Fatal("no bridge may be handed out before Home Assistant has a broker")
	}

	login := &switchableAuth{}
	login.allow.Store(true)
	mb := startTestBroker(t, port, login)
	discovery := make(chan string, 1)
	if err := mb.Subscribe("homeassistant/#", 1, func(_ *mqttserver.Client, _ packets.Subscription, pk packets.Packet) {
		if strings.HasSuffix(pk.TopicName, "/config") {
			select {
			case discovery <- pk.TopicName:
			default:
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	mosquitto.Store(true)
	waitConnected(t, owner)

	select {
	case <-discovery:
	case <-time.After(5 * time.Second):
		t.Fatal("Home Assistant received no discovery message")
	}
	owner.Stop()
	closeTestBroker(t, mb)
}
