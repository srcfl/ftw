// Package ha is the Home Assistant MQTT bridge: MQTT autodiscovery +
// periodic state publish + command subscriber for mode/target/peak/ev.
//
// Uses the same site sign convention as the rest of the app. HA users see
// grid_w as + import / − export, PV as negative (generation), battery as
// + charge / − discharge. That matches everyone else's conventions so
// HA charts can be dropped in without sign fiddling.
package ha

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/control"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// CommandCallbacks is how the bridge hands received commands back to the
// control loop. Caller provides these at construction time.
type CommandCallbacks struct {
	SetMode            func(string) error
	SetGridTarget      func(float64) error
	SetPeakLimit       func(float64) error
	SetEVCharging      func(float64, bool) error
	SetBatteryCoversEV func(bool) error
}

// PlanSource is the minimal interface the bridge needs to read the MPC plan.
// Pass an adapter wrapping *mpc.Service at construction; nil disables plan sensors.
type PlanSource interface {
	LatestActions() []PlanAction
}

// PlanAction is one slot in the MPC plan as seen by the HA bridge.
type PlanAction struct {
	SlotStartMs int64
	SlotLenMin  int
	BatteryW    float64
	GridW       float64
	SoCPct      float64
	PriceOre    float64 // total consumer price (öre/kWh)
	SpotOre     float64 // raw wholesale spot price (öre/kWh)
	CostOre     float64 // expected cost this slot (öre, negative = revenue)
	Confidence  float64 // forecast confidence 0–1
	Reason      string  // human-readable DP reason
	EMSMode     string  // effective EMS mode the planner chose
	PVW         float64 // planned PV output (site-sign, ≤ 0)
	LoadW       float64 // planned load (site-sign, ≥ 0)
}

// Bridge is an instance of the HA MQTT bridge.
//
// Lifecycle invariants:
//   - lifecycleMu serializes Reload + Stop so the connect/disconnect
//     dance never races with a second config-reload tick.
//   - mu guards every data field a diagnostic reader (IsConnected,
//     BrokerAddr, LastPublishMs, SensorsAnnounced) might touch.
//     publishLoop / publishState briefly acquire mu when bumping the
//     diagnostics counters; Reload deliberately does NOT hold mu while
//     waiting on the old loop's done channel — the loop needs mu to
//     update lastPublishMs on its way out and would otherwise deadlock.
type Bridge struct {
	tel    *telemetry.Store
	ctrl   *control.State
	ctrlMu *sync.Mutex
	cb     CommandCallbacks
	plan   PlanSource // nil when MPC is not configured

	topicPrefix string // e.g. "forty-two-watts"
	discoPrefix string // e.g. "homeassistant"
	deviceID    string

	lifecycleMu sync.Mutex // serializes Reload + Stop

	mu               sync.Mutex
	cfg              *config.HomeAssistant
	client           paho.Client
	driverNames      []string
	stop             chan struct{}
	done             chan struct{}
	lastPublishMs    int64
	sensorsAnnounced int
	stopped          bool

	// announcedMetrics tracks emit_metric sensors already registered with HA.
	// Keys are "driverName:metricName". Reset on Reload so reconnect re-announces.
	announcedMetrics map[string]struct{}
	// announcedEVDrivers / announcedVehicleDrivers track lazily-discovered
	// EV charger and vehicle readers. Keys are driver names.
	announcedEVDrivers      map[string]struct{}
	announcedVehicleDrivers map[string]struct{}

	// connectTimeout is the WaitTimeout passed to paho's Connect token.
	// 0 means "use defaultConnectTimeout"; tests override to keep the
	// refused-broker path fast.
	connectTimeout time.Duration
}

const defaultConnectTimeout = 10 * time.Second

// IsConnected returns true if the Paho MQTT client currently has an
// active connection to the broker.
func (b *Bridge) IsConnected() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	cli := b.client
	b.mu.Unlock()
	if cli == nil {
		return false
	}
	return cli.IsConnected()
}

// BrokerAddr returns the configured "host:port" string for diagnostics.
func (b *Bridge) BrokerAddr() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cfg == nil {
		return ""
	}
	return fmt.Sprintf("%s:%d", b.cfg.Broker, b.cfg.Port)
}

// LastPublishMs is the Unix milliseconds when the last state publish
// went out. 0 if nothing has been published yet.
func (b *Bridge) LastPublishMs() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastPublishMs
}

// SensorsAnnounced is the count of HA-discovery sensors we registered
// on connect. Non-zero means the auto-discovery worked.
func (b *Bridge) SensorsAnnounced() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sensorsAnnounced
}

