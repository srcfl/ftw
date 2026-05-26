// Package notifications delivers operator-facing push notifications via
// a pluggable provider (ntfy today; future providers register via
// RegisterProvider).
//
// The rule engine is driven by telemetry.DriverHealth snapshots passed in
// via Service.Observe. It supports two built-in event types today:
//
//   - driver_offline   — fires once per outage after a per-rule threshold
//   - driver_recovered — fires when a driver that previously tripped
//     driver_offline reports successful telemetry again
//
// The user-visible threshold is independent of site.watchdog_timeout_s:
// the watchdog is a safety shortcut that drops stale drivers to autonomous
// mode immediately, while notifications are here to tell the human after
// a longer-than-noise outage.
package notifications

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/events"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Event types.
const (
	EventDriverOffline   = "driver_offline"
	EventDriverRecovered = "driver_recovered"
	EventUpdateAvailable = "update_available"
	EventFuseOverLimit   = "fuse_over_limit"

	// EventConcurrentDriversOffline fires when ≥ ThresholdN drivers
	// are simultaneously stale beyond ThresholdS — the typical
	// signature of a fuse blow / breaker trip / power outage that
	// took out multiple inverters at once. A single flaky driver
	// uses driver_offline; this catches the multi-driver
	// "something bigger just happened" pattern that motivated
	// the 2026-05-02 incident: house fuse blew, both ferroamp +
	// sungrow went silent, the dashboard kept rendering stale
	// numbers and nobody noticed for 2+ hours.
	//
	// Default ThresholdN = 2. Configure via the rule's
	// `threshold_n` field. ThresholdS reuses the same per-driver
	// staleness threshold semantics — a driver is "stale" when its
	// LastSuccess is older than ThresholdS (default DefaultThresholdS).
	EventConcurrentDriversOffline = "concurrent_drivers_offline"
)

// DefaultConcurrentThresholdN is the count of drivers that must be
// concurrently offline before EventConcurrentDriversOffline fires,
// when the rule doesn't override.
const DefaultConcurrentThresholdN = 2

// Defaults for a freshly-seeded rule.
const (
	DefaultThresholdS = 600
	DefaultCooldownS  = 3600
)

// DefaultRules returns the built-in event types, disabled by default so
// the operator opts in per event via the UI.
func DefaultRules() []config.NotificationRule {
	return []config.NotificationRule{
		{Type: EventDriverOffline, Enabled: false, ThresholdS: DefaultThresholdS, Priority: 4, CooldownS: DefaultCooldownS},
		{Type: EventDriverRecovered, Enabled: false, Priority: 3, CooldownS: 0},
		{Type: EventUpdateAvailable, Enabled: false, Priority: 3, CooldownS: 3600},
		// Default threshold 30 s — brief inrush / dryer starts shouldn't
		// page the operator. Cooldown 15 min per phase.
		{Type: EventFuseOverLimit, Enabled: false, ThresholdS: 30, Priority: 5, CooldownS: 900},
		// Concurrent-drivers-offline: the fuse-blow signature. Default
		// thresholds: 2 drivers offline for 5 minutes, 30 min cooldown
		// (it's an "infrastructure" alert, you don't want it pinging
		// every minute while the operator is on the way home to fix
		// the breaker).
		{Type: EventConcurrentDriversOffline, Enabled: false,
			ThresholdS: 300, ThresholdN: DefaultConcurrentThresholdN,
			Priority: 5, CooldownS: 1800},
	}
}

// DeviceLookup resolves a driver name to its hardware-stable identity.
// Returns ok=false when no device has been registered yet (cold start).
type DeviceLookup = func(name string) (deviceID, makeStr, serial string, ok bool)

// FuseReader is a live snapshot of per-phase currents and the site fuse
// rating, used by the fuse_over_limit rule. Returns ok=false when the
// site meter hasn't emitted phase currents yet or no fuse is configured.
// main.go wires this against telemetry.Store.LatestMetric + cfg.Fuse.
type FuseReader = func() (amps map[string]float64, limitA float64, ok bool)

// Message is a rendered notification payload.
type Message struct {
	Title    string
	Body     string
	Priority int
	Tags     []string
}

