package calendar

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
	webdav "github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/loadmodel"
)

// LoadProfiler is the slice of *loadmodel.Service the calendar service needs:
// flip the active load profile to "away" while the house is empty and back to
// "home" when it isn't. Declared as an interface so tests can inject a fake.
type LoadProfiler interface {
	SetProfile(loadmodel.Profile) error
}

// LoadpointTargeter is the slice of *loadpoint.Manager the calendar service
// needs: push an EV "charged-by-departure" deadline onto a loadpoint. The MPC
// loadpoint probe already reads the resulting target/time off the manager.
type LoadpointTargeter interface {
	SetTarget(id string, socPct float64, targetTime time.Time) bool
}

// Service is a CalDAV *client*. It periodically polls a calendar collection
// (served by the bundled Radicale sidecar, or any CalDAV server), classifies
// events by title keyword, and applies the resulting intents:
//
//   - "away" intervals → loadmodel away/home profile switch (live + training),
//     plus an IsAwayAt hook the MPC load predictor consults per slot;
//   - EV deadlines → loadpoint SetTarget(socPct, departure).
//
// Everything is opt-in and fail-soft: an unreachable server logs a warning and
// leaves the last-good intents (and control) untouched.
type Service struct {
	lp LoadpointTargeter
	lm LoadProfiler

	// lifecycleMu serialises Start / Stop / Reload.
	lifecycleMu sync.Mutex
	stop        chan struct{}
	wg          sync.WaitGroup
	running     bool

	// evSource yields current EV charge-point samples (set by main.go from
	// telemetry before Start). Drives the outbound history writer.
	evSource         EVSource
	evSampleInterval time.Duration
	ev               map[string]*evTrack

	// planSource yields the current MPC plan slots (set by main.go before
	// Start). Drives the forward-looking plan publisher.
	planSource PlanSource

	// mu guards the resolved config + live diagnostic state below.
	mu             sync.RWMutex
	enabled        bool
	url            string
	calendarPath   string
	username       string
	password       string
	pollInterval   time.Duration
	horizon        time.Duration
	prs            *parser
	historyEnabled bool
	historyPath    string
	intents        Intents
	lastSyncMs     int64
	lastErr        string
	reachable      bool
	awayActive     bool
	profileApplied bool
	lastEV         *EVDeadline
	historyWritten int
	lastHistoryMs  int64

	planEnabled         bool
	planPath            string
	planPublishInterval time.Duration
	planWritten         map[string]string // uid -> content hash
	planEventCount      int
	lastPlanMs          int64
}

// Status is the read-only snapshot rendered by GET /api/caldav/status.
type Status struct {
	Enabled        bool        `json:"enabled"`
	Reachable      bool        `json:"reachable"`
	LastSyncMs     int64       `json:"last_sync_ms"`
	LastError      string      `json:"last_error,omitempty"`
	EventCount     int         `json:"event_count"`
	AwayActive     bool        `json:"away_active"`
	NextEVDeadline *EVDeadline `json:"next_ev_deadline,omitempty"`
	SubscribeURL   string      `json:"subscribe_url,omitempty"`
	Username       string      `json:"username,omitempty"`

	// Outbound EVSE-history writer.
	HistoryEnabled bool   `json:"history_enabled"`
	HistoryURL     string `json:"history_url,omitempty"`
	HistoryWritten int    `json:"history_written"`
	LastHistoryMs  int64  `json:"last_history_ms,omitempty"`

	// Outbound forward-looking plan publisher.
	PlanEnabled bool   `json:"plan_enabled"`
	PlanURL     string `json:"plan_url,omitempty"`
	PlanEvents  int    `json:"plan_events"`
	LastPlanMs  int64  `json:"last_plan_ms,omitempty"`
}

// New builds a calendar service from config. firstLoadpointID is the fallback
// loadpoint an EV event targets when neither the event title nor
// cfg.EVLoadpointID names one (main.go passes cfg.Loadpoints[0].ID).
func New(cfg config.CalDAV, lp LoadpointTargeter, lm LoadProfiler, firstLoadpointID string) *Service {
	s := &Service{
		lp:               lp,
		lm:               lm,
		ev:               make(map[string]*evTrack),
		evSampleInterval: 30 * time.Second,
		planWritten:      make(map[string]string),
	}
	s.applyConfig(cfg, firstLoadpointID)
	return s
}

