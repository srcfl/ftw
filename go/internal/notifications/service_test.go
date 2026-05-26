package notifications

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/events"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// ---- test helpers ----

type fakePub struct {
	mu    sync.Mutex
	msgs  []Message
	errOn int // fail after this many (0 means never)
	count int
	fail  bool
}

func (f *fakePub) Publish(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	if f.fail {
		return fmt.Errorf("boom")
	}
	f.msgs = append(f.msgs, m)
	return nil
}

func (f *fakePub) Messages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Message, len(f.msgs))
	copy(out, f.msgs)
	return out
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newSvc(cfg *config.Notifications, pub Publisher) (*Service, *clock) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	s := New(cfg, pub, nil)
	s.now = clk.now
	return s, clk
}

func healthOk(lastSuccess time.Time) map[string]telemetry.DriverHealth {
	return map[string]telemetry.DriverHealth{
		"ferroamp": {Name: "ferroamp", LastSuccess: &lastSuccess, TickCount: 10},
	}
}

func healthStale(lastSuccess time.Time) map[string]telemetry.DriverHealth {
	return map[string]telemetry.DriverHealth{
		"ferroamp": {Name: "ferroamp", LastSuccess: &lastSuccess, TickCount: 10, Status: telemetry.StatusOffline},
	}
}

func baseCfg() *config.Notifications {
	return &config.Notifications{
		Enabled:         true,
		Provider:        "ntfy",
		DefaultPriority: 3,
		Ntfy:            &config.NtfyConfig{Server: "https://example", Topic: "test"},
		Events: []config.NotificationRule{
			{Type: EventDriverOffline, Enabled: true, ThresholdS: 600, Priority: 4, CooldownS: 3600},
			{Type: EventDriverRecovered, Enabled: true, Priority: 3},
		},
	}
}

// ---- tests ----

func TestOfflineFiresAfterThreshold(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	last := clk.now()
	// Below threshold — no fire.
	clk.advance(400 * time.Second)
	svc.Observe(healthOk(last))
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("below threshold: got %d msgs", n)
	}
	// Cross threshold — one fire.
	clk.advance(400 * time.Second)
	svc.Observe(healthStale(last))
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("above threshold: got %d msgs", n)
	}
}

func TestOfflineFiresOncePerOutage(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	last := clk.now()
	clk.advance(800 * time.Second)
	for i := 0; i < 20; i++ {
		svc.Observe(healthStale(last))
		clk.advance(5 * time.Second)
	}
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("expected 1 fire per outage, got %d", n)
	}
}

func TestCooldownBlocksBackToBack(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	last := clk.now()
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last)) // fires
	// Recover
	last2 := clk.now()
	clk.advance(5 * time.Second)
	svc.Observe(healthOk(last2)) // recovered fires
	// New outage inside cooldown
	clk.advance(100 * time.Second)
	svc.Observe(healthStale(last2)) // threshold not yet met
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last2)) // threshold met but cooldown blocks
	msgs := pub.Messages()
	// Expect: offline-1 + recovered-1, no second offline due to cooldown.
	offlineCount := 0
	for _, m := range msgs {
		if strings.Contains(m.Title, "offline") {
			offlineCount++
		}
	}
	if offlineCount != 1 {
		t.Fatalf("expected 1 offline fire, got %d (%+v)", offlineCount, msgs)
	}
}

func TestDisabledServiceNoops(t *testing.T) {
	cfg := baseCfg()
	cfg.Enabled = false
	pub := &fakePub{}
	svc, clk := newSvc(cfg, pub)
	last := clk.now()
	clk.advance(1000 * time.Second)
	svc.Observe(healthStale(last))
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("disabled should not publish, got %d", n)
	}
	if err := svc.SendTest(); err == nil {
		t.Fatalf("SendTest should error when disabled")
	}
}

func TestSendTestProducesOneTestMsg(t *testing.T) {
	pub := &fakePub{}
	svc, _ := newSvc(baseCfg(), pub)
	if err := svc.SendTest(); err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d msgs", len(msgs))
	}
	if !strings.Contains(strings.ToLower(msgs[0].Title), "test") {
		t.Fatalf("title missing 'test': %q", msgs[0].Title)
	}
}