// Publisher dispatches a rendered Message to its transport.
type Publisher interface {
	Publish(ctx context.Context, m Message) error
}

// Provider is a hot-reloadable Publisher. Implementations must be safe to
// call Publish from multiple goroutines and must honor SetConfig without
// dropping in-flight requests.
type Provider interface {
	Publisher
	Name() string
	SetConfig(cfg *config.Notifications)
}

// ProviderFactory builds a Provider from the top-level Notifications cfg.
type ProviderFactory func(cfg *config.Notifications) Provider

var (
	providersMu sync.RWMutex
	providers   = map[string]ProviderFactory{}
)

// RegisterProvider wires a new transport into the registry. Called from
// provider init() functions. Builtin "ntfy" is registered below.
func RegisterProvider(name string, factory ProviderFactory) {
	providersMu.Lock()
	defer providersMu.Unlock()
	providers[name] = factory
}

// NewProvider constructs the provider named by cfg.Provider (defaulting to
// "ntfy") using the registry. Returns nil if the provider is unknown — the
// Service treats that as disabled.
func NewProvider(cfg *config.Notifications) Provider {
	if cfg == nil {
		return nil
	}
	name := cfg.Provider
	if name == "" {
		name = "ntfy"
	}
	providersMu.RLock()
	f := providers[name]
	providersMu.RUnlock()
	if f == nil {
		return nil
	}
	return f(cfg)
}

// Status is a snapshot of the service state for the UI.
type Status struct {
	Enabled     bool   `json:"enabled"`
	Provider    string `json:"provider,omitempty"`
	Server      string `json:"server,omitempty"`
	Topic       string `json:"topic,omitempty"`
	Sent        uint64 `json:"sent"`
	Failed      uint64 `json:"failed"`
	RuleCount   int    `json:"rule_count"`
	ActiveAlert int    `json:"active_alerts"`
}

// Service is the rule engine + dispatcher.
//
// All public pointer methods are safe on a nil receiver — main.go always
// constructs a Service, but tests sometimes pass nil Deps so handlers
// must remain nil-safe.
type Service struct {
	mu           sync.Mutex
	cfg          *config.Notifications
	pub          Publisher
	lookup       DeviceLookup
	fuseReader   FuseReader
	bus          *events.Bus
	lastFired    map[string]time.Time
	alreadyFired map[string]bool
	activeAlert  map[string]bool
	// fuseFirstOverAt[phase] is the tick we first saw a phase over its
	// limit. Cleared on tick that observes the phase back under limit.
	// Used by the fuse_over_limit rule to enforce the threshold_s
	// sustained-over check, analogous to driver_offline's threshold.
	fuseFirstOverAt map[string]time.Time
	sent            uint64
	failed          uint64
	now             func() time.Time
}

// New constructs a Service. cfg may be nil (no-op until Reload is called).
func New(cfg *config.Notifications, pub Publisher, lookup DeviceLookup) *Service {
	return &Service{
		cfg:             cfg,
		pub:             pub,
		lookup:          lookup,
		lastFired:       map[string]time.Time{},
		alreadyFired:    map[string]bool{},
		activeAlert:     map[string]bool{},
		fuseFirstOverAt: map[string]time.Time{},
		now:             time.Now,
	}
}

// SetFuseReader wires the per-tick phase-current snapshot fn used by
// the fuse_over_limit rule. Safe to call at any point; nil disables
// the rule (the evaluator short-circuits).
func (s *Service) SetFuseReader(fr FuseReader) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.fuseReader = fr
	s.mu.Unlock()
}

// SetPublisher swaps the transport. Used by main.go's reload applier to
// install a provider built from fresh config when the notifications
// section was absent at startup (or when the provider type changed).
func (s *Service) SetPublisher(pub Publisher) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.pub = pub
	s.mu.Unlock()
}

