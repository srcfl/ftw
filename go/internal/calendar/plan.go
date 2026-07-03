package calendar

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	webdav "github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
)

// PlanSlot is one MPC plan slot, supplied by the host (main.go reads it off
// mpc.Service.Latest()). The plan publisher coalesces consecutive slots with
// the same battery action into human-readable "decision blocks".
type PlanSlot struct {
	Start, End time.Time
	BatteryW   float64 // site sign: + charging, - discharging
	GridW      float64 // resulting grid power (+ import, - export)
	SoCPct     float64 // SoC at END of slot
	Confidence float64
}

// PlanSource returns the current plan slots (ordered by time).
type PlanSource func() []PlanSlot

// planChargeThreshW is the |battery power| above which a slot counts as an
// active charge/discharge window worth publishing. Below it the slot is
// "hold" and is omitted to keep the calendar to actual charging windows.
const planChargeThreshW = 150.0

// planBlock is a coalesced, ready-to-write plan event.
type planBlock struct {
	uid         string
	start, end  time.Time
	summary     string
	description string
}

// hash is a cheap content fingerprint for reconcile (re-PUT only on change).
func (b planBlock) hash() string {
	return b.summary + "|" + b.start.UTC().Format(time.RFC3339) + "|" + b.end.UTC().Format(time.RFC3339)
}

// buildPlanBlocks coalesces plan slots into forward-looking charge/discharge
// blocks. Pure + deterministic for unit testing. Only blocks that extend past
// `now` are returned (the plan is forward-looking; past windows belong to the
// history calendar). "Hold" slots break a run but are not published.
func buildPlanBlocks(slots []PlanSlot, now time.Time) []planBlock {
	var blocks []planBlock

	flush := func(run []PlanSlot, cat int) {
		if len(run) == 0 || cat == 0 {
			return
		}
		start := run[0].Start
		end := run[len(run)-1].End
		if !end.After(now) {
			return // entirely in the past
		}
		var sum float64
		for _, s := range run {
			sum += s.BatteryW
		}
		avgKW := math.Abs(sum/float64(len(run))) / 1000
		endSoC := run[len(run)-1].SoCPct
		var verb, short string
		if cat > 0 {
			verb, short = "Charge battery", "chg"
		} else {
			verb, short = "Discharge battery", "dis"
		}
		blocks = append(blocks, planBlock{
			uid:     fmt.Sprintf("ftw-plan-%s-%d@fortytwowatts", short, start.Unix()),
			start:   start,
			end:     end,
			summary: fmt.Sprintf("%s ~%.1f kW", verb, avgKW),
			description: fmt.Sprintf(
				"forty-two-watts plan: %s at about %.1f kW, SoC ≈ %.0f%% by end of window.",
				strings.ToLower(verb), avgKW, endSoC),
		})
	}

	var run []PlanSlot
	runCat := 0
	for _, s := range slots {
		cat := 0
		if s.BatteryW > planChargeThreshW {
			cat = 1
		} else if s.BatteryW < -planChargeThreshW {
			cat = -1
		}
		if cat != runCat {
			flush(run, runCat)
			run = run[:0]
			runCat = cat
		}
		if cat != 0 {
			run = append(run, s)
		}
	}
	flush(run, runCat)
	return blocks
}

// SetPlanSource installs the MPC plan source for the forward-looking plan
// publisher. Call before Start. nil disables publishing.
func (s *Service) SetPlanSource(src PlanSource) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.planSource = src
	s.mu.Unlock()
}

// planPublishLoop renders the plan into the plan collection on a ticker,
// reconciling against what it wrote last cycle. Fail-soft.
func (s *Service) planPublishLoop(ctx context.Context) {
	s.publishPlan(ctx) // prime
	s.mu.RLock()
	interval := s.planPublishInterval
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
			s.publishPlan(ctx)
		}
	}
}

