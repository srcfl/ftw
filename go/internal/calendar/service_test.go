package calendar

import (
	"context"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadmodel"
)

type fakeLP struct {
	id     string
	soc    float64
	when   time.Time
	called int
}

func (f *fakeLP) SetTarget(id string, soc float64, t time.Time) bool {
	f.id, f.soc, f.when = id, soc, t
	f.called++
	return true
}

type fakeLM struct {
	profile loadmodel.Profile
	calls   int
}

func (f *fakeLM) SetProfile(p loadmodel.Profile) error {
	f.profile = p
	f.calls++
	return nil
}

func newTestService(lp LoadpointTargeter, lm LoadProfiler) *Service {
	return New(config.CalDAV{Enabled: true}, lp, lm, "garage")
}

func TestApplyAwayProfileTransitions(t *testing.T) {
	lm := &fakeLM{}
	s := newTestService(&fakeLP{}, lm)
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	away := Intents{Away: []Interval{{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}}}

	s.apply(away, now)
	if lm.profile != loadmodel.ProfileAway || lm.calls != 1 {
		t.Fatalf("expected away profile after entering window: profile=%v calls=%d", lm.profile, lm.calls)
	}

	// Same state again → idempotent, no extra SetProfile call.
	s.apply(away, now)
	if lm.calls != 1 {
		t.Fatalf("idempotent away should not re-call SetProfile: calls=%d", lm.calls)
	}

	// Leaving the window restores home.
	s.apply(Intents{}, now)
	if lm.profile != loadmodel.ProfileHome || lm.calls != 2 {
		t.Fatalf("expected home profile after leaving window: profile=%v calls=%d", lm.profile, lm.calls)
	}
}

func TestPollFailureExpiresCachedAwayIntent(t *testing.T) {
	lm := &fakeLM{}
	s := newTestService(&fakeLP{}, lm)
	now := time.Now()
	cached := Intents{Away: []Interval{{Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour)}}}

	// Simulate the earlier successful application while the event was active.
	s.apply(cached, now.Add(-90*time.Minute))
	s.mu.Lock()
	s.intents = cached
	s.hasIntents = true
	s.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // force the refresh to fail immediately
	s.pollOnce(ctx)

	if lm.profile != loadmodel.ProfileHome || lm.calls != 2 {
		t.Fatalf("failed refresh must expire known away window: profile=%v calls=%d", lm.profile, lm.calls)
	}
}

func TestFirstPollFailureDoesNotOverrideManualProfile(t *testing.T) {
	lm := &fakeLM{profile: loadmodel.ProfileAway}
	s := newTestService(&fakeLP{}, lm)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.pollOnce(ctx)

	if lm.calls != 0 || lm.profile != loadmodel.ProfileAway {
		t.Fatalf("first failed poll must leave manual profile untouched: profile=%v calls=%d", lm.profile, lm.calls)
	}
}

func TestApplyEVTargetSet(t *testing.T) {
	lp := &fakeLP{}
	s := newTestService(lp, &fakeLM{})
	now := time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC)
	dep := now.Add(2 * time.Hour)

	s.apply(Intents{EV: []EVDeadline{{LoadpointID: "garage", TargetSoCPct: 80, Departure: dep}}}, now)

	if lp.called != 1 {
		t.Fatalf("expected SetTarget once, got %d", lp.called)
	}
	if lp.id != "garage" || lp.soc != 80 || !lp.when.Equal(dep) {
		t.Fatalf("SetTarget args wrong: id=%q soc=%v when=%v", lp.id, lp.soc, lp.when)
	}
}

func TestApplyEVPicksEarliestUpcoming(t *testing.T) {
	lp := &fakeLP{}
	s := newTestService(lp, &fakeLM{})
	now := time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC)

	s.apply(Intents{EV: []EVDeadline{
		{LoadpointID: "garage", TargetSoCPct: 70, Departure: now.Add(-time.Hour)}, // past — ignored
		{LoadpointID: "garage", TargetSoCPct: 90, Departure: now.Add(5 * time.Hour)},
		{LoadpointID: "garage", TargetSoCPct: 60, Departure: now.Add(1 * time.Hour)}, // earliest upcoming
	}}, now)

	if lp.soc != 60 {
		t.Fatalf("expected earliest upcoming deadline (soc 60), got %v", lp.soc)
	}
}

func TestApplyEVMissingLoadpointIgnored(t *testing.T) {
	lp := &fakeLP{}
	// firstLoadpointID empty → no default → EV event with no id is unactionable.
	s := New(config.CalDAV{Enabled: true}, lp, &fakeLM{}, "")
	now := time.Now()
	s.apply(Intents{EV: []EVDeadline{{TargetSoCPct: 80, Departure: now.Add(time.Hour)}}}, now)
	if lp.called != 0 {
		t.Fatalf("EV event without a loadpoint must not call SetTarget")
	}
}

func TestIsAwayAt(t *testing.T) {
	s := newTestService(&fakeLP{}, &fakeLM{})
	now := time.Now()
	s.intents = Intents{Away: []Interval{{Start: now.Add(-time.Hour), End: now.Add(time.Hour)}}}

	if !s.IsAwayAt(now) {
		t.Fatal("now should be inside the away window")
	}
	if s.IsAwayAt(now.Add(2 * time.Hour)) {
		t.Fatal("time outside the window should not be away")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	s := New(config.CalDAV{Enabled: true}, &fakeLP{}, &fakeLM{}, "garage")
	st := s.Status()
	if !st.Enabled {
		t.Fatal("should be enabled")
	}
	// SubscribeURL should join the default URL + default calendar path.
	want := joinURL(config.DefaultCalDAVURL, config.DefaultCalDAVCalendarPath)
	if st.SubscribeURL != want {
		t.Fatalf("subscribe URL: want %q, got %q", want, st.SubscribeURL)
	}
}

func TestStatusNilSafe(t *testing.T) {
	var s *Service
	if st := s.Status(); st.Enabled {
		t.Fatal("nil service status should be zero value")
	}
	if s.IsAwayAt(time.Now()) {
		t.Fatal("nil service is never away")
	}
}
