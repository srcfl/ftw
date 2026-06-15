// Package matter provides a capability client for python-matter-server.
//
// python-matter-server exposes a WebSocket API on ws://<host>:<port>/ws.
// Each Capability is one persistent connection owned by one driver.
// Protocol is JSON with request/response correlation via message_id:
//
//	→ {"message_id":"1","command":"read_attribute","args":{...}}
//	← {"message_id":"1","result":<value>,"error_code":null}
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

// Capability implements drivers.MatterCap backed by python-matter-server.
type Capability struct {
	wsURL  string
	logger *slog.Logger

	mu      sync.Mutex
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

// Dial connects to python-matter-server and returns a ready Capability.
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
	err = conn.WriteMessage(websocket.TextMessage, b)
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("matter: write: %w", err)
	}
	c.mu.Unlock()
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

// ReadAttribute reads a cluster attribute from a Matter node.
// Attribute path is formatted as "endpoint/0xCLUSTER/0xATTRIBUTE".
func (c *Capability) ReadAttribute(nodeID, endpoint, clusterID, attributeID uint32) (any, error) {
	path := fmt.Sprintf("%d/0x%04X/0x%04X", endpoint, clusterID, attributeID)
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
	path := fmt.Sprintf("%d/0x%04X/0x%04X", endpoint, clusterID, attributeID)
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

// Close disconnects from python-matter-server. Safe to call multiple times.
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
