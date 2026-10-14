package drivers

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Restart backoff after a driver has been offline, stale, or unable to
// start. The last step repeats.
var restartBackoffSteps = [...]time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
}

// restartWatchInterval is how often the running watchdog looks. The
// shortest backoff is 30s, so this only needs to be a fraction of that.
const restartWatchInterval = 5 * time.Second

// DriverMaintenance is one automatic start or restart MaintainDrivers performed.
type DriverMaintenance struct {
	Name   string
	Reason string // "offline", "stale", or "start_failed"
	Err    error
}

// DriverRestartStatus is the watchdog counters for one driver.
type DriverRestartStatus struct {
	Restarts    uint64
	LastAttempt time.Time
	LastError   string
	LastReason  string
	NextAttempt time.Time
}

// driverRestartState is the backoff memory for one driver name. seenData
// survives a restart wiping the health record, so a push driver that has
// already delivered telemetry is not later treated as "still waiting".
type driverRestartState struct {
	step             int
	unhealthySince   time.Time
	nextAttempt      time.Time
	restarts         uint64
	lastAttempt      time.Time
	lastError        string
	lastReason       string
	seenData         bool
	awaitingRecovery bool
	notedCloudSkip   bool
}

type restartLive int

const (
	liveHealthy restartLive = iota
	liveWaiting
	liveDown
	liveStartFailed
	liveCloud
)

