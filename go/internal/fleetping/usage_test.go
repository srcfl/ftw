package fleetping

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestUsageReadsTheNewestDayAndKeepsIt(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path != "/v1/fleet/public" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"days":[{"date":"2026-11-06","drivers":{"pixii@2.1.9":3}},{"date":"2026-11-07","drivers":{"pixii@2.1.9":14,"sma_pv@1.1.1":1}}]}`))
	}))
	defer srv.Close()
	u := NewUsage(srv.URL + "/v1/fleet")
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	u.now = func() time.Time { return at }
	got := u.Counts(context.Background())
	if got["pixii@2.1.9"] != 14 || got["sma_pv@1.1.1"] != 1 {
		t.Fatalf("counts = %v", got)
	}
	u.Counts(context.Background())
	if asked.Load() != 1 {
		t.Fatalf("asked %d times, want a kept answer", asked.Load())
	}
	at = at.Add(7 * time.Hour)
	u.Counts(context.Background())
	if asked.Load() != 2 {
		t.Fatalf("asked %d times after it went old", asked.Load())
	}
}

func TestUsageWithNoAnswerShowsNothingAndWaits(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	u := NewUsage(srv.URL)
	if got := u.Counts(context.Background()); got != nil {
		t.Fatalf("counts = %v", got)
	}
	u.Counts(context.Background())
	if asked.Load() != 1 {
		t.Fatalf("asked %d times, want a wait after a failure", asked.Load())
	}
	var none *Usage
	if none.Counts(context.Background()) != nil {
		t.Fatal("a nil Usage should know nothing")
	}
}

func TestReportCountsOnlyDriversThatDeliveredWithTheirVersion(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{Drivers: []config.Driver{
		{Name: "my-battery", Lua: "/data/drivers/pixii.lua"},
		{Name: "silent", Lua: "drivers/sma_pv.lua"},
		{Name: "old", Lua: "drivers/zap.lua"},
		{Name: "off", Lua: "drivers/ferroamp.lua", Disabled: true},
		{Name: "mine", Lua: "/home/x/pappas-laddbox.lua"},
	}}
	seen := map[string]time.Time{"my-battery": now.Add(-time.Minute), "old": now.Add(-25 * time.Hour), "mine": now, "off": now}
	running := RunningDrivers(cfg, now, func(name string) (time.Time, bool) {
		at, ok := seen[name]
		return at, ok
	}, func(lua string) string {
		if lua == "/data/drivers/pixii.lua" {
			return "2.1.9"
		}
		return "weird"
	})
	f := FactsFromConfig(cfg, "v0.162.0", []string{"pixii", "sma_pv", "zap", "ferroamp"}, now)
	f.Running = running
	got := Build(f, now).Drivers
	want := []string{"other", "pixii@2.1.9"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("drivers = %v, want %v", got, want)
	}
}
