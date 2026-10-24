package fleetping

import (
	"bytes"
	"context"

	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/statsrelay/statsrelaytest"
	"github.com/srcfl/ftw/go/protocol/fleetwire"
)

// The whole point of this package is what it does NOT send. These tests are
// written against the marshalled JSON rather than the struct, because the
// struct is not what leaves the house.

func facts() Facts {
	return Facts{
		Version:     "v1.4.0-beta.2",
		Drivers:     []string{"sungrow", "easee_cloud"},
		Catalogue:   []string{"sungrow", "easee_cloud", "ferroamp"},
		BatteryWh:   12_400,
		PriceZone:   "SE3",
		InstalledAt: time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC),
	}
}

func marshal(t *testing.T, p Payload) map[string]any {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got
}

func TestPayloadCarriesOnlyTheAgreedFields(t *testing.T) {
	// Field by field, and the count too: a new field added without thinking
	// about what it says about a household fails here before it ships.
	got := marshal(t, Build(facts(), time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)))

	want := map[string]any{
		"schema":         "ftw.fleet/2",
		"ftw_version":    "v1.4.0-beta.2",
		"channel":        "beta",
		"battery_kwh":    "5-15",
		"price_zone":     "SE3",
		"install_age":    "6-12m",
		"plan_solve":     "none",
		"plan_fallbacks": "none",
		"driver_errors":  "none",
		"update":         "none",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %v, want %v", key, got[key], value)
		}
	}
	drivers, ok := got["drivers"].([]any)
	if !ok || len(drivers) != 2 {
		t.Fatalf("drivers = %v, want two entries", got["drivers"])
	}
	if len(got) != len(want)+1 {
		t.Fatalf("payload has %d fields, want %d — a new field needs a privacy argument, not a struct tag",
			len(got), len(want)+1)
	}
}

func TestPayloadCarriesNothingThatIdentifiesAHousehold(t *testing.T) {
	// The values below are the ones a box actually holds and could leak by
	// accident: the operator's driver names, the lua paths, a serial, a
	// gateway id, a key. None of them may appear anywhere in the message.
	f := facts()
	f.Drivers = []string{"sungrow", "sungrow-vasagatan-12", "/home/fredrik/drivers/pappas-laddbox.lua"}

	raw, err := json.Marshal(Build(f, time.Now()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	for _, forbidden := range []string{
		"vasagatan", "fredrik", "pappas", "/home/", ".lua",
	} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Errorf("payload contains %q: %s", forbidden, body)
		}
	}
}

func TestUnknownDriverNamesCollapseToOther(t *testing.T) {
	// A hand-written local driver can be called anything, including the
	// household's own name. Only names the shipped catalogue knows travel.
	got := driverTypes(
		[]string{"sungrow", "fredriks_hus", "grannens_pump"},
		[]string{"sungrow", "easee_cloud"},
	)
	want := []string{"other", "sungrow"}
	if len(got) != len(want) {
		t.Fatalf("driverTypes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("driverTypes = %v, want %v", got, want)
		}
	}
}

func TestDriverListIsSortedDedupedAndCapped(t *testing.T) {
	// Order is a detail of the config file, a repeat is a count of inverters,
	// and an unusually long list is a signature. None of the three is an
	// answer to "which driver types are in use".
	catalogue := []string{"sungrow", "easee_cloud", "ferroamp"}
	got := driverTypes([]string{"sungrow", "ferroamp", "sungrow", "easee_cloud"}, catalogue)
	want := []string{"easee_cloud", "ferroamp", "sungrow"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("driverTypes = %v, want %v", got, want)
		}
	}

	many := make([]string, 0, 40)
	big := make([]string, 0, 40)
	for i := range 40 {
		name := string(rune('a'+i%26)) + string(rune('a'+i/26)) + "driver"
		many = append(many, name)
		big = append(big, name)
	}
	if n := len(driverTypes(many, big)); n != maxDrivers {
		t.Fatalf("driver list capped at %d, want %d", n, maxDrivers)
	}
}

