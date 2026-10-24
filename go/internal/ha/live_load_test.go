package ha

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type statePublishClient struct {
	paho.Client
	mu       sync.Mutex
	payloads map[string][]string
}

type completedPublish struct{ paho.Token }

func (completedPublish) WaitTimeout(time.Duration) bool { return true }

func (c *statePublishClient) Publish(topic string, _ byte, _ bool, payload any) paho.Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads[topic] = append(c.payloads[topic], string(payload.([]byte)))
	return completedPublish{}
}

func (c *statePublishClient) last(t *testing.T, topic string) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	values := c.payloads[topic]
	if len(values) == 0 {
		t.Fatal("sensor state was not published")
	}
	return values[len(values)-1]
}

func liveLoadBridge(options func() telemetry.ForecastOptions) (*Bridge, *statePublishClient) {
	client := &statePublishClient{payloads: map[string][]string{}}
	return &Bridge{
		tel: telemetry.NewStore(), ctrl: &control.State{SiteMeterDriver: "meter"}, ctrlMu: &sync.Mutex{},
		client: client, topicPrefix: "ftw", measurementOptions: options,
	}, client
}

func TestLoadPublishUsesCoreRawMeasurement(t *testing.T) {
	b, client := liveLoadBridge(nil)
	b.tel.EnsureDriverHealth("meter")
	b.tel.Update("meter", telemetry.DerMeter, 1000, nil, nil)
	b.tel.Update("meter", telemetry.DerMeter, 5000, nil, nil)
	if r := b.tel.Get("meter", telemetry.DerMeter); r == nil || r.RawW == r.SmoothedW {
		t.Fatal("test requires distinct raw and smoothed readings")
	}
	b.publishState()
	if got := client.last(t, b.stateTopic("load_w")); got != "5000.00" {
		t.Fatal("HA load did not use the Core live measurement")
	}
}

func TestLoadPublishUsesCoreControlPowerPolicy(t *testing.T) {
	b, client := liveLoadBridge(func() telemetry.ForecastOptions {
		return telemetry.ForecastOptions{MaxAge: time.Second}
	})
	for _, driver := range []string{"meter", "battery"} {
		b.tel.EnsureDriverHealth(driver)
	}
	at := time.Now().Add(-3 * time.Second)
	data, err := json.Marshal(map[string]any{
		"control_power_w": -2000, "control_power_available": true,
		"control_power_observed_at": at.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal("invalid test power evidence")
	}
	b.tel.Update("meter", telemetry.DerMeter, 1000, nil, nil)
	b.tel.Update("battery", telemetry.DerBattery, -2500, nil, data)
	b.tel.DriverHealthMut("battery").WatchdogTimeoutOverride = 10 * time.Second
	b.publishState()
	if got := client.last(t, b.stateTopic("load_w")); got != "3000.00" {
		t.Fatal("HA load did not use Core AC power and driver cadence")
	}
}

func TestLoadPublishClearsUnknownAndRecovers(t *testing.T) {
	options := telemetry.ForecastOptions{}
	b, client := liveLoadBridge(func() telemetry.ForecastOptions { return options })
	b.tel.EnsureDriverHealth("meter")
	b.tel.Update("meter", telemetry.DerMeter, 1000, nil, nil)
	check := func(want string) {
		t.Helper()
		b.publishState()
		if got := client.last(t, b.stateTopic("load_w")); got != want {
			t.Fatal("HA load state did not follow Core qualification")
		}
	}
	check("1000.00")

	// A declared battery that never reported is left out, not a blank.
	options.ExpectedFlows = []telemetry.ForecastFlow{{Driver: "battery", DerType: telemetry.DerBattery}}
	check("1000.00")
	b.tel.EnsureDriverHealth("battery")
	b.tel.Update("battery", telemetry.DerBattery, -200, nil, nil)
	check("1200.00")

	b.tel.Update("meter", telemetry.DerMeter, 0, nil, nil)
	b.tel.Update("battery", telemetry.DerBattery, 0, nil, nil)
	check("0.00")

	// Unresolved topology still has a read site meter: an estimate.
	options.HouseholdInvalidReason = "unresolved_topology"
	check("0.00")
	b.tel.DriverHealthMut("meter").SetOffline()
	check("None") // HA ignores an empty numeric payload; None clears it to unknown.
	b.tel.DriverHealthMut("meter").RecordSuccess()
	options.HouseholdInvalidReason = ""
	check("0.00")
}

// HA gets one Home use: the live balance as the dashboard shows it,
// negative included.
func TestLoadPublishesOneHomeUse(t *testing.T) {
	b, client := liveLoadBridge(nil)
	for _, d := range []string{"meter", "battery"} {
		b.tel.EnsureDriverHealth(d)
	}
	b.tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	b.tel.Update("battery", telemetry.DerBattery, 2000, nil, nil)
	b.publishState()
	if got := client.last(t, b.stateTopic("load_w")); got != "-800.00" {
		t.Fatalf("display load = %s, want the negative balance as is", got)
	}
	b.publishDiscovery()
	var display, energy bool
	client.mu.Lock()
	for topic, payloads := range client.payloads {
		for _, p := range payloads {
			display = display || strings.Contains(topic, "load_power/config") && strings.Contains(p, b.stateTopic("load_w"))
			energy = energy || strings.Contains(topic, "load_energy_power/config")
		}
	}
	client.mu.Unlock()
	if !display || energy {
		t.Fatalf("discovery: display=%t energy=%t", display, energy)
	}
}