// SetPlanSource is defined in plan.go.

// SetEVSource installs the EV telemetry source for the outbound history
// writer. Call before Start. nil disables the writer.
func (s *Service) SetEVSource(src EVSource) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.evSource = src
	s.mu.Unlock()
}

// applyConfig resolves config (filling defaults) into the live fields. Caller
// must not hold mu.
func (s *Service) applyConfig(cfg config.CalDAV, firstLoadpointID string) {
	url := strings.TrimSpace(cfg.URL)
	if url == "" {
		url = config.DefaultCalDAVURL
	}
	calPath := strings.TrimSpace(cfg.CalendarPath)
	if calPath == "" {
		calPath = config.DefaultCalDAVCalendarPath
	}
	poll := cfg.PollIntervalS
	if poll <= 0 {
		poll = config.DefaultCalDAVPollS
	}
	horizonDays := cfg.HorizonDays
	if horizonDays <= 0 {
		horizonDays = config.DefaultCalDAVHorizonDays
	}
	targetSoC := cfg.EVDefaultTargetSoCPct
	if targetSoC <= 0 {
		targetSoC = config.DefaultCalDAVEVTargetSoC
	}
	away := cfg.AwayKeywords
	if len(away) == 0 {
		away = config.DefaultAwayKeywords
	}
	evk := cfg.EVKeywords
	if len(evk) == 0 {
		evk = config.DefaultEVKeywords
	}
	defaultLP := strings.TrimSpace(cfg.EVLoadpointID)
	if defaultLP == "" {
		defaultLP = firstLoadpointID
	}
	histPath := strings.TrimSpace(cfg.HistoryPath)
	if histPath == "" {
		histPath = config.DefaultCalDAVHistoryPath
	}
	// Refuse to write history into the same collection we read intents from —
	// 42W would re-ingest its own "EV charged …" events as EV deadlines.
	histEnabled := cfg.EVSEHistoryEnabled() && histPath != "" && histPath != calPath
	if cfg.EVSEHistoryEnabled() && histPath == calPath {
		slog.Warn("caldav: history_path equals calendar_path; disabling EVSE history to avoid a feedback loop",
			"path", histPath)
	}
	planPath := strings.TrimSpace(cfg.PlanPath)
	if planPath == "" {
		planPath = config.DefaultCalDAVPlanPath
	}
	planPub := cfg.PlanPublishIntervalS
	if planPub <= 0 {
		planPub = config.DefaultCalDAVPlanPublishS
	}
	// Plan collection must be distinct from the inbound calendar (else the
	// publisher's events would be re-read as intents) and from history.
	planEnabled := cfg.PublishPlanEnabled() && planPath != "" && planPath != calPath && planPath != histPath
	if cfg.PublishPlanEnabled() && (planPath == calPath || planPath == histPath) {
		slog.Warn("caldav: plan_path collides with calendar_path/history_path; disabling plan publishing",
			"path", planPath)
	}

	s.mu.Lock()
	s.enabled = cfg.Enabled
	s.url = url
	s.calendarPath = calPath
	s.username = cfg.Username
	s.password = cfg.Password
	s.pollInterval = time.Duration(poll) * time.Second
	s.horizon = time.Duration(horizonDays) * 24 * time.Hour
	s.prs = newParser(away, evk, defaultLP, targetSoC)
	s.historyPath = histPath
	s.historyEnabled = histEnabled
	s.planPath = planPath
	s.planEnabled = planEnabled
	s.planPublishInterval = time.Duration(planPub) * time.Second
	s.mu.Unlock()
}

// Reload swaps in a changed caldav config without a restart (URL, credentials,
// keywords, interval). Enabling/disabling the feature is restart-gated in
// config.RestartRequiredFor, so this only ever runs while enabled.
func (s *Service) Reload(cfg config.CalDAV, firstLoadpointID string) {
	if s == nil {
		return
	}
	s.applyConfig(cfg, firstLoadpointID)
	slog.Info("caldav config reloaded")
}

