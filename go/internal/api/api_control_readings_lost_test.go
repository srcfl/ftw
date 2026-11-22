package api

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Field record, 8-22 Nov 2026, a Sungrow SH hybrid polled once a second: the
// control journal held 341 readings_lost alarms for the battery, each one
// 5 s journal sample long, about 23 a day, and none longer than 40 s. One
// gap in what a single sample sees is not lost readings. Each case below
// breaks one poll of a battery that follows its order; no sample may then
// alarm, and the verdict must come back.
func TestOnePollGapIsNotReadingsLost(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gap stores the broken poll and returns the clock the sample uses.
		gap func(h *hybridTrace) time.Time
	}{
		// The journal samples at its ticker's time, and GET /api/status takes
		// its clock before it waits for the controller's lock. A poll the
		// store takes in between is newer than that clock, not stale.
		{"poll stored after the sample's clock", func(h *hybridTrace) time.Time {
			now := time.Now()
			time.Sleep(time.Millisecond)
			h.poll()
			return now
		}},
		{"failed read, nothing stored", func(h *hybridTrace) time.Time {
			h.tel.RecordDriverError("hybrid", "modbus: i/o timeout")
			return time.Now()
		}},
		{"power not available once", func(h *hybridTrace) time.Time {
			h.tel.Update("hybrid", telemetry.DerBattery, h.batW, nil, []byte(`{"control_power_available":false}`))
			return time.Now()
		}},
		{"power older than its max age once", func(h *hybridTrace) time.Time {
			at := time.Now().Add(-61 * time.Second).Format(time.RFC3339Nano)
			h.tel.Update("hybrid", telemetry.DerBattery, h.batW, nil, []byte(fmt.Sprintf(`{"setpoint_w":%g,"power_observed_at":%q}`, h.cmdW, at)))
			return time.Now()
		}},
		{"driver says offline once", func(h *hybridTrace) time.Time {
			h.tel.Update("hybrid", telemetry.DerBattery, h.batW, nil, []byte(fmt.Sprintf(`{"setpoint_w":%g,"is_online":false}`, h.cmdW)))
			return time.Now()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHybridTrace()
				h.houseW, h.batW = 600, -3000
				h.command(-3000)
				for range 90 {
					time.Sleep(time.Second)
					h.poll()
				}
				if f := h.battery(time.Now()); f.Status != "following" {
					t.Fatalf("set-up battery not following: %s/%s", f.Status, f.Reason)
				}
				time.Sleep(time.Second)
				at := tc.gap(h)
				seen := []ControlFeedback{h.battery(at)}
				for range 30 {
					time.Sleep(time.Second)
					h.poll()
					seen = append(seen, h.battery(time.Now()))
				}
				for i, f := range seen {
					if f.Reason == "readings_lost" || f.Severity == "alarm" || f.VerificationLost {
						t.Fatalf("sample %d after one broken poll: %s/%s/%s", i, f.Status, f.Reason, f.Severity)
					}
				}
				if f := seen[len(seen)-1]; f.Status != "following" || f.Reason != "power_observed" {
					t.Fatalf("verdict did not come back: %s/%s", f.Status, f.Reason)
				}
			})
		})
	}
}