func TestBucketsAreCoarse(t *testing.T) {
	// Two boxes a kilowatt-hour apart, or a week apart in age, must land in
	// the same bucket. Fine resolution here is what would make the rest of
	// the payload a lookup key.
	if a, b := batteryBucket(12_400), batteryBucket(13_600); a != b {
		t.Errorf("12.4 kWh -> %q and 13.6 kWh -> %q; buckets are too fine", a, b)
	}
	now := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	if a, b := ageBucket(now.AddDate(0, 0, -100), now), ageBucket(now.AddDate(0, 0, -107), now); a != b {
		t.Errorf("100 days -> %q and 107 days -> %q; buckets are too fine", a, b)
	}

	for _, tc := range []struct {
		wh   float64
		want string
	}{
		{0, "none"}, {-1, "none"}, {4_900, "0-5"}, {5_000, "5-15"},
		{14_999, "5-15"}, {15_000, "15-30"}, {29_999, "15-30"}, {30_000, "30+"},
	} {
		if got := batteryBucket(tc.wh); got != tc.want {
			t.Errorf("batteryBucket(%v) = %q, want %q", tc.wh, got, tc.want)
		}
	}

	for _, tc := range []struct {
		days int
		want string
	}{
		{0, "0-1m"}, {29, "0-1m"}, {30, "1-6m"}, {179, "1-6m"},
		{180, "6-12m"}, {364, "6-12m"}, {365, "1y+"}, {5_000, "1y+"},
	} {
		if got := ageBucket(now.AddDate(0, 0, -tc.days), now); got != tc.want {
			t.Errorf("ageBucket(%d days) = %q, want %q", tc.days, got, tc.want)
		}
	}

	if got := ageBucket(time.Time{}, now); got != "unknown" {
		t.Errorf("unknown install date -> %q, want unknown", got)
	}
	if got := ageBucket(now.AddDate(0, 0, 3), now); got != "unknown" {
		t.Errorf("install date in the future -> %q, want unknown", got)
	}
}

func TestOnlyReleaseTagsAndKnownZonesTravel(t *testing.T) {
	// A one-off build string names the person who made it, and a hand-edited
	// price zone is free text. Both are replaced rather than passed on.
	for _, tc := range []struct{ in, version, channel string }{
		{"v1.4.0", "v1.4.0", "stable"},
		{"v1.4.0-beta.2", "v1.4.0-beta.2", "beta"},
		// The two channels this repo publishes are the two above. Everything
		// else is one build, on one machine, and travels as unknown.
		{"v1.4.0-rc.1", "unknown", "unknown"},
		{"v0.0.0-dev", "unknown", "unknown"},
		// The default build path: the Makefile stamps
		// `git describe --tags --always --dirty`, so any tagged tree with an
		// uncommitted file produces this. It is the shape of a release and is
		// not one.
		{"v1.4.0-dirty", "unknown", "unknown"},
		{"v1.4.0-fredrik", "unknown", "unknown"},
		{"dev", "unknown", "unknown"},
		// Rejected on the prerelease pattern, like the two above it. It used
		// to be rejected only because the old character class happened to
		// exclude the hyphen, which caught this and let "v1.4.0-fredrik"
		// through — so it proves nothing on its own.
		{"v1.4.0-fredriks-laptop-2026-08-05", "unknown", "unknown"},
		{"", "unknown", "unknown"},
	} {
		version, channel := releaseVersion(tc.in)
		if version != tc.version || channel != tc.channel {
			t.Errorf("releaseVersion(%q) = %q/%q, want %q/%q", tc.in, version, channel, tc.version, tc.channel)
		}
	}

	if got := knownZone("SE3"); got != "SE3" {
		t.Errorf("knownZone(SE3) = %q", got)
	}
	if got := knownZone("Fredriks hus"); got != "unknown" {
		t.Errorf("knownZone of free text = %q, want unknown", got)
	}
	if got := knownZone(""); got != "unknown" {
		t.Errorf("knownZone of an unset zone = %q, want unknown", got)
	}
}

// ---- schedule ----

type recorder struct {
	mu   sync.Mutex
	sent []Payload
	err  error
}

func (r *recorder) Send(_ context.Context, p Payload) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, p)
	return r.err
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