// Reload swaps the config. Preserves lastFired (cooldown must survive a
// settings toggle) but clears per-outage state so freshly-enabled rules
// don't fire retroactively. If the current publisher implements Provider
// it also gets the fresh config (hot-reload without reconstruction).
func (s *Service) Reload(cfg *config.Notifications) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cfg = cfg
	pub := s.pub
	s.alreadyFired = map[string]bool{}
	s.activeAlert = map[string]bool{}
	s.mu.Unlock()
	if p, ok := pub.(Provider); ok {
		p.SetConfig(cfg)
	}
}

// Subscribe wires this service onto a shared event bus. The core control
// loop publishes HealthTick each tick; the API's /test endpoint publishes
// NotificationTest. Neither producer knows about notifications internals.
// The bus is also retained on the Service so dispatch() can emit
// NotificationDispatched events for downstream subscribers (history log).
func (s *Service) Subscribe(bus *events.Bus) {
	if s == nil || bus == nil {
		return
	}
	s.mu.Lock()
	s.bus = bus
	s.mu.Unlock()
	bus.Subscribe(events.KindHealthTick, func(e events.Event) {
		ev, ok := e.(events.HealthTick)
		if !ok {
			return
		}
		// Offload to a goroutine: the bus runs handlers inline on the
		// publisher (the control loop), and observeAt can trigger a
		// 10 s HTTP Publish inside dispatch(). Without this, a slow
		// ntfy server would stall the next control tick.
		go s.observeAt(ev.Health, ev.Now)
		// Fuse rule runs off the same HealthTick cadence so brief
		// inrush currents don't fire without the threshold sustaining.
		go s.evaluateFuse(ev.Now)
	})
	bus.Subscribe(events.KindNotificationTest, func(e events.Event) {
		ev, ok := e.(events.NotificationTest)
		if !ok {
			return
		}
		err := s.SendTest()
		if ev.Reply != nil {
			select {
			case ev.Reply <- err:
			default:
			}
		}
	})
	bus.Subscribe(events.KindUpdateAvailable, func(e events.Event) {
		ev, ok := e.(events.UpdateAvailable)
		if !ok {
			return
		}
		// Dispatch in a goroutine for the same reason as HealthTick —
		// the bus is synchronous and Publish can block up to 10 s.
		go s.handleUpdateAvailable(ev)
	})
}

// handleUpdateAvailable dispatches a notification for a newly-discovered
// release, honoring the update_available rule's enabled/priority/
// cooldown settings. Keyed per Version so a new release isn't blocked
// by the previous one's cooldown.
func (s *Service) handleUpdateAvailable(ev events.UpdateAvailable) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.cfg == nil || !s.cfg.Enabled {
		s.mu.Unlock()
		return
	}
	rule, ok := findRule(s.cfg.Events, EventUpdateAvailable)
	if !ok || !rule.Enabled {
		s.mu.Unlock()
		return
	}
	cfg := s.cfg
	key := EventUpdateAvailable + "|" + ev.Version
	now := s.now()
	if rule.CooldownS > 0 {
		if last, ok := s.lastFired[key]; ok && now.Sub(last) < time.Duration(rule.CooldownS)*time.Second {
			s.mu.Unlock()
			return
		}
	}
	s.lastFired[key] = now
	s.mu.Unlock()

	data := templateData{
		EventType:       EventUpdateAvailable,
		Timestamp:       ev.At.UTC().Format(time.RFC3339),
		Version:         ev.Version,
		PreviousVersion: ev.PreviousVersion,
		ReleaseURL:      ev.ReleaseNotesURL,
	}
	s.dispatch(cfg, rule, data)
}