func TestTemplateInterpolation(t *testing.T) {
	cfg := baseCfg()
	cfg.Events[0].TitleTemplate = "offline: {{.Device}}"
	cfg.Events[0].BodyTemplate = "dur: {{.Duration}} ({{.DurationS}}s)"
	pub := &fakePub{}
	svc, clk := newSvc(cfg, pub)
	last := clk.now()
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last))
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d", len(msgs))
	}
	if msgs[0].Title != "offline: ferroamp" {
		t.Fatalf("title: %q", msgs[0].Title)
	}
	if !strings.Contains(msgs[0].Body, "11m") || !strings.Contains(msgs[0].Body, "700s") {
		t.Fatalf("body: %q", msgs[0].Body)
	}
}

func TestDefaultTemplatesWhenBlank(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	last := clk.now()
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last))
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Title, "ferroamp offline") {
		t.Fatalf("default title missing: %q", msgs[0].Title)
	}
	if !strings.Contains(msgs[0].Body, "has not reported telemetry") {
		t.Fatalf("default body missing: %q", msgs[0].Body)
	}
}

func TestNilServiceSafe(t *testing.T) {
	var s *Service
	s.Observe(nil)
	s.Reload(nil)
	s.Subscribe(nil)
	_ = s.Enabled()
	_ = s.Status()
	if err := s.SendTest(); err == nil {
		t.Error("nil SendTest must error")
	}
}

func TestRecoveredOnlyAfterOfflineFired(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	// Healthy → healthy: zero msgs.
	last := clk.now()
	for i := 0; i < 5; i++ {
		last = clk.now()
		svc.Observe(healthOk(last))
		clk.advance(5 * time.Second)
	}
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("healthy→healthy: got %d", n)
	}
	// healthy → offline → healthy: expect 2 msgs.
	lockedLast := last
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(lockedLast)) // offline
	newLast := clk.now()
	clk.advance(5 * time.Second)
	svc.Observe(healthOk(newLast)) // recovered
	if n := len(pub.Messages()); n != 2 {
		t.Fatalf("lifecycle: got %d msgs (%+v)", n, pub.Messages())
	}
}

func TestDisabledRuleSkipped(t *testing.T) {
	cfg := baseCfg()
	cfg.Events[0].Enabled = false
	pub := &fakePub{}
	svc, clk := newSvc(cfg, pub)
	last := clk.now()
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last))
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("disabled rule: got %d", n)
	}
}

func TestPublisherErrorBumpsFailed(t *testing.T) {
	pub := &fakePub{fail: true}
	svc, clk := newSvc(baseCfg(), pub)
	last := clk.now()
	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last))
	st := svc.Status()
	if st.Failed != 1 {
		t.Fatalf("expected Failed=1, got %+v", st)
	}
}

// ---- ntfy provider ----

func TestNtfyHeadersAndBearer(t *testing.T) {
	var got *http.Request
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := newNtfyProvider(&config.Notifications{
		Provider: "ntfy",
		Ntfy:     &config.NtfyConfig{Server: srv.URL, Topic: "my topic", AccessToken: "tk"},
	})
	err := p.Publish(context.Background(), Message{Title: "T", Body: "B", Priority: 4, Tags: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestURI != "/my%20topic" {
		t.Errorf("request URI: %q", got.RequestURI)
	}
	if got.Header.Get("Title") != "T" {
		t.Errorf("Title header: %q", got.Header.Get("Title"))
	}
	if got.Header.Get("Priority") != "4" {
		t.Errorf("Priority header: %q", got.Header.Get("Priority"))
	}
	if got.Header.Get("Tags") != "a,b" {
		t.Errorf("Tags header: %q", got.Header.Get("Tags"))
	}
	if got.Header.Get("Authorization") != "Bearer tk" {
		t.Errorf("Authorization: %q", got.Header.Get("Authorization"))
	}
	if gotBody != "B" {
		t.Errorf("body: %q", gotBody)
	}
}

func TestNtfyBasicAuthFallback(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.WriteHeader(200)
	}))
	defer srv.Close()
	p := newNtfyProvider(&config.Notifications{
		Provider: "ntfy",
		Ntfy:     &config.NtfyConfig{Server: srv.URL, Topic: "t", Username: "u", Password: "p"},
	})
	if err := p.Publish(context.Background(), Message{Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got.Header.Get("Authorization"), "Basic ") {
		t.Errorf("basic auth: %q", got.Header.Get("Authorization"))
	}
}

func TestNtfyNon2xxSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("topic reserved"))
	}))
	defer srv.Close()
	p := newNtfyProvider(&config.Notifications{
		Provider: "ntfy",
		Ntfy:     &config.NtfyConfig{Server: srv.URL, Topic: "t"},
	})
	err := p.Publish(context.Background(), Message{Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "topic reserved") {
		t.Fatalf("expected 403+body in error, got %v", err)
	}
}

