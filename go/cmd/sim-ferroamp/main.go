// sim-ferroamp: embedded MQTT broker + realistic Ferroamp EnergyHub simulator.
//
// Starts an MQTT broker on :1883 (configurable) and publishes realistic
// extapi/data/{ehub,eso,sso} telemetry every second. Subscribes to
// extapi/control/request and acts on charge/discharge/auto/pplim commands,
// with first-order response lag so you can actually see the controller chase
// targets.
//
// Run:    go run ./cmd/sim-ferroamp
// Debug:  mosquitto_sub -h localhost -t 'extapi/#' -v
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/srcfl/ftw/go/cmd/sim-ferroamp/ferroamp"
)

func main() {
	addr := flag.String("addr", ":1883", "MQTT broker bind address")
	tick := flag.Duration("tick", time.Second, "telemetry publish interval")
	initSoC := flag.Float64("soc", 0.5, "starting SoC (0..1)")
	capWh := flag.Float64("capacity-wh", 15200, "battery capacity in Wh")
	pvPeak := flag.Float64("pv-peak", 0, "override PV power (constant W); 0 = time-of-day curve")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg := ferroamp.Default()
	cfg.SoC = *initSoC
	cfg.CapacityWh = *capWh
	cfg.PVPeakW = *pvPeak
	sim := ferroamp.New(cfg)

	// ---- Embedded MQTT broker ----
	server := mqttserver.New(&mqttserver.Options{InlineClient: true})
	if err := server.AddHook(new(auth.AllowHook), nil); err != nil {
		slog.Error("failed to add allow hook", "err", err)
		os.Exit(1)
	}
	tcp := listeners.NewTCP(listeners.Config{ID: "tcp1", Address: *addr})
	if err := server.AddListener(tcp); err != nil {
		slog.Error("failed to add listener", "err", err)
		os.Exit(1)
	}
	go func() {
		if err := server.Serve(); err != nil {
			slog.Error("broker failed", "err", err)
		}
	}()
	defer server.Close()
	time.Sleep(100 * time.Millisecond)
	slog.Info("MQTT broker listening", "addr", *addr)

	// ---- Control command subscriber: inline client subscribes to request topic ----
	if err := server.Subscribe("extapi/control/request", 1, func(cl *mqttserver.Client, sub packets.Subscription, pk packets.Packet) {
		handleCommand(server, sim, pk.Payload)
	}); err != nil {
		slog.Error("failed to subscribe", "err", err)
		os.Exit(1)
	}
	slog.Info("subscribed to extapi/control/request")

	// ---- Telemetry publisher loop ----
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go publishLoop(ctx, server, sim, *tick)

	// ---- Graceful shutdown ----
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	<-sigc
	slog.Info("shutdown")
}