// Start launches the inbound poll loop, plus the outbound EVSE-history loop
// when a source + history collection are configured. No-op if disabled or
// already running.
func (s *Service) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.running {
		return
	}
	s.mu.RLock()
	enabled := s.enabled
	historyEnabled := s.historyEnabled && s.evSource != nil
	planEnabled := s.planEnabled && s.planSource != nil
	s.mu.RUnlock()
	if !enabled {
		return
	}
	s.stop = make(chan struct{})
	s.running = true
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.loop(ctx) }()
	if historyEnabled {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.evHistoryLoop(ctx) }()
	}
	if planEnabled {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.planPublishLoop(ctx) }()
	}
}

// Stop terminates both loops.
func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if !s.running {
		return
	}
	close(s.stop)
	s.wg.Wait()
	s.running = false
}

func (s *Service) loop(ctx context.Context) {
	// Prime once promptly so the dashboard + planner see intents without
	// waiting a full interval.
	s.pollOnce(ctx)

	s.mu.RLock()
	interval := s.pollInterval
	s.mu.RUnlock()
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.pollOnce(ctx)
			// Pick up an interval change from Reload.
			s.mu.RLock()
			next := s.pollInterval
			s.mu.RUnlock()
			if next != interval {
				interval = next
				t.Reset(interval)
			}
		}
	}
}

// pollOnce fetches + applies once. Errors are recorded but never propagated —
// control must not stall on a flaky calendar server.
func (s *Service) pollOnce(ctx context.Context) {
	intents, err := s.fetch(ctx)
	now := time.Now()
	if err != nil {
		s.mu.Lock()
		s.reachable = false
		s.lastErr = err.Error()
		s.lastSyncMs = now.UnixMilli()
		s.mu.Unlock()
		slog.Warn("caldav poll failed", "err", err)
		return
	}

	s.mu.Lock()
	s.reachable = true
	s.lastErr = ""
	s.lastSyncMs = now.UnixMilli()
	s.intents = intents
	s.mu.Unlock()

	s.apply(intents, now)
}

// newClient builds a CalDAV client with Basic auth against the configured
// server. Shared by the inbound poll (fetch) and the outbound history writer.
func (s *Service) newClient(url, user, pass string) (*caldav.Client, error) {
	httpClient := webdav.HTTPClientWithBasicAuth(&http.Client{Timeout: 15 * time.Second}, user, pass)
	return caldav.NewClient(httpClient, url)
}

// fetch runs a time-ranged calendar-query REPORT and parses the result. The
// server expands recurrences within the horizon (Expand), so we never compute
// RRULE instances ourselves.
func (s *Service) fetch(ctx context.Context) (Intents, error) {
	s.mu.RLock()
	url, calPath, user, pass := s.url, s.calendarPath, s.username, s.password
	horizon, prs := s.horizon, s.prs
	s.mu.RUnlock()

	client, err := s.newClient(url, user, pass)
	if err != nil {
		return Intents{}, err
	}

	start := time.Now()
	end := start.Add(horizon)
	query := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name: "VCALENDAR",
			Comps: []caldav.CalendarCompRequest{{
				Name:     "VEVENT",
				AllProps: true,
				Expand:   &caldav.CalendarExpandRequest{Start: start, End: end},
			}},
		},
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{{
				Name:  "VEVENT",
				Start: start,
				End:   end,
			}},
		},
	}

	objs, err := client.QueryCalendar(ctx, calPath, query)
	if err != nil {
		return Intents{}, err
	}

	var out Intents
	for _, obj := range objs {
		if obj.Data == nil {
			continue
		}
		events := obj.Data.Events()
		for i := range events {
			ev := events[i]
			title, _ := ev.Props.Text(ical.PropSummary)
			uid, _ := ev.Props.Text(ical.PropUID)
			st, _ := ev.DateTimeStart(time.Local)
			en, _ := ev.DateTimeEnd(time.Local)
			away, evd := prs.classify(title, st, en, uid)
			if away != nil {
				out.Away = append(out.Away, *away)
			}
			if evd != nil {
				out.EV = append(out.EV, *evd)
			}
		}
	}
	return out, nil
}

