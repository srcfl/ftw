package main

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// SessionConfig captures everything decided at pair-start.
type SessionConfig struct {
	TTL    time.Duration
	Intent string // free-form owner-supplied description threaded into the friend-side prompt
	As     string // optional friend identity ("@erikarenhill")
}

// Session is the lifecycle owner. It enforces the TTL and surfaces a
// Done() channel everything else listens on for shutdown. ExitReason
// is set exactly once and is visible after Done() closes.
type Session struct {
	ID        string
	StartedAt time.Time
	cfg       SessionConfig

	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	reason string
	ended  bool
}

func NewSession(parent context.Context, cfg SessionConfig) *Session {
	if cfg.TTL <= 0 {
		cfg.TTL = 4 * time.Hour
	}
	ctx, cancel := context.WithCancel(parent)
	s := &Session{
		ID:        uuid.NewString(),
		StartedAt: time.Now(),
		cfg:       cfg,
		cancel:    cancel,
		done:      make(chan struct{}),
	}
	go s.timerLoop(ctx)
	return s
}

func (s *Session) timerLoop(ctx context.Context) {
	timer := time.NewTimer(s.cfg.TTL)
	defer timer.Stop()
	select {
	case <-timer.C:
		s.End("ttl_expired")
	case <-ctx.Done():
		s.End("context_cancelled")
	}
}

func (s *Session) End(reason string) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.reason = reason
	s.mu.Unlock()
	s.cancel()
	close(s.done)
}

func (s *Session) Done() <-chan struct{} { return s.done }

func (s *Session) Remaining() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return 0
	}
	r := s.cfg.TTL - time.Since(s.StartedAt)
	if r < 0 {
		return 0
	}
	return r
}

func (s *Session) ExitReason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

func (s *Session) Intent() string { return s.cfg.Intent }
func (s *Session) As() string     { return s.cfg.As }
