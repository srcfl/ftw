// Package homelinkuplink owns Core's outbound Home Link relay connection.
package homelinkuplink

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
)

const (
	Endpoint = "wss://uplink.home.sourceful.energy/v1/uplink"

	dialTimeout       = 10 * time.Second
	handshakeTimeout  = 5 * time.Second
	maxChallengeAhead = 30 * time.Second
	writeTimeout      = 5 * time.Second
	stableConnection  = 30 * time.Second
)

type dialer interface {
	DialContext(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error)
}

type Client struct {
	identity gatewayidentity.Identity
	endpoint string
	dialer   dialer
	now      func() time.Time
	random   io.Reader
	wait     func(context.Context, time.Duration) error
}

type Connection struct {
	socket          *websocket.Conn
	routeHandle     string
	connectionID    string
	routeGeneration uint64
	writeMu         sync.Mutex
}

type Frame struct {
	Type         wire.Type
	Open         *wire.StreamOpen
	SessionHello *wire.SessionHello
	Sealed       *wire.Sealed
	Close        *wire.StreamClose
}

func New(identity gatewayidentity.Identity) (*Client, error) {
	return newClient(identity, Endpoint, productionDialer(), time.Now)
}

func newClient(identity gatewayidentity.Identity, endpoint string, socketDialer dialer, now func() time.Time) (*Client, error) {
	if err := gatewayidentity.Validate(identity); err != nil {
		return nil, fmt.Errorf("Home Link uplink identity: %w", err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Path != "/v1/uplink" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("Home Link uplink endpoint is invalid")
	}
	if socketDialer == nil || now == nil {
		return nil, errors.New("Home Link uplink dependency is missing")
	}
	return &Client{
		identity: identity, endpoint: endpoint, dialer: socketDialer, now: now,
		random: rand.Reader, wait: waitContext,
	}, nil
}

// Run maintains only the authenticated machine connection. Application
// requests are never queued or replayed across a reconnect.
func (c *Client) Run(ctx context.Context, service *Service) error {
	return c.RunWithStatus(ctx, service, nil)
}

// RunWithStatus maintains the outbound connection and reports only its
// connected state plus the fixed local error class chosen by the caller.
func (c *Client) RunWithStatus(
	ctx context.Context,
	service *Service,
	status func(bool, error),
) error {
	if service == nil {
		return errors.New("Home Link uplink service is missing")
	}
	return c.runWithStatus(ctx, c.Dial, service.Serve, c.retryDelay, status)
}

func (c *Client) run(
	ctx context.Context,
	dial func(context.Context) (*Connection, error),
	serve func(context.Context, *Connection) error,
	retry func(int) (time.Duration, error),
) error {
	return c.runWithStatus(ctx, dial, serve, retry, nil)
}

func (c *Client) runWithStatus(
	ctx context.Context,
	dial func(context.Context) (*Connection, error),
	serve func(context.Context, *Connection) error,
	retry func(int) (time.Duration, error),
	status func(bool, error),
) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		connection, err := dial(ctx)
		if err == nil {
			if status != nil {
				status(true, nil)
			}
			connectedAt := c.now()
			err = serve(ctx, connection)
			if c.now().Sub(connectedAt) >= stableConnection {
				attempt = 0
			}
		}
		if status != nil && ctx.Err() == nil {
			status(false, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay, delayErr := retry(attempt)
		if delayErr != nil {
			return delayErr
		}
		if attempt < 6 {
			attempt++
		}
		if err := c.wait(ctx, delay); err != nil {
			return err
		}
	}
}

func (c *Client) retryDelay(attempt int) (time.Duration, error) {
	maximumSeconds := int64(1)
	if attempt > 0 {
		maximumSeconds <<= min(attempt, 6)
	}
	if maximumSeconds > 60 {
		maximumSeconds = 60
	}
	span := big.NewInt(maximumSeconds)
	value, err := rand.Int(c.random, span)
	if err != nil {
		return 0, fmt.Errorf("create Home Link retry delay: %w", err)
	}
	return time.Duration(value.Int64()+1) * time.Second, nil
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func productionDialer() *websocket.Dialer {
	return &websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: dialTimeout,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			ServerName: gatewayidentity.SoftwareIdentityUplinkHost,
		},
	}
}