// Readings that stay away are still lost, 30 s after the last measured power,
// the grace a new order gets.
func TestSustainedGapIsReadingsLost(t *testing.T) {
	for _, tc := range []struct {
		name string
		// poll stores what the driver sends each second while the gap lasts.
		poll func(h *hybridTrace)
		// lostBy is when the alarm must stand; earlier samples must not alarm.
		lostBy time.Duration
	}{
		{"driver stores nothing", func(h *hybridTrace) {}, 31 * time.Second},
		// Every emit counts as a good poll (drivers.HostEnv.emitTelemetry),
		// so the watchdog never gives up on this one; its readings stop
		// being measurements, and that is lost after the same grace.
		{"driver says offline on every poll", func(h *hybridTrace) {
			h.tel.Update("hybrid", telemetry.DerBattery, h.batW, nil, []byte(`{"control_power_available":false,"is_online":false}`))
			h.tel.RecordDriverSuccess("hybrid")
		}, 31 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHybridTrace()
				h.houseW, h.batW = 600, -3000
				h.command(-3000)
				for range 90 {
					time.Sleep(time.Second)
					h.poll()
					h.tel.RecordDriverSuccess("hybrid")
				}
				start := time.Now()
				for time.Since(start) < 70*time.Second {
					time.Sleep(time.Second)
					tc.poll(h)
					h.tel.WatchdogScan(time.Minute)
					f, gap := h.battery(time.Now()), time.Since(start)
					if gap < 30*time.Second && (f.Severity == "alarm" || f.Reason == "readings_lost") {
						t.Fatalf("%v into the gap: %s/%s/%s", gap, f.Status, f.Reason, f.Severity)
					}
					if gap >= tc.lostBy && (f.Reason != "readings_lost" || f.Status != "no_contact" || f.Severity != "alarm") {
						t.Fatalf("%v into the gap: %s/%s/%s, want no_contact/readings_lost/alarm", gap, f.Status, f.Reason, f.Severity)
					}
				}
			})
		})
	}
}

// Easee's cloud sends op_mode 0 when it has not heard from the charger, and
// then stops vouching for the last power value. One such poll is a warning;
// two minutes of them, a charger's grace, are lost readings.
func TestCloudChargerOfflineIsLostAfterItsGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEVTrace(7000)
		for range 24 {
			e.tick(7000, "plan")
		}
		if f := e.verdict(t); f.Status != "following" {
			t.Fatalf("set-up charger not following: %s/%s", f.Status, f.Reason)
		}
		start := time.Now()
		for time.Since(start) < 3*time.Minute {
			c := e.tel.BeginCommand("easee", []byte(`{"action":"ev_set_current","power_w":7000}`), time.Now())
			e.tel.CompleteCommand(c, "accepted")
			e.tel.Update("easee", telemetry.DerEV, 7000, nil, []byte(fmt.Sprintf(
				`{"connected":true,"is_online":false,"control_power_observed_at":%q,"power_max_age_s":180}`, e.changedAt.Format(time.RFC3339Nano))))
			f, gap := e.verdict(t), time.Since(start)
			if gap < 110*time.Second && (f.Reason != "telemetry_stale" || f.Severity != "warning") {
				t.Fatalf("%v offline: %s/%s/%s, want telemetry_stale warning", gap, f.Status, f.Reason, f.Severity)
			}
			if gap >= 2*time.Minute+5*time.Second && (f.Reason != "readings_lost" || f.Severity != "alarm") {
				t.Fatalf("%v offline: %s/%s/%s, want readings_lost alarm", gap, f.Status, f.Reason, f.Severity)
			}
			time.Sleep(5 * time.Second)
		}
	})
}