// evaluateFuse reads the live per-phase current snapshot and fires the
// fuse_over_limit rule when a phase has been over its rating for at
// least threshold_s. State is per-phase: firstOverAt records when the
// over-window started, alreadyFired latches to fire once per outage,
// and cooldownS (shared lastFired map) rate-limits re-fires across
// quick recover/over cycles.
func (s *Service) evaluateFuse(now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.cfg == nil || !s.cfg.Enabled {
		s.mu.Unlock()
		return
	}
	rule, ok := findRule(s.cfg.Events, EventFuseOverLimit)
	if !ok || !rule.Enabled {
		s.mu.Unlock()
		return
	}
	reader := s.fuseReader
	if reader == nil {
		s.mu.Unlock()
		return
	}
	amps, limitA, ok := reader()
	if !ok || limitA <= 0 {
		s.mu.Unlock()
		return
	}
	cfg := s.cfg
	threshold := time.Duration(rule.ThresholdS) * time.Second
	if threshold == 0 {
		threshold = 30 * time.Second
	}
	type toFire struct {
		rule config.NotificationRule
		data templateData
	}
	var pending []toFire
	for phase, a := range amps {
		key := EventFuseOverLimit + "|" + phase
		if a <= limitA {
			// Back under: reset the over-window and per-outage latch.
			delete(s.fuseFirstOverAt, phase)
			delete(s.alreadyFired, key)
			continue
		}
		first, ok := s.fuseFirstOverAt[phase]
		if !ok {
			s.fuseFirstOverAt[phase] = now
			continue
		}
		if now.Sub(first) < threshold {
			continue
		}
		if s.alreadyFired[key] {
			continue
		}
		if rule.CooldownS > 0 {
			if last, ok := s.lastFired[key]; ok && now.Sub(last) < time.Duration(rule.CooldownS)*time.Second {
				continue
			}
		}
		s.alreadyFired[key] = true
		s.lastFired[key] = now
		pending = append(pending, toFire{
			rule: rule,
			data: templateData{
				EventType: EventFuseOverLimit,
				Timestamp: now.UTC().Format(time.RFC3339),
				Duration:  humanDuration(now.Sub(first)),
				DurationS: int(now.Sub(first) / time.Second),
				Phase:     phase,
				Amps:      a,
				LimitA:    limitA,
			},
		})
	}
	s.mu.Unlock()

	for _, f := range pending {
		s.dispatch(cfg, f.rule, f.data)
	}
}

func findRule(rules []config.NotificationRule, eventType string) (config.NotificationRule, bool) {
	for _, r := range rules {
		if r.Type == eventType {
			return r, true
		}
	}
	return config.NotificationRule{}, false
}

// Enabled reports whether the top-level toggle is on.
func (s *Service) Enabled() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg != nil && s.cfg.Enabled
}

// Status returns a read-only snapshot for the UI.
func (s *Service) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Status{Sent: s.sent, Failed: s.failed}
	if s.cfg != nil {
		out.Enabled = s.cfg.Enabled
		out.Provider = s.cfg.Provider
		if s.cfg.Ntfy != nil {
			out.Server = s.cfg.Ntfy.Server
			out.Topic = s.cfg.Ntfy.Topic
		}
		out.RuleCount = len(s.cfg.Events)
	}
	for _, v := range s.activeAlert {
		if v {
			out.ActiveAlert++
		}
	}
	return out
}

// Observe is a compatibility wrapper; prefer the event-bus route.
func (s *Service) Observe(health map[string]telemetry.DriverHealth) {
	if s == nil {
		return
	}
	s.observeAt(health, s.now())
}

