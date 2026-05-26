package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// LogEntry is one captured log line. Times are wall-clock; level mirrors
// slog levels.
type LogEntry struct {
	TS     time.Time `json:"ts"`
	Level  string    `json:"level"`
	Msg    string    `json:"msg"`
	Driver string    `json:"driver,omitempty"`
	Attrs  string    `json:"attrs,omitempty"` // pre-formatted "k=v k=v" tail
}

const (
	// Per-driver ring depth. 500 lines is enough to diagnose a typical
	// stuck-poll incident (~5 minutes at one log/poll-interval) without
	// growing memory unboundedly when 8+ drivers run.
	ringPerDriver = 500
	// Global ring depth — captures everything that didn't carry a
	// "driver" attribute (control loop, MPC, HA bridge, …). Larger than
	// the per-driver ring because every driver's emits also pass through.
	ringGlobal = 2000
)

// LogRing is an in-memory ring of recent log entries, split into a
// global stream and one stream per driver name. Thread-safe.
type LogRing struct {
	mu      sync.RWMutex
	global  []LogEntry
	gNext   int
	gFilled bool
	drivers map[string]*driverRing
}

type driverRing struct {
	buf    []LogEntry
	next   int
	filled bool
}

// NewLogRing constructs an empty ring.
func NewLogRing() *LogRing {
	return &LogRing{
		global:  make([]LogEntry, ringGlobal),
		drivers: make(map[string]*driverRing),
	}
}

// Append records one entry. If e.Driver is non-empty it lands in BOTH
// the per-driver ring and the global ring so a support dump catches
// the whole story regardless of where the operator drilled in.
func (r *LogRing) Append(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.global[r.gNext] = e
	r.gNext++
	if r.gNext == len(r.global) {
		r.gNext = 0
		r.gFilled = true
	}
	if e.Driver == "" {
		return
	}
	dr, ok := r.drivers[e.Driver]
	if !ok {
		dr = &driverRing{buf: make([]LogEntry, ringPerDriver)}
		r.drivers[e.Driver] = dr
	}
	dr.buf[dr.next] = e
	dr.next++
	if dr.next == len(dr.buf) {
		dr.next = 0
		dr.filled = true
	}
}

// RecentByDriver returns up to limit most-recent entries for driver,
// oldest first. Empty driver returns the global ring.
func (r *LogRing) RecentByDriver(driver string, limit int) []LogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if driver == "" {
		return drain(r.global, r.gNext, r.gFilled, limit)
	}
	dr, ok := r.drivers[driver]
	if !ok {
		return nil
	}
	return drain(dr.buf, dr.next, dr.filled, limit)
}

// RecentGlobal returns up to limit most-recent entries from the global
// ring (oldest first).
func (r *LogRing) RecentGlobal(limit int) []LogEntry {
	return r.RecentByDriver("", limit)
}

// Drivers lists the names that have any captured log lines.
func (r *LogRing) Drivers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.drivers))
	for k := range r.drivers {
		out = append(out, k)
	}
	return out
}

func drain(buf []LogEntry, next int, filled bool, limit int) []LogEntry {
	n := next
	if filled {
		n = len(buf)
	}
	if n == 0 {
		return nil
	}
	out := make([]LogEntry, 0, n)
	if filled {
		// Walk from `next` (oldest) wrapping around.
		for i := 0; i < len(buf); i++ {
			idx := (next + i) % len(buf)
			out = append(out, buf[idx])
		}
	} else {
		out = append(out, buf[:next]...)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// LogHandler is a slog.Handler that mirrors records into a LogRing
// while delegating actual stdout/file emission to an inner handler.
// Records carrying a "driver" attribute (set per-driver via
// slog.With("driver", name) in drivers/host.go) are routed into the
// per-driver ring; everything else lands in the global ring.
type LogHandler struct {
	inner slog.Handler
	ring  *LogRing
	attrs []slog.Attr
	group string
}

// NewLogHandler wraps inner with capture into ring. Pass the
// stdout text handler as inner so existing log behaviour is unchanged.
func NewLogHandler(inner slog.Handler, ring *LogRing) *LogHandler {
	return &LogHandler{inner: inner, ring: ring}
}

func (h *LogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	driver := ""
	var tail bytes.Buffer
	for _, a := range h.attrs {
		if a.Key == "driver" {
			driver = a.Value.String()
			continue
		}
		writeAttr(&tail, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "driver" {
			driver = a.Value.String()
			return true
		}
		writeAttr(&tail, a)
		return true
	})
	h.ring.Append(LogEntry{
		TS:     r.Time,
		Level:  r.Level.String(),
		Msg:    r.Message,
		Driver: driver,
		Attrs:  strings.TrimSpace(tail.String()),
	})
	return h.inner.Handle(ctx, r)
}

func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &LogHandler{
		inner: h.inner.WithAttrs(attrs),
		ring:  h.ring,
		attrs: merged,
		group: h.group,
	}
}

func (h *LogHandler) WithGroup(name string) slog.Handler {
	return &LogHandler{
		inner: h.inner.WithGroup(name),
		ring:  h.ring,
		attrs: h.attrs,
		group: name,
	}
}

func writeAttr(b *bytes.Buffer, a slog.Attr) {
	if a.Key == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	fmt.Fprintf(b, "%s=%v", a.Key, a.Value.Any())
}
