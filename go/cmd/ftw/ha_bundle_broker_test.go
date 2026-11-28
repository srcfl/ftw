package main

import (
	"strings"
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

func TestHABundleBrokerFillsOnlyAnEmptyBroker(t *testing.T) {
	b := &haBundleBroker{Host: "core-mosquitto", Port: 1883, Username: "addons", Password: "secret"}

	saved := &config.HomeAssistant{Enabled: true, Broker: " ", Port: 1999, Username: "old", PublishIntervalS: 7}
	got := b.fill(saved)
	if got.Broker != "core-mosquitto" || got.Port != 1883 || got.Username != "addons" || got.Password != "secret" {
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
		if got := b.fill(cfg); got != cfg {
			t.Errorf("%s: fill = %+v, want the saved settings unchanged", name, got)
		}
	}
	if got := (*haBundleBroker)(nil).fill(&config.HomeAssistant{Enabled: true}); got.Broker != "" {
		t.Errorf("outside the app an empty broker must stay empty, got %q", got.Broker)
	}
}

func TestHABundleBrokerTurnsOnABridgeThatWasNeverSetUp(t *testing.T) {
	b := &haBundleBroker{Host: "core-mosquitto", Port: 1883}
	got := b.defaultSettings(nil)
	if got == nil || !got.Enabled || got.Broker != "" || got.Password != "" {
		t.Fatalf("default = %+v, want enabled with no saved broker or password", got)
	}
	off := &config.HomeAssistant{Enabled: false}
	if b.defaultSettings(off) != off {
		t.Fatal("a bridge the owner turned off must stay off")
	}
	if (*haBundleBroker)(nil).defaultSettings(nil) != nil {
		t.Fatal("without Home Assistant's broker the bridge must stay unset")
	}
}

func TestHABridgeOwnerUsesHomeAssistantsBrokerForAnEmptyBroker(t *testing.T) {
	f := newFakeHAStarter()
	o := newFakeHAOwner(f)
	o.broker = &haBundleBroker{Host: "core-mosquitto", Port: 1883}
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
	owner.broker = &haBundleBroker{Host: "127.0.0.1", Port: port, Username: "ha", Password: "pw"}
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