// RunRestartWatchdog restarts drivers that stay offline or stale, and
// retries drivers that failed to start. It stops when ctx is cancelled.
// snapshot must return the drivers that should be running (the same list
// startup and reload use) and the site watchdog timeout.
func (r *Registry) RunRestartWatchdog(ctx context.Context, snapshot func() ([]config.Driver, time.Duration)) {
	if r == nil || snapshot == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tick := func() {
		drivers, timeout := snapshot()
		if timeout <= 0 {
			timeout = 60 * time.Second
		}
		r.MaintainDrivers(ctx, time.Now(), drivers, timeout)
	}
	tick()
	ticker := time.NewTicker(restartWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}

// MaintainDrivers starts or restarts drivers that have been offline, stale,
// or unable to start. now is the decision clock. Disabled and observe-only
// drivers are left alone. A running cloud or OAuth driver is left alone too,
// so a quiet stream cannot become a re-login loop; a failed start of one is
// still retried. An MQTT, WebSocket, or TCP driver that is polling and has
// never delivered data is waiting, not dead.
func (r *Registry) MaintainDrivers(ctx context.Context, now time.Time, drivers []config.Driver, staleAfter time.Duration) []DriverMaintenance {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	desired := orderedDesiredDrivers(drivers)
	keep := make(map[string]struct{}, len(desired))
	var out []DriverMaintenance
	for _, cfg := range desired {
		keep[cfg.Name] = struct{}{}
		if err := ctx.Err(); err != nil {
			break
		}
		if ev, ok := r.maintainDriver(ctx, now, cfg, staleAfter); ok {
			out = append(out, ev)
		}
	}
	r.forgetRestartExcept(keep)
	return out
}

// RestartStatus reports watchdog counters for one driver.
func (r *Registry) RestartStatus(name string) (DriverRestartStatus, bool) {
	if r == nil || name == "" {
		return DriverRestartStatus{}, false
	}
	r.restartMu.Lock()
	defer r.restartMu.Unlock()
	st := r.restarts[name]
	if st == nil {
		return DriverRestartStatus{}, false
	}
	out := DriverRestartStatus{
		Restarts:    st.restarts,
		LastAttempt: st.lastAttempt,
		LastError:   st.lastError,
		LastReason:  st.lastReason,
		NextAttempt: st.nextAttempt,
	}
	if out.NextAttempt.IsZero() && !st.unhealthySince.IsZero() {
		out.NextAttempt = st.unhealthySince.Add(restartDelay(st.step))
	}
	if out.Restarts == 0 && out.LastReason == "" && out.NextAttempt.IsZero() && st.unhealthySince.IsZero() {
		return DriverRestartStatus{}, false
	}
	return out, true
}

func (r *Registry) maintainDriver(ctx context.Context, now time.Time, cfg config.Driver, staleAfter time.Duration) (DriverMaintenance, bool) {
	if cfg.Disabled || cfg.ObserveOnly || cfg.Name == "" {
		r.deleteRestart(cfg.Name)
		return DriverMaintenance{}, false
	}
	// Same gate Restart and Reload use, so a config reload cannot start
	// this driver between the liveness check and the replacement.
	release := r.acquireLifecycle(cfg.Name)
	defer release()

	entry, _ := ParseCatalogFile(cfg.Lua)
	live, h := r.classifyDriver(cfg, entry, now, staleAfter)
	switch live {
	case liveHealthy, liveWaiting:
		r.noteHealthy(cfg.Name, h, live == liveWaiting)
		return DriverMaintenance{}, false
	case liveCloud:
		r.noteCloudRunning(cfg.Name, h, now, staleAfter)
		return DriverMaintenance{}, false
	case liveDown, liveStartFailed:
		// A cancelled watchdog must not stop a driver shutdown is about to
		// remove. The check is after the lifecycle gate so a reload that
		// held the gate cannot sneak a start in either.
		if err := ctx.Err(); err != nil {
			return DriverMaintenance{}, false
		}
		if !r.restartDue(cfg.Name, h, now, staleAfter, live) {
			return DriverMaintenance{}, false
		}
		reason := restartReason(live, h, now, staleAfter)
		attempt, next := r.scheduleRestart(cfg.Name, h, now, reason)
		err := r.restartLocked(ctx, cfg)
		if err != nil {
			r.noteRestartError(cfg.Name, err)
			slog.Warn("automatic driver restart failed",
				"name", cfg.Name, "reason", reason, "restarts", attempt, "next_backoff", next.String(), "err", err)
		} else {
			slog.Warn("automatic driver restart",
				"name", cfg.Name, "reason", reason, "restarts", attempt, "next_backoff", next.String())
			r.finishRestart(cfg.Name)
		}
		return DriverMaintenance{Name: cfg.Name, Reason: reason, Err: err}, true
	default:
		return DriverMaintenance{}, false
	}
}

// restartLocked replaces a running driver, or starts one that is not
// running. The caller holds the lifecycle gate. Shutdown still runs
// driver_default_mode, and the new generation confirms it again, which is
// the same hand-back Restart uses.
func (r *Registry) restartLocked(ctx context.Context, cfg config.Driver) error {
	r.mu.Lock()
	_, running := r.rec[cfg.Name]
	r.mu.Unlock()
	if running {
		r.removeLocked(cfg.Name, false)
	}
	if cfg.Disabled || cfg.ObserveOnly {
		return nil
	}
	return r.add(ctx, cfg, true)
}

func (r *Registry) finishRestart(name string) {
	var h *telemetry.DriverHealth
	if r.tel != nil {
		h = r.tel.DriverHealth(name)
	}
	r.noteHealthy(name, h, false)
}

func (r *Registry) classifyDriver(cfg config.Driver, entry CatalogEntry, now time.Time, staleAfter time.Duration) (restartLive, *telemetry.DriverHealth) {
	r.mu.Lock()
	_, running := r.rec[cfg.Name]
	r.mu.Unlock()
	if !running {
		return liveStartFailed, nil
	}
	var h *telemetry.DriverHealth
	if r.tel != nil {
		h = r.tel.DriverHealth(cfg.Name)
	}
	// A running cloud or OAuth driver is not torn down for staleness.
	// driver_init would log in again, and for a rotating refresh token
	// that is the loop registry.Add warns about.
	if cloudOrOAuth(cfg, entry) {
		return liveCloud, h
	}
	seen := r.driverSeenData(cfg.Name)
	if h != nil && h.LastSuccess != nil {
		seen = true
	}
	if pushTransport(cfg, entry) && pushWaiting(h, seen) {
		return liveWaiting, h
	}
	if h == nil || !driverDown(h, now, staleAfter) {
		return liveHealthy, h
	}
	return liveDown, h
}

func cloudOrOAuth(cfg config.Driver, entry CatalogEntry) bool {
	if strings.TrimSpace(cfg.CredentialOwner) != "" {
		return true
	}
	if strings.TrimSpace(entry.AuthPostPath) != "" || len(entry.AuthPostPaths) > 0 {
		return true
	}
	if len(entry.HTTPHosts) == 0 {
		return false
	}
	for _, p := range entry.Protocols {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "modbus", "mqtt", "serial", "tcp":
			// A local bus owns the session. HTTP beside it is not a cloud login.
			return false
		}
	}
	for _, p := range entry.Protocols {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "http", "websocket":
			return true
		}
	}
	return false
}