func (s *Service) observeAt(health map[string]telemetry.DriverHealth, now time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.cfg == nil || !s.cfg.Enabled {
		s.mu.Unlock()
		return
	}
	rules := s.cfg.Events
	cfg := s.cfg
	type dispatch struct {
		rule config.NotificationRule
		data templateData
	}
	var pending []dispatch

	for _, rule := range rules {
		if !rule.Enabled || rule.Type == "" {
			continue
		}
		for driver, h := range health {
			var since time.Duration
			if h.LastSuccess != nil {
				since = now.Sub(*h.LastSuccess)
			}
			key := rule.Type + "|" + driver

			switch rule.Type {
			case EventDriverOffline:
				threshold := time.Duration(rule.ThresholdS) * time.Second
				if threshold == 0 {
					threshold = time.Duration(DefaultThresholdS) * time.Second
				}
				// Cold start: never-started driver shouldn't alarm.
				if h.LastSuccess == nil && h.TickCount == 0 {
					continue
				}
				// Fresh — clear the per-outage latch so the NEXT outage
				// can fire. Don't touch activeAlert here: the recovered
				// rule consumes + clears it, and this branch may run
				// before the recovered branch in the same Observe pass.
				// If driver_recovered is DISABLED it would leak — a
				// post-pass cleanup below handles that case.
				if since < threshold {
					delete(s.alreadyFired, key)
					continue
				}
				if s.alreadyFired[key] {
					continue
				}
				if rule.CooldownS > 0 {
					if last, ok := s.lastFired[key]; ok && now.Sub(last) < time.Duration(rule.CooldownS)*time.Second {
						continue
					}
				}
				s.alreadyFired[key] = true
				s.activeAlert[driver] = true
				s.lastFired[key] = now
				pending = append(pending, dispatch{rule, s.buildData(driver, rule.Type, since, now)})
			case EventDriverRecovered:
				if !s.activeAlert[driver] {
					continue
				}
				stillStale := h.LastSuccess == nil || since > 30*time.Second
				if stillStale {
					continue
				}
				if rule.CooldownS > 0 {
					if last, ok := s.lastFired[key]; ok && now.Sub(last) < time.Duration(rule.CooldownS)*time.Second {
						delete(s.activeAlert, driver)
						continue
					}
				}
				delete(s.activeAlert, driver)
				s.lastFired[key] = now
				pending = append(pending, dispatch{rule, s.buildData(driver, rule.Type, since, now)})
			}
		}
	}
	// Per-rule (post per-driver loop): concurrent drivers offline.
	// This rule is ABOUT the fleet, not a single driver, so it can't
	// be evaluated inside the per-driver inner loop. We collect the
	// stale set once and decide whether the count crosses threshold.
	for _, rule := range rules {
		if !rule.Enabled || rule.Type != EventConcurrentDriversOffline {
			continue
		}
		threshS := time.Duration(rule.ThresholdS) * time.Second
		if threshS == 0 {
			threshS = time.Duration(DefaultThresholdS) * time.Second
		}
		threshN := rule.ThresholdN
		if threshN <= 0 {
			threshN = DefaultConcurrentThresholdN
		}
		var stale []string
		for d, h := range health {
			// Cold-start drivers don't count — same exception as
			// driver_offline. A driver that's never emitted yet
			// shouldn't pull the fleet into a fuse-blow alert.
			if h.LastSuccess == nil && h.TickCount == 0 {
				continue
			}
			var since time.Duration
			if h.LastSuccess != nil {
				since = now.Sub(*h.LastSuccess)
			}
			if h.LastSuccess == nil || since >= threshS {
				stale = append(stale, d)
			}
		}
		sort.Strings(stale)
		key := rule.Type + "|fleet"
		if len(stale) < threshN {
			// Healthy / partial outage — clear the latch so the
			// next concurrent failure can fire.
			delete(s.alreadyFired, key)
			continue
		}
		if s.alreadyFired[key] {
			continue
		}
		if rule.CooldownS > 0 {
			if last, ok := s.lastFired[key]; ok && now.Sub(last) < time.Duration(rule.CooldownS)*time.Second {
				continue
			}
		}
		s.alreadyFired[key] = true
		s.lastFired[key] = now
		td := templateData{
			Device:    strings.Join(stale, ", "),
			Devices:   stale,
			EventType: rule.Type,
			Timestamp: now.UTC().Format(time.RFC3339),
		}
		pending = append(pending, dispatch{rule, td})
	}
	// Post-pass: if no enabled driver_recovered rule exists, nothing
	// above ever clears activeAlert when a driver goes healthy again,
	// and Status().ActiveAlert would leak upward forever. Clean up
	// here for every driver whose telemetry is fresh. The 30 s window
	// mirrors the recovered rule's stillStale check.
	recoveredEnabled := false
	for _, r := range rules {
		if r.Enabled && r.Type == EventDriverRecovered {
			recoveredEnabled = true
			break
		}
	}
	if !recoveredEnabled {
		for driver := range s.activeAlert {
			h, ok := health[driver]
			if !ok || h.LastSuccess == nil {
				continue
			}
			if now.Sub(*h.LastSuccess) < 30*time.Second {
				delete(s.activeAlert, driver)
			}
		}
	}
	s.mu.Unlock()

	for _, d := range pending {
		s.dispatch(cfg, d.rule, d.data)
	}
}