func TestNtfyErrorsWhenUnset(t *testing.T) {
	p := newNtfyProvider(&config.Notifications{Provider: "ntfy", Ntfy: &config.NtfyConfig{}})
	if err := p.Publish(context.Background(), Message{Body: "x"}); err == nil {
		t.Error("expected error for empty server/topic")
	}
	p = newNtfyProvider(&config.Notifications{Provider: "ntfy", Ntfy: &config.NtfyConfig{Server: "http://x", Topic: ""}})
	if err := p.Publish(context.Background(), Message{Body: "x"}); err == nil {
		t.Error("expected error for empty topic")
	}
}

func TestHumanDurationBoundaries(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{time.Hour + 30*time.Minute, "1h30m"},
		{3 * time.Hour, "3h"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%v) = %q; want %q", c.d, got, c.want)
		}
	}
}

func TestStrategyRegistry(t *testing.T) {
	p := NewProvider(&config.Notifications{Provider: "ntfy", Ntfy: &config.NtfyConfig{Server: "http://x", Topic: "t"}})
	if p == nil || p.Name() != "ntfy" {
		t.Fatalf("ntfy not registered; got %+v", p)
	}
	if NewProvider(&config.Notifications{Provider: "nonesuch"}) != nil {
		t.Fatal("unknown provider must return nil")
	}
}

