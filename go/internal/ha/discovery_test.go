package ha

import (
	"encoding/json"
	"sync"
	"testing"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// discoveryClient keeps the last payload and retained flag per topic.
type discoveryClient struct {
	paho.Client
	mu       sync.Mutex
	payloads map[string][]byte
	retained map[string]bool
	sends    map[string]int
}

func (c *discoveryClient) Publish(topic string, _ byte, retained bool, payload any) paho.Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads[topic] = append([]byte(nil), payload.([]byte)...)
	c.retained[topic] = retained
	c.sends[topic]++
	return completedPublish{}
}

func (c *discoveryClient) config(t *testing.T, topic string) map[string]any {
	t.Helper()
	c.mu.Lock()
	raw, ok := c.payloads[topic]
	c.mu.Unlock()
	if !ok {
		t.Fatalf("no discovery message on %s", topic)
	}
	if len(raw) == 0 {
		return nil
	}
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("%s: %v", topic, err)
	}
	return msg
}

type onePlan struct{}

func (onePlan) LatestActions() []PlanAction { return nil }

func discoveryBridge(drivers ...string) (*Bridge, *discoveryClient) {
	client := &discoveryClient{payloads: map[string][]byte{}, retained: map[string]bool{}, sends: map[string]int{}}
	return &Bridge{
		tel: telemetry.NewStore(), ctrl: &control.State{}, ctrlMu: &sync.Mutex{},
		client: client, plan: onePlan{}, driverNames: drivers,
		topicPrefix: "forty-two-watts", discoPrefix: "homeassistant", deviceID: "forty_two_watts",
		swVersion:               "0.171.0",
		announcedMetrics:        map[string]struct{}{},
		announcedEVDrivers:      map[string]struct{}{},
		announcedVehicleDrivers: map[string]struct{}{},
	}, client
}

func sensorTopic(id string) string { return "homeassistant/sensor/forty_two_watts/" + id + "/config" }

func TestDiscoveryDeviceNamesCoreVersion(t *testing.T) {
	b, client := discoveryBridge()
	b.publishDiscovery()
	dev, _ := client.config(t, sensorTopic("grid_power"))["device"].(map[string]any)
	if dev["sw_version"] != "0.171.0" {
		t.Fatalf("device = %v, want sw_version 0.171.0", dev)
	}
	if _, ok := dev["configuration_url"]; ok {
		t.Fatal("no reliable address exists for configuration_url; it must stay out")
	}
	if dev["name"] != "FTW" || dev["identifiers"].([]any)[0] != "forty_two_watts" {
		t.Fatalf("device identity changed: %v", dev)
	}

	b.swVersion = ""
	b.publishDiscovery()
	dev, _ = client.config(t, sensorTopic("grid_power"))["device"].(map[string]any)
	if _, ok := dev["sw_version"]; ok {
		t.Fatal("an unknown version must be left out, not sent empty")
	}
}

// Names, object ids and unique ids are stable wire identifiers. This guards
// the ones this change touched.
func TestDiscoveryKeepsNamesAndUniqueIDs(t *testing.T) {
	b, client := discoveryBridge("inv")
	b.ctrl.BatteryCoversEV = true
	b.tel.EmitMetric("inv", "battery_temp_c", 21, "°C", "", "")
	b.publishDiscovery()
	for topic, want := range map[string][2]string{
		sensorTopic("grid_target"):                                      {"Grid Target", "forty_two_watts_grid_target"},
		sensorTopic("peak_import_ceiling"):                              {"Peak Import Ceiling", "forty_two_watts_peak_import_ceiling"},
		sensorTopic("phase_1_current"):                                  {"Phase L1 Current", "forty_two_watts_phase_1_current"},
		sensorTopic("today_import"):                                     {"Today Grid Import", "forty_two_watts_today_import"},
		sensorTopic("inv_bat_w"):                                        {"inv Battery Power", "forty_two_watts_invbat_w"},
		sensorTopic("inv_battery_temp_c"):                               {"inv battery temp c", "forty_two_watts_inv_battery_temp_c"},
		"homeassistant/binary_sensor/forty_two_watts/inv_online/config": {"inv Online", "forty_two_watts_inv_online"},
		"homeassistant/binary_sensor/forty_two_watts/plan_stale/config": {"Plan Stale", "forty_two_watts_plan_stale"},
		"homeassistant/switch/forty_two_watts/battery_covers_ev/config": {"Legacy Site-wide Battery Cover", "forty_two_watts_battery_covers_ev_cmd"},
		"homeassistant/number/forty_two_watts/grid_target/config":       {"Grid Target", "forty_two_watts_grid_target_cmd"},
	} {
		msg := client.config(t, topic)
		if msg["name"] != want[0] || msg["unique_id"] != want[1] {
			t.Errorf("%s: name %v, unique_id %v; want %q, %q", topic, msg["name"], msg["unique_id"], want[0], want[1])
		}
	}
}

