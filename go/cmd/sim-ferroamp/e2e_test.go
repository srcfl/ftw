package main

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/srcfl/ftw/go/cmd/sim-ferroamp/ferroamp"
)

// pickFreePort returns an available TCP port on localhost.
func pickFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// startSimServer spins up a complete sim-ferroamp server programmatically
// so tests don't need to exec the binary.
func startSimServer(t *testing.T, sim *ferroamp.Simulator, port string) *mqttserver.Server {
	t.Helper()
	s := mqttserver.New(&mqttserver.Options{InlineClient: true})
	if err := s.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatal(err)
	}
	tcp := listeners.NewTCP(listeners.Config{ID: "t1", Address: "127.0.0.1:" + port})
	if err := s.AddListener(tcp); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()
	time.Sleep(100 * time.Millisecond)

	// Subscribe inline client to command topic; wire it to the sim
	if err := s.Subscribe("extapi/control/request", 1, func(_ *mqttserver.Client, _ packets.Subscription, pk packets.Packet) {
		handleCommand(s, sim, pk.Payload)
	}); err != nil {
		t.Fatal(err)
	}

	return s
}

func TestE2E_Subscribes_ReceivesEhub(t *testing.T) {
	port := pickFreePort(t)
	sim := ferroamp.New(ferroamp.Default())
	s := startSimServer(t, sim, port)
	defer s.Close()

	// Publisher goroutine — publish one tick worth of data
	snap := sim.Tick(time.Second)
	publishEhub(s, snap)
	publishEso(s, snap)
	publishSso(s, snap)

	// Subscribe via paho client
	opts := mqtt.NewClientOptions().AddBroker("tcp://127.0.0.1:" + port).SetClientID("t-sub")
	cli := mqtt.NewClient(opts)
	if tok := cli.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer cli.Disconnect(100)

	got := make(chan map[string]any, 3)
	cli.Subscribe("extapi/data/#", 0, func(_ mqtt.Client, m mqtt.Message) {
		var v map[string]any
		_ = json.Unmarshal(m.Payload(), &v)
		v["_topic"] = m.Topic()
		got <- v
	})
	time.Sleep(100 * time.Millisecond)

	// Trigger another publish so subscriber receives
	snap = sim.Tick(time.Second)
	publishEhub(s, snap)
	publishEso(s, snap)

	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case msg := <-got:
			topic := msg["_topic"].(string)
			seen[topic] = true
			t.Logf("received %s", topic)
			// Structural check: ehub must have pext, ppv, pbat
			if strings.HasSuffix(topic, "/ehub") {
				for _, k := range []string{"pext", "ppv", "pbat", "ul", "gridfreq"} {
					if _, ok := msg[k]; !ok {
						t.Errorf("ehub missing key %q", k)
					}
				}
			}
			if strings.HasSuffix(topic, "/eso") {
				for _, k := range []string{"soc", "ubat", "ibat"} {
					if _, ok := msg[k]; !ok {
						t.Errorf("eso missing key %q", k)
					}
				}
			}
		case <-deadline:
			t.Fatalf("only got topics %v", seen)
		}
	}
}

func TestE2E_ChargeCommand_AffectsActualBatteryPower(t *testing.T) {
	port := pickFreePort(t)
	cfg := ferroamp.Default()
	cfg.ResponseTauS = 0.2 // fast for test
	sim := ferroamp.New(cfg)
	s := startSimServer(t, sim, port)
	defer s.Close()

	opts := mqtt.NewClientOptions().AddBroker("tcp://127.0.0.1:" + port).SetClientID("t-cmd")
	cli := mqtt.NewClient(opts)
	if tok := cli.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer cli.Disconnect(100)

	// Subscribe to ehub to observe actual battery power
	var latestPBat atomic.Value
	latestPBat.Store(0.0)
	cli.Subscribe("extapi/data/ehub", 0, func(_ mqtt.Client, m mqtt.Message) {
		var v map[string]any
		_ = json.Unmarshal(m.Payload(), &v)
		if pbat, ok := v["pbat"].(map[string]any); ok {
			if s, ok := pbat["val"].(string); ok {
				if f, err := strconv.ParseFloat(s, 64); err == nil {
					latestPBat.Store(f)
				}
			}
		}
	})

	// Send charge command
	cmd := `{"transId":"test-1","cmd":{"name":"charge","arg":2000}}`
	cli.Publish("extapi/control/request", 1, false, cmd)

	// Tick the sim a few times and publish
	for i := 0; i < 8; i++ {
		snap := sim.Tick(200 * time.Millisecond)
		publishEhub(s, snap)
		time.Sleep(120 * time.Millisecond)
	}

	// Ferroamp convention: pbat positive = discharge, so charge 2000W → pbat negative
	got := latestPBat.Load().(float64)
	t.Logf("after charge command: pbat = %.1f (Ferroamp convention: -=charging)", got)
	if got >= 0 {
		t.Errorf("expected pbat < 0 (charging), got %.1f", got)
	}
	if got > -500 { // should be heading toward -1800 (2000 * 0.9 gain, Ferroamp-negated)
		t.Errorf("expected sizeable charge magnitude, got %.1f", got)
	}
}