// Start connects to the HA broker, publishes autodiscovery, and begins
// periodic state publishes. Returns immediately; the goroutine runs until
// Stop() is called.
//
// plan may be nil when the MPC planner is not configured; plan sensors
// are skipped in that case.
func Start(
	cfg *config.HomeAssistant,
	tel *telemetry.Store,
	ctrl *control.State, ctrlMu *sync.Mutex,
	driverNames []string,
	cb CommandCallbacks,
	plan PlanSource,
) (*Bridge, error) {
	b := &Bridge{
		tel:                     tel,
		ctrl:                    ctrl,
		ctrlMu:                  ctrlMu,
		cb:                      cb,
		plan:                    plan,
		topicPrefix:             "forty-two-watts",
		discoPrefix:             "homeassistant",
		deviceID:                "forty_two_watts",
		announcedMetrics:        make(map[string]struct{}),
		announcedEVDrivers:      make(map[string]struct{}),
		announcedVehicleDrivers: make(map[string]struct{}),
	}
	if err := b.connectAndStart(cfg, driverNames); err != nil {
		return nil, err
	}
	return b, nil
}

// Reload swaps the bridge's broker / credentials / driver list without
// requiring a process restart. The current MQTT client is disconnected,
// the publish loop drained, then a fresh paho client is built from
// newCfg and a new loop is started. Diagnostic counters reset because
// the new connection is its own thing. announcedMetrics is also reset
// so dynamic emit_metric sensors are re-announced on the new connection.
func (b *Bridge) Reload(newCfg *config.HomeAssistant, driverNames []string) error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()

	if b.stopped {
		return fmt.Errorf("ha: bridge stopped, cannot reload")
	}
	b.mu.Lock()
	b.announcedMetrics = make(map[string]struct{})
	b.announcedEVDrivers = make(map[string]struct{})
	b.announcedVehicleDrivers = make(map[string]struct{})
	b.mu.Unlock()
	b.teardown()
	return b.connectAndStart(newCfg, driverNames)
}

// connectAndStart wires a paho client from cfg, opens the connection,
// then starts the publish loop. Shared by Start (first-time wiring)
// and Reload (after a teardown). Caller is responsible for serializing
// access via lifecycleMu.
func (b *Bridge) connectAndStart(cfg *config.HomeAssistant, driverNames []string) error {
	b.mu.Lock()
	b.cfg = cfg
	b.driverNames = driverNames
	b.stop = make(chan struct{})
	b.done = make(chan struct{})
	b.lastPublishMs = 0
	b.sensorsAnnounced = 0
	b.mu.Unlock()

	opts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("tcp://%s:%d", cfg.Broker, cfg.Port)).
		SetClientID("forty-two-watts-ha").
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(10 * time.Second).
		// LWT: broker publishes "offline" if the EMS vanishes without a clean disconnect.
		// QoS 1 + retained so HA sees it immediately after reconnecting to the broker.
		SetWill(b.availTopic(), "offline", 1, true).
		SetOnConnectHandler(func(_ paho.Client) {
			slog.Info("HA MQTT connected", "broker", cfg.Broker)
			// Mark EMS as online before discovery so HA doesn't briefly
			// show entities as unavailable on the same connect.
			b.publish(b.availTopic(), []byte("online"), true)
			b.publishDiscovery()
			b.subscribeCommands()
		})
	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
	}
	if cfg.Password != "" {
		opts.SetPassword(cfg.Password)
	}
	cli := paho.NewClient(opts)

	b.mu.Lock()
	b.client = cli
	b.mu.Unlock()

	started := false
	defer func() {
		if !started {
			b.mu.Lock()
			close(b.done)
			b.mu.Unlock()
		}
	}()

	timeout := b.connectTimeout
	if timeout == 0 {
		timeout = defaultConnectTimeout
	}
	tok := cli.Connect()
	if !tok.WaitTimeout(timeout) {
		return fmt.Errorf("ha: connect timeout after %s", timeout)
	}
	if err := tok.Error(); err != nil {
		return err
	}

	go b.publishLoop()
	started = true
	return nil
}

// teardown drops the current MQTT client + publish loop. Mirrors Stop()
// but doesn't flip the stopped flag, so it can be followed by another
// connectAndStart. Caller must hold lifecycleMu.
func (b *Bridge) teardown() {
	b.mu.Lock()
	stopCh := b.stop
	doneCh := b.done
	cli := b.client
	b.mu.Unlock()

	if stopCh != nil {
		close(stopCh)
	}
	if doneCh != nil {
		<-doneCh
	}
	if cli != nil {
		// Publish "offline" synchronously before disconnecting so HA
		// sees the EMS go away cleanly (e.g. on a planned restart).
		// Use IsConnectionOpen (not IsConnected): with ConnectRetry=true,
		// IsConnected() returns true even while retrying, so Publish would
		// block for WaitTimeout. Only publish "offline" when the TCP link
		// is actually up.
		if cli.IsConnectionOpen() {
			cli.Publish(b.availTopic(), 1, true, []byte("offline")).WaitTimeout(2 * time.Second)
		}
		cli.Disconnect(500)
	}
}