func newPinger(t *testing.T, opts Options) *Pinger {
	t.Helper()
	if opts.Facts == nil {
		opts.Facts = facts
	}
	p, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestScheduleJitters(t *testing.T) {
	// A box that pings at 03:17 every night has named itself in a field
	// nobody reviewed. Two consecutive draws from the same pinger must differ,
	// the first one must be able to land anywhere in the day, and no draw may
	// leave the [18h, 30h] band once the box is running.
	var draws []float64
	next := func() float64 {
		v := draws[0]
		draws = draws[1:]
		return v
	}

	draws = []float64{0.0, 0.0, 1.0, 0.5}
	p := newPinger(t, Options{Provider: &recorder{}, Random: next})

	if first := p.delay(); first != 0 {
		t.Errorf("first delay = %v, want the full range to start at zero", first)
	}
	if got := p.delay(); got != period-spread {
		t.Errorf("delay at random 0 = %v, want %v", got, period-spread)
	}
	if got := p.delay(); got != period+spread {
		t.Errorf("delay at random 1 = %v, want %v", got, period+spread)
	}
	if got := p.delay(); got != period {
		t.Errorf("delay at random 0.5 = %v, want %v", got, period)
	}

	// The first draw covers the whole day, so a fleet rebooted together by an
	// update does not ping together, and boot time is not the anchor.
	draws = []float64{0.99}
	late := newPinger(t, Options{Provider: &recorder{}, Random: next}).delay()
	if late < 23*time.Hour || late >= period {
		t.Errorf("first delay at random 0.99 = %v, want most of a day", late)
	}
}

func TestScheduleDrawsFreshEachTime(t *testing.T) {
	// The real source, not an injected one: the point is that two boxes and
	// two days do not share a slot.
	p := newPinger(t, Options{Provider: &recorder{}})
	p.delay() // burn the first-boot draw
	seen := map[time.Duration]bool{}
	for range 20 {
		seen[p.delay()] = true
	}
	if len(seen) < 15 {
		t.Fatalf("20 draws produced %d distinct delays; the schedule is not jittering", len(seen))
	}
}

func TestFailedSendIsSilentAndNotRetried(t *testing.T) {
	// The endpoint may be unavailable. One household's ping is a rounding
	// error in an aggregate, and a fleet retrying against a dead endpoint is
	// an outage the fleet inflicted on itself.
	rec := &recorder{err: errors.New("no such host")}
	loud := &levelCounter{}
	p := newPinger(t, Options{
		Provider: rec,
		Logger:   slog.New(loud),
		// Zero delay: without a retry guard a failure would spin here.
		Random: func() float64 { return 0 },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	// period-spread is 18 h of wall clock away, so anything beyond the first
	// send inside this window is a retry loop.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if n := rec.count(); n != 1 {
		t.Fatalf("sent %d pings after a failure, want exactly 1 — a failure must wait for tomorrow", n)
	}
	if n := loud.above(slog.LevelDebug); n != 0 {
		t.Fatalf("%d log records above debug; nothing about the house is wrong when a count does not arrive", n)
	}
}

// levelCounter counts records at or above each level, so a test can say
// "silent" and mean it.
type levelCounter struct {
	mu      sync.Mutex
	records []slog.Level
}

func (l *levelCounter) Enabled(context.Context, slog.Level) bool { return true }

func (l *levelCounter) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r.Level)
	return nil
}

func (l *levelCounter) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *levelCounter) WithGroup(string) slog.Handler      { return l }

func (l *levelCounter) above(level slog.Level) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, got := range l.records {
		if got > level {
			n++
		}
	}
	return n
}

func TestDisabledSendsNothing(t *testing.T) {
	rec := &recorder{}
	p := newPinger(t, Options{
		Provider: rec,
		Enabled:  func() bool { return false },
		Random:   func() float64 { return 0 },
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if n := rec.count(); n != 0 {
		t.Fatalf("sent %d pings while switched off", n)
	}
}

func TestPayloadPreviewIsWhatWouldBeSent(t *testing.T) {
	// Settings renders Payload(); the sender posts Payload(). The screen is
	// only a checkable claim if they are the same call.
	rec := &recorder{}
	p := newPinger(t, Options{Provider: rec, Random: func() float64 { return 0 }})
	preview := p.Payload()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for rec.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	if rec.count() == 0 {
		t.Fatal("nothing was sent")
	}
	rec.mu.Lock()
	sent := rec.sent[0]
	rec.mu.Unlock()

	want, _ := json.Marshal(preview)
	got, _ := json.Marshal(sent)
	if string(want) != string(got) {
		t.Fatalf("Settings shows %s but the box sends %s", want, got)
	}
}

// ---- transport ----

func TestHTTPProviderPostsTheJSONAndReadsNothingBack(t *testing.T) {
	var body atomic.Value
	var agent atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		body.Store(string(buf[:n]))
		agent.Store(r.Header.Get("User-Agent"))
		// A reply the box must not act on.
		w.Write([]byte(`{"restart":true}`))
	}))
	defer srv.Close()

	provider, err := NewHTTPProvider(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewHTTPProvider: %v", err)
	}
	if err := provider.Send(context.Background(), Build(facts(), time.Now())); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := body.Load().(string); !strings.Contains(got, `"schema":"ftw.fleet/2"`) {
		t.Fatalf("posted body = %s", got)
	}
	if got := agent.Load().(string); got != "ftw-fleet-ping" {
		t.Errorf("User-Agent = %q; it must not grow a build detail", got)
	}
}

func TestHTTPProviderReportsNon2xx(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	provider, err := NewHTTPProvider(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewHTTPProvider: %v", err)
	}
	if err := provider.Send(context.Background(), Payload{}); err == nil {
		t.Fatal("a 500 must be reported to the caller, which then forgets it")
	}
}

func TestHTTPProviderDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer plain.Close()

	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusTemporaryRedirect)
	}))
	defer tls.Close()

	provider, err := NewHTTPProvider(tls.URL, tls.Client())
	if err != nil {
		t.Fatalf("NewHTTPProvider: %v", err)
	}
	if err := provider.Send(context.Background(), Payload{}); err == nil {
		t.Fatal("a redirect must be refused")
	}
	if got := redirected.Load(); got != 0 {
		t.Fatalf("the report was resent to the redirect target %d time(s)", got)
	}
}

