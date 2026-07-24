// Package homelinkrelay routes opaque Home Link frames between one gateway
// uplink and bounded browser streams.
package homelinkrelay

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
)

const (
	DefaultBrowserOrigin = "https://home.sourceful.energy"
	MaxBrowserStreams    = wire.MaxBrowserStreams

	authTimeout       = 5 * time.Second
	confirmTimeout    = 10 * time.Second
	browserIdle       = 60 * time.Second
	sessionLifetime   = 5 * time.Minute
	writeTimeout      = 5 * time.Second
	pongTimeout       = 45 * time.Second
	pingInterval      = 20 * time.Second
	browserQueueDepth = 4
)

type InviteLookup interface {
	CanonicalPublicKey(context.Context, string) ([]byte, error)
}

type Options struct {
	Invites       InviteLookup
	Random        io.Reader
	Now           func() time.Time
	BrowserOrigin string
}

type Server struct {
	invites       InviteLookup
	random        io.Reader
	now           func() time.Time
	browserOrigin string
	confirmLimit  time.Duration
	upgrader      websocket.Upgrader

	mu          sync.Mutex
	routes      map[string]*gatewayRoute
	generations map[string]uint64
}

type gatewayRoute struct {
	server       *Server
	connectionID string
	handle       string
	gatewayID    string
	generation   uint64
	conn         *websocket.Conn
	writeMu      sync.Mutex

	mu      sync.Mutex
	streams map[string]*browserStream
	closed  bool
}

type browserStream struct {
	id   string
	conn *websocket.Conn

	mu          sync.Mutex
	helloSeen   bool
	acceptSeen  bool
	confirmed   bool
	confirmedAt time.Time
	browserSeq  uint64
	gatewaySeq  uint64
	toBrowser   chan []byte
	confirmedC  chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
}