// Measured proof needs 10 s of distinct fresh readings, and a reading
// without power, or a new step of 500 W or more, starts that window again.
// So the last measured proof can lag well behind healthy readings. Gaps that
// keep coming, alone or under a target that steps every few seconds, are
// still not lost readings.
func TestRepeatedOnePollGapsAreNotReadingsLost(t *testing.T) {
	gaps := []func(h *hybridTrace) time.Time{
		func(h *hybridTrace) time.Time { now := time.Now(); time.Sleep(time.Millisecond); h.poll(); return now },
		func(h *hybridTrace) time.Time {
			h.tel.RecordDriverError("hybrid", "modbus: i/o timeout")
			return time.Now()
		},
		func(h *hybridTrace) time.Time {
			h.tel.Update("hybrid", telemetry.DerBattery, h.batW, nil, []byte(`{"control_power_available":false}`))
			return time.Now()
		},
		func(h *hybridTrace) time.Time {
			h.tel.Update("hybrid", telemetry.DerBattery, h.batW, nil, []byte(fmt.Sprintf(`{"setpoint_w":%g,"is_online":false}`, h.cmdW)))
			return time.Now()
		},
	}
	for _, tc := range []struct {
		name        string
		every, step time.Duration
	}{
		{"a gap every 25 s", 25 * time.Second, 0},
		{"a gap every 8 s", 8 * time.Second, 0},
		{"a gap every 12 s under 600 W steps every 4 s", 12 * time.Second, 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHybridTrace()
				h.houseW, h.batW = 600, -3000
				h.command(-3000)
				for range 90 {
					time.Sleep(time.Second)
					h.poll()
				}
				for sec := 1; sec <= 5*60; sec++ {
					time.Sleep(time.Second)
					if tc.step > 0 && sec%int(tc.step.Seconds()) == 0 {
						target := -3000.0
						if h.cmdW == -3000 {
							target = -3600
						}
						h.command(target)
						h.batW = target
					}
					at := time.Now()
					if sec%int(tc.every.Seconds()) == 0 {
						at = gaps[(sec/int(tc.every.Seconds()))%len(gaps)](h)
					} else {
						h.poll()
					}
					if f := h.battery(at); f.Reason == "readings_lost" || f.Severity == "alarm" {
						t.Fatalf("%d s: %s/%s/%s", sec, f.Status, f.Reason, f.Severity)
					}
				}
			})
		})
	}
}

// A driver that has died while Core keeps stepping its order by 600 W
// every 10 s. Readings lost waits for 30 s without a new step as well as
// without measured power, so it is late here by design: each step restarts
// the response wait. The stale readings still show as no contact, a
// warning, the whole time, and the alarm comes 30 s after the steps stop.
// In the field the 60 s watchdog hands such a battery back to its own
// control, so the steps stop.
func TestDeadDriverUnderSteppingOrders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHybridTrace()
		h.houseW, h.batW = 600, -3000
		h.command(-3000)
		for range 90 {
			time.Sleep(time.Second)
			h.poll()
		}
		died := time.Now()
		for sec := 1; sec <= 4*60; sec++ {
			time.Sleep(time.Second)
			if sec%10 == 0 {
				target := -3000.0
				if h.cmdW == -3000 {
					target = -3600
				}
				h.command(target)
			}
			f, gap := h.battery(time.Now()), time.Since(died)
			if f.Severity == "alarm" || f.Reason == "readings_lost" {
				t.Fatalf("%v after the driver died, under steps: %s/%s/%s", gap, f.Status, f.Reason, f.Severity)
			}
			if gap > 61*time.Second && (f.Status != "no_contact" || f.Reason != "telemetry_stale" || f.Severity != "warning") {
				t.Fatalf("%v after the driver died: %s/%s/%s, want no_contact/telemetry_stale/warning", gap, f.Status, f.Reason, f.Severity)
			}
		}
		stopped := time.Now()
		for time.Since(stopped) < 40*time.Second {
			time.Sleep(time.Second)
			f, quiet := h.battery(time.Now()), time.Since(stopped)
			if quiet < 30*time.Second && f.Reason != "telemetry_stale" {
				t.Fatalf("%v after the last step: %s/%s/%s", quiet, f.Status, f.Reason, f.Severity)
			}
			if quiet >= 30*time.Second && (f.Reason != "readings_lost" || f.Severity != "alarm") {
				t.Fatalf("%v after the last step: %s/%s/%s, want readings_lost alarm", quiet, f.Status, f.Reason, f.Severity)
			}
		}
	})
}

// battery is the battery row of the verdict at now.
func (h *hybridTrace) battery(now time.Time) ControlFeedback {
	for _, f := range h.srv.controlFeedback(now) {
		if f.Kind == "battery" {
			return f
		}
	}
	return ControlFeedback{}
}