// publishPlan builds the current plan blocks and reconciles the plan
// collection: PUT new/changed blocks, DELETE blocks that are no longer in the
// plan (or have fallen into the past). Churn is bounded to real plan changes.
func (s *Service) publishPlan(ctx context.Context) {
	s.mu.RLock()
	src := s.planSource
	url, planPath, user, pass := s.url, s.planPath, s.username, s.password
	interval := s.planPublishInterval
	s.mu.RUnlock()
	if src == nil {
		return
	}

	// If the planner has produced nothing yet (e.g. just after start, before the
	// MPC restores or computes a plan), leave the calendar untouched. Reconciling
	// against an empty want-set here would delete every published window — and
	// with the seed below that would wipe the whole plan calendar on each restart.
	slots := src()
	if len(slots) == 0 {
		return
	}

	blocks := buildPlanBlocks(slots, time.Now())
	// Append the reachability / refresh-cadence footer to every block. Done here
	// (not in the pure buildPlanBlocks) because it needs the live interval, and
	// after hashing-relevant fields are set — hash() ignores the description, so
	// the note never causes reconcile churn. The wording is deliberately precise:
	// 42W re-checks on a timer but only rewrites an event when the plan changes,
	// so the description must not imply the calendar churns every interval.
	note := lanNote("re-checks the plan about every " + friendlyInterval(interval) +
		", and updates an event only when the plan actually changes (so windows you have already seen stay put)")
	for i := range blocks {
		blocks[i].description += note
	}
	want := make(map[string]planBlock, len(blocks))
	for _, b := range blocks {
		want[b.uid] = b
	}

	s.mu.Lock()
	prev := s.planWritten
	if prev == nil {
		prev = map[string]string{}
	}
	seeded := s.planSeeded
	s.mu.Unlock()

	// planWritten is in-memory and resets on restart, so on the first real
	// reconcile after (re)start we seed it from what is actually in the plan
	// collection. Without this, objects written before the restart whose windows
	// have since passed are invisible to the DELETE loop and linger forever,
	// accumulating stale past events across restarts. Seeding lets this cycle's
	// DELETE loop reclaim them. Seeded uids not in `want` are deleted; seeded
	// uids still wanted carry an unknown hash and are re-PUT once (harmless —
	// a restart re-PUTs the current plan regardless).
	if !seeded {
		if uids, err := s.listPlanObjectUIDs(ctx); err != nil {
			slog.Warn("caldav: could not enumerate plan collection to seed reconcile; deferring orphan cleanup", "err", err)
		} else {
			merged := make(map[string]string, len(prev)+len(uids))
			for k, v := range prev {
				merged[k] = v
			}
			for _, uid := range uids {
				if _, ok := merged[uid]; !ok {
					merged[uid] = "" // unknown hash: forces one re-PUT if still wanted, else a DELETE
				}
			}
			prev = merged
			s.mu.Lock()
			s.planSeeded = true
			s.mu.Unlock()
		}
	}

	next := make(map[string]string, len(want))
	var puts, dels int

	// PUT new or changed blocks.
	for uid, b := range want {
		h := b.hash()
		next[uid] = h
		if prev[uid] == h {
			continue // unchanged
		}
		if err := s.putPlanBlock(ctx, url, planPath, user, pass, b); err != nil {
			slog.Warn("caldav: failed to publish plan event", "uid", uid, "err", err)
			// keep last-known state for this uid so we retry next cycle
			if old, ok := prev[uid]; ok {
				next[uid] = old
			} else {
				delete(next, uid)
			}
			continue
		}
		puts++
	}
	// DELETE blocks we wrote before but no longer want.
	for uid := range prev {
		if _, keep := want[uid]; keep {
			continue
		}
		if err := s.deleteObject(ctx, url, planPath, user, pass, uid); err != nil {
			slog.Warn("caldav: failed to delete stale plan event", "uid", uid, "err", err)
			next[uid] = prev[uid] // retain so we retry the delete next cycle
			continue
		}
		dels++
	}

	s.mu.Lock()
	s.planWritten = next
	s.planEventCount = len(want)
	s.lastPlanMs = time.Now().UnixMilli()
	s.mu.Unlock()

	if puts > 0 || dels > 0 {
		slog.Info("caldav: plan calendar reconciled", "events", len(want), "put", puts, "deleted", dels)
	}
}

