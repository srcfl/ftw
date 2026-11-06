package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/localzone"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestCalendarMonthBoundsUsesLocalCalendarBoundaries(t *testing.T) {
	tests := []struct {
		name, month, zone string
		now               time.Time
		wantDays          int
		wantHours         float64
	}{
		{"February common year", "2025-02", "UTC", time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC), 28, 672},
		{"February leap year", "2024-02", "UTC", time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC), 29, 696},
		{"31 day month", "2025-01", "UTC", time.Date(2025, 2, 1, 12, 0, 0, 0, time.UTC), 31, 744},
		{"DST spring month", "2025-03", "America/New_York", time.Date(2025, 4, 1, 12, 0, 0, 0, mustLoadLocation(t, "America/New_York")), 31, 743},
		{"DST autumn month", "2025-11", "America/New_York", time.Date(2025, 12, 1, 12, 0, 0, 0, mustLoadLocation(t, "America/New_York")), 30, 721},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc := mustLoadLocation(t, tt.zone)
			now := tt.now.In(loc)
			start, end, err := calendarMonthBounds(tt.month, now)
			if err != nil {
				t.Fatal(err)
			}
			if start.Location() != loc || end.Location() != loc || start.Day() != 1 || start.Hour() != 0 || end.Day() != 1 || end.Hour() != 0 {
				t.Fatalf("bounds = %s to %s, want local midnight boundaries", start, end)
			}
			if got := start.AddDate(0, 1, -1).Day(); got != tt.wantDays {
				t.Fatalf("days = %d, want %d", got, tt.wantDays)
			}
			if got := end.Sub(start).Hours(); got != tt.wantHours {
				t.Fatalf("elapsed hours = %.0f, want %.0f", got, tt.wantHours)
			}
		})
	}
}

func TestCalendarMonthBoundsRejectsInvalidAndFutureMonths(t *testing.T) {
	now := time.Date(2025, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, month := range []string{"2025-1", "2025-13", "not-a-month", "2025-11"} {
		if _, _, err := calendarMonthBounds(month, now); err == nil {
			t.Errorf("calendarMonthBounds(%q) unexpectedly succeeded", month)
		}
	}
}

func TestHandleEnergyDailyUsesBoxTimezoneAcrossMonthTransition(t *testing.T) {
	const helperEnv = "FTW_BOX_TIMEZONE_TEST_HELPER"
	if os.Getenv(helperEnv) != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHandleEnergyDailyUsesBoxTimezoneAcrossMonthTransition$")
		cmd.Env = make([]string, 0, len(os.Environ())+2)
		for _, env := range os.Environ() {
			if strings.HasPrefix(env, "TZ=") || strings.HasPrefix(env, helperEnv+"=") {
				continue
			}
			cmd.Env = append(cmd.Env, env)
		}
		cmd.Env = append(cmd.Env, helperEnv+"=1", "TZ=America/Los_Angeles")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("timezone-isolated test process failed: %v\n%s", err, output)
		}
		return
	}

	// Run with a Los Angeles process timezone while the box reports Stockholm.
	// localzone.Name is once-cached, so a subprocess gives this test a clean box
	// timezone before any other API test can initialize it.
	processLoc := mustLoadLocation(t, "America/Los_Angeles")
	boxLoc := mustLoadLocation(t, "Europe/Stockholm")
	time.Local = processLoc
	t.Setenv("TZ", "Europe/Stockholm")
	if time.Local.String() == boxLoc.String() {
		t.Fatal("test setup did not separate process and box timezones")
	}
	if got := localzone.Name(); got != "Europe/Stockholm" {
		t.Fatalf("box timezone = %q, want Europe/Stockholm", got)
	}

	st, err := state.Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{State: st})
	req := httptest.NewRequest(http.MethodGet, "/api/energy/daily?month=2024-03&days=1", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Timezone string `json:"tz"`
		Days     []struct {
			Day      string `json:"day"`
			PeriodMS int64  `json:"period_ms"`
		} `json:"days"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if body.Timezone != "Europe/Stockholm" {
		t.Fatalf("response timezone = %q, want Europe/Stockholm", body.Timezone)
	}
	if len(body.Days) != 31 {
		t.Fatalf("month=2024-03&days=1 returned %d days, want 31 (month takes precedence)", len(body.Days))
	}

	var monthDuration time.Duration
	for i, day := range body.Days {
		start := time.Date(2024, time.March, i+1, 0, 0, 0, 0, boxLoc)
		end := start.AddDate(0, 0, 1)
		if day.Day != start.Format("2006-01-02") {
			t.Errorf("day[%d] = %q, want %s", i, day.Day, start.Format("2006-01-02"))
		}
		wantPeriod := end.Sub(start).Milliseconds()
		if day.PeriodMS != wantPeriod {
			t.Errorf("day[%d] period_ms = %d, want %d for %s to %s", i, day.PeriodMS, wantPeriod, start, end)
		}
		monthDuration += time.Duration(day.PeriodMS) * time.Millisecond
	}
	if got, want := monthDuration, 743*time.Hour; got != want {
		t.Errorf("March 2024 covered duration = %s, want %s across Stockholm's DST transition", got, want)
	}
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