func (c *Client) Dial(ctx context.Context) (*Connection, error) {
	dialContext, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	socket, response, err := c.dialer.DialContext(dialContext, c.endpoint, nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return nil, fmt.Errorf("connect Home Link uplink: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = socket.Close()
		}
	}()
	socket.SetReadLimit(wire.MaxHandshakeBytes)
	_ = socket.SetReadDeadline(c.now().Add(handshakeTimeout))

	publicKey := c.identity.PublicKey()
	routeHandle, err := gatewayidentity.RouteHandle(publicKey)
	if err != nil {
		return nil, err
	}
	hello := wire.MachineHello{
		Version: wire.Version, Type: wire.TypeMachineHello,
		GatewayID: c.identity.GatewayID(), RouteHandle: routeHandle,
		PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
	}
	if err := writeJSON(socket, hello, wire.MaxHandshakeBytes); err != nil {
		return nil, fmt.Errorf("send Home Link machine hello: %w", err)
	}
	challengeData, err := readText(socket)
	if err != nil {
		return nil, fmt.Errorf("read Home Link machine challenge: %w", err)
	}
	challenge, err := wire.DecodeMachineChallenge(challengeData)
	if err != nil {
		return nil, err
	}
	now := c.now().UTC()
	expiresAt := time.UnixMilli(challenge.ExpiresAtMS)
	if !expiresAt.After(now) || expiresAt.After(now.Add(maxChallengeAhead)) {
		return nil, errors.New("Home Link machine challenge expiry is invalid")
	}
	proof := wire.MachineProof{
		Version: wire.Version, Type: wire.TypeMachineProof,
		ConnectionID: challenge.ConnectionID,
		GatewayID:    c.identity.GatewayID(), RouteHandle: routeHandle,
		PublicKey: hello.PublicKey, Nonce: challenge.Nonce,
		ExpiresAtMS: challenge.ExpiresAtMS,
	}
	transcript, err := wire.MachineProofMessage(proof)
	if err != nil {
		return nil, err
	}
	signature, err := c.identity.Sign(transcript)
	if err != nil {
		return nil, fmt.Errorf("sign Home Link machine proof: %w", err)
	}
	proof.Signature = base64.RawURLEncoding.EncodeToString(signature)
	if err := writeJSON(socket, proof, wire.MaxHandshakeBytes); err != nil {
		return nil, fmt.Errorf("send Home Link machine proof: %w", err)
	}
	readyData, err := readText(socket)
	if err != nil {
		return nil, fmt.Errorf("read Home Link machine ready: %w", err)
	}
	ready, err := wire.DecodeMachineReady(readyData)
	if err != nil {
		return nil, err
	}
	if ready.ConnectionID != challenge.ConnectionID || ready.RouteHandle != routeHandle {
		return nil, errors.New("Home Link machine ready does not match the authenticated connection")
	}
	socket.SetReadLimit(wire.MaxSealedFrameBytes)
	_ = socket.SetReadDeadline(time.Time{})
	ok = true
	return &Connection{
		socket: socket, routeHandle: routeHandle,
		connectionID: ready.ConnectionID, routeGeneration: ready.RouteGeneration,
	}, nil
}

func (c *Connection) RouteHandle() string     { return c.routeHandle }
func (c *Connection) ConnectionID() string    { return c.connectionID }
func (c *Connection) RouteGeneration() uint64 { return c.routeGeneration }

func (c *Connection) ReadFrame() (Frame, error) {
	data, err := readText(c.socket)
	if err != nil {
		return Frame{}, err
	}
	messageType, err := wire.MessageType(data, wire.MaxSealedFrameBytes)
	if err != nil {
		return Frame{}, err
	}
	switch messageType {
	case wire.TypeStreamOpen:
		message, err := wire.DecodeStreamOpen(data)
		if err != nil || message.RouteHandle != c.routeHandle ||
			message.ConnectionID != c.connectionID ||
			message.RouteGeneration != c.routeGeneration {
			return Frame{}, errors.New("Home Link stream open is invalid")
		}
		return Frame{Type: messageType, Open: &message}, nil
	case wire.TypeSessionHello:
		message, _, err := wire.DecodeSessionHello(data)
		if err != nil || message.RouteHandle != c.routeHandle ||
			message.ConnectionID != c.connectionID ||
			message.RouteGeneration != c.routeGeneration {
			return Frame{}, errors.New("Home Link session hello is invalid")
		}
		return Frame{Type: messageType, SessionHello: &message}, nil
	case wire.TypeSealed:
		message, err := wire.DecodeSealed(data)
		if err != nil {
			return Frame{}, err
		}
		return Frame{Type: messageType, Sealed: &message}, nil
	case wire.TypeStreamClose:
		message, err := wire.DecodeStreamClose(data)
		if err != nil {
			return Frame{}, err
		}
		return Frame{Type: messageType, Close: &message}, nil
	default:
		return Frame{}, errors.New("Home Link uplink frame type is not allowed")
	}
}

func (c *Connection) SendSessionAccept(message wire.SessionAccept) error {
	data, err := wire.Encode(message, wire.MaxHandshakeBytes)
	if err != nil {
		return err
	}
	if _, _, _, _, err := wire.DecodeSessionAccept(data); err != nil {
		return err
	}
	if message.RouteHandle != c.routeHandle {
		return errors.New("Home Link session accept route does not match the connection")
	}
	if message.ConnectionID != c.connectionID ||
		message.RouteGeneration != c.routeGeneration {
		return errors.New("Home Link session accept generation does not match the connection")
	}
	return c.writeJSON(message, wire.MaxHandshakeBytes)
}

func (c *Connection) SendSealed(message wire.Sealed) error {
	data, err := wire.Encode(message, wire.MaxSealedFrameBytes)
	if err != nil {
		return err
	}
	if _, err := wire.DecodeSealed(data); err != nil {
		return err
	}
	return c.writeJSON(message, wire.MaxSealedFrameBytes)
}

func (c *Connection) CloseStream(streamID, code string) error {
	message := wire.StreamClose{
		Version: wire.Version, Type: wire.TypeStreamClose,
		StreamID: streamID, Code: code,
	}
	data, err := wire.Encode(message, wire.MaxHandshakeBytes)
	if err != nil {
		return err
	}
	if _, err := wire.DecodeStreamClose(data); err != nil {
		return err
	}
	return c.writeJSON(message, wire.MaxHandshakeBytes)
}

func (c *Connection) Close() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.socket.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		time.Now().Add(writeTimeout))
	return c.socket.Close()
}

func (c *Connection) writeJSON(value any, limit int) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeJSON(c.socket, value, limit)
}

func writeJSON(socket *websocket.Conn, value any, limit int) error {
	data, err := wire.Encode(value, limit)
	if err != nil {
		return err
	}
	_ = socket.SetWriteDeadline(time.Now().Add(writeTimeout))
	return socket.WriteMessage(websocket.TextMessage, data)
}

func readText(socket *websocket.Conn) ([]byte, error) {
	messageType, data, err := socket.ReadMessage()
	if err != nil {
		return nil, err
	}
	if messageType != websocket.TextMessage {
		return nil, errors.New("Home Link uplink accepts text envelopes only")
	}
	return data, nil
}
