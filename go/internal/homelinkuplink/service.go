package homelinkuplink

import (
	"context"
	"errors"
	"sync"

	"github.com/srcfl/ftw/go/internal/gatewayidentity"
	"github.com/srcfl/ftw/go/internal/homelink/wire"
	"github.com/srcfl/ftw/go/internal/homelinksession"
)

const maxApplicationHandshakeBytes = 16 * 1024

type transport interface {
	ReadFrame() (Frame, error)
	SendSessionAccept(wire.SessionAccept) error
	SendSealed(wire.Sealed) error
	CloseStream(string, string) error
	Close() error
}

type Service struct {
	sessions *homelinksession.Manager
}

type streamState struct {
	open      wire.StreamOpen
	confirmed bool
	session   *homelinksession.Session
}

type sessionConfirm struct {
	Version int    `json:"version"`
	Type    string `json:"type"`
}

type sessionReady struct {
	Version int    `json:"version"`
	Type    string `json:"type"`
}

func NewService(identity gatewayidentity.Identity) (*Service, error) {
	sessions, err := homelinksession.NewManager(identity)
	if err != nil {
		return nil, err
	}
	return &Service{sessions: sessions}, nil
}

func (s *Service) Serve(ctx context.Context, connection *Connection) error {
	if connection == nil {
		return errors.New("Home Link uplink connection is missing")
	}
	return s.serve(ctx, connection)
}

func (s *Service) serve(ctx context.Context, connection transport) error {
	streams := make(map[string]*streamState)
	var closeOnce sync.Once
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			closeOnce.Do(func() { _ = connection.Close() })
		case <-stop:
		}
	}()
	defer close(stop)
	defer closeOnce.Do(func() { _ = connection.Close() })

	for {
		frame, err := connection.ReadFrame()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		switch frame.Type {
		case wire.TypeStreamOpen:
			if frame.Open == nil || len(streams) >= wire.MaxBrowserStreams {
				return errors.New("Home Link stream limit is invalid")
			}
			if _, exists := streams[frame.Open.StreamID]; exists {
				return errors.New("Home Link stream was opened twice")
			}
			streams[frame.Open.StreamID] = &streamState{open: *frame.Open}
		case wire.TypeSessionHello:
			if frame.SessionHello == nil {
				return errors.New("Home Link session hello is missing")
			}
			state := streams[frame.SessionHello.StreamID]
			if state == nil || state.session != nil ||
				frame.SessionHello.ConnectionID != state.open.ConnectionID ||
				frame.SessionHello.RouteGeneration != state.open.RouteGeneration ||
				frame.SessionHello.RouteHandle != state.open.RouteHandle {
				_ = connection.CloseStream(frame.SessionHello.StreamID, "invalid-session")
				continue
			}
			accept, session, err := s.sessions.Accept(*frame.SessionHello)
			if err != nil {
				_ = connection.CloseStream(frame.SessionHello.StreamID, "invalid-session")
				delete(streams, frame.SessionHello.StreamID)
				continue
			}
			if err := connection.SendSessionAccept(accept); err != nil {
				return err
			}
			state.session = session
		case wire.TypeSealed:
			if frame.Sealed == nil {
				return errors.New("Home Link sealed frame is missing")
			}
			state := streams[frame.Sealed.StreamID]
			if state == nil || state.session == nil || state.confirmed {
				_ = connection.CloseStream(frame.Sealed.StreamID, "invalid-session")
				delete(streams, frame.Sealed.StreamID)
				continue
			}
			plaintext, err := state.session.Decrypt(*frame.Sealed)
			if err != nil || !validSessionConfirm(plaintext) {
				_ = connection.CloseStream(frame.Sealed.StreamID, "invalid-confirmation")
				delete(streams, frame.Sealed.StreamID)
				continue
			}
			readyBytes, err := wire.Encode(sessionReady{Version: 1, Type: "session.ready"}, maxApplicationHandshakeBytes)
			if err != nil {
				return err
			}
			ready, err := state.session.Encrypt(readyBytes)
			if err != nil {
				return err
			}
			if err := connection.SendSealed(ready); err != nil {
				return err
			}
			state.confirmed = true
		case wire.TypeStreamClose:
			if frame.Close != nil {
				delete(streams, frame.Close.StreamID)
			}
		default:
			return errors.New("Home Link uplink delivered an unsupported frame")
		}
	}
}

func validSessionConfirm(data []byte) bool {
	var message sessionConfirm
	if err := wire.DecodeStrict(data, maxApplicationHandshakeBytes, &message); err != nil {
		return false
	}
	return message.Version == 1 && message.Type == "session.confirm"
}