func pushTransport(cfg config.Driver, entry CatalogEntry) bool {
	if cfg.EffectiveMQTT() != nil || cfg.Capabilities.WebSocket != nil || cfg.Capabilities.TCP != nil {
		return true
	}
	for _, p := range entry.Protocols {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "mqtt", "websocket", "tcp":
			return true
		}
	}
	return false
}

// pushWaiting is an MQTT, WebSocket, or TCP driver whose poll loop is
// healthy and that has not delivered a reading yet. Silence here is the
// device not having spoken, not a dead session.
func pushWaiting(h *telemetry.DriverHealth, seenData bool) bool {
	if h == nil || seenData || h.LastSuccess != nil {
		return false
	}
	if h.ConsecutiveErrors > 0 || strings.TrimSpace(h.LastError) != "" {
		return false
	}
	return h.TickCount > 0
}

func driverDown(h *telemetry.DriverHealth, now time.Time, timeout time.Duration) bool {
	if h == nil {
		return false
	}
	if h.Status == telemetry.StatusOffline {
		return true
	}
	return healthIsStale(h, now, timeout)
}

func healthIsStale(h *telemetry.DriverHealth, now time.Time, timeout time.Duration) bool {
	if h == nil {
		return false
	}
	eff := effectiveWatchdog(h, timeout)
	since := h.StartedAt
	if h.LastSuccess != nil {
		since = *h.LastSuccess
	}
	if since.IsZero() {
		return true
	}
	return now.Sub(since) > eff
}

func effectiveWatchdog(h *telemetry.DriverHealth, timeout time.Duration) time.Duration {
	if h != nil && h.WatchdogTimeoutOverride > 0 {
		return h.WatchdogTimeoutOverride
	}
	if timeout <= 0 {
		return 60 * time.Second
	}
	return timeout
}

func restartReason(live restartLive, h *telemetry.DriverHealth, now time.Time, timeout time.Duration) string {
	if live == liveStartFailed {
		return "start_failed"
	}
	if h != nil && h.Status == telemetry.StatusOffline {
		return "offline"
	}
	if h != nil && healthIsStale(h, now, timeout) {
		return "stale"
	}
	return "offline"
}

func restartDelay(step int) time.Duration {
	if step < 0 {
		step = 0
	}
	if step >= len(restartBackoffSteps) {
		return restartBackoffSteps[len(restartBackoffSteps)-1]
	}
	return restartBackoffSteps[step]
}

// restartDue reports whether the current backoff has elapsed. The first
// time a start failure or a fresh-but-offline driver is seen, it records
// the observation and waits. A reading that is already stale long enough
// is due immediately.
func (r *Registry) restartDue(name string, h *telemetry.DriverHealth, now time.Time, timeout time.Duration, live restartLive) bool {
	var st driverRestartState
	r.updateRestart(name, true, func(s *driverRestartState) {
		if h != nil && h.LastSuccess != nil {
			s.seenData = true
		}
		readingStale := h != nil && healthIsStale(h, now, timeout)
		if live == liveStartFailed || (live == liveDown && !readingStale) {
			if s.unhealthySince.IsZero() {
				s.unhealthySince = now
			}
		}
		st = *s
	})
	if !st.nextAttempt.IsZero() && now.Before(st.nextAttempt) {
		return false
	}
	since := unhealthyAt(st, h, now, timeout, live)
	if since.IsZero() || since.After(now) {
		return false
	}
	return now.Sub(since) >= restartDelay(st.step)
}

func unhealthyAt(st driverRestartState, h *telemetry.DriverHealth, now time.Time, timeout time.Duration, live restartLive) time.Time {
	if live != liveStartFailed && h != nil && healthIsStale(h, now, timeout) {
		eff := effectiveWatchdog(h, timeout)
		since := h.StartedAt
		if h.LastSuccess != nil {
			since = *h.LastSuccess
		}
		if !since.IsZero() {
			return since.Add(eff)
		}
	}
	if !st.unhealthySince.IsZero() {
		return st.unhealthySince
	}
	return now
}