func TestDriverEntitiesHaveStateClasses(t *testing.T) {
	b, client := discoveryBridge("inv")
	for _, m := range []struct {
		name, unit string
		value      float64
	}{
		{"battery_temp_c", "°C", 21},
		{"battery_soc_max_pct", "", 95},
		{"meter_l1_v", "V", 230},
		{"ev_l1_a", "", 6},
		{"grid_frequency_hz", "", 50},
		{"battery_available_charge_w", "", 3000},
		{"ev_lifetime_wh", "", 1.2e6},
		{"ev_lifetime_kwh", "", 1200},
		{"meter_total_import_wh", "Wh", 5e6},
		{"ev_session_wh", "Wh", 9000},
		{"battery_capacity_wh", "", 10000},
		{"hp_energy_consumed_kwh", "Wh", 5.4e6},
		{"sungrow_dc_fault_bits", "", 0},
		{"extapi_ack_count", "", 4},
	} {
		b.tel.EmitMetric("inv", m.name, m.value, m.unit, "", "")
	}
	b.publishDiscovery()

	want := map[string]string{
		// emit_metric: values at one moment
		"inv_battery_temp_c":             "measurement",
		"inv_battery_soc_max_pct":        "measurement",
		"inv_meter_l1_v":                 "measurement",
		"inv_ev_l1_a":                    "measurement",
		"inv_grid_frequency_hz":          "measurement",
		"inv_battery_available_charge_w": "measurement",
		// emit_metric: energy that only grows
		"inv_ev_lifetime_wh":        "total_increasing",
		"inv_ev_lifetime_kwh":       "total_increasing",
		"inv_meter_total_import_wh": "total_increasing",
		// emit_metric: energy that can fall, and values without a unit
		"inv_ev_session_wh":          "",
		"inv_battery_capacity_wh":    "",
		"inv_hp_energy_consumed_kwh": "",
		"inv_sungrow_dc_fault_bits":  "",
		"inv_extapi_ack_count":       "",
		// structured readings
		"inv_meter_w":             "measurement",
		"inv_pv_w":                "measurement",
		"inv_bat_w":               "measurement",
		"inv_bat_soc_pct":         "measurement",
		"inv_v2x_w":               "measurement",
		"inv_v2x_vehicle_soc_pct": "measurement",
	}
	for id, sc := range want {
		msg := client.config(t, sensorTopic(id))
		got, _ := msg["state_class"].(string)
		if got != sc {
			t.Errorf("%s: state_class %q, want %q (unit %v, class %v)", id, got, sc, msg["unit_of_measurement"], msg["device_class"])
		}
	}
	if u := client.config(t, sensorTopic("inv_ev_lifetime_kwh"))["unit_of_measurement"]; u != "kWh" {
		t.Errorf("ev_lifetime_kwh unit = %v, want kWh", u)
	}

	b.announceEVDriver(b.discoveryDevice(), "charger")
	b.announceVehicleDriver(b.discoveryDevice(), "car")
	for _, id := range []string{"charger_ev_w", "car_vehicle_soc"} {
		if sc := client.config(t, sensorTopic(id))["state_class"]; sc != "measurement" {
			t.Errorf("%s: state_class %v, want measurement", id, sc)
		}
	}
}