// SendTest renders and publishes a synthetic notification. Errors when disabled.
func (s *Service) SendTest() error {
	if s == nil {
		return fmt.Errorf("notifications not configured")
	}
	s.mu.Lock()
	cfg := s.cfg
	pub := s.pub
	s.mu.Unlock()
	if cfg == nil || !cfg.Enabled {
		return fmt.Errorf("notifications disabled")
	}
	if pub == nil {
		return fmt.Errorf("notifications: no publisher")
	}
	prio := cfg.DefaultPriority
	if prio <= 0 {
		prio = 3
	}
	msg := Message{
		Title:    "forty-two-watts: test notification",
		Body:     fmt.Sprintf("Test notification sent at %s.", time.Now().UTC().Format(time.RFC3339)),
		Priority: prio,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := pub.Publish(ctx, msg)
	s.mu.Lock()
	if err != nil {
		s.failed++
	} else {
		s.sent++
	}
	s.mu.Unlock()
	if err != nil {
		s.emitDispatched("test", "", msg, "failed", err.Error())
	} else {
		s.emitDispatched("test", "", msg, "sent", "")
	}
	return err
}

// templateData is the {{.}} for rule templates.
type templateData struct {
	Device    string
	DeviceID  string
	Make      string
	Serial    string
	EventType string
	DurationS int
	Duration  string
	Timestamp string
	// Populated for update_available events only.
	Version         string
	PreviousVersion string
	ReleaseURL      string
	// Populated for fuse_over_limit events only.
	Phase  string  // "L1" | "L2" | "L3"
	Amps   float64 // current reading on that phase
	LimitA float64 // fuse rating (site.fuse.max_amps)
	// Populated for concurrent_drivers_offline only. Devices is the
	// sorted list of stale drivers; Device contains the same names
	// joined by ", " for the default body template.
	Devices []string
}

func (s *Service) buildData(driver, eventType string, since time.Duration, now time.Time) templateData {
	td := templateData{
		Device:    driver,
		EventType: eventType,
		DurationS: int(since / time.Second),
		Duration:  humanDuration(since),
		Timestamp: now.UTC().Format(time.RFC3339),
	}
	if s.lookup != nil {
		if id, mk, sn, ok := s.lookup(driver); ok {
			td.DeviceID = id
			td.Make = mk
			td.Serial = sn
		}
	}
	return td
}

func (s *Service) dispatch(cfg *config.Notifications, rule config.NotificationRule, data templateData) {
	titleTpl := rule.TitleTemplate
	if strings.TrimSpace(titleTpl) == "" {
		titleTpl = defaultTitleFor(rule.Type)
	}
	bodyTpl := rule.BodyTemplate
	if strings.TrimSpace(bodyTpl) == "" {
		bodyTpl = defaultBodyFor(rule.Type)
	}
	title, err := renderTemplate("title", titleTpl, data)
	if err != nil {
		slog.Warn("notifications: title render failed", "event", rule.Type, "err", err)
		s.bumpFailed()
		// Still emit a failed event so the history UI shows render
		// errors — otherwise operators see only the bumped Failed
		// counter with no trail of what broke.
		s.emitDispatched(rule.Type, data.Device,
			Message{Title: titleTpl, Body: bodyTpl, Priority: rule.Priority},
			"failed", "title render: "+err.Error())
		return
	}
	body, err := renderTemplate("body", bodyTpl, data)
	if err != nil {
		slog.Warn("notifications: body render failed", "event", rule.Type, "err", err)
		s.bumpFailed()
		s.emitDispatched(rule.Type, data.Device,
			Message{Title: strings.TrimSpace(title), Body: bodyTpl, Priority: rule.Priority},
			"failed", "body render: "+err.Error())
		return
	}
	prio := rule.Priority
	if prio == 0 {
		prio = cfg.DefaultPriority
	}
	msg := Message{
		Title:    strings.TrimSpace(title),
		Body:     strings.TrimSpace(body),
		Priority: prio,
		Tags:     splitTags(rule.Tags),
	}
	// Nil-publisher guard: cold-start / reload can leave pub unset when
	// notifications are enabled but the configured provider isn't
	// installed (NewProvider returned nil). Treat as a failed dispatch
	// so history + counters stay truthful instead of panicking.
	s.mu.Lock()
	pub := s.pub
	s.mu.Unlock()
	if pub == nil {
		slog.Warn("notifications: no publisher installed — skipping", "event", rule.Type, "driver", data.Device)
		s.bumpFailed()
		s.emitDispatched(rule.Type, data.Device, msg, "failed", "no publisher installed")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pub.Publish(ctx, msg); err != nil {
		slog.Warn("notifications: publish failed", "event", rule.Type, "driver", data.Device, "err", err)
		s.bumpFailed()
		s.emitDispatched(rule.Type, data.Device, msg, "failed", err.Error())
		return
	}
	slog.Info("notifications: sent", "event", rule.Type, "driver", data.Device)
	s.bumpSent()
	s.emitDispatched(rule.Type, data.Device, msg, "sent", "")
}

// emitDispatched publishes a NotificationDispatched event for history
// loggers + future audit subscribers. Safe when the bus is nil.
func (s *Service) emitDispatched(eventType, driver string, m Message, status, errStr string) {
	s.mu.Lock()
	bus := s.bus
	s.mu.Unlock()
	if bus == nil {
		return
	}
	bus.Publish(events.NotificationDispatched{
		Time:      time.Now(),
		EventType: eventType,
		Driver:    driver,
		Title:     m.Title,
		Body:      m.Body,
		Priority:  m.Priority,
		Status:    status,
		Error:     errStr,
	})
}

func (s *Service) bumpSent() {
	s.mu.Lock()
	s.sent++
	s.mu.Unlock()
}

func (s *Service) bumpFailed() {
	s.mu.Lock()
	s.failed++
	s.mu.Unlock()
}

// EventDefaults returns the built-in title/body template for every known
// event type. Exposed via the API so the UI can pre-fill form inputs
// with exactly what the backend will render when the operator leaves the
// custom template blank.
func EventDefaults() map[string]struct {
	Title string `json:"title"`
	Body  string `json:"body"`
} {
	types := []string{EventDriverOffline, EventDriverRecovered, EventUpdateAvailable, EventFuseOverLimit}
	out := make(map[string]struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}, len(types))
	for _, t := range types {
		out[t] = struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}{Title: defaultTitleFor(t), Body: defaultBodyFor(t)}
	}
	return out
}

