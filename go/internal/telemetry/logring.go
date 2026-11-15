package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
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

const (
	// digestPatternsPerHour caps the distinct warning patterns kept per
	// hour. A driver that puts a changing word in every warning would
	// otherwise add a row per line; past the cap they are only counted.
	digestPatternsPerHour = 200
	// digestOverflowPattern is the row that counts them.
	digestOverflowPattern = "(more distinct messages this hour)"
	// digestTextMax bounds the stored message and attribute text.
	digestTextMax = 300
	// digestHeldMax is how long rows wait in memory for TakeDigest.
	digestHeldMax = 24 * time.Hour
)

// LogRing is an in-memory ring of recent log entries, split into a
// global stream and one stream per driver name. Thread-safe.
//
// Warnings and errors also go to a digest, one row per pattern and hour,
// which Core writes to the store (TakeDigest). Lines logged every control
// tick can push them out of the rings within an hour; the digest keeps a
// week of them.
type LogRing struct {
	mu      sync.RWMutex
	global  []LogEntry
	gNext   int
	gFilled bool
	drivers map[string]*driverRing

	digest      map[digestKey]*digestRow
	digestHours map[time.Time]int // distinct patterns per hour
}

// DigestEntry is the count of one warning or error pattern in one hour
// since the previous TakeDigest.
type DigestEntry struct {
	Hour    time.Time
	Level   string
	Driver  string
	Pattern string
	Msg     string
	Attrs   string
	Count   int
	First   time.Time
	Last    time.Time
}

type digestKey struct {
	hour                   time.Time
	level, driver, pattern string
}

type digestRow struct {
	DigestEntry
	sent int // Count already handed out
}

type driverRing struct {
	buf    []LogEntry
	next   int
	filled bool
}

// NewLogRing constructs an empty ring.
func NewLogRing() *LogRing {
	return &LogRing{
		global:      make([]LogEntry, ringGlobal),
		drivers:     make(map[string]*driverRing),
		digest:      make(map[digestKey]*digestRow),
		digestHours: make(map[time.Time]int),
	}
}

// addDigest counts a warning or error. Caller holds r.mu.
func (r *LogRing) addDigest(e LogEntry) {
	level := strings.ToUpper(e.Level)
	if level == "WARNING" {
		level = "WARN"
	}
	if level != "WARN" && level != "ERROR" {
		return
	}
	ts := e.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	ts = ts.UTC()
	hour := ts.Truncate(time.Hour)
	k := digestKey{hour: hour, level: level, driver: e.Driver, pattern: clip(logPattern(e.Msg))}
	row, ok := r.digest[k]
	if !ok && r.digestHours[hour] >= digestPatternsPerHour {
		k.driver, k.pattern = "", digestOverflowPattern
		row, ok = r.digest[k]
	}
	if _, seen := r.digestHours[hour]; !seen {
		// Nothing has taken the digest for a day: keep memory bounded.
		for old := range r.digest {
			if old.hour.Before(hour.Add(-digestHeldMax)) {
				delete(r.digest, old)
				delete(r.digestHours, old.hour)
			}
		}
		r.digestHours[hour] = 0
	}
	if !ok {
		row = &digestRow{DigestEntry: DigestEntry{Hour: hour, Level: level, Driver: k.driver, Pattern: k.pattern, First: ts}}
		r.digest[k] = row
		if k.pattern != digestOverflowPattern {
			r.digestHours[hour]++
		}
	}
	row.Count++
	if ts.Before(row.First) {
		row.First = ts
	}
	if !ts.Before(row.Last) {
		row.Last = ts
		if k.pattern == digestOverflowPattern {
			row.Msg = "warnings and errors past the hourly limit of distinct messages"
		} else {
			row.Msg, row.Attrs = clip(e.Msg), clip(e.Attrs)
		}
	}
}

// TakeDigest returns the counts added since the last call, oldest hour
// first, and forgets every hour before now's once its counts are out. The
// store adds what it is given, so a row appears again only with new counts.
func (r *LogRing) TakeDigest(now time.Time) []DigestEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := now.UTC().Truncate(time.Hour)
	var out []DigestEntry
	for k, row := range r.digest {
		if delta := row.Count - row.sent; delta > 0 {
			e := row.DigestEntry
			e.Count = delta
			out = append(out, e)
			row.sent = row.Count
		}
		if k.hour.Before(current) {
			delete(r.digest, k)
		}
	}
	for h := range r.digestHours {
		if h.Before(current) {
			delete(r.digestHours, h)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Hour.Equal(out[j].Hour) {
			return out[i].Hour.Before(out[j].Hour)
		}
		return out[i].First.Before(out[j].First)
	})
	return out
}

// logPattern folds every number in a message to "#", so the same warning
// with different figures counts as one.
func logPattern(msg string) string {
	var b strings.Builder
	b.Grow(len(msg))
	for i := 0; i < len(msg); {
		if !isDigit(msg[i]) {
			b.WriteByte(msg[i])
			i++
			continue
		}
		// One number: digits, joined by single . : or , between digits.
		j := i + 1
		for j < len(msg) {
			if isDigit(msg[j]) {
				j++
				continue
			}
			if (msg[j] == '.' || msg[j] == ':' || msg[j] == ',') && j+1 < len(msg) && isDigit(msg[j+1]) {
				j += 2
				continue
			}
			break
		}
		b.WriteByte('#')
		i = j
	}
	return b.String()
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func clip(s string) string {
	if len(s) <= digestTextMax {
		return s
	}
	return s[:digestTextMax]
}

// Append records one entry. If e.Driver is non-empty it lands in BOTH
// the per-driver ring and the global ring so a support dump catches
// the whole story regardless of where the operator drilled in.
func (r *LogRing) Append(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addDigest(e)
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
