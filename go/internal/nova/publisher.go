package nova

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Publisher owns one MQTT connection to Nova's broker plus a periodic
// publish loop. It mirrors the shape of internal/ha.Bridge so the
// wiring in main.go reads consistently for operators who know HA.
type Publisher struct {
	cfg   *config.Nova
	id    *Identity
	store *state.Store
	tel   *telemetry.Store

	client paho.Client

	stop chan struct{}
	done chan struct{}

	mu             sync.Mutex
	lastPublishMs  int64
	publishedCount int64
	missingDERs    map[string]bool // one-shot WARN dedupe per (device,type)
}

// Start connects to Nova's MQTT broker (JWT-as-password) and begins the
// publish loop. Returns immediately; the goroutine runs until Stop.
// Safe to pass a nil cfg — returns (nil, nil) so callers can gate on
// `cfg.Nova != nil && cfg.Nova.Enabled`.
func Start(cfg *config.Nova, id *Identity, store *state.Store, tel *telemetry.Store) (*Publisher, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, nil
	}
	if id == nil {
		return nil, fmt.Errorf("nova.Start: identity is required")
	}
	p := &Publisher{
		cfg:         cfg,
		id:          id,
		store:       store,
		tel:         tel,
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		missingDERs: make(map[string]bool),
	}

	scheme := "tcp"
	if cfg.MQTTTLS {
		scheme = "ssl"
	}
	opts := paho.NewClientOptions().
		AddBroker(fmt.Sprintf("%s://%s:%d", scheme, cfg.MQTTHost, cfg.MQTTPort)).
		SetClientID("ftw-nova-" + sanitizeTopicSegment(cfg.GatewaySerial)).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(10 * time.Second).
		SetKeepAlive(30 * time.Second).
		// JWT-as-password. The provider is called on every (re)connect,
		// so an expired token is always replaced before the next attempt.
		SetCredentialsProvider(func() (string, string) {
			tok, err := id.SignJWT(cfg.GatewaySerial, 10*time.Minute)
			if err != nil {
				slog.Error("nova: JWT mint failed", "err", err)
				return cfg.GatewaySerial, ""
			}
			return cfg.GatewaySerial, tok
		}).
		SetOnConnectHandler(func(_ paho.Client) {
			slog.Info("nova MQTT connected",
				"broker", fmt.Sprintf("%s:%d", cfg.MQTTHost, cfg.MQTTPort),
				"gateway_serial", cfg.GatewaySerial)
		}).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			slog.Warn("nova MQTT connection lost", "err", err)
		})
	p.client = paho.NewClient(opts)
	if tok := p.client.Connect(); tok.WaitTimeout(15*time.Second) && tok.Error() != nil {
		return nil, fmt.Errorf("nova MQTT connect: %w", tok.Error())
	}

	go p.run()
	return p, nil
}

// Stop shuts down the publish loop and disconnects from the broker.
// Idempotent.
func (p *Publisher) Stop() {
	if p == nil {
		return
	}
	select {
	case <-p.stop:
		return
	default:
		close(p.stop)
	}
	<-p.done
	p.client.Disconnect(500)
}

// IsConnected reports whether the paho client currently has an active
// connection.
func (p *Publisher) IsConnected() bool {
	if p == nil || p.client == nil {
		return false
	}
	return p.client.IsConnected()
}

// LastPublishMs is the Unix-milli timestamp of the most recent successful
// publish, or 0 if none yet.
func (p *Publisher) LastPublishMs() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastPublishMs
}

// PublishedCount is the lifetime count of successful DER publishes.
func (p *Publisher) PublishedCount() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publishedCount
}

func (p *Publisher) run() {
	defer close(p.done)
	tick := time.NewTicker(time.Duration(p.cfg.PublishIntervalS) * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-tick.C:
			p.publishOnce()
		}
	}
}

// publishOnce snapshots every registered device × der_type, assembles
// a clean DerTelemetry, translates per SchemaMode, and publishes.
// Skips devices/DERs that have not been provisioned in Nova — those
// would be dropped by Nova's topic-router anyway, and every drop
// gets a negative-cache hit that slows legitimate traffic.
func (p *Publisher) publishOnce() {
	devices, err := p.store.AllDevices()
	if err != nil {
		slog.Warn("nova: list devices", "err", err)
		return
	}
	mode := SchemaMode(p.cfg.SchemaMode)
	if mode == "" {
		mode = SchemaLegacy
	}
	nowMs := time.Now().UnixMilli()
	for _, d := range devices {
		for _, kind := range []telemetry.DerType{telemetry.DerMeter, telemetry.DerPV, telemetry.DerBattery, telemetry.DerEV, telemetry.DerV2X} {
			r := p.tel.Get(d.DriverName, kind)
			if r == nil {
				continue
			}
			nd := p.store.GetNovaDER(d.DeviceID, kind.String())
			if nd == nil {
				p.warnMissingOnce(d.DeviceID, kind.String())
				continue
			}
			payload := assemble(r, d, nowMs)
			wire, err := Encode(payload, mode)
			if err != nil {
				slog.Warn("nova: encode", "driver", d.DriverName, "type", kind, "err", err)
				continue
			}
			topic := TopicFor(p.cfg.GatewaySerial, d.DeviceID, nd.DerName)
			tok := p.client.Publish(topic, 0, false, wire)
			if !tok.WaitTimeout(2*time.Second) || tok.Error() != nil {
				slog.Warn("nova: publish", "topic", topic, "err", tok.Error())
				continue
			}
			p.mu.Lock()
			p.lastPublishMs = nowMs
			p.publishedCount++
			p.mu.Unlock()
		}
	}
}

// warnMissingOnce WARNs exactly once per (device_id, der_type) for a
// DER that isn't provisioned in Nova yet. Avoids log spam on the 5-s
// tick while still surfacing "you need to run `forty-two-watts
// nova-claim` after adding this driver".
func (p *Publisher) warnMissingOnce(deviceID, derType string) {
	k := deviceID + "|" + derType
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.missingDERs[k] {
		return
	}
	p.missingDERs[k] = true
	slog.Warn("nova: DER not provisioned — run `forty-two-watts nova-claim --reconcile`",
		"device_id", deviceID, "der_type", derType)
}

// assemble fuses the telemetry snapshot, device identity, and clean-
// payload fields carried in DerReading.Data into a DerTelemetry ready
// for Encode. Lua drivers emit with snake_case keys that already match
// the clean payload, so most extras (mppt1_v, l1_v, temp_c, …) flow
// through via json.Unmarshal verbatim.
func assemble(r *telemetry.DerReading, d state.Device, nowMs int64) *DerTelemetry {
	var out DerTelemetry
	if len(r.Data) > 0 {
		_ = json.Unmarshal(r.Data, &out)
	}
	// Envelope — overwrites whatever the driver may have set in Data.
	out.Type = r.DerType.String()
	out.Make = d.Make
	out.Serial = d.Serial
	out.HardwareID = d.DeviceID
	out.TimestampMs = nowMs
	// Prefer the raw value — ground truth in site convention. Consumers
	// can smooth as they see fit (mirrors what we store in the TS DB).
	out.W = r.RawW
	if r.SoC != nil && (r.DerType == telemetry.DerEV || r.DerType == telemetry.DerV2X) && out.VehicleSoC == nil {
		soc := *r.SoC
		out.VehicleSoC = &soc
	}
	if r.SoC != nil && r.DerType != telemetry.DerEV && r.DerType != telemetry.DerV2X && out.SoC == nil {
		soc := *r.SoC
		out.SoC = &soc
	}
	return &out
}