func New(opts Options) (*Server, error) {
	if opts.Invites == nil {
		return nil, errors.New("relay invite lookup is required")
	}
	if opts.Random == nil {
		opts.Random = rand.Reader
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.BrowserOrigin == "" {
		opts.BrowserOrigin = DefaultBrowserOrigin
	}
	if opts.BrowserOrigin != DefaultBrowserOrigin {
		return nil, errors.New("relay browser origin must use the Home Link origin")
	}
	return &Server{
		invites: opts.Invites, random: opts.Random, now: opts.Now,
		browserOrigin: opts.BrowserOrigin,
		confirmLimit:  confirmTimeout,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: authTimeout,
			CheckOrigin:      func(*http.Request) bool { return true },
		},
		routes: make(map[string]*gatewayRoute), generations: make(map[string]uint64),
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/uplink", s.handleUplink)
	mux.HandleFunc("GET /v1/browser/{route}", s.handleBrowser)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`)
}

func (s *Server) handleUplink(w http.ResponseWriter, r *http.Request) {
	connection, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	connection.SetReadLimit(wire.MaxHandshakeBytes)
	_ = connection.SetReadDeadline(s.now().Add(authTimeout))

	helloBytes, err := readText(connection)
	if err != nil {
		closePolicy(connection, "invalid machine hello")
		return
	}
	hello, publicKey, err := wire.DecodeMachineHello(helloBytes)
	if err != nil {
		closePolicy(connection, "invalid machine hello")
		return
	}
	expectedKey, err := s.invites.CanonicalPublicKey(r.Context(), hello.GatewayID)
	if err != nil || gatewayidentity.ValidatePublicKey(expectedKey) != nil ||
		subtle.ConstantTimeCompare(expectedKey, publicKey) != 1 {
		closePolicy(connection, "machine is not invited")
		return
	}

	connectionID, err := s.randomToken(wire.ConnectionIDBytes)
	if err != nil {
		closeInternal(connection)
		return
	}
	nonce, err := s.randomToken(wire.MachineNonceBytes)
	if err != nil {
		closeInternal(connection)
		return
	}
	expiresAt := s.now().UTC().Add(authTimeout)
	challenge := wire.MachineChallenge{
		Version: wire.Version, Type: wire.TypeMachineChallenge,
		ConnectionID: connectionID, Nonce: nonce, ExpiresAtMS: expiresAt.UnixMilli(),
	}
	if err := writeJSON(connection, challenge, wire.MaxHandshakeBytes); err != nil {
		return
	}

	proofBytes, err := readText(connection)
	if err != nil {
		closePolicy(connection, "invalid machine proof")
		return
	}
	proof, proofKey, signature, err := wire.DecodeMachineProof(proofBytes)
	if err != nil ||
		proof.ConnectionID != connectionID ||
		proof.GatewayID != hello.GatewayID ||
		proof.RouteHandle != hello.RouteHandle ||
		subtle.ConstantTimeCompare(proofKey, publicKey) != 1 ||
		proof.Nonce != nonce ||
		proof.ExpiresAtMS != challenge.ExpiresAtMS ||
		s.now().UTC().UnixMilli() >= challenge.ExpiresAtMS {
		closePolicy(connection, "invalid machine proof")
		return
	}
	transcript, err := wire.MachineProofMessage(proof)
	if err != nil || !gatewayidentity.Verify(expectedKey, transcript, signature) {
		closePolicy(connection, "invalid machine proof")
		return
	}

	route := &gatewayRoute{
		server: s, connectionID: connectionID, handle: hello.RouteHandle,
		gatewayID: hello.GatewayID, conn: connection,
		streams: make(map[string]*browserStream),
	}
	old := s.register(route)
	defer s.unregister(route)
	if old != nil {
		old.close(websocket.ClosePolicyViolation, "route replaced")
	}
	ready := wire.MachineReady{
		Version: wire.Version, Type: wire.TypeMachineReady,
		ConnectionID: connectionID, RouteHandle: route.handle,
		RouteGeneration: route.generation,
	}
	if err := route.writeJSON(ready, wire.MaxHandshakeBytes); err != nil {
		return
	}

	connection.SetReadLimit(wire.MaxSealedFrameBytes)
	_ = connection.SetReadDeadline(s.now().Add(pongTimeout))
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(s.now().Add(pongTimeout))
	})
	pingDone := make(chan struct{})
	defer close(pingDone)
	go route.ping(pingDone)
	route.readGateway()
}

func (s *Server) handleBrowser(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.browserOrigin {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	handle := r.PathValue("route")
	if !validRouteHandle(handle) {
		http.NotFound(w, r)
		return
	}
	connection, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	connection.SetReadLimit(wire.MaxSealedFrameBytes)

	streamID, err := s.randomToken(wire.StreamIDBytes)
	if err != nil {
		closeInternal(connection)
		return
	}
	stream := &browserStream{
		id: streamID, conn: connection,
		toBrowser:  make(chan []byte, browserQueueDepth),
		confirmedC: make(chan struct{}), done: make(chan struct{}),
	}
	route, err := s.addStream(handle, stream)
	if err != nil {
		closePolicy(connection, "route unavailable")
		return
	}
	defer route.removeStream(streamID, "browser closed")

	open := wire.StreamOpen{
		Version: wire.Version, Type: wire.TypeStreamOpen,
		ConnectionID: route.connectionID, RouteGeneration: route.generation,
		RouteHandle: handle, StreamID: streamID,
	}
	if err := route.writeJSON(open, wire.MaxHandshakeBytes); err != nil {
		return
	}
	if err := writeJSON(connection, open, wire.MaxHandshakeBytes); err != nil {
		return
	}
	writerDone := make(chan struct{})
	go stream.writeBrowser(writerDone)
	defer func() { <-writerDone }()
	defer stream.close()

	startedAt := s.now()
	deadline := startedAt.Add(sessionLifetime)
	confirmDeadline := startedAt.Add(s.confirmLimit)
	for {
		next := s.now().Add(browserIdle)
		if !stream.sessionConfirmed() && confirmDeadline.Before(next) {
			next = confirmDeadline
		}
		if next.After(deadline) {
			next = deadline
		}
		if !s.now().Before(deadline) ||
			(!stream.sessionConfirmed() && !s.now().Before(confirmDeadline)) {
			closePolicy(connection, "session expired")
			return
		}
		_ = connection.SetReadDeadline(next)
		data, err := readText(connection)
		if err != nil {
			return
		}
		messageType, err := wire.MessageType(data, wire.MaxSealedFrameBytes)
		if err != nil {
			closePolicy(connection, "invalid sealed frame")
			return
		}
		switch messageType {
		case wire.TypeSessionHello:
			hello, _, err := wire.DecodeSessionHello(data)
			if err != nil || hello.StreamID != streamID || hello.RouteHandle != handle ||
				hello.ConnectionID != route.connectionID ||
				hello.RouteGeneration != route.generation ||
				!stream.markHello() {
				closePolicy(connection, "invalid session hello")
				return
			}
			if err := route.writeBytes(data); err != nil {
				return
			}
		case wire.TypeSealed:
			sealed, err := wire.DecodeSealed(data)
			if err != nil || sealed.StreamID != streamID || !stream.sessionAccepted() ||
				!stream.nextBrowserSequence(sealed.Sequence) {
				closePolicy(connection, "invalid sealed frame")
				return
			}
			if err := route.writeBytes(data); err != nil {
				return
			}
			if sealed.Sequence == 1 &&
				!s.waitForConfirmation(stream, confirmDeadline, deadline) {
				closePolicy(connection, "session confirmation timed out")
				return
			}
		case wire.TypeStreamClose:
			closed, err := wire.DecodeStreamClose(data)
			if err != nil || closed.StreamID != streamID {
				closePolicy(connection, "invalid stream close")
			}
			return
		default:
			closePolicy(connection, "browser message type is not allowed")
			return
		}
	}
}

func (s *Server) waitForConfirmation(
	stream *browserStream,
	confirmDeadline, sessionDeadline time.Time,
) bool {
	if stream.confirmedBefore(confirmDeadline) {
		return true
	}
	remaining := confirmDeadline.Sub(s.now())
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-stream.confirmedC:
		if !stream.confirmedBefore(confirmDeadline) {
			return false
		}
		next := s.now().Add(browserIdle)
		if sessionDeadline.Before(next) {
			next = sessionDeadline
		}
		return stream.conn.SetReadDeadline(next) == nil
	case <-stream.done:
		return false
	case <-timer.C:
		return false
	}
}

func (r *gatewayRoute) readGateway() {
	for {
		data, err := readText(r.conn)
		if err != nil {
			return
		}
		messageType, err := wire.MessageType(data, wire.MaxSealedFrameBytes)
		if err != nil {
			closePolicy(r.conn, "invalid gateway frame")
			return
		}
		switch messageType {
		case wire.TypeSessionAccept:
			accepted, _, _, _, err := wire.DecodeSessionAccept(data)
			stream := r.stream(accepted.StreamID)
			if err != nil || stream == nil || accepted.RouteHandle != r.handle ||
				accepted.GatewayID != r.gatewayID ||
				accepted.ConnectionID != r.connectionID ||
				accepted.RouteGeneration != r.generation ||
				!stream.markAccepted() || !stream.enqueue(data) {
				closePolicy(r.conn, "invalid session accept")
				return
			}
		case wire.TypeSealed:
			sealed, err := wire.DecodeSealed(data)
			if err != nil {
				closePolicy(r.conn, "invalid gateway frame")
				return
			}
			stream := r.stream(sealed.StreamID)
			if stream == nil || !stream.sessionAccepted() ||
				!stream.nextGatewaySequence(sealed.Sequence) {
				r.removeStream(sealed.StreamID, "stream unavailable")
				continue
			}
			if sealed.Sequence == 1 && !stream.markConfirmed(r.server.now()) {
				r.removeStream(sealed.StreamID, "invalid-confirmation")
				continue
			}
			if !stream.enqueue(data) {
				r.removeStream(sealed.StreamID, "stream unavailable")
			}
		case wire.TypeStreamClose:
			closed, err := wire.DecodeStreamClose(data)
			if err != nil {
				closePolicy(r.conn, "invalid gateway frame")
				return
			}
			r.removeStream(closed.StreamID, closed.Code)
		default:
			closePolicy(r.conn, "gateway message type is not allowed")
			return
		}
	}
}

func (s *Server) register(route *gatewayRoute) *gatewayRoute {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generations[route.handle]++
	route.generation = s.generations[route.handle]
	old := s.routes[route.handle]
	s.routes[route.handle] = route
	return old
}

func (s *Server) unregister(route *gatewayRoute) {
	s.mu.Lock()
	if s.routes[route.handle] == route {
		delete(s.routes, route.handle)
	}
	s.mu.Unlock()
	route.close(websocket.CloseGoingAway, "uplink closed")
}

func (s *Server) addStream(handle string, stream *browserStream) (*gatewayRoute, error) {
	s.mu.Lock()
	route := s.routes[handle]
	if route == nil {
		s.mu.Unlock()
		return nil, errors.New("route is offline")
	}
	route.mu.Lock()
	s.mu.Unlock()
	defer route.mu.Unlock()
	if route.closed || len(route.streams) >= MaxBrowserStreams {
		return nil, errors.New("route has no stream capacity")
	}
	if _, exists := route.streams[stream.id]; exists {
		return nil, errors.New("stream id collision")
	}
	route.streams[stream.id] = stream
	return route, nil
}

func (r *gatewayRoute) stream(id string) *browserStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.streams[id]
}

func (r *gatewayRoute) removeStream(id, code string) {
	r.mu.Lock()
	stream := r.streams[id]
	delete(r.streams, id)
	r.mu.Unlock()
	if stream == nil {
		return
	}
	stream.close()
	message := wire.StreamClose{
		Version: wire.Version, Type: wire.TypeStreamClose,
		StreamID: id, Code: code,
	}
	_ = r.writeJSON(message, wire.MaxHandshakeBytes)
}

func (r *gatewayRoute) close(code int, reason string) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	streams := make([]*browserStream, 0, len(r.streams))
	for id, stream := range r.streams {
		delete(r.streams, id)
		streams = append(streams, stream)
	}
	r.mu.Unlock()
	for _, stream := range streams {
		stream.close()
	}
	r.writeMu.Lock()
	_ = r.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(writeTimeout))
	_ = r.conn.Close()
	r.writeMu.Unlock()
}

func (r *gatewayRoute) ping(done <-chan struct{}) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			r.writeMu.Lock()
			err := r.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeTimeout))
			r.writeMu.Unlock()
			if err != nil {
				r.close(websocket.CloseGoingAway, "uplink ping failed")
				return
			}
		}
	}
}

func (r *gatewayRoute) writeJSON(value any, limit int) error {
	data, err := wire.Encode(value, limit)
	if err != nil {
		return err
	}
	return r.writeBytes(data)
}

func (r *gatewayRoute) writeBytes(data []byte) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_ = r.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return r.conn.WriteMessage(websocket.TextMessage, data)
}

func (s *browserStream) writeBrowser(done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-s.done:
			return
		case data := <-s.toBrowser:
			_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := s.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				s.close()
				return
			}
		}
	}
}

func (s *browserStream) enqueue(data []byte) bool {
	copyOfData := append([]byte(nil), data...)
	select {
	case <-s.done:
		return false
	case s.toBrowser <- copyOfData:
		return true
	default:
		return false
	}
}

func (s *browserStream) nextBrowserSequence(sequence uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence != s.browserSeq+1 {
		return false
	}
	s.browserSeq = sequence
	return true
}

func (s *browserStream) markHello() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.helloSeen || s.acceptSeen {
		return false
	}
	s.helloSeen = true
	return true
}

func (s *browserStream) markAccepted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.helloSeen || s.acceptSeen {
		return false
	}
	s.acceptSeen = true
	return true
}

func (s *browserStream) sessionAccepted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acceptSeen
}

func (s *browserStream) markConfirmed(at time.Time) bool {
	s.mu.Lock()
	if !s.acceptSeen || s.browserSeq == 0 || s.gatewaySeq != 1 || s.confirmed {
		s.mu.Unlock()
		return false
	}
	s.confirmed = true
	s.confirmedAt = at
	close(s.confirmedC)
	s.mu.Unlock()
	return true
}

func (s *browserStream) confirmedBefore(deadline time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confirmed && s.confirmedAt.Before(deadline)
}

func (s *browserStream) sessionConfirmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confirmed
}

func (s *browserStream) nextGatewaySequence(sequence uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sequence != s.gatewaySeq+1 {
		return false
	}
	s.gatewaySeq = sequence
	return true
}

func (s *browserStream) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}

func (s *Server) randomToken(length int) (string, error) {
	raw := make([]byte, length)
	if _, err := io.ReadFull(s.random, raw); err != nil {
		return "", fmt.Errorf("create relay token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func readText(connection *websocket.Conn) ([]byte, error) {
	messageType, data, err := connection.ReadMessage()
	if err != nil {
		return nil, err
	}
	if messageType != websocket.TextMessage {
		return nil, errors.New("relay accepts text envelopes only")
	}
	return data, nil
}

func writeJSON(connection *websocket.Conn, value any, limit int) error {
	data, err := wire.Encode(value, limit)
	if err != nil {
		return err
	}
	_ = connection.SetWriteDeadline(time.Now().Add(writeTimeout))
	return connection.WriteMessage(websocket.TextMessage, data)
}

func validRouteHandle(value string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == gatewayidentity.RouteHandleBytes &&
		base64.RawURLEncoding.EncodeToString(raw) == value &&
		!strings.Contains(value, "=")
}

func closePolicy(connection *websocket.Conn, reason string) {
	_ = connection.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason),
		time.Now().Add(writeTimeout))
}

func closeInternal(connection *websocket.Conn) {
	_ = connection.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "internal error"),
		time.Now().Add(writeTimeout))
}

type StaticInvites struct {
	keys map[string][]byte
}

type StaticInvite struct {
	GatewayID string `json:"gateway_id"`
	PublicKey string `json:"public_key"`
}

func NewStaticInvites(entries []StaticInvite) (*StaticInvites, error) {
	keys := make(map[string][]byte, len(entries))
	usedKeys := make(map[string]struct{}, len(entries))
	usedRoutes := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		normalized, err := gatewayidentity.NormalizeGatewayID(entry.GatewayID)
		if err != nil || normalized != entry.GatewayID {
			return nil, errors.New("invite gateway id is invalid")
		}
		key, err := base64.RawURLEncoding.DecodeString(entry.PublicKey)
		if err != nil || base64.RawURLEncoding.EncodeToString(key) != entry.PublicKey ||
			gatewayidentity.ValidatePublicKey(key) != nil {
			return nil, errors.New("invite public key is invalid")
		}
		if _, exists := keys[normalized]; exists {
			return nil, errors.New("invite gateway id is duplicated")
		}
		canonicalKey := base64.RawURLEncoding.EncodeToString(key)
		if _, exists := usedKeys[canonicalKey]; exists {
			return nil, errors.New("invite public key is duplicated")
		}
		handle, err := gatewayidentity.RouteHandle(key)
		if err != nil {
			return nil, errors.New("invite route handle is invalid")
		}
		if _, exists := usedRoutes[handle]; exists {
			return nil, errors.New("invite route handle is duplicated")
		}
		keys[normalized] = append([]byte(nil), key...)
		usedKeys[canonicalKey] = struct{}{}
		usedRoutes[handle] = struct{}{}
	}
	if len(keys) == 0 {
		return nil, errors.New("at least one relay invite is required")
	}
	return &StaticInvites{keys: keys}, nil
}

func (s *StaticInvites) CanonicalPublicKey(_ context.Context, gatewayID string) ([]byte, error) {
	key := s.keys[gatewayID]
	if key == nil {
		return nil, errors.New("gateway is not invited")
	}
	return append([]byte(nil), key...), nil
}

func ParseStaticInvites(data []byte) (*StaticInvites, error) {
	const maxInviteFileBytes = 1024 * 1024
	var entries []StaticInvite
	if err := wire.DecodeStrict(data, maxInviteFileBytes, &entries); err != nil {
		return nil, errors.New("relay invite file is invalid")
	}
	return NewStaticInvites(entries)
}
