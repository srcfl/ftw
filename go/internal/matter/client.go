// Package matter provides a capability client for the 42W Matter sidecar
// (matter-sidecar/, built on matter.js). Speaks a JSON-over-WebSocket
// request/response protocol with correlation via message_id:
//
//	→ {"message_id":"1","command":"read_attribute","args":{...}}
//	← {"message_id":"1","result":<value>,"error_code":null}
//
// 42W does not commission devices itself — see matter-sidecar/src/index.ts
// and drivers/matter.lua's header comment for the multi-fabric "share
// device into 42W" onboarding flow.
package matter

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Capability implements drivers.MatterCap backed by matter-sidecar.
type Capability struct {
	wsURL  string
	logger *slog.Logger

	mu      sync.Mutex // guards conn + pending
	writeMu sync.Mutex // serializes WriteMessage (gorilla disallows concurrent writes)
	conn    *websocket.Conn
	pending map[string]chan wsResponse

	counter atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

type wsRequest struct {
	MessageID string `json:"message_id"`
	Command   string `json:"command"`
	Args      any    `json:"args"`
}

type wsResponse struct {
	MessageID string          `json:"message_id"`
	Result    json.RawMessage `json:"result"`
	ErrorCode *string         `json:"error_code"`
	ErrorMsg  string          `json:"error_message,omitempty"`
}

// Dial connects to the matter-sidecar process and returns a ready Capability.
// Port defaults to 5580 if zero.
func Dial(host string, port int) (*Capability, error) {
	if port == 0 {
		port = 5580
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Capability{
		wsURL:   fmt.Sprintf("ws://%s:%d/ws", host, port),
		logger:  slog.With("matter_host", host, "matter_port", port),
		pending: make(map[string]chan wsResponse),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	if err := c.connect(); err != nil {
		cancel()
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

func (c *Capability) connect() error {
	conn, _, err := websocket.DefaultDialer.Dial(c.wsURL, nil)
	if err != nil {
		return fmt.Errorf("matter: dial %s: %w", c.wsURL, err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	return nil
}

func (c *Capability) readLoop() {
	defer close(c.done)
	for {
		if c.ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()
		if conn == nil {
			time.Sleep(2 * time.Second)
			if c.ctx.Err() != nil {
				return
			}
			if err := c.connect(); err != nil {
				c.logger.Warn("matter: reconnect failed", "err", err)
			}
			continue
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			c.logger.Warn("matter: read error, reconnecting", "err", err)
			c.mu.Lock()
			c.conn = nil
			for id, ch := range c.pending {
				ch <- wsResponse{MessageID: id, ErrorCode: strPtr("connection_lost")}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			time.Sleep(2 * time.Second)
			if c.ctx.Err() != nil {
				return
			}
			if err := c.connect(); err != nil {
				c.logger.Warn("matter: reconnect failed", "err", err)
			}
			continue
		}
		var resp wsResponse
		if err := json.Unmarshal(msg, &resp); err != nil || resp.MessageID == "" {
			continue // event or unparseable — ignore
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.MessageID]
		if ok {
			delete(c.pending, resp.MessageID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
}

func (c *Capability) call(ctx context.Context, command string, args any) (json.RawMessage, error) {
	id := fmt.Sprintf("%d", c.counter.Add(1))
	ch := make(chan wsResponse, 1)
	b, err := json.Marshal(wsRequest{MessageID: id, Command: command, Args: args})
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("matter: not connected")
	}
	c.pending[id] = ch
	c.mu.Unlock()
	// Write OUTSIDE c.mu so a slow/half-open socket can't block the readLoop
	// (which needs c.mu to deliver responses or handle a read error). A
	// dedicated writeMu still serializes concurrent writes per gorilla's
	// contract.
	c.writeMu.Lock()
	err = conn.WriteMessage(websocket.TextMessage, b)
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("matter: write: %w", err)
	}
	select {
	case resp := <-ch:
		if resp.ErrorCode != nil {
			return nil, fmt.Errorf("matter: %s: %s", *resp.ErrorCode, resp.ErrorMsg)
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// NodeInfo describes one node the sidecar has joined, as returned by
// list_nodes. NodeID is the small logical id matter-sidecar/src/nodemap.ts
// hands out — the value drivers/matter.lua's config.node_id expects.
type NodeInfo struct {
	NodeID       int    `json:"node_id"`
	MatterNodeID string `json:"matter_node_id"`
}

// Commission joins a device shared from another controller's fabric using
// a one-time pairing code minted by that controller's "share device" /
// multi-admin flow. Returns the logical node_id to put in the driver's
// config.node_id field.
func (c *Capability) Commission(pairingCode string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := c.call(ctx, "commission", map[string]any{
		"pairing_code": pairingCode,
	})
	if err != nil {
		return 0, err
	}
	var res struct {
		NodeID int `json:"node_id"`
	}
	if err := json.Unmarshal(result, &res); err != nil {
		return 0, err
	}
	return res.NodeID, nil
}

// ListNodes returns every node the sidecar has joined so far.
func (c *Capability) ListNodes() ([]NodeInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := c.call(ctx, "list_nodes", map[string]any{})
	if err != nil {
		return nil, err
	}
	var nodes []NodeInfo
	if err := json.Unmarshal(result, &nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// matterEpochOffsetS is 2000-01-01T00:00:00Z expressed in Unix seconds —
// the base Matter uses for its "epoch-s" timestamp fields (see
// CommodityPriceStruct.PeriodStart/PeriodEnd in the Matter 1.5 spec).
const matterEpochOffsetS = 946684800

// UnixMsToMatterEpochS converts a Unix millisecond timestamp to Matter's
// epoch-s base, as required by PricePeriod.PeriodStartS/PeriodEndS.
func UnixMsToMatterEpochS(unixMs int64) int64 {
	return unixMs/1000 - matterEpochOffsetS
}

// PricePeriod mirrors the Matter CommodityPriceStruct (one period's price)
// exposed by matter-sidecar/src/priceserver.ts's CommodityPrice server
// endpoint. PeriodStartS/PeriodEndS use the Matter epoch — convert with
// UnixMsToMatterEpochS. PriceMinorUnits is price per the cluster's
// TariffUnit (kWh) in the currency's minor unit — for SEK/öre this is
// simply öre/kWh, since SetPriceFeed's currency is configured with
// decimalPoints=2.
type PricePeriod struct {
	PeriodStartS    int64  `json:"periodStart"`
	PeriodEndS      *int64 `json:"periodEnd"`
	PriceMinorUnits int64  `json:"price"`
}

// SetPriceFeed pushes 42W's current price and forecast into the sidecar's
// CommodityPrice server endpoint (Phase 2 — 42W exposed as a Matter
// energy-management device other controllers can read). current may be nil
// if no price is known right now; forecast may be empty.
func (c *Capability) SetPriceFeed(current *PricePeriod, forecast []PricePeriod) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if forecast == nil {
		forecast = []PricePeriod{}
	}
	_, err := c.call(ctx, "set_price_feed", map[string]any{
		"current":  current,
		"forecast": forecast,
	})
	return err
}

// PairingCode is the one-time code a third-party controller (Apple Home,
// Home Assistant, ...) needs to commission 42W itself onto their fabric —
// the inverse of Commission, where 42W joins someone else's device.
type PairingCode struct {
	ManualPairingCode string `json:"manual_pairing_code"`
	QRPairingCode     string `json:"qr_pairing_code"`
}

// GetPairingCode returns 42W's own commissioning codes.
func (c *Capability) GetPairingCode() (PairingCode, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := c.call(ctx, "get_pairing_code", map[string]any{})
	if err != nil {
		return PairingCode{}, err
	}
	var pc PairingCode
	if err := json.Unmarshal(result, &pc); err != nil {
		return PairingCode{}, err
	}
	return pc, nil
}

// ReadAttribute reads a cluster attribute from a Matter node.
// Attribute path is formatted as "endpoint/cluster/attribute" with decimal integers,
// matching the matter-sidecar wire protocol (matter-sidecar/src/index.ts).
func (c *Capability) ReadAttribute(nodeID, endpoint, clusterID, attributeID uint32) (any, error) {
	path := fmt.Sprintf("%d/%d/%d", endpoint, clusterID, attributeID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := c.call(ctx, "read_attribute", map[string]any{
		"node_id":        nodeID,
		"attribute_path": path,
	})
	if err != nil {
		return nil, err
	}
	var val any
	if err := json.Unmarshal(result, &val); err != nil {
		return nil, err
	}
	return val, nil
}

// WriteAttribute writes a value to a cluster attribute on a Matter node.
func (c *Capability) WriteAttribute(nodeID, endpoint, clusterID, attributeID uint32, value any) error {
	path := fmt.Sprintf("%d/%d/%d", endpoint, clusterID, attributeID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.call(ctx, "write_attribute", map[string]any{
		"node_id":        nodeID,
		"attribute_path": path,
		"value":          value,
	})
	return err
}

// InvokeCommand sends a cluster command to a Matter node.
func (c *Capability) InvokeCommand(nodeID, endpoint, clusterID uint32, commandName string, payload any) (any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := c.call(ctx, "send_command", map[string]any{
		"node_id":      nodeID,
		"endpoint_id":  endpoint,
		"cluster_id":   clusterID,
		"command_name": commandName,
		"payload":      payload,
	})
	if err != nil {
		return nil, err
	}
	var val any
	if err := json.Unmarshal(result, &val); err != nil {
		return nil, err
	}
	return val, nil
}

// Close disconnects from the matter-sidecar. Safe to call multiple times.
func (c *Capability) Close() error {
	c.cancel()
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		_ = conn.Close()
	}
	<-c.done
	return nil
}

func strPtr(s string) *string { return &s }