func (s *Service) putPlanBlock(ctx context.Context, url, planPath, user, pass string, b planBlock) error {
	client, err := s.newClient(url, user, pass)
	if err != nil {
		return err
	}
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//forty-two-watts//plan//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, b.uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, time.Now().UTC())
	ev.Props.SetDateTime(ical.PropDateTimeStart, b.start)
	ev.Props.SetDateTime(ical.PropDateTimeEnd, b.end)
	ev.Props.SetText(ical.PropSummary, b.summary)
	ev.Props.SetText(ical.PropDescription, b.description)
	// Mark as tentative — a plan, not a commitment.
	ev.Props.SetText(ical.PropStatus, "TENTATIVE")
	cal.Children = append(cal.Children, ev.Component)
	_, err = client.PutCalendarObject(ctx, planObjectPath(planPath, b.uid), cal)
	return err
}

// deleteObject removes a plan event by uid via a WebDAV DELETE. The caldav
// client has no delete, so we use a plain webdav client.
func (s *Service) deleteObject(ctx context.Context, url, planPath, user, pass, uid string) error {
	hc := webdav.HTTPClientWithBasicAuth(&http.Client{Timeout: 15 * time.Second}, user, pass)
	wc, err := webdav.NewClient(hc, url)
	if err != nil {
		return err
	}
	return wc.RemoveAll(ctx, planObjectPath(planPath, uid))
}

func planObjectPath(planPath, uid string) string {
	return strings.TrimRight(planPath, "/") + "/" + uid + ".ics"
}

// listPlanObjectUIDs enumerates the plan collection and returns the uid of every
// object currently in it. Used once after (re)start to seed the reconcile map so
// orphaned objects from a previous process can be reclaimed. The uid is derived
// from the object path (planObjectPath writes "<planPath>/<uid>.ics"), so it
// round-trips through deleteObject without parsing the calendar body.
func (s *Service) listPlanObjectUIDs(ctx context.Context) ([]string, error) {
	s.mu.RLock()
	url, planPath, user, pass := s.url, s.planPath, s.username, s.password
	s.mu.RUnlock()

	client, err := s.newClient(url, user, pass)
	if err != nil {
		return nil, err
	}
	// Wide time-range filter: plan objects are near-term, but a stale one may sit
	// in the past, so cover a year either side to catch every orphan.
	now := time.Now()
	query := &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{
			Name:  "VCALENDAR",
			Comps: []caldav.CalendarCompRequest{{Name: "VEVENT"}},
		},
		CompFilter: caldav.CompFilter{
			Name: "VCALENDAR",
			Comps: []caldav.CompFilter{{
				Name:  "VEVENT",
				Start: now.Add(-365 * 24 * time.Hour),
				End:   now.Add(365 * 24 * time.Hour),
			}},
		},
	}
	objs, err := client.QueryCalendar(ctx, planPath, query)
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(objs))
	for _, obj := range objs {
		if uid := strings.TrimSuffix(path.Base(obj.Path), ".ics"); uid != "" && uid != "." && uid != "/" {
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

// lanNote is the footer appended to every published event's description so a
// subscriber understands the refresh behaviour: forty-two-watts' CalDAV server
// is never published to the internet, so a calendar app can only pull updates
// while it can reach forty-two-watts — on the home network or over a VPN into
// it — plus when forty-two-watts actually changes the feed. `refresh` is the
// trailing clause, e.g. "re-checks the plan about every 15 min, and updates an
// event only when the plan actually changes".
func lanNote(refresh string) string {
	return "\n\nThis calendar lives on your home network and is never " +
		"published to the internet. Your calendar app can refresh it only " +
		"while it can reach forty-two-watts — on your home network or over a " +
		"VPN into it — otherwise events stay as last synced. " +
		"forty-two-watts " + refresh + "."
}

// friendlyInterval renders a poll/publish interval in a human-readable unit.
func friendlyInterval(d time.Duration) string {
	switch {
	case d >= time.Hour:
		if h := d.Hours(); h == math.Trunc(h) {
			return fmt.Sprintf("%.0f h", h)
		}
		return fmt.Sprintf("%.1f h", d.Hours())
	case d >= time.Minute:
		return fmt.Sprintf("%.0f min", d.Minutes())
	default:
		return fmt.Sprintf("%.0f s", d.Seconds())
	}
}