// apply pushes intents into the load model + loadpoint manager.
func (s *Service) apply(intents Intents, now time.Time) {
	// Away → profile switch on transition only (idempotent). On leaving an
	// away window we restore the home profile; this intentionally overrides a
	// manual UI profile choice while/after an away event — documented.
	awayNow := false
	for _, iv := range intents.Away {
		if iv.Contains(now) {
			awayNow = true
			break
		}
	}
	s.mu.Lock()
	changed := !s.profileApplied || awayNow != s.awayActive
	s.awayActive = awayNow
	s.profileApplied = true
	s.mu.Unlock()
	if changed && s.lm != nil {
		profile := loadmodel.ProfileHome
		if awayNow {
			profile = loadmodel.ProfileAway
		}
		if err := s.lm.SetProfile(profile); err != nil {
			slog.Warn("caldav: failed to set load profile", "profile", profile, "err", err)
		} else {
			slog.Info("caldav: load profile switched", "profile", profile, "away_active", awayNow)
		}
	}

	// EV → set the next upcoming deadline's target on its loadpoint. We never
	// clear an existing manual/UI target when no event is upcoming.
	next := nextEV(intents.EV, now)
	if next != nil && s.lp != nil {
		if next.LoadpointID == "" {
			slog.Warn("caldav: EV event has no loadpoint to target; ignoring", "title", next.Title)
		} else if s.lp.SetTarget(next.LoadpointID, next.TargetSoCPct, next.Departure) {
			s.mu.Lock()
			prev := s.lastEV
			s.lastEV = next
			s.mu.Unlock()
			if prev == nil || *prev != *next {
				slog.Info("caldav: EV target set from calendar",
					"loadpoint", next.LoadpointID,
					"target_soc_pct", next.TargetSoCPct,
					"departure", next.Departure)
			}
		}
	} else {
		s.mu.Lock()
		s.lastEV = nil
		s.mu.Unlock()
	}
}

// nextEV returns the earliest EV deadline strictly after now, or nil.
func nextEV(evs []EVDeadline, now time.Time) *EVDeadline {
	var best *EVDeadline
	for i := range evs {
		if !evs[i].Departure.After(now) {
			continue
		}
		if best == nil || evs[i].Departure.Before(best.Departure) {
			best = &evs[i]
		}
	}
	return best
}

// IsAwayAt reports whether time t falls inside any parsed away interval. The
// MPC load predictor consults this per slot so a horizon crossing an away
// window predicts reduced load over exactly the away slots.
func (s *Service) IsAwayAt(t time.Time) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, iv := range s.intents.Away {
		if iv.Contains(t) {
			return true
		}
	}
	return false
}

// Status returns a read-only snapshot for the diagnostics endpoint.
func (s *Service) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{
		Enabled:        s.enabled,
		Reachable:      s.reachable,
		LastSyncMs:     s.lastSyncMs,
		LastError:      s.lastErr,
		EventCount:     len(s.intents.Away) + len(s.intents.EV),
		AwayActive:     s.awayActive,
		SubscribeURL:   joinURL(s.url, s.calendarPath),
		Username:       s.username,
		HistoryEnabled: s.historyEnabled,
		HistoryWritten: s.historyWritten,
		LastHistoryMs:  s.lastHistoryMs,
		PlanEnabled:    s.planEnabled,
		PlanEvents:     s.planEventCount,
		LastPlanMs:     s.lastPlanMs,
	}
	if s.historyEnabled {
		st.HistoryURL = joinURL(s.url, s.historyPath)
	}
	if s.planEnabled {
		st.PlanURL = joinURL(s.url, s.planPath)
	}
	if n := nextEV(s.intents.EV, time.Now()); n != nil {
		cp := *n
		st.NextEVDeadline = &cp
	}
	return st
}

func joinURL(base, path string) string {
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}