// Stop disconnects and waits for the publish loop to exit. Idempotent.
func (b *Bridge) Stop() {
	if b == nil {
		return
	}
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	if b.stopped {
		return
	}
	b.teardown()
	b.stopped = true
}

// ---- Topic helpers ----

func (b *Bridge) availTopic() string  { return b.topicPrefix + "/status" }
func (b *Bridge) stateTopic(name string) string { return b.topicPrefix + "/state/" + name }
func (b *Bridge) cmdTopic(name string) string   { return b.topicPrefix + "/cmd/" + name }
func (b *Bridge) driverTopic(driver, field string) string {
	return fmt.Sprintf("%s/driver/%s/%s", b.topicPrefix, driver, field)
}

// ---- Autodiscovery ----

// discoveryDevice is the device block embedded in every discovery message
// so HA groups all the sensors under one device page.
func (b *Bridge) discoveryDevice() map[string]any {
	return map[string]any{
		"identifiers":  []string{b.deviceID},
		"name":         "forty-two-watts",
		"manufacturer": "Sourceful",
		"model":        "Home EMS",
	}
}

// modeSelectOptions is the option list for the HA mode `select`. It MUST
// cover every value publishState can emit on the mode state topic (which is
// string(ctrl.Mode)), otherwise HA logs "Invalid option for select" the
// moment the active mode falls outside the list (e.g. any planner_* mode,
// which is the default UI choice). Derived from control.AllModes so the enum
// is the single source of truth and the two lists can't drift again — see
// TestModeSelectOptionsMatchAllModes for the guard.
func modeSelectOptions() []string {
	modes := control.AllModes()
	opts := make([]string, len(modes))
	for i, m := range modes {
		opts[i] = string(m)
	}
	return opts
}

// withAvail adds the availability topic keys to a discovery message map.
func (b *Bridge) withAvail(m map[string]any) map[string]any {
	m["availability_topic"] = b.availTopic()
	m["payload_available"] = "online"
	m["payload_not_available"] = "offline"
	return m
}