func TestEventBusSubscribe(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(baseCfg(), pub)
	bus := events.NewBus()
	svc.Subscribe(bus)
	last := clk.now()
	clk.advance(700 * time.Second)
	bus.Publish(events.HealthTick{Health: healthStale(last), Now: clk.now()})
	// HealthTick handler runs observeAt in a goroutine so it can't stall
	// the control loop on HTTP publish — poll for the dispatch.
	deadline := time.Now().Add(time.Second)
	for len(pub.Messages()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("bus did not wire tick: got %d msgs", n)
	}
	// Test event round-trips via Reply chan.
	reply := make(chan error, 1)
	bus.Publish(events.NotificationTest{Reply: reply})
	select {
	case err := <-reply:
		if err != nil {
			t.Fatalf("test publish err: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("no reply from test event")
	}
}

func TestActiveAlertClearsWhenRecoveredDisabled(t *testing.T) {
	cfg := baseCfg()
	// Disable the recovered rule — offline still fires, but nothing
	// should leak activeAlert once the driver comes back.
	cfg.Events[1].Enabled = false
	pub := &fakePub{}
	svc, clk := newSvc(cfg, pub)
	last := clk.now()

	clk.advance(700 * time.Second)
	svc.Observe(healthStale(last))
	if st := svc.Status(); st.ActiveAlert != 1 {
		t.Fatalf("after outage: got ActiveAlert=%d, want 1", st.ActiveAlert)
	}

	// Driver comes back; with recovered disabled the post-pass cleanup
	// must drop activeAlert so Status().ActiveAlert returns to 0.
	recover := clk.now()
	clk.advance(5 * time.Second)
	svc.Observe(healthOk(recover))
	if st := svc.Status(); st.ActiveAlert != 0 {
		t.Fatalf("after recovery: got ActiveAlert=%d, want 0", st.ActiveAlert)
	}
}

func TestNilPublisherNoPanic(t *testing.T) {
	// Publisher starts nil (simulates cold-start where NewProvider
	// returned nil) but config is enabled — dispatch must not panic,
	// it should treat this as a failed send and log it to history.
	svc, clk := newSvc(baseCfg(), nil)
	last := clk.now()
	clk.advance(700 * time.Second)
	// Ensure no panic.
	svc.Observe(healthStale(last))
	if st := svc.Status(); st.Failed != 1 {
		t.Fatalf("expected Failed=1 after nil-publisher dispatch, got %+v", st)
	}
}

func TestUpdateAvailableDispatches(t *testing.T) {
	cfg := baseCfg()
	// Add an enabled update_available rule.
	cfg.Events = append(cfg.Events, config.NotificationRule{
		Type: EventUpdateAvailable, Enabled: true, Priority: 3, CooldownS: 3600,
	})
	pub := &fakePub{}
	svc, _ := newSvc(cfg, pub)
	bus := events.NewBus()
	svc.Subscribe(bus)

	bus.Publish(events.UpdateAvailable{
		Version:         "v1.2.3",
		PreviousVersion: "v1.2.2",
		ReleaseNotesURL: "https://example/r/v1.2.3",
		At:              time.Now(),
	})
	deadline := time.Now().Add(time.Second)
	for len(pub.Messages()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 msg, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Title, "v1.2.3") {
		t.Errorf("title missing version: %q", msgs[0].Title)
	}
	if !strings.Contains(msgs[0].Body, "v1.2.2") {
		t.Errorf("body missing previous version: %q", msgs[0].Body)
	}

	// Second emission with same version should be blocked by cooldown.
	bus.Publish(events.UpdateAvailable{Version: "v1.2.3", At: time.Now()})
	time.Sleep(50 * time.Millisecond)
	if len(pub.Messages()) != 1 {
		t.Fatalf("cooldown did not dedupe: got %d msgs", len(pub.Messages()))
	}
}

func TestFuseOverLimitFiresAfterThresholdAndResets(t *testing.T) {
	cfg := baseCfg()
	cfg.Events = append(cfg.Events, config.NotificationRule{
		Type: EventFuseOverLimit, Enabled: true, ThresholdS: 30, Priority: 5, CooldownS: 900,
	})
	pub := &fakePub{}
	svc, clk := newSvc(cfg, pub)
	bus := events.NewBus()
	svc.Subscribe(bus)

	// Reader starts with L1 at 20 A, limit 16 A.
	var l1 float64 = 20.0
	svc.SetFuseReader(func() (map[string]float64, float64, bool) {
		return map[string]float64{"L1": l1, "L2": 10, "L3": 11}, 16.0, true
	})

	// First tick: over the limit, but threshold hasn't sustained.
	bus.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{}, Now: clk.now()})
	time.Sleep(20 * time.Millisecond)
	if n := len(pub.Messages()); n != 0 {
		t.Fatalf("before threshold: got %d msgs", n)
	}

	// Advance past threshold — should fire once.
	clk.advance(40 * time.Second)
	bus.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{}, Now: clk.now()})
	deadline := time.Now().Add(time.Second)
	for len(pub.Messages()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("after threshold: got %d msgs", len(msgs))
	}
	if !strings.Contains(msgs[0].Title, "L1") {
		t.Errorf("title missing phase: %q", msgs[0].Title)
	}
	if !strings.Contains(msgs[0].Body, "20.0") || !strings.Contains(msgs[0].Body, "16") {
		t.Errorf("body missing amps/limit: %q", msgs[0].Body)
	}

	// Still over — latch prevents a second fire in the same outage.
	clk.advance(5 * time.Second)
	bus.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{}, Now: clk.now()})
	time.Sleep(20 * time.Millisecond)
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("still-over: expected 1, got %d", n)
	}

	// Back under: the window + latch reset.
	l1 = 12
	bus.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{}, Now: clk.now()})
	time.Sleep(20 * time.Millisecond)

	// New outage — cooldown still blocks a quick refire.
	l1 = 20
	clk.advance(40 * time.Second)
	bus.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{}, Now: clk.now()})
	clk.advance(60 * time.Second)
	bus.Publish(events.HealthTick{Health: map[string]telemetry.DriverHealth{}, Now: clk.now()})
	time.Sleep(20 * time.Millisecond)
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("cooldown did not block refire: got %d msgs", n)
	}
}

// ---- concurrent_drivers_offline ----

func concurrentCfg() *config.Notifications {
	return &config.Notifications{
		Enabled:         true,
		Provider:        "ntfy",
		DefaultPriority: 3,
		Ntfy:            &config.NtfyConfig{Server: "https://example", Topic: "test"},
		Events: []config.NotificationRule{
			{Type: EventConcurrentDriversOffline, Enabled: true,
				ThresholdS: 300, ThresholdN: 2,
				Priority: 5, CooldownS: 1800},
		},
	}
}