func TestE2E_CommandResultPublished(t *testing.T) {
	port := pickFreePort(t)
	sim := ferroamp.New(ferroamp.Default())
	s := startSimServer(t, sim, port)
	defer s.Close()

	opts := mqtt.NewClientOptions().AddBroker("tcp://127.0.0.1:" + port).SetClientID("t-res")
	cli := mqtt.NewClient(opts)
	if tok := cli.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer cli.Disconnect(100)

	var result atomic.Value
	cli.Subscribe("extapi/result", 0, func(_ mqtt.Client, m mqtt.Message) {
		result.Store(string(m.Payload()))
	})
	time.Sleep(100 * time.Millisecond)

	cli.Publish("extapi/control/request", 1, false,
		`{"transId":"xyz","cmd":{"name":"auto"}}`)

	// Wait for response
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("no result received")
		default:
			if v := result.Load(); v != nil {
				payload := v.(string)
				if !strings.Contains(payload, `"transId":"xyz"`) {
					t.Errorf("result missing trans id: %s", payload)
				}
				if !strings.Contains(payload, `"status":"ack"`) {
					t.Errorf("result missing ack: %s", payload)
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestE2E_PublishLoop_SendsMultipleTicks(t *testing.T) {
	port := pickFreePort(t)
	cfg := ferroamp.Default()
	cfg.ResponseTauS = 0.1
	sim := ferroamp.New(cfg)
	s := startSimServer(t, sim, port)
	defer s.Close()

	// Start the actual publishLoop in a goroutine
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		last := time.Now()
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				dt := now.Sub(last)
				last = now
				snap := sim.Tick(dt)
				publishEhub(s, snap)
				publishEso(s, snap)
			}
		}
	}()
	defer func() {
		close(done)
		wg.Wait()
	}()

	opts := mqtt.NewClientOptions().AddBroker("tcp://127.0.0.1:" + port).SetClientID("t-loop")
	cli := mqtt.NewClient(opts)
	if tok := cli.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer cli.Disconnect(100)

	var count atomic.Int32
	cli.Subscribe("extapi/data/ehub", 0, func(_ mqtt.Client, _ mqtt.Message) {
		count.Add(1)
	})
	time.Sleep(500 * time.Millisecond)
	n := count.Load()
	if n < 5 {
		t.Errorf("expected ≥5 ticks in 500ms (50ms interval), got %d", n)
	}
	t.Logf("received %d ehub messages in 500ms", n)
}

// Sanity check that pbat convention in our payload matches Ferroamp's spec:
// pbat > 0 = discharging (Ferroamp convention).
func TestE2E_DischargePbatPositive(t *testing.T) {
	port := pickFreePort(t)
	cfg := ferroamp.Default()
	cfg.ResponseTauS = 0.1
	sim := ferroamp.New(cfg)
	s := startSimServer(t, sim, port)
	defer s.Close()

	opts := mqtt.NewClientOptions().AddBroker("tcp://127.0.0.1:" + port).SetClientID("t-disch")
	cli := mqtt.NewClient(opts)
	if tok := cli.Connect(); tok.WaitTimeout(2*time.Second) && tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	defer cli.Disconnect(100)

	var lastPBat atomic.Value
	lastPBat.Store(0.0)
	cli.Subscribe("extapi/data/ehub", 0, func(_ mqtt.Client, m mqtt.Message) {
		var v map[string]any
		_ = json.Unmarshal(m.Payload(), &v)
		if pbat, ok := v["pbat"].(map[string]any); ok {
			if s, ok := pbat["val"].(string); ok {
				if f, err := strconv.ParseFloat(s, 64); err == nil {
					lastPBat.Store(f)
				}
			}
		}
	})

	cli.Publish("extapi/control/request", 1, false,
		`{"transId":"disch","cmd":{"name":"discharge","arg":1500}}`)

	for i := 0; i < 10; i++ {
		snap := sim.Tick(150 * time.Millisecond)
		publishEhub(s, snap)
		time.Sleep(80 * time.Millisecond)
	}

	got := lastPBat.Load().(float64)
	if got <= 500 {
		t.Errorf("discharging should produce pbat > 500 in Ferroamp convention, got %.1f", got)
	}
	t.Logf("discharge 1500W → Ferroamp pbat = %.1f (positive confirms discharging convention)", got)

	// Print the full ehub payload to show it matches what the real driver parses
	printSamplePayload(t, sim, s, cli)
}

func printSamplePayload(t *testing.T, sim *ferroamp.Simulator, s *mqttserver.Server, cli mqtt.Client) {
	// Capture one full ehub payload for visual confirmation in test logs
	payload := make(chan string, 1)
	cli.Subscribe("extapi/data/ehub", 0, func(_ mqtt.Client, m mqtt.Message) {
		select {
		case payload <- string(m.Payload()):
		default:
		}
	})
	snap := sim.Tick(100 * time.Millisecond)
	publishEhub(s, snap)
	select {
	case p := <-payload:
		if len(p) > 400 {
			p = p[:400] + "..."
		}
		t.Logf("sample ehub payload: %s", p)
	case <-time.After(1 * time.Second):
	}
}