// publishDiscovery registers all sensors and controls with HA. Called on
// every reconnect — HA de-dupes by unique_id so it's safe to re-publish.
func (b *Bridge) publishDiscovery() {
	dev := b.discoveryDevice()
	total := 0

	// ---- Site-level sensors ----
	sensors := []struct {
		id, name, unit, devClass string
		state                    string
	}{
		{"grid_power", "Grid Power", "W", "power", b.stateTopic("grid_w")},
		{"pv_power", "PV Power", "W", "power", b.stateTopic("pv_w")},
		{"battery_power", "Battery Power", "W", "power", b.stateTopic("bat_w")},
		{"load_power", "Load Power", "W", "power", b.stateTopic("load_w")},
		{"battery_soc", "Battery SoC", "%", "battery", b.stateTopic("bat_soc_pct")},
		{"grid_target", "Grid Target", "W", "power", b.stateTopic("grid_target_w")},
		{"peak_limit", "Peak Limit", "W", "power", b.stateTopic("peak_limit_w")},
		{"ev_charging", "EV Charging Power", "W", "power", b.stateTopic("ev_charging_w")},
	}
	for _, s := range sensors {
		msg := b.withAvail(map[string]any{
			"name":                s.name,
			"unique_id":           b.deviceID + "_" + s.id,
			"state_topic":         s.state,
			"unit_of_measurement": s.unit,
			"device_class":        s.devClass,
			"device":              dev,
		})
		data, _ := json.Marshal(msg)
		topic := fmt.Sprintf("%s/sensor/%s/%s/config", b.discoPrefix, b.deviceID, s.id)
		b.publish(topic, data, true)
	}
	total += len(sensors)

	// ---- Mode as HA select ----
	modeMsg := b.withAvail(map[string]any{
		"name":          "Mode",
		"unique_id":     b.deviceID + "_mode",
		"state_topic":   b.stateTopic("mode"),
		"command_topic": b.cmdTopic("mode"),
		"options":       modeSelectOptions(),
		"device":        dev,
	})
	data, _ := json.Marshal(modeMsg)
	b.publish(fmt.Sprintf("%s/select/%s/mode/config", b.discoPrefix, b.deviceID), data, true)
	total++

	// ---- Grid target as HA number ----
	targetMsg := b.withAvail(map[string]any{
		"name":                "Grid Target",
		"unique_id":           b.deviceID + "_grid_target_cmd",
		"state_topic":         b.stateTopic("grid_target_w"),
		"command_topic":       b.cmdTopic("grid_target_w"),
		"min":                 -10000,
		"max":                 10000,
		"step":                50,
		"unit_of_measurement": "W",
		"device":              dev,
	})
	data, _ = json.Marshal(targetMsg)
	b.publish(fmt.Sprintf("%s/number/%s/grid_target/config", b.discoPrefix, b.deviceID), data, true)
	total++

	// ---- Peak limit as HA number ----
	peakMsg := b.withAvail(map[string]any{
		"name":                "Peak Limit",
		"unique_id":           b.deviceID + "_peak_limit_cmd",
		"state_topic":         b.stateTopic("peak_limit_w"),
		"command_topic":       b.cmdTopic("peak_limit_w"),
		"min":                 0,
		"max":                 25000,
		"step":                100,
		"unit_of_measurement": "W",
		"icon":                "mdi:gauge",
		"device":              dev,
	})
	data, _ = json.Marshal(peakMsg)
	b.publish(fmt.Sprintf("%s/number/%s/peak_limit/config", b.discoPrefix, b.deviceID), data, true)
	total++

	// ---- EV charging target as HA number ----
	evMsg := b.withAvail(map[string]any{
		"name":                "EV Charging Power",
		"unique_id":           b.deviceID + "_ev_charging_cmd",
		"state_topic":         b.stateTopic("ev_charging_w"),
		"command_topic":       b.cmdTopic("ev_charging_w"),
		"min":                 0,
		"max":                 22000,
		"step":                100,
		"unit_of_measurement": "W",
		"icon":                "mdi:car-electric",
		"device":              dev,
	})
	data, _ = json.Marshal(evMsg)
	b.publish(fmt.Sprintf("%s/number/%s/ev_charging/config", b.discoPrefix, b.deviceID), data, true)
	total++

	// ---- Battery-covers-EV as HA switch ----
	bceMsg := b.withAvail(map[string]any{
		"name":          "Battery Covers EV",
		"unique_id":     b.deviceID + "_battery_covers_ev_cmd",
		"state_topic":   b.stateTopic("battery_covers_ev"),
		"command_topic": b.cmdTopic("battery_covers_ev"),
		"payload_on":    "ON",
		"payload_off":   "OFF",
		"state_on":      "ON",
		"state_off":     "OFF",
		"icon":          "mdi:car-battery",
		"device":        dev,
	})
	data, _ = json.Marshal(bceMsg)
	b.publish(fmt.Sprintf("%s/switch/%s/battery_covers_ev/config", b.discoPrefix, b.deviceID), data, true)
	total++

	// ---- MPC plan + price + plan-stale sensors (only when planner is wired) ----
	if b.plan != nil {
		// Plan action sensor: state = "charge"/"discharge"/"idle", attrs = full 24h schedule.
		planMsg := b.withAvail(map[string]any{
			"name":                  "Plan",
			"unique_id":             b.deviceID + "_plan",
			"state_topic":           b.stateTopic("plan_action"),
			"json_attributes_topic": b.stateTopic("plan_json"),
			"icon":                  "mdi:calendar-clock",
			"device":                dev,
		})
		data, _ = json.Marshal(planMsg)
		b.publish(fmt.Sprintf("%s/sensor/%s/plan/config", b.discoPrefix, b.deviceID), data, true)
		total++

		// Current electricity price sensor: state = total consumer price (öre/kWh),
		// attrs = spot_ore, cost_ore, confidence, reason, ems_mode.
		priceMsg := b.withAvail(map[string]any{
			"name":                  "Electricity Price",
			"unique_id":             b.deviceID + "_price_ore",
			"state_topic":           b.stateTopic("price_ore"),
			"json_attributes_topic": b.stateTopic("price_json"),
			"unit_of_measurement":   "öre/kWh",
			"icon":                  "mdi:cash-multiple",
			"device":                dev,
		})
		data, _ = json.Marshal(priceMsg)
		b.publish(fmt.Sprintf("%s/sensor/%s/price/config", b.discoPrefix, b.deviceID), data, true)
		total++

		// Plan-stale binary_sensor: "on" = planner fell back to self_consumption
		// because the plan was missing or older than 30 min.
		staleMsg := b.withAvail(map[string]any{
			"name":         "Plan Stale",
			"unique_id":    b.deviceID + "_plan_stale",
			"state_topic":  b.stateTopic("plan_stale"),
			"payload_on":   "true",
			"payload_off":  "false",
			"device_class": "problem",
			"icon":         "mdi:calendar-alert",
			"device":       dev,
		})
		data, _ = json.Marshal(staleMsg)
		b.publish(fmt.Sprintf("%s/binary_sensor/%s/plan_stale/config", b.discoPrefix, b.deviceID), data, true)
		total++
	}

	// ---- Per-driver sensors + health ----
	b.mu.Lock()
	knownMetrics := make(map[string]struct{}, len(b.announcedMetrics))
	for k, v := range b.announcedMetrics {
		knownMetrics[k] = v
	}
	knownEVs := make(map[string]struct{}, len(b.announcedEVDrivers))
	for k, v := range b.announcedEVDrivers {
		knownEVs[k] = v
	}
	knownVehicles := make(map[string]struct{}, len(b.announcedVehicleDrivers))
	for k, v := range b.announcedVehicleDrivers {
		knownVehicles[k] = v
	}
	b.mu.Unlock()

	for _, name := range b.driverNames {
		// Data sensors
		for _, s := range []struct{ id, label, unit, class string }{
			{"meter_w", " Meter Power", "W", "power"},
			{"pv_w", " PV Power", "W", "power"},
			{"bat_w", " Battery Power", "W", "power"},
			{"bat_soc_pct", " Battery SoC", "%", "battery"},
		} {
			msg := b.withAvail(map[string]any{
				"name":                name + s.label,
				"unique_id":           b.deviceID + "_" + name + s.id,
				"state_topic":         b.driverTopic(name, s.id),
				"unit_of_measurement": s.unit,
				"device_class":        s.class,
				"device":              dev,
			})
			d, _ := json.Marshal(msg)
			topic := fmt.Sprintf("%s/sensor/%s/%s_%s/config", b.discoPrefix, b.deviceID, name, s.id)
			b.publish(topic, d, true)
		}
		total += 4

		// Health binary_sensor: online/offline with JSON attributes for rich diagnostics.
		healthMsg := b.withAvail(map[string]any{
			"name":                  name + " Online",
			"unique_id":             b.deviceID + "_" + name + "_online",
			"state_topic":           b.driverTopic(name, "online"),
			"json_attributes_topic": b.driverTopic(name, "health_json"),
			"payload_on":            "true",
			"payload_off":           "false",
			"device_class":          "connectivity",
			"icon":                  "mdi:lan-connect",
			"device":                dev,
		})
		d, _ := json.Marshal(healthMsg)
		b.publish(fmt.Sprintf("%s/binary_sensor/%s/%s_online/config", b.discoPrefix, b.deviceID, name), d, true)
		total++

		// Re-announce any emit_metric sensors already known from a previous cycle.
		for key := range knownMetrics {
			prefix := name + ":"
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			metricName := key[len(prefix):]
			b.announceMetric(dev, name, metricName)
			total++
		}
	}

	// Re-announce EV charger and vehicle sensors lazily discovered before reconnect.
	for name := range knownEVs {
		b.announceEVDriver(dev, name)
		total++
	}
	for name := range knownVehicles {
		b.announceVehicleDriver(dev, name)
		total++
	}

	b.mu.Lock()
	b.sensorsAnnounced = total
	b.mu.Unlock()
}