func TestPlainHTTPEndpointIsRefused(t *testing.T) {
	// The site's shape in the clear would be a worse leak than any this
	// package guards against.
	if _, err := NewHTTPProvider("http://relay.ftw.energy/fleet", nil); err == nil {
		t.Fatal("http:// endpoint accepted")
	}
	if _, err := NewHTTPProvider("https://relay.ftw.energy/fleet?box=abc", nil); err == nil {
		t.Fatal("a query string is a place to hide an identifier and must be refused")
	}
	if _, err := NewHTTPProvider(config.DefaultFleetPingEndpoint, nil); err != nil {
		t.Fatalf("the shipped default must be usable: %v", err)
	}
}

func TestAStraySpaceInTheEndpointDoesNotSurviveStartup(t *testing.T) {
	// Validated trimmed and then stored untrimmed, every send would fail for
	// the life of the process — at debug level, so the fleet would just be
	// short one box and nobody would know why.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	p, err := NewHTTPProvider("  "+srv.URL+"\n", srv.Client())
	if err != nil {
		t.Fatalf("NewHTTPProvider: %v", err)
	}
	if p.Endpoint != srv.URL {
		t.Errorf("stored endpoint = %q, want %q", p.Endpoint, srv.URL)
	}
	if err := p.Send(context.Background(), Build(facts(), time.Now())); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestHTTPProviderDefaultClientHasATimeout(t *testing.T) {
	// A ping that never returns would hold a goroutine for the life of the
	// process, once a day, forever.
	p, err := NewHTTPProvider(config.DefaultFleetPingEndpoint, nil)
	if err != nil {
		t.Fatalf("NewHTTPProvider: %v", err)
	}
	if p.Client.Timeout <= 0 {
		t.Fatal("the default client has no timeout")
	}
}

// Through the relay the gateway reads the same report the box would post
// directly, and the relay sees none of it.
func TestTheReportGoesThroughTheRelayUnchanged(t *testing.T) {
	client, gw, relay := statsrelaytest.New(t, time.Now())
	p := NewRelayProvider(client)
	facts := Facts{Version: "v1.4.0", Drivers: []string{"sungrow"}, Catalogue: []string{"sungrow"}, PriceZone: "SE3"}
	pinger, err := New(Options{Facts: func() Facts { return facts }, Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	want := pinger.Payload()
	if err := p.Send(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got := gw.Fleet()
	if len(got) != 1 || got[0].Version != want.Version || got[0].Zone != "SE3" || len(got[0].Drivers) != 1 {
		t.Fatalf("gateway got %+v", got)
	}
	if bytes.Contains(relay.Bodies()[0], []byte("sungrow")) || !pinger.ThroughRelay() || pinger.Endpoint() != relay.URL {
		t.Fatal("the relay saw the report, or the pinger does not say where it sends")
	}
}

// ---- health ----

func TestHealthTravelsOnlyAsBuckets(t *testing.T) {
	// Each case is what a box held over its last day; what leaves is a word
	// from a short list, never the count or the time behind it.
	for _, c := range []struct {
		h                                Health
		solve, fallbacks, errors, update string
	}{
		{Health{}, "none", "none", "none", "none"},
		{Health{Plans: 96, SlowestSolve: 420 * time.Millisecond, Polls: 86_400}, "0-1s", "0", "0", "none"},
		{Health{Plans: 96, SlowestSolve: time.Second, Fallbacks: 3, Polls: 1000, PollErrors: 9, Update: "ok"}, "1-5s", "1-3", "0-1%", "ok"},
		{Health{Plans: 12, SlowestSolve: 5 * time.Second, Fallbacks: 4, Polls: 1000, PollErrors: 10, Update: "failed"}, "5s+", "4+", "1%+", "failed"},
		{Health{Plans: 1, SlowestSolve: 47 * time.Second, Polls: 3, PollErrors: 3, Update: "v1.4.0"}, "5s+", "0", "1%+", "none"},
	} {
		f := facts()
		f.Health = c.h
		p := Build(f, time.Now())
		if p.Solve != c.solve || p.Fallbacks != c.fallbacks || p.Errors != c.errors || p.Update != c.update {
			t.Errorf("%+v: got %s %s %s %s", c.h, p.Solve, p.Fallbacks, p.Errors, p.Update)
		}
		raw, _ := json.Marshal(p)
		if _, err := fleetwire.Decode(raw); err != nil {
			t.Errorf("the service would refuse %s", raw)
		}
		for _, leak := range []string{"420", "47", "86400", "v1.4.0\"", "1000"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("%s carries %s", raw, leak)
			}
		}
	}
}
