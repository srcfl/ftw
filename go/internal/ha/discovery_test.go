package ha

import (
	"encoding/json"
	"strings"
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

// Every discovery message names one entity. Home Assistant puts the device
// name "FTW" first, so a name never repeats it, reads in sentence case and
// differs from every other name on the device, number twins included.
// unique_ids never change: Home Assistant keeps the entity ID and history it
// tied to each one.
func TestDiscoveryNamesAndUniqueIDs(t *testing.T) {
	b, client := discoveryBridge("inv")
	b.ctrl.BatteryCoversEV = true
	b.tel.EmitMetric("inv", "battery_temp_c", 21, "°C", "", "")
	b.publishDiscovery()
	b.announceEVDriver(b.discoveryDevice(), "charger")
	b.announceVehicleDriver(b.discoveryDevice(), "car")

	const sensor, binary, number = "homeassistant/sensor/forty_two_watts/", "homeassistant/binary_sensor/forty_two_watts/", "homeassistant/number/forty_two_watts/"
	want := map[string][2]string{
		sensor + "grid_power":                                    {"Grid power", "forty_two_watts_grid_power"},
		sensor + "pv_power":                                      {"PV power", "forty_two_watts_pv_power"},
		sensor + "battery_power":                                 {"Battery power", "forty_two_watts_battery_power"},
		sensor + "ev_power":                                      {"EV power", "forty_two_watts_ev_power"},
		sensor + "v2x_power":                                     {"V2X power", "forty_two_watts_v2x_power"},
		sensor + "load_power":                                    {"Load power", "forty_two_watts_load_power"},
		sensor + "battery_soc":                                   {"Battery SoC", "forty_two_watts_battery_soc"},
		sensor + "grid_target":                                   {"Grid target setpoint", "forty_two_watts_grid_target"},
		sensor + "peak_limit":                                    {"Peak limit setpoint", "forty_two_watts_peak_limit"},
		sensor + "peak_import_ceiling":                           {"Peak import ceiling", "forty_two_watts_peak_import_ceiling"},
		sensor + "ev_charging":                                   {"EV charging setpoint", "forty_two_watts_ev_charging"},
		sensor + "pv_predicted":                                  {"PV power predicted", "forty_two_watts_pv_predicted"},
		sensor + "load_predicted":                                {"Load power predicted", "forty_two_watts_load_predicted"},
		sensor + "phase_1_power":                                 {"Phase L1 power", "forty_two_watts_phase_1_power"},
		sensor + "phase_2_power":                                 {"Phase L2 power", "forty_two_watts_phase_2_power"},
		sensor + "phase_3_power":                                 {"Phase L3 power", "forty_two_watts_phase_3_power"},
		sensor + "phase_1_current":                               {"Phase L1 current", "forty_two_watts_phase_1_current"},
		sensor + "phase_2_current":                               {"Phase L2 current", "forty_two_watts_phase_2_current"},
		sensor + "phase_3_current":                               {"Phase L3 current", "forty_two_watts_phase_3_current"},
		sensor + "today_import":                                  {"Today grid import", "forty_two_watts_today_import"},
		sensor + "today_export":                                  {"Today grid export", "forty_two_watts_today_export"},
		sensor + "today_pv":                                      {"Today PV generation", "forty_two_watts_today_pv"},
		sensor + "today_bat_charged":                             {"Today battery charged", "forty_two_watts_today_bat_charged"},
		sensor + "today_bat_discharged":                          {"Today battery discharged", "forty_two_watts_today_bat_discharged"},
		sensor + "today_load":                                    {"Today load", "forty_two_watts_today_load"},
		sensor + "total_import":                                  {"Total grid import", "forty_two_watts_total_import"},
		sensor + "total_export":                                  {"Total grid export", "forty_two_watts_total_export"},
		sensor + "total_pv":                                      {"Total PV generation", "forty_two_watts_total_pv"},
		sensor + "total_bat_charged":                             {"Total battery charged", "forty_two_watts_total_bat_charged"},
		sensor + "total_bat_discharged":                          {"Total battery discharged", "forty_two_watts_total_bat_discharged"},
		sensor + "total_load":                                    {"Total load", "forty_two_watts_total_load"},
		sensor + "plan":                                          {"Plan", "forty_two_watts_plan"},
		sensor + "price":                                         {"Electricity price", "forty_two_watts_price_ore"},
		binary + "plan_stale":                                    {"Plan stale", "forty_two_watts_plan_stale"},
		"homeassistant/select/forty_two_watts/mode":              {"Mode", "forty_two_watts_mode"},
		number + "grid_target":                                   {"Grid target", "forty_two_watts_grid_target_cmd"},
		number + "peak_limit":                                    {"Peak limit", "forty_two_watts_peak_limit_cmd"},
		number + "ev_charging":                                   {"EV charging power", "forty_two_watts_ev_charging_cmd"},
		"homeassistant/switch/forty_two_watts/battery_covers_ev": {"Legacy home-wide battery cover", "forty_two_watts_battery_covers_ev_cmd"},
		// Driver entities start with the driver's own name.
		sensor + "inv_meter_w":             {"inv meter power", "forty_two_watts_invmeter_w"},
		sensor + "inv_pv_w":                {"inv PV power", "forty_two_watts_invpv_w"},
		sensor + "inv_bat_w":               {"inv battery power", "forty_two_watts_invbat_w"},
		sensor + "inv_bat_soc_pct":         {"inv battery SoC", "forty_two_watts_invbat_soc_pct"},
		sensor + "inv_v2x_w":               {"inv V2X power", "forty_two_watts_invv2x_w"},
		sensor + "inv_v2x_vehicle_soc_pct": {"inv V2X vehicle SoC", "forty_two_watts_invv2x_vehicle_soc_pct"},
		sensor + "inv_battery_temp_c":      {"inv battery temp c", "forty_two_watts_inv_battery_temp_c"},
		binary + "inv_online":              {"inv online", "forty_two_watts_inv_online"},
		sensor + "charger_ev_w":            {"charger EV power", "forty_two_watts_charger_ev_w"},
		sensor + "car_vehicle_soc":         {"car vehicle SoC", "forty_two_watts_car_vehicle_soc"},
	}

	names := map[string]string{}
	for topic, raw := range client.payloads {
		if !strings.HasSuffix(topic, "/config") || len(raw) == 0 {
			continue
		}
		object := strings.TrimSuffix(topic, "/config")
		w, ok := want[object]
		if !ok {
			t.Errorf("%s: entity missing from this test", object)
			continue
		}
		msg := client.config(t, topic)
		name, _ := msg["name"].(string)
		if name != w[0] || msg["unique_id"] != w[1] {
			t.Errorf("%s: name %q, unique_id %v; want %q, %q", object, name, msg["unique_id"], w[0], w[1])
		}
		if other, dup := names[strings.ToLower(name)]; dup {
			t.Errorf("%s and %s share the name %q", object, other, name)
		}
		names[strings.ToLower(name)] = object
		if strings.HasPrefix(strings.ToLower(name), "ftw") {
			t.Errorf("%s: %q repeats the device name", object, name)
		}
		if _, ok := msg["has_entity_name"]; ok {
			t.Errorf("%s: Home Assistant ignores has_entity_name in MQTT discovery; leave it out", object)
		}
	}
	for object := range want {
		if raw, ok := client.payloads[object+"/config"]; !ok || len(raw) == 0 {
			t.Errorf("%s: not announced", object)
		}
	}
}

// Sentence case: a capital first letter, then lower case except the
// abbreviations FTW uses. Driver entities start with the driver's name.
func TestDiscoveryNamesAreSentenceCase(t *testing.T) {
	b, client := discoveryBridge()
	b.ctrl.BatteryCoversEV = true
	b.publishDiscovery()
	abbreviations := map[string]bool{"PV": true, "EV": true, "V2X": true, "SoC": true, "L1": true, "L2": true, "L3": true}
	for topic, raw := range client.payloads {
		if !strings.HasSuffix(topic, "/config") || len(raw) == 0 {
			continue
		}
		name, _ := client.config(t, topic)["name"].(string)
		for i, word := range strings.Fields(name) {
			if abbreviations[word] {
				continue
			}
			want := strings.ToLower(word)
			if i == 0 {
				want = strings.ToUpper(want[:1]) + want[1:]
			}
			if word != want {
				t.Errorf("%s: %q is not sentence case", topic, name)
				break
			}
		}
	}
}

type fakeEnergy struct {
	today, lifetime EnergyTotals
	ready           bool
}

func (f fakeEnergy) TodayEnergy() (EnergyTotals, bool)    { return f.today, true }
func (f fakeEnergy) LifetimeEnergy() (EnergyTotals, bool) { return f.lifetime, f.ready }

// The lifetime totals are what Home Assistant's Energy dashboard wants:
// kWh, total_increasing, and on no Diagnostic list.
func TestLifetimeTotalsForTheEnergyDashboard(t *testing.T) {
	b, client := discoveryBridge()
	b.publishDiscovery()
	for _, id := range []string{"total_import", "total_export", "total_pv", "total_bat_charged", "total_bat_discharged", "total_load"} {
		msg := client.config(t, sensorTopic(id))
		if msg["unit_of_measurement"] != "kWh" || msg["device_class"] != "energy" || msg["state_class"] != "total_increasing" {
			t.Errorf("%s: unit %v, class %v, state_class %v; want kWh, energy, total_increasing", id, msg["unit_of_measurement"], msg["device_class"], msg["state_class"])
		}
		if c, ok := msg["entity_category"]; ok {
			t.Errorf("%s: entity_category %v, want none", id, c)
		}
		if msg["state_topic"] != "forty-two-watts/state/"+id+"_kwh" {
			t.Errorf("%s: state_topic %v", id, msg["state_topic"])
		}
	}

	// Nothing goes out until the store knows the totals, so Home Assistant
	// never starts a counter from a low value.
	energy := &fakeEnergy{lifetime: EnergyTotals{ImportWh: 1234567.8, ExportWh: 5.4, PVWh: 999.9996, BatChargedWh: 10, BatDischargedWh: 20, LoadWh: 0}}
	b.energy = energy
	b.publishState()
	if _, ok := client.payloads["forty-two-watts/state/total_import_kwh"]; ok {
		t.Fatal("lifetime totals were published before they were known")
	}
	energy.ready = true
	b.publishState()
	for topic, want := range map[string]string{
		"total_import_kwh": "1234.568", "total_export_kwh": "0.005", "total_pv_kwh": "1.000",
		"total_bat_charged_kwh": "0.010", "total_bat_discharged_kwh": "0.020", "total_load_kwh": "0.000",
	} {
		got := string(client.payloads["forty-two-watts/state/"+topic])
		if got != want || client.retained["forty-two-watts/state/"+topic] {
			t.Errorf("%s = %q (retained %v), want %q, not retained", topic, got, client.retained["forty-two-watts/state/"+topic], want)
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
		"ev_lifetime_wh":            true,
		"meter_total_export_wh":     true,
		"pv_total_import_kwh":       true,
		"ev_session_wh":             false,
		"battery_capacity_wh":       false,
		"today_pv_wh":               false,
		"hp_lifetimer_wh":           false, // "lifetime" must be a whole word
		"total_import_wh":           true,
		"meter_total_export":        true,
		"total_import_last_hour_wh": false, // the register must end the name
		"hp_total_exported_wh":      false,
		"mytotal_import_wh":         false,
	} {
		if got := lifetimeEnergyCounter(name); got != want {
			t.Errorf("lifetimeEnergyCounter(%q) = %v, want %v", name, got, want)
		}
	}
}