// announceMetric publishes a single HA discovery message for a driver emit_metric.
// Called when a metric is seen for the first time and on every reconnect.
func (b *Bridge) announceMetric(dev map[string]any, driver, metricName string) {
	unit, devClass := metricUnitAndClass(metricName)
	uid := b.deviceID + "_" + driver + "_" + metricName
	msg := b.withAvail(map[string]any{
		"name":      driver + " " + strings.ReplaceAll(metricName, "_", " "),
		"unique_id": uid,
		"state_topic": b.driverTopic(driver, metricName),
		"device":    dev,
	})
	if unit != "" {
		msg["unit_of_measurement"] = unit
	}
	if devClass != "" {
		msg["device_class"] = devClass
	}
	d, _ := json.Marshal(msg)
	topic := fmt.Sprintf("%s/sensor/%s/%s_%s/config", b.discoPrefix, b.deviceID, driver, metricName)
	b.publish(topic, d, true)
}

// metricUnitAndClass infers HA unit and device_class from an emit_metric name suffix.
func metricUnitAndClass(name string) (unit, devClass string) {
	switch {
	case strings.HasSuffix(name, "_w"):
		return "W", "power"
	case strings.HasSuffix(name, "_wh"):
		return "Wh", "energy"
	case strings.HasSuffix(name, "_c"):
		return "°C", "temperature"
	case strings.HasSuffix(name, "_soc_pct"), strings.HasSuffix(name, "_soc"):
		return "%", "battery"
	case strings.HasSuffix(name, "_pct"):
		return "%", ""
	case strings.HasSuffix(name, "_v"):
		return "V", "voltage"
	case strings.HasSuffix(name, "_a"):
		return "A", "current"
	case strings.HasSuffix(name, "_hz"):
		return "Hz", "frequency"
	default:
		return "", ""
	}
}

// ---- Command subscriber ----