func TestConcurrentOffline_FiresOnFleetOutage(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(concurrentCfg(), pub)
	// Both drivers were healthy 10 minutes ago; rule threshold is 5
	// minutes. Now, both have gone stale at the same time — the
	// fuse-blow signature.
	last := clk.now().Add(-10 * time.Minute)
	health := map[string]telemetry.DriverHealth{
		"ferroamp": {Name: "ferroamp", LastSuccess: &last, TickCount: 100,
			Status: telemetry.StatusOffline},
		"sungrow": {Name: "sungrow", LastSuccess: &last, TickCount: 100,
			Status: telemetry.StatusOffline},
		"easee": {Name: "easee", LastSuccess: addr(clk.now()), TickCount: 100,
			Status: telemetry.StatusOk},
	}
	svc.Observe(health)
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("want 1 fire, got %d", len(msgs))
	}
	body := msgs[0].Body
	if !strings.Contains(body, "ferroamp") || !strings.Contains(body, "sungrow") {
		t.Errorf("body should list both stale drivers; got %q", body)
	}
	if strings.Contains(body, "easee") {
		t.Errorf("healthy driver should NOT appear in body; got %q", body)
	}
}

func TestConcurrentOffline_DoesNotFireForSingleStale(t *testing.T) {
	// Single stale driver below ThresholdN — driver_offline's job,
	// not concurrent's.
	pub := &fakePub{}
	svc, clk := newSvc(concurrentCfg(), pub)
	last := clk.now().Add(-10 * time.Minute)
	health := map[string]telemetry.DriverHealth{
		"ferroamp": {Name: "ferroamp", LastSuccess: &last, TickCount: 100,
			Status: telemetry.StatusOffline},
		"sungrow": {Name: "sungrow", LastSuccess: addr(clk.now()), TickCount: 100,
			Status: telemetry.StatusOk},
	}
	svc.Observe(health)
	if n := len(pub.Messages()); n != 0 {
		t.Errorf("single stale should not fire concurrent rule; got %d msgs", n)
	}
}

func TestConcurrentOffline_FiresOnceUntilRecovery(t *testing.T) {
	pub := &fakePub{}
	svc, clk := newSvc(concurrentCfg(), pub)
	last := clk.now().Add(-10 * time.Minute)
	stale := map[string]telemetry.DriverHealth{
		"ferroamp": {Name: "ferroamp", LastSuccess: &last, TickCount: 100,
			Status: telemetry.StatusOffline},
		"sungrow": {Name: "sungrow", LastSuccess: &last, TickCount: 100,
			Status: telemetry.StatusOffline},
	}
	// Fire once.
	svc.Observe(stale)
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("first observe: want 1, got %d", n)
	}
	// Re-observing while still stale must NOT refire (the latch).
	for i := 0; i < 5; i++ {
		clk.advance(60 * time.Second)
		svc.Observe(stale)
	}
	if n := len(pub.Messages()); n != 1 {
		t.Fatalf("repeat-stale: want still 1, got %d", n)
	}
	// Recovery — both back to healthy. Latch clears.
	now := clk.now()
	healthy := map[string]telemetry.DriverHealth{
		"ferroamp": {Name: "ferroamp", LastSuccess: &now, TickCount: 200,
			Status: telemetry.StatusOk},
		"sungrow": {Name: "sungrow", LastSuccess: &now, TickCount: 200,
			Status: telemetry.StatusOk},
	}
	svc.Observe(healthy)
	// New outage triggers a NEW fire (after cooldown).
	clk.advance(40 * time.Minute) // > CooldownS (1800)
	last2 := clk.now().Add(-10 * time.Minute)
	stale2 := map[string]telemetry.DriverHealth{
		"ferroamp": {Name: "ferroamp", LastSuccess: &last2, TickCount: 300,
			Status: telemetry.StatusOffline},
		"sungrow": {Name: "sungrow", LastSuccess: &last2, TickCount: 300,
			Status: telemetry.StatusOffline},
	}
	svc.Observe(stale2)
	if n := len(pub.Messages()); n != 2 {
		t.Fatalf("after recovery + cooldown: want 2 fires, got %d", n)
	}
}

func TestConcurrentOffline_IgnoresColdStartDrivers(t *testing.T) {
	// A driver that never emitted (cold start) shouldn't pull the
	// fleet into a concurrent-offline alert. Only previously-healthy-
	// now-silent drivers count, mirroring driver_offline's exception.
	pub := &fakePub{}
	svc, _ := newSvc(concurrentCfg(), pub)
	health := map[string]telemetry.DriverHealth{
		"ferroamp": {Name: "ferroamp", LastSuccess: nil, TickCount: 0}, // cold start
		"sungrow":  {Name: "sungrow", LastSuccess: nil, TickCount: 0},  // cold start
	}
	svc.Observe(health)
	if n := len(pub.Messages()); n != 0 {
		t.Errorf("cold-start drivers should be excluded; got %d msgs", n)
	}
}

func addr(t time.Time) *time.Time { return &t }
