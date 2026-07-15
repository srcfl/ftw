package calendar

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// EVSample is a point-in-time observation of one EV charge point, supplied by
// the host (main.go reads it off telemetry). A charging→idle (or unplug)
// transition becomes a history VEVENT.
type EVSample struct {
	ID        string  // stable per charge point (driver / loadpoint id)
	Connected bool    // plug present
	Charging  bool    // actively delivering power
	SessionWh float64 // driver-reported session energy, 0 if unknown
	PowerW    float64 // current charge power (integration fallback)
}

// EVSource returns the current EV samples, one per charge point.
type EVSource func() []EVSample

// CompletedSession is a finished charge used to author a history event.
type CompletedSession struct {
	ID       string
	Start    time.Time
	End      time.Time
	EnergyWh float64
}

// evTrack is the per-charge-point session state machine.
type evTrack struct {
	active       bool
	start        time.Time
	lastSeen     time.Time
	baseWh       float64 // SessionWh at session start
	lastWh       float64 // most recent SessionWh seen while charging
	integratedWh float64 // ∫ power·dt fallback for drivers without SessionWh
}

// Noise filter — ignore contact bounce / trivial top-ups.
const (
	minSessionDur    = 60 * time.Second
	minSessionEnergy = 100.0 // Wh
)

// observeEV folds a batch of samples into the per-charge-point trackers and
// returns the sessions that just completed. It is pure with respect to the
// network — the caller writes the returned sessions. Not safe for concurrent
// use; the history loop is the single caller.
func (s *Service) observeEV(samples []EVSample, now time.Time) []CompletedSession {
	var done []CompletedSession
	seen := make(map[string]bool, len(samples))

	for _, smp := range samples {
		if smp.ID == "" {
			continue
		}
		seen[smp.ID] = true
		t := s.ev[smp.ID]
		if t == nil {
			t = &evTrack{}
			s.ev[smp.ID] = t
		}
		charging := smp.Charging && smp.Connected

		if charging && !t.active {
			// Session start.
			t.active = true
			t.start = now
			t.baseWh = smp.SessionWh
			t.lastWh = smp.SessionWh
			t.integratedWh = 0
			t.lastSeen = now
			continue
		}

		if t.active {
			if !t.lastSeen.IsZero() {
				// Integrate, ignoring long gaps (sleep / restart) that would
				// otherwise inflate the estimate.
				if dtH := now.Sub(t.lastSeen).Hours(); dtH > 0 && dtH < 1 {
					t.integratedWh += smp.PowerW * dtH
				}
			}
			t.lastSeen = now
			if smp.SessionWh > 0 {
				t.lastWh = smp.SessionWh
			}
			if !charging { // stopped charging or unplugged
				if cs, ok := finishSession(smp.ID, t, now); ok {
					done = append(done, cs)
				}
			}
		}
	}

	// A tracked charge point that vanished entirely (driver offline) ends its
	// session at the last time we saw it.
	for id, t := range s.ev {
		if t.active && !seen[id] {
			if cs, ok := finishSession(id, t, t.lastSeen); ok {
				done = append(done, cs)
			}
		}
	}
	return done
}

// finishSession closes a track and decides whether it is worth recording.
func finishSession(id string, t *evTrack, end time.Time) (CompletedSession, bool) {
	t.active = false
	energy := t.lastWh - t.baseWh
	if energy <= 0 {
		energy = t.integratedWh
	}
	if end.Sub(t.start) < minSessionDur || energy < minSessionEnergy {
		return CompletedSession{}, false
	}
	return CompletedSession{ID: id, Start: t.start, End: end, EnergyWh: energy}, true
}

// evHistoryLoop samples EV state on a fast ticker and writes a history VEVENT
// for each completed session. Fail-soft: a write error is logged, the session
// is dropped, control is never blocked.
func (s *Service) evHistoryLoop(ctx context.Context) {
	t := time.NewTicker(s.evSampleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-t.C:
			s.mu.RLock()
			src := s.evSource
			s.mu.RUnlock()
			if src == nil {
				continue
			}
			for _, cs := range s.observeEV(src(), time.Now()) {
				if err := s.writeSession(ctx, cs); err != nil {
					slog.Warn("caldav: failed to write EV history event", "id", cs.ID, "err", err)
					continue
				}
				s.mu.Lock()
				s.historyWritten++
				s.lastHistoryMs = time.Now().UnixMilli()
				s.mu.Unlock()
				slog.Info("caldav: wrote EV history event",
					"id", cs.ID, "energy_wh", cs.EnergyWh,
					"start", cs.Start, "end", cs.End)
			}
		}
	}
}

// writeSession PUTs a single VEVENT describing a completed charge into the
// history collection. The UID is stable per (charge point, session start) so a
// retried write is idempotent rather than duplicating the event.
func (s *Service) writeSession(ctx context.Context, cs CompletedSession) error {
	s.mu.RLock()
	url, histPath, user, pass := s.url, s.historyPath, s.username, s.password
	s.mu.RUnlock()

	client, err := s.newClient(url, user, pass)
	if err != nil {
		return err
	}

	uid := fmt.Sprintf("ftw-ev-%s-%d@fortytwowatts", sanitizeUID(cs.ID), cs.Start.Unix())

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//FTW//EV history//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")

	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	ev.Props.SetDateTime(ical.PropDateTimeStart, cs.Start)
	ev.Props.SetDateTime(ical.PropDateTimeEnd, cs.End)
	ev.Props.SetText(ical.PropSummary, fmt.Sprintf("EV charged %.1f kWh", cs.EnergyWh/1000))
	ev.Props.SetText(ical.PropDescription, fmt.Sprintf(
		"FTW: %s delivered %.0f Wh over %s.",
		cs.ID, cs.EnergyWh, cs.End.Sub(cs.Start).Round(time.Minute))+
		lanNote("adds a new event here after each completed charge session"))
	cal.Children = append(cal.Children, ev.Component)

	objPath := strings.TrimRight(histPath, "/") + "/" + uid + ".ics"
	_, err = client.PutCalendarObject(ctx, objPath, cal)
	return err
}

// sanitizeUID keeps a charge-point id safe for a CalDAV object path / UID.
func sanitizeUID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, id)
}
