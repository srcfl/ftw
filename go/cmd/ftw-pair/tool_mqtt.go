package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Compile-time check: MQTTObserveTool must satisfy the Tool interface.
var _ Tool = (*MQTTObserveTool)(nil)

const (
	mqttObserveDefaultDuration = 10.0
	mqttObserveMaxDuration     = 60.0
	mqttObserveMaxMessages     = 500
)

// MQTTObserveTool subscribes to an MQTT topic glob for a bounded duration and
// returns all received messages. It is stateless — each Handle call creates a
// fresh paho client.
type MQTTObserveTool struct{}

func NewMQTTObserveTool() *MQTTObserveTool { return &MQTTObserveTool{} }

func (t *MQTTObserveTool) Name() string { return "mqtt_observe" }

func (t *MQTTObserveTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        "mqtt_observe",
		Description: "Subscribe to an MQTT topic (supports wildcards) and collect messages for a bounded duration. Returns up to 500 messages with topic, payload, QoS, and timestamp.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"broker":     map[string]any{"type": "string", "description": "MQTT broker URL (e.g. tcp://192.168.1.10:1883)"},
				"topic":      map[string]any{"type": "string", "description": "Topic filter to subscribe to (MQTT wildcards # and + supported)"},
				"duration_s": map[string]any{"type": "number", "description": "How long to collect messages in seconds (default 10, max 60)"},
			},
			"required": []string{"broker", "topic"},
		},
	}
}

func (t *MQTTObserveTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	broker, _ := args["broker"].(string)
	topic, _ := args["topic"].(string)

	if broker == "" {
		return nil, fmt.Errorf("broker is required")
	}
	if topic == "" {
		return nil, fmt.Errorf("topic is required")
	}

	durSeconds := mqttObserveDefaultDuration
	if v, ok := args["duration_s"].(float64); ok && v > 0 {
		durSeconds = v
	}
	if durSeconds > mqttObserveMaxDuration {
		durSeconds = mqttObserveMaxDuration
	}
	dur := time.Duration(durSeconds * float64(time.Second))

	var (
		mu   sync.Mutex
		msgs []map[string]any
	)

	clientID := fmt.Sprintf("ftw-pair-observe-%d", rand.Int63()) //nolint:gosec

	opts := paho.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetConnectTimeout(5 * time.Second).
		SetAutoReconnect(false)

	client := paho.NewClient(opts)
	tok := client.Connect()
	if !tok.WaitTimeout(5 * time.Second) {
		return nil, fmt.Errorf("mqtt_observe: connect timeout")
	}
	if err := tok.Error(); err != nil {
		return nil, fmt.Errorf("mqtt_observe: connect: %w", err)
	}
	defer client.Disconnect(250)

	subTok := client.Subscribe(topic, 0, func(_ paho.Client, msg paho.Message) {
		mu.Lock()
		defer mu.Unlock()
		if len(msgs) >= mqttObserveMaxMessages {
			return
		}
		msgs = append(msgs, map[string]any{
			"topic":   msg.Topic(),
			"payload": string(msg.Payload()),
			"qos":     int(msg.Qos()),
			"at":      time.Now().UTC().Format(time.RFC3339Nano),
		})
	})
	if !subTok.WaitTimeout(5 * time.Second) {
		return nil, fmt.Errorf("mqtt_observe: subscribe timeout")
	}
	if err := subTok.Error(); err != nil {
		return nil, fmt.Errorf("mqtt_observe: subscribe: %w", err)
	}

	select {
	case <-time.After(dur):
	case <-ctx.Done():
	}

	mu.Lock()
	result := make([]map[string]any, len(msgs))
	copy(result, msgs)
	mu.Unlock()

	return map[string]any{"messages": result}, nil
}