// handleCommand parses an extapi/control/request payload and mutates the sim.
// Also publishes a confirmation to extapi/result, the topic real EnergyHubs use.
func handleCommand(server *mqttserver.Server, sim *ferroamp.Simulator, payload []byte) {
	var msg struct {
		TransID string `json:"transId"`
		Cmd     struct {
			Name string  `json:"name"`
			Arg  float64 `json:"arg"`
		} `json:"cmd"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		slog.Warn("bad command payload", "err", err, "payload", string(payload))
		return
	}

	status := "ack"
	switch msg.Cmd.Name {
	case "charge":
		sim.SetMode(ferroamp.ModeCharge, msg.Cmd.Arg)
		slog.Info("cmd charge", "trans", msg.TransID, "arg", msg.Cmd.Arg)
	case "discharge":
		sim.SetMode(ferroamp.ModeDischarge, msg.Cmd.Arg)
		slog.Info("cmd discharge", "trans", msg.TransID, "arg", msg.Cmd.Arg)
	case "auto":
		sim.SetMode(ferroamp.ModeAuto, 0)
		slog.Info("cmd auto", "trans", msg.TransID)
	case "extapiversion":
		slog.Info("cmd extapiversion", "trans", msg.TransID)
	case "pplim":
		// Curtail — we just accept it silently
		slog.Info("cmd pplim", "trans", msg.TransID, "arg", msg.Cmd.Arg)
	default:
		status = "nak"
		slog.Warn("unknown command", "name", msg.Cmd.Name)
	}

	result := fmt.Sprintf(`{"transId":"%s","status":"%s"}`, msg.TransID, status)
	if err := server.Publish("extapi/result", []byte(result), false, 0); err != nil {
		slog.Warn("failed to publish result", "err", err)
	}
}

// publishLoop ticks the sim and publishes the three telemetry topics at each interval.
func publishLoop(ctx context.Context, server *mqttserver.Server, sim *ferroamp.Simulator, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()

	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			dt := now.Sub(last)
			last = now
			snap := sim.Tick(dt)

			publishEhub(server, snap)
			publishEso(server, snap)
			// sso published less frequently (per-string PV details we don't really use)
			if now.Unix()%5 == 0 {
				publishSso(server, snap)
			}
		}
	}
}

// Ferroamp payload helpers -----------------------------------------------------

// valStr wraps a value in Ferroamp's {"val":"..."} format. Ferroamp publishes
// many values as strings even though they're numeric, so we match that.
func valStr(v float64) any {
	return map[string]string{"val": fmt.Sprintf("%.3f", v)}
}

// phaseStr wraps three per-phase values in {"L1":..,"L2":..,"L3":..} format.
func phaseStr(v float64) any {
	// Split evenly across three phases — good enough for sim
	each := v / 3
	return map[string]string{
		"L1": fmt.Sprintf("%.3f", each),
		"L2": fmt.Sprintf("%.3f", each),
		"L3": fmt.Sprintf("%.3f", each),
	}
}

// mJstr converts Wh to mJ (Ferroamp's native unit) and formats as string.
// Wh × 3_600_000 = mJ (since 1 Wh = 3600 J = 3_600_000 mJ).
func mJstr(wh float64) any {
	return map[string]string{"val": fmt.Sprintf("%.0f", wh*3_600_000)}
}

func publishEhub(server *mqttserver.Server, s ferroamp.Snapshot) {
	payload := map[string]any{
		// Per-phase grid power (W), sign convention: import=+, export=-
		"pext": phaseStr(s.GridW),
		// Per-phase voltage
		"ul": map[string]string{
			"L1": fmt.Sprintf("%.1f", 230.0),
			"L2": fmt.Sprintf("%.1f", 230.0),
			"L3": fmt.Sprintf("%.1f", 230.0),
		},
		// Per-phase grid current at the service-entrance CTs (the
		// quantity pext measures). In the live protocol iext is what
		// the driver reads for fuse bars + fuse_over_limit detection
		// (#160); il is inverter AC current and is not what we need
		// here. Populate both at W/V so the sim stays valid for any
		// downstream that still peeks at il.
		"iext": map[string]string{
			"L1": fmt.Sprintf("%.2f", s.GridW/3/230.0),
			"L2": fmt.Sprintf("%.2f", s.GridW/3/230.0),
			"L3": fmt.Sprintf("%.2f", s.GridW/3/230.0),
		},
		"il": map[string]string{
			"L1": fmt.Sprintf("%.2f", s.GridW/3/230.0),
			"L2": fmt.Sprintf("%.2f", s.GridW/3/230.0),
			"L3": fmt.Sprintf("%.2f", s.GridW/3/230.0),
		},
		"gridfreq":     map[string]string{"val": "50.00"},
		"wextconsq3p":  mJstr(s.ImportWh),
		"wextprodq3p":  mJstr(s.ExportWh),
		// PV: Ferroamp reports positive magnitude
		"ppv": valStr(s.PVW),
		// Battery power: Ferroamp convention = positive means discharging.
		// Our snap.ActualBatW is EMS convention: +=charging. So negate.
		"pbat": valStr(-s.ActualBatW),
		// Timestamp (ISO 8601)
		"ts": map[string]string{"val": time.Now().Format(time.RFC3339)},
	}
	b, _ := json.Marshal(payload)
	_ = server.Publish("extapi/data/ehub", b, false, 0)
}

func publishEso(server *mqttserver.Server, s ferroamp.Snapshot) {
	payload := map[string]any{
		"soc": map[string]string{"val": fmt.Sprintf("%.2f", s.SoC*100)}, // 0-100%
		// Battery voltage & current — approximate
		"ubat":     map[string]string{"val": "48.0"},
		"ibat":     map[string]string{"val": fmt.Sprintf("%.2f", -s.ActualBatW/48.0)},
		"wbatprod": mJstr(s.BatDischargeWh),
		"wbatcons": mJstr(s.BatChargeWh),
		"ts":       map[string]string{"val": time.Now().Format(time.RFC3339)},
	}
	b, _ := json.Marshal(payload)
	_ = server.Publish("extapi/data/eso", b, false, 0)
}

func publishSso(server *mqttserver.Server, s ferroamp.Snapshot) {
	// One solar string for simplicity
	payload := map[string]any{
		"id":   map[string]string{"val": "sso1"},
		"ppv":  valStr(s.PVW),
		"upv":  map[string]string{"val": "400.0"},
		"ipv":  map[string]string{"val": fmt.Sprintf("%.2f", s.PVW/400.0)},
		"ts":   map[string]string{"val": time.Now().Format(time.RFC3339)},
	}
	b, _ := json.Marshal(payload)
	_ = server.Publish("extapi/data/sso", b, false, 0)
}
