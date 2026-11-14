package api

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Field trace, 23 Oct 2026 (srcfl/ftw#1763), Easee Cloud: FTW ordered 15 A
// (10.35 kW) throughout and the charger read back 15 A, while Easee's own
// load balancing signalled 13 A to the car, which took 8.85 kW. FTW said
// "Not following". The driver now reports the limit and, when its
// allocation shows it, names the load balancing.
func TestEaseeLoadBalancingReadsAsLimited(t *testing.T) {
	// The driver always reads back the order it holds (max_a, observation 48).
	const held = `,"max_a":15`
	const named = held + `,"current_limit_a":13,"current_limited_by":"load_balancer"`
	for _, tc := range []struct {
		name, extra              string
		drawW                    float64
		status, reason, severity string
	}{
		{"named", named, 8850, "limited", "load_balancer_limit", "info"},
		{"cause not shown", held + `,"current_limit_a":13`, 8850, "limited", "offered_current_lower", "warning"},
		{"no limit reported", held, 8850, "not_following", "power_below_target", "warning"},
		{"a 13 A limit does not explain zero", named, 0, "not_following", "no_power_response", "warning"},
		{"nor a car taking far less", named, 4000, "not_following", "power_below_target", "warning"},
		{"nor a car 2 A under it", named, 7500, "not_following", "power_below_target", "warning"},
		{"nor a car 1.5 A under a 7 A limit", held + `,"current_limit_a":7,"current_limited_by":"load_balancer"`, 3800, "not_following", "power_below_target", "warning"},
		{"nor a car at the order", named, 10300, "following", "power_observed", "info"},
		{"limit at the order", held + `,"current_limit_a":15,"current_limited_by":"load_balancer"`, 8850, "not_following", "power_below_target", "warning"},
		{"a zero limit", held + `,"current_limit_a":0,"current_limited_by":"load_balancer"`, 0, "not_following", "no_power_response", "warning"},
		{"a zero limit, no readback", `,"current_limit_a":0,"current_limited_by":"load_balancer"`, 0, "not_following", "no_power_response", "warning"},
		// A charger that holds a lower order than FTW sent says so first.
		{"charger holds 12 A", `,"max_a":12,"current_limit_a":10,"current_limited_by":"load_balancer"`, 6900, "limited", "offered_current_lower", "warning"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				e := newEVTrace(10370)
				e.extra = held
				for range 5 * 60 / 5 {
					e.tick(10350, "plan")
				}
				if f := e.verdict(t); f.Status != "following" {
					t.Fatalf("before the cut: %s/%s", f.Status, f.Reason)
				}
				e.drawW, e.changedAt, e.extra = tc.drawW, time.Now(), tc.extra
				cut := time.Now()
				var f ControlFeedback
				for range 6 * 60 / 5 {
					e.tick(10350, "plan")
					f = e.verdict(t)
					if tc.status == "limited" && f.Status == "not_following" {
						t.Fatalf("at %v: not following (%s)", time.Since(cut), f.Reason)
					}
				}
				if f.Status != tc.status || f.Reason != tc.reason || f.Severity != tc.severity {
					t.Fatalf("got %s/%s/%s, want %s/%s/%s", f.Status, f.Reason, f.Severity, tc.status, tc.reason, tc.severity)
				}
				if tc.reason != "load_balancer_limit" {
					return
				}
				// Requested, allocated and delivered stay apart.
				if f.RequestedA == nil || math.Abs(*f.RequestedA-15) > 0.01 || f.OfferedA == nil || *f.OfferedA != 15 ||
					f.LimitA == nil || *f.LimitA != 13 || f.LimitedBy != "load_balancer" || f.ActualW == nil || *f.ActualW != 8850 {
					t.Fatalf("numbers: requested %v, offer %v, limit %v by %q, actual %v", f.RequestedA, f.OfferedA, f.LimitA, f.LimitedBy, f.ActualW)
				}
			})
		})
	}
}

// The trace's balancer stepped between 12, 14 and 13 A. When it lets the
// car take more, the cloud reports the car's old draw for a while; that is
// the balancer's limit, not a charger that stopped following.
func TestEaseeLoadBalancingStepsUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEVTrace(8280)
		e.extra = `,"max_a":15,"current_limit_a":12,"current_limited_by":"load_balancer"`
		for range 5 * 60 / 5 {
			e.tick(10350, "plan")
		}
		if f := e.verdict(t); f.Reason != "load_balancer_limit" {
			t.Fatalf("at 12 A: %s/%s", f.Status, f.Reason)
		}
		e.extra = `,"max_a":15,"current_limit_a":14,"current_limited_by":"load_balancer"`
		for range 4 {
			e.tick(10350, "plan")
			if f := e.verdict(t); f.Reason != "load_balancer_limit" {
				t.Fatalf("after the step to 14 A, car still at 8,280 W: %s/%s", f.Status, f.Reason)
			}
		}
		e.drawW, e.changedAt = 9660, time.Now()
		e.tick(10350, "plan")
		if f := e.verdict(t); f.Reason != "load_balancer_limit" || *f.LimitA != 14 {
			t.Fatalf("at 14 A: %s/%s", f.Status, f.Reason)
		}
	})
}

// The sentence names the charger's make with its limit, from the identity
// its driver reported. Rows without a limit do not carry it.
func TestChargerLimitCarriesTheMake(t *testing.T) {
	dir := t.TempDir()
	lua := filepath.Join(dir, "easee_like.lua")
	src := `
function driver_init(config) host.set_make("Easee") end
function driver_poll() return 60000 end
function driver_command(action, power_w, cmd) return true end
function driver_default_mode() end
`
	if err := os.WriteFile(lua, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	reg := drivers.NewRegistry(tel)
	t.Cleanup(reg.ShutdownAll)
	if err := reg.Add(context.Background(), config.Driver{Name: "easee", Lua: lua}); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{Tel: tel, Registry: reg})
	for _, tc := range []struct {
		extra, make string
	}{
		{`,"current_limit_a":13,"current_limited_by":"load_balancer"`, "Easee"},
		{``, ""},
	} {
		c := tel.BeginCommand("easee", []byte(`{"action":"ev_set_current","power_w":10350}`), time.Now())
		tel.CompleteCommand(c, "accepted")
		tel.Update("easee", telemetry.DerEV, 8850, nil, []byte(`{"connected":true`+tc.extra+`}`))
		rows := srv.controlFeedback(time.Now())
		if len(rows) != 1 || rows[0].Make != tc.make {
			t.Fatalf("extra %q: rows %+v, want make %q", tc.extra, rows, tc.make)
		}
	}
	// Stale readings drop the limit, its cause and the make together.
	tel.Update("easee", telemetry.DerEV, 8850, nil, []byte(`{"connected":true,"current_limit_a":13,"current_limited_by":"load_balancer"}`))
	rows := srv.controlFeedback(time.Now().Add(2 * time.Minute))
	if len(rows) != 1 || rows[0].Reason != "telemetry_stale" || rows[0].LimitA != nil || rows[0].LimitedBy != "" || rows[0].Make != "" {
		t.Fatalf("stale row keeps its limit: %+v", rows)
	}
}