func (r *Registry) scheduleRestart(name string, h *telemetry.DriverHealth, now time.Time, reason string) (uint64, time.Duration) {
	var restarts uint64
	var next time.Duration
	r.updateRestart(name, true, func(s *driverRestartState) {
		if h != nil && h.LastSuccess != nil {
			s.seenData = true
		}
		s.step++
		if s.step >= len(restartBackoffSteps) {
			s.step = len(restartBackoffSteps) - 1
		}
		next = restartDelay(s.step)
		s.nextAttempt = now.Add(next)
		s.restarts++
		restarts = s.restarts
		s.lastAttempt = now
		s.lastReason = reason
		s.lastError = ""
		s.awaitingRecovery = true
		s.notedCloudSkip = false
		if reason == "start_failed" {
			s.unhealthySince = now
		} else {
			s.unhealthySince = time.Time{}
		}
	})
	return restarts, next
}

func (r *Registry) noteRestartError(name string, err error) {
	if err == nil {
		return
	}
	r.updateRestart(name, true, func(s *driverRestartState) {
		s.lastError = err.Error()
	})
}

func (r *Registry) noteHealthy(name string, h *telemetry.DriverHealth, waiting bool) {
	var recovered bool
	var restarts uint64
	ok := r.updateRestart(name, false, func(s *driverRestartState) {
		if h != nil && h.LastSuccess != nil {
			s.seenData = true
		}
		if waiting || s.lastAttempt.IsZero() || telemetrySince(h, s.lastAttempt) {
			recovered = s.awaitingRecovery
			restarts = s.restarts
			clearRestartBackoff(s)
		} else {
			s.unhealthySince = time.Time{}
		}
	})
	if ok && recovered {
		slog.Info("driver recovered after automatic restart", "name", name, "restarts", restarts)
	}
}

func (r *Registry) noteCloudRunning(name string, h *telemetry.DriverHealth, now time.Time, timeout time.Duration) {
	down := h != nil && driverDown(h, now, timeout)
	var skipLog bool
	r.updateRestart(name, down, func(s *driverRestartState) {
		if h != nil && h.LastSuccess != nil {
			s.seenData = true
		}
		if !down {
			if s.lastAttempt.IsZero() || telemetrySince(h, s.lastAttempt) {
				clearRestartBackoff(s)
			}
			s.notedCloudSkip = false
			return
		}
		if !s.notedCloudSkip {
			s.notedCloudSkip = true
			skipLog = true
		}
	})
	if skipLog {
		slog.Info("cloud driver left running", "name", name, "reason", "automatic restart would re-login")
	}
}

func telemetrySince(h *telemetry.DriverHealth, since time.Time) bool {
	if h == nil || h.LastSuccess == nil || h.Status == telemetry.StatusOffline {
		return false
	}
	if !since.IsZero() && h.LastSuccess.Before(since) {
		return false
	}
	return true
}

func clearRestartBackoff(s *driverRestartState) {
	s.step = 0
	s.unhealthySince = time.Time{}
	s.nextAttempt = time.Time{}
	s.lastError = ""
	s.awaitingRecovery = false
	s.notedCloudSkip = false
}

func (r *Registry) driverSeenData(name string) bool {
	r.restartMu.Lock()
	defer r.restartMu.Unlock()
	st := r.restarts[name]
	return st != nil && st.seenData
}

func (r *Registry) updateRestart(name string, create bool, fn func(*driverRestartState)) bool {
	r.restartMu.Lock()
	defer r.restartMu.Unlock()
	if r.restarts == nil {
		r.restarts = map[string]*driverRestartState{}
	}
	st := r.restarts[name]
	if st == nil {
		if !create {
			return false
		}
		st = &driverRestartState{}
		r.restarts[name] = st
	}
	fn(st)
	return true
}

func (r *Registry) deleteRestart(name string) {
	r.restartMu.Lock()
	defer r.restartMu.Unlock()
	delete(r.restarts, name)
}

func (r *Registry) forgetRestartExcept(keep map[string]struct{}) {
	r.restartMu.Lock()
	defer r.restartMu.Unlock()
	for name := range r.restarts {
		if _, ok := keep[name]; !ok {
			delete(r.restarts, name)
		}
	}
}

func orderedDesiredDrivers(in []config.Driver) []config.Driver {
	index := make(map[string]int, len(in))
	out := make([]config.Driver, 0, len(in))
	for _, cfg := range in {
		if cfg.Name == "" {
			continue
		}
		if i, ok := index[cfg.Name]; ok {
			out[i] = cfg
			continue
		}
		index[cfg.Name] = len(out)
		out = append(out, cfg)
	}
	return out
}