func defaultTitleFor(eventType string) string {
	switch eventType {
	case EventDriverOffline:
		return "forty-two-watts: {{.Device}} offline"
	case EventDriverRecovered:
		return "forty-two-watts: {{.Device}} recovered"
	case EventUpdateAvailable:
		return "forty-two-watts: update {{.Version}} available"
	case EventFuseOverLimit:
		return "forty-two-watts: {{.Phase}} over fuse limit"
	case EventConcurrentDriversOffline:
		return "forty-two-watts: {{len .Devices}} drivers offline (likely fuse / outage)"
	}
	return "forty-two-watts: {{.EventType}}"
}

func defaultBodyFor(eventType string) string {
	switch eventType {
	case EventDriverOffline:
		return "{{.Device}} has not reported telemetry for {{.Duration}}."
	case EventDriverRecovered:
		return "{{.Device}} is reporting telemetry again."
	case EventUpdateAvailable:
		return "Version {{.Version}} is available (running {{.PreviousVersion}}). {{.ReleaseURL}}"
	case EventFuseOverLimit:
		return "{{.Phase}} draw {{printf \"%.1f\" .Amps}} A exceeded the {{printf \"%.0f\" .LimitA}} A fuse for {{.Duration}}."
	case EventConcurrentDriversOffline:
		return "{{len .Devices}} drivers went silent at once: {{.Device}}. Check the fuse / breaker / wifi."
	}
	return "{{.EventType}} for {{.Device}}"
}

func renderTemplate(name, tpl string, data any) (string, error) {
	t, err := template.New(name).Parse(tpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// humanDuration formats a duration in the short style used in defaults.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