func (b *Bridge) subscribeCommands() {
	b.client.Subscribe(b.cmdTopic("mode"), 0, func(_ paho.Client, m paho.Message) {
		mode := string(m.Payload())
		if b.cb.SetMode != nil {
			if err := b.cb.SetMode(mode); err != nil {
				slog.Warn("HA set mode failed", "err", err)
			}
		}
	})
	b.client.Subscribe(b.cmdTopic("grid_target_w"), 0, func(_ paho.Client, m paho.Message) {
		f, err := strconv.ParseFloat(string(m.Payload()), 64)
		if err != nil {
			return
		}
		if b.cb.SetGridTarget != nil {
			_ = b.cb.SetGridTarget(f)
		}
	})
	b.client.Subscribe(b.cmdTopic("peak_limit_w"), 0, func(_ paho.Client, m paho.Message) {
		f, err := strconv.ParseFloat(string(m.Payload()), 64)
		if err != nil {
			return
		}
		if b.cb.SetPeakLimit != nil {
			_ = b.cb.SetPeakLimit(f)
		}
	})
	b.client.Subscribe(b.cmdTopic("ev_charging_w"), 0, func(_ paho.Client, m paho.Message) {
		f, err := strconv.ParseFloat(string(m.Payload()), 64)
		if err != nil {
			return
		}
		if b.cb.SetEVCharging != nil {
			_ = b.cb.SetEVCharging(f, f > 0)
		}
	})
	b.client.Subscribe(b.cmdTopic("battery_covers_ev"), 0, func(_ paho.Client, m paho.Message) {
		on := string(m.Payload()) == "ON"
		if b.cb.SetBatteryCoversEV != nil {
			_ = b.cb.SetBatteryCoversEV(on)
		}
	})
}

// ---- State publish loop ----

func (b *Bridge) publishLoop() {
	defer close(b.done)
	interval := time.Duration(b.cfg.PublishIntervalS) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			b.publishState()
		}
	}
}