func TestDiagnosticEntitiesAreMarked(t *testing.T) {
	b, client := discoveryBridge("inv")
	b.tel.EmitMetric("inv", "battery_temp_c", 21, "°C", "", "")
	b.publishDiscovery()

	diagnostic := []string{
		sensorTopic("grid_target"), sensorTopic("peak_limit"), sensorTopic("peak_import_ceiling"), sensorTopic("ev_charging"),
		sensorTopic("phase_1_power"), sensorTopic("phase_2_power"), sensorTopic("phase_3_power"),
		sensorTopic("phase_1_current"), sensorTopic("phase_2_current"), sensorTopic("phase_3_current"),
		sensorTopic("inv_battery_temp_c"),
		"homeassistant/binary_sensor/forty_two_watts/inv_online/config",
		"homeassistant/binary_sensor/forty_two_watts/plan_stale/config",
	}
	primary := []string{
		sensorTopic("grid_power"), sensorTopic("pv_power"), sensorTopic("battery_power"), sensorTopic("battery_soc"),
		sensorTopic("load_power"), sensorTopic("today_import"), sensorTopic("pv_predicted"),
		sensorTopic("inv_bat_w"), sensorTopic("inv_bat_soc_pct"),
		"homeassistant/sensor/forty_two_watts/plan/config",
		"homeassistant/sensor/forty_two_watts/price/config",
		"homeassistant/select/forty_two_watts/mode/config",
		"homeassistant/number/forty_two_watts/grid_target/config",
		"homeassistant/number/forty_two_watts/peak_limit/config",
		"homeassistant/number/forty_two_watts/ev_charging/config",
	}
	for _, topic := range diagnostic {
		if c := client.config(t, topic)["entity_category"]; c != "diagnostic" {
			t.Errorf("%s: entity_category %v, want diagnostic", topic, c)
		}
	}
	for _, topic := range primary {
		if c, ok := client.config(t, topic)["entity_category"]; ok {
			t.Errorf("%s: entity_category %v, want none", topic, c)
		}
	}
}

// The older site-wide battery cover is shown in Home Assistant only while it
// is on, as on FTW's own pages. An empty retained config removes it.
func TestLegacyCoverSwitchShowsOnlyWhileOn(t *testing.T) {
	const topic = "homeassistant/switch/forty_two_watts/battery_covers_ev/config"
	b, client := discoveryBridge()

	b.publishDiscovery()
	if msg := client.config(t, topic); msg != nil || !client.retained[topic] {
		t.Fatalf("with the setting off the switch must be removed by an empty retained config, got %v", msg)
	}
	off := b.SensorsAnnounced()

	b.publishState()
	b.publishState()
	if client.sends[topic] != 1 {
		t.Fatalf("an unchanged setting was sent %d times", client.sends[topic])
	}

	b.ctrl.BatteryCoversEV = true
	b.publishState()
	msg := client.config(t, topic)
	if msg["unique_id"] != "forty_two_watts_battery_covers_ev_cmd" || msg["command_topic"] != "forty-two-watts/cmd/battery_covers_ev" || !client.retained[topic] {
		t.Fatalf("turning the setting on must announce the switch, got %v", msg)
	}
	b.publishDiscovery()
	if b.SensorsAnnounced() != off+1 {
		t.Fatalf("SensorsAnnounced = %d, want %d with the switch shown", b.SensorsAnnounced(), off+1)
	}

	// Turning it off from Home Assistant removes the switch again.
	b.ctrl.BatteryCoversEV = false
	b.publishState()
	if msg := client.config(t, topic); msg != nil {
		t.Fatalf("the switch must go away once the setting is off, got %v", msg)
	}
}

func TestMetricStateClassNeedsALifetimeName(t *testing.T) {
	for name, want := range map[string]bool{
		"ev_lifetime_wh":        true,
		"meter_total_export_wh": true,
		"pv_total_import_kwh":   true,
		"ev_session_wh":         false,
		"battery_capacity_wh":   false,
		"today_pv_wh":           false,
		"hp_lifetimer_wh":       false, // "lifetime" must be a whole word
	} {
		if got := lifetimeEnergyCounter(name); got != want {
			t.Errorf("lifetimeEnergyCounter(%q) = %v, want %v", name, got, want)
		}
	}
}
