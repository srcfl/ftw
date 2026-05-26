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
	"strconv"
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
	SetMode             func(string) error
	SetGridTarget       func(float64) error
	SetPeakLimit        func(float64) error
	SetEVCharging       func(float64, bool) error
	SetBatteryCoversEV  func(bool) error
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
func Start(
	cfg *config.HomeAssistant,
	tel *telemetry.Store,
	ctrl *control.State, ctrlMu *sync.Mutex,
	driverNames []string,
	cb CommandCallbacks,
) (*Bridge, error) {
	b := &Bridge{
		tel:         tel,
		ctrl:        ctrl,
		ctrlMu:      ctrlMu,
		cb:          cb,
		topicPrefix: "forty-two-watts",
		discoPrefix: "homeassistant",
		deviceID:    "forty_two_watts",
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
// the new connection is its own thing — operators reading
// LastPublishMs / SensorsAnnounced after a reload should see "fresh
// connection" semantics, not stale figures from the previous broker.
//
// driverNames is the current driver registry as of reload time. The
// applier in cmd/forty-two-watts/main.go passes in reg.Names() so a
// driver added or removed in the same config-reload tick is reflected
// in HA discovery without a second round-trip.
func (b *Bridge) Reload(newCfg *config.HomeAssistant, driverNames []string) error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()

	if b.stopped {
		return fmt.Errorf("ha: bridge stopped, cannot reload")
	}
	b.teardown()
	return b.connectAndStart(newCfg, driverNames)
}

// connectAndStart wires a paho client from cfg, opens the connection,
// then starts the publish loop. Shared by Start (first-time wiring)
// and Reload (after a teardown). Caller is responsible for serializing
// access via lifecycleMu.
func (b *Bridge) connectAndStart(cfg *config.HomeAssistant, driverNames []string) error {
	// Swap data fields BEFORE Connect: paho fires OnConnectHandler from
	// inside Connect() and that handler calls publishDiscovery, which
	// reads b.driverNames. Updating after Connect would publish discovery
	// for the previous driver list.
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
		SetOnConnectHandler(func(_ paho.Client) {
			slog.Info("HA MQTT connected", "broker", cfg.Broker)
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

	// We own b.done until publishLoop takes over — close it on any
	// early return so the next teardown() doesn't block on <-doneCh.
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

// publishDiscovery registers all sensors and controls with HA. Called on
// every reconnect — HA de-dupes by unique_id so it's safe to re-publish.
func (b *Bridge) publishDiscovery() {
	dev := b.discoveryDevice()

	// ---- Sensors (site level) ----
	sensors := []struct {
		id, name, unit, devClass string
		state                    string // the topic to read from
	}{
		{"grid_power", "Grid Power", "W", "power", b.stateTopic("grid_w")},
		{"pv_power", "PV Power", "W", "power", b.stateTopic("pv_w")},
		{"battery_power", "Battery Power", "W", "power", b.stateTopic("bat_w")},
		{"load_power", "Load Power", "W", "power", b.stateTopic("load_w")},
		{"battery_soc", "Battery SoC", "%", "battery", b.stateTopic("bat_soc_pct")},
		{"grid_target", "Grid Target", "W", "power", b.stateTopic("grid_target_w")},
	}
	for _, s := range sensors {
		msg := map[string]any{
			"name":                s.name,
			"unique_id":           b.deviceID + "_" + s.id,
			"state_topic":         s.state,
			"unit_of_measurement": s.unit,
			"device_class":        s.devClass,
			"device":              dev,
		}
		data, _ := json.Marshal(msg)
		topic := fmt.Sprintf("%s/sensor/%s/%s/config", b.discoPrefix, b.deviceID, s.id)
		b.publish(topic, data, true) // retained
	}

	// ---- Mode as HA select ----
	modeMsg := map[string]any{
		"name":             "Mode",
		"unique_id":        b.deviceID + "_mode",
		"state_topic":      b.stateTopic("mode"),
		"command_topic":    b.cmdTopic("mode"),
		"options":          []string{"idle", "self_consumption", "peak_shaving", "charge", "priority", "weighted"},
		"device":           dev,
	}
	data, _ := json.Marshal(modeMsg)
	b.publish(fmt.Sprintf("%s/select/%s/mode/config", b.discoPrefix, b.deviceID), data, true)

	// ---- Grid target as HA number ----
	targetMsg := map[string]any{
		"name":                "Grid Target",
		"unique_id":           b.deviceID + "_grid_target_cmd",
		"state_topic":         b.stateTopic("grid_target_w"),
		"command_topic":       b.cmdTopic("grid_target_w"),
		"min":                 -10000,
		"max":                 10000,
		"step":                50,
		"unit_of_measurement": "W",
		"device":              dev,
	}
	data, _ = json.Marshal(targetMsg)
	b.publish(fmt.Sprintf("%s/number/%s/grid_target/config", b.discoPrefix, b.deviceID), data, true)

	// ---- Battery-covers-EV as HA switch ----
	// Operator-facing override: when ON, the control loop lets the battery
	// discharge into the EV (price-arbitrage scenarios). Default OFF.
	bceMsg := map[string]any{
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
	}
	data, _ = json.Marshal(bceMsg)
	b.publish(fmt.Sprintf("%s/switch/%s/battery_covers_ev/config", b.discoPrefix, b.deviceID), data, true)

	// ---- Per-driver sensors ----
	for _, name := range b.driverNames {
		for _, s := range []struct{ id, label, unit, class string }{
			{"_meter_w", " Meter Power", "W", "power"},
			{"_pv_w", " PV Power", "W", "power"},
			{"_bat_w", " Battery Power", "W", "power"},
			{"_bat_soc_pct", " Battery SoC", "%", "battery"},
		} {
			msg := map[string]any{
				"name":                name + s.label,
				"unique_id":           b.deviceID + "_" + name + s.id,
				"state_topic":         b.driverTopic(name, s.id[1:]),
				"unit_of_measurement": s.unit,
				"device_class":        s.class,
				"device":              dev,
			}
			data, _ := json.Marshal(msg)
			topic := fmt.Sprintf("%s/sensor/%s/%s%s/config", b.discoPrefix, b.deviceID, name, s.id)
			b.publish(topic, data, true)
		}
	}
	// Count total sensors announced (site + per-driver).
	b.mu.Lock()
	b.sensorsAnnounced = len(sensors) + len(b.driverNames)*5 // 5 per driver
	b.mu.Unlock()
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
		if err != nil { return }
		if b.cb.SetGridTarget != nil {
			_ = b.cb.SetGridTarget(f)
		}
	})
	b.client.Subscribe(b.cmdTopic("peak_limit_w"), 0, func(_ paho.Client, m paho.Message) {
		f, err := strconv.ParseFloat(string(m.Payload()), 64)
		if err != nil { return }
		if b.cb.SetPeakLimit != nil {
			_ = b.cb.SetPeakLimit(f)
		}
	})
	b.client.Subscribe(b.cmdTopic("ev_charging_w"), 0, func(_ paho.Client, m paho.Message) {
		f, err := strconv.ParseFloat(string(m.Payload()), 64)
		if err != nil { return }
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
	if interval <= 0 { interval = 5 * time.Second }
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
	// Record the publish tick so /api/ha/status can show liveness.
	b.mu.Lock()
	b.lastPublishMs = time.Now().UnixMilli()
	b.mu.Unlock()
	// Site-level aggregates
	b.ctrlMu.Lock()
	siteMeter := b.ctrl.SiteMeterDriver
	mode := string(b.ctrl.Mode)
	gridTarget := b.ctrl.GridTargetW
	batteryCoversEV := b.ctrl.BatteryCoversEV
	b.ctrlMu.Unlock()

	gridW := 0.0
	if r := b.tel.Get(siteMeter, telemetry.DerMeter); r != nil {
		gridW = r.SmoothedW
	}
	var pvW, batW, sumSoC float64
	var socCount int
	for _, r := range b.tel.ReadingsByType(telemetry.DerPV) { pvW += r.SmoothedW }
	for _, r := range b.tel.ReadingsByType(telemetry.DerBattery) {
		batW += r.SmoothedW
		if r.SoC != nil {
			sumSoC += *r.SoC
			socCount++
		}
	}
	avgSoC := 0.0
	if socCount > 0 { avgSoC = sumSoC / float64(socCount) }
	loadW := gridW - batW - pvW
	if loadW < 0 { loadW = 0 }

	b.publishValue("grid_w", gridW)
	b.publishValue("pv_w", pvW)
	b.publishValue("bat_w", batW)
	b.publishValue("load_w", loadW)
	b.publishValue("bat_soc_pct", avgSoC*100)
	b.publishValue("grid_target_w", gridTarget)
	b.publishString("mode", mode)
	bceState := "OFF"
	if batteryCoversEV {
		bceState = "ON"
	}
	b.publishString("battery_covers_ev", bceState)

	// Per-driver
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
	}
}

// ---- Helpers ----

func (b *Bridge) stateTopic(name string) string { return b.topicPrefix + "/state/" + name }
func (b *Bridge) cmdTopic(name string) string   { return b.topicPrefix + "/cmd/" + name }
func (b *Bridge) driverTopic(driver, field string) string {
	return fmt.Sprintf("%s/driver/%s/%s", b.topicPrefix, driver, field)
}

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