func (b *Bridge) publishState() {
	b.mu.Lock()
	b.lastPublishMs = time.Now().UnixMilli()
	b.mu.Unlock()

	// ---- Site-level aggregates ----
	b.ctrlMu.Lock()
	siteMeter := b.ctrl.SiteMeterDriver
	mode := string(b.ctrl.Mode)
	gridTarget := b.ctrl.GridTargetW
	peakLimit := b.ctrl.PeakLimitW
	evCharging := b.ctrl.EVChargingW
	batteryCoversEV := b.ctrl.BatteryCoversEV
	planStale := b.ctrl.PlanStale
	b.ctrlMu.Unlock()

	gridW := 0.0
	if r := b.tel.Get(siteMeter, telemetry.DerMeter); r != nil {
		gridW = r.SmoothedW
	}
	var pvW, batW, sumSoC float64
	var socCount int
	for _, r := range b.tel.ReadingsByType(telemetry.DerPV) {
		pvW += r.SmoothedW
	}
	for _, r := range b.tel.ReadingsByType(telemetry.DerBattery) {
		batW += r.SmoothedW
		if r.SoC != nil {
			sumSoC += *r.SoC
			socCount++
		}
	}
	avgSoC := 0.0
	if socCount > 0 {
		avgSoC = sumSoC / float64(socCount)
	}
	loadW := gridW - batW - pvW
	if loadW < 0 {
		loadW = 0
	}

	b.publishValue("grid_w", gridW)
	b.publishValue("pv_w", pvW)
	b.publishValue("bat_w", batW)
	b.publishValue("load_w", loadW)
	b.publishValue("bat_soc_pct", avgSoC*100)
	b.publishValue("grid_target_w", gridTarget)
	b.publishValue("peak_limit_w", peakLimit)
	b.publishValue("ev_charging_w", evCharging)
	b.publishString("mode", mode)
	bceState := "OFF"
	if batteryCoversEV {
		bceState = "ON"
	}
	b.publishString("battery_covers_ev", bceState)

	// ---- MPC plan + price + plan-stale ----
	if b.plan != nil {
		b.publish(b.stateTopic("plan_stale"), []byte(strconv.FormatBool(planStale)), false)
		b.publishPlan()
	}

	// ---- Per-driver ----
	dev := b.discoveryDevice()
	for _, name := range b.driverNames {
		if r := b.tel.Get(name, telemetry.DerMeter); r != nil {
			b.publishDriver(name, "meter_w", r.SmoothedW)
		}
		if r := b.tel.Get(name, telemetry.DerPV); r != nil {
			b.publishDriver(name, "pv_w", r.SmoothedW)
		}
		if r := b.tel.Get(name, telemetry.DerBattery); r != nil {
			b.publishDriver(name, "bat_w", r.SmoothedW)
			if r.SoC != nil {
				b.publishDriver(name, "bat_soc_pct", *r.SoC*100)
			}
		}

		// Health: boolean state + rich JSON attributes.
		online := false
		if h := b.tel.DriverHealth(name); h != nil {
			online = h.IsOnline()
			attrs := map[string]any{
				"status":             h.Status.String(),
				"consecutive_errors": h.ConsecutiveErrors,
				"last_error":         h.LastError,
				"tick_count":         h.TickCount,
			}
			if h.LastSuccess != nil {
				attrs["last_seen"] = h.LastSuccess.UTC().Format(time.RFC3339)
			}
			if d, err := json.Marshal(attrs); err == nil {
				b.publish(b.driverTopic(name, "health_json"), d, false)
			}
		}
		b.publish(b.driverTopic(name, "online"), []byte(strconv.FormatBool(online)), false)

		// Emit_metric sensors — discover new ones, publish all.
		for _, snap := range b.tel.LatestMetricsByDriver(name) {
			key := name + ":" + snap.Name
			b.mu.Lock()
			_, known := b.announcedMetrics[key]
			if !known {
				b.announcedMetrics[key] = struct{}{}
			}
			b.mu.Unlock()
			if !known {
				b.announceMetric(dev, name, snap.Name)
			}
			b.publishDriver(name, snap.Name, snap.Value)
		}
	}

	// ---- EV charger readings (DerEV) ----
	// Lazily discovered: on first sighting of a driver we register it with HA.
	for _, r := range b.tel.ReadingsByType(telemetry.DerEV) {
		name := r.Driver
		b.mu.Lock()
		_, known := b.announcedEVDrivers[name]
		if !known {
			b.announcedEVDrivers[name] = struct{}{}
		}
		b.mu.Unlock()
		if !known {
			b.announceEVDriver(dev, name)
		}
		b.publishDriver(name, "ev_w", r.SmoothedW)
		// Publish raw Data as JSON attributes so HA can surface charging_state,
		// charge_limit_pct, and any other fields the driver emits.
		attrs := map[string]any{
			"power_w":    r.SmoothedW,
			"updated_at": r.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if len(r.Data) > 0 {
			var extra map[string]any
			if json.Unmarshal(r.Data, &extra) == nil {
				for k, v := range extra {
					attrs[k] = v
				}
			}
		}
		if d, err := json.Marshal(attrs); err == nil {
			b.publish(b.driverTopic(name, "ev_json"), d, false)
		}
	}

	// ---- Vehicle readings (DerVehicle) ----
	// Vehicle SoC + charging metadata from the vehicle itself (e.g. Tesla BLE).
	for _, r := range b.tel.ReadingsByType(telemetry.DerVehicle) {
		name := r.Driver
		b.mu.Lock()
		_, known := b.announcedVehicleDrivers[name]
		if !known {
			b.announcedVehicleDrivers[name] = struct{}{}
		}
		b.mu.Unlock()
		if !known {
			b.announceVehicleDriver(dev, name)
		}
		if r.SoC != nil {
			b.publishDriver(name, "vehicle_soc_pct", *r.SoC*100)
		}
		attrs := map[string]any{
			"updated_at": r.UpdatedAt.UTC().Format(time.RFC3339),
		}
		if r.SoC != nil {
			attrs["soc_pct"] = *r.SoC * 100
		}
		if len(r.Data) > 0 {
			var extra map[string]any
			if json.Unmarshal(r.Data, &extra) == nil {
				for k, v := range extra {
					attrs[k] = v
				}
			}
		}
		if d, err := json.Marshal(attrs); err == nil {
			b.publish(b.driverTopic(name, "vehicle_json"), d, false)
		}
	}
}

// announceEVDriver publishes HA discovery for an EV charger reader.
func (b *Bridge) announceEVDriver(dev map[string]any, driver string) {
	msg := b.withAvail(map[string]any{
		"name":                  driver + " EV Power",
		"unique_id":             b.deviceID + "_" + driver + "_ev_w",
		"state_topic":           b.driverTopic(driver, "ev_w"),
		"json_attributes_topic": b.driverTopic(driver, "ev_json"),
		"unit_of_measurement":   "W",
		"device_class":          "power",
		"icon":                  "mdi:car-electric",
		"device":                dev,
	})
	d, _ := json.Marshal(msg)
	b.publish(fmt.Sprintf("%s/sensor/%s/%s_ev_w/config", b.discoPrefix, b.deviceID, driver), d, true)
}

// announceVehicleDriver publishes HA discovery for a vehicle SoC reader.
func (b *Bridge) announceVehicleDriver(dev map[string]any, driver string) {
	msg := b.withAvail(map[string]any{
		"name":                  driver + " Vehicle SoC",
		"unique_id":             b.deviceID + "_" + driver + "_vehicle_soc",
		"state_topic":           b.driverTopic(driver, "vehicle_soc_pct"),
		"json_attributes_topic": b.driverTopic(driver, "vehicle_json"),
		"unit_of_measurement":   "%",
		"device_class":          "battery",
		"icon":                  "mdi:car-electric",
		"device":                dev,
	})
	d, _ := json.Marshal(msg)
	b.publish(fmt.Sprintf("%s/sensor/%s/%s_vehicle_soc/config", b.discoPrefix, b.deviceID, driver), d, true)
}

// publishPlan reads the current MPC plan and publishes:
//   - plan_action: current slot action string (charge/discharge/idle)
//   - plan_json: full attributes including 24 h schedule with price/cost data
//   - price_ore: current consumer electricity price (öre/kWh)
//   - price_json: price attributes (spot_ore, cost_ore, confidence, reason, ems_mode)
func (b *Bridge) publishPlan() {
	actions := b.plan.LatestActions()
	now := time.Now()
	nowMs := now.UnixMilli()

	type slotJSON struct {
		Start      string  `json:"start"`
		End        string  `json:"end"`
		Action     string  `json:"action"`
		BatteryW   float64 `json:"battery_w"`
		GridW      float64 `json:"grid_w"`
		SoCPct     float64 `json:"soc_pct"`
		PVW        float64 `json:"pv_w,omitempty"`
		LoadW      float64 `json:"load_w,omitempty"`
		PriceOre   float64 `json:"price_ore,omitempty"`
		SpotOre    float64 `json:"spot_ore,omitempty"`
		CostOre    float64 `json:"cost_ore,omitempty"`
		Confidence float64 `json:"confidence,omitempty"`
		Reason     string  `json:"reason,omitempty"`
		EMSMode    string  `json:"ems_mode,omitempty"`
	}

	currentAction := "unavailable"
	var curBatW, curGridW, curSoCPct float64
	var curStart, curEnd string
	var curPriceOre, curSpotOre, curCostOre, curConfidence float64
	var curReason, curEMSMode string

	var schedule []slotJSON
	horizon := nowMs + 24*60*60*1000 // 24 h ahead

	sort.Slice(actions, func(i, j int) bool {
		return actions[i].SlotStartMs < actions[j].SlotStartMs
	})

	for _, a := range actions {
		endMs := a.SlotStartMs + int64(a.SlotLenMin)*60*1000
		if endMs <= nowMs {
			continue // past slot
		}
		if a.SlotStartMs > horizon {
			break // beyond 24 h
		}

		label := planActionLabel(a.BatteryW)
		start := time.UnixMilli(a.SlotStartMs).UTC().Format(time.RFC3339)
		end := time.UnixMilli(endMs).UTC().Format(time.RFC3339)

		if a.SlotStartMs <= nowMs && nowMs < endMs {
			currentAction = label
			curBatW = a.BatteryW
			curGridW = a.GridW
			curSoCPct = a.SoCPct
			curStart = start
			curEnd = end
			curPriceOre = a.PriceOre
			curSpotOre = a.SpotOre
			curCostOre = a.CostOre
			curConfidence = a.Confidence
			curReason = a.Reason
			curEMSMode = a.EMSMode
		}
		schedule = append(schedule, slotJSON{
			Start:      start,
			End:        end,
			Action:     label,
			BatteryW:   a.BatteryW,
			GridW:      a.GridW,
			SoCPct:     a.SoCPct,
			PVW:        a.PVW,
			LoadW:      a.LoadW,
			PriceOre:   a.PriceOre,
			SpotOre:    a.SpotOre,
			CostOre:    a.CostOre,
			Confidence: a.Confidence,
			Reason:     a.Reason,
			EMSMode:    a.EMSMode,
		})
	}

	b.publishString("plan_action", currentAction)

	planAttrs := map[string]any{
		"action":     currentAction,
		"battery_w":  curBatW,
		"grid_w":     curGridW,
		"soc_pct":    curSoCPct,
		"slot_start": curStart,
		"slot_end":   curEnd,
		"price_ore":  curPriceOre,
		"spot_ore":   curSpotOre,
		"cost_ore":   curCostOre,
		"confidence": curConfidence,
		"reason":     curReason,
		"ems_mode":   curEMSMode,
		"schedule":   schedule,
	}
	if d, err := json.Marshal(planAttrs); err == nil {
		b.publish(b.stateTopic("plan_json"), d, false)
	}

	// Price sensor: standalone value + rich attributes for HA energy dashboard.
	b.publishValue("price_ore", curPriceOre)
	priceAttrs := map[string]any{
		"price_ore":  curPriceOre,
		"spot_ore":   curSpotOre,
		"cost_ore":   curCostOre,
		"confidence": curConfidence,
		"reason":     curReason,
		"ems_mode":   curEMSMode,
	}
	if d, err := json.Marshal(priceAttrs); err == nil {
		b.publish(b.stateTopic("price_json"), d, false)
	}
}

// planActionLabel converts a signed battery power to a human-readable label.
// Mirrors the logic in mpc.Service.SlotAt.
func planActionLabel(batteryW float64) string {
	if batteryW > 100 {
		return "charge"
	}
	if batteryW < -100 {
		return "discharge"
	}
	return "idle"
}

// ---- Publish helpers ----

func (b *Bridge) publishValue(name string, v float64) {
	b.publish(b.stateTopic(name), []byte(strconv.FormatFloat(v, 'f', 2, 64)), false)
}
func (b *Bridge) publishString(name string, s string) {
	b.publish(b.stateTopic(name), []byte(s), false)
}
func (b *Bridge) publishDriver(driver, field string, v float64) {
	b.publish(b.driverTopic(driver, field), []byte(strconv.FormatFloat(v, 'f', 2, 64)), false)
}
func (b *Bridge) publish(topic string, payload []byte, retained bool) {
	tok := b.client.Publish(topic, 0, retained, payload)
	tok.WaitTimeout(3 * time.Second)
}

// unused import suppressors
var _ = context.Background
