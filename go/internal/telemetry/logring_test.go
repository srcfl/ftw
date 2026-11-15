package telemetry

import (
	"fmt"
	"testing"
	"time"
)

// A driver that logs every control tick at INFO filled the 2000-line ring
// in 75 minutes on a real site. Warnings and errors must not depend on that
// ring: they go to a digest that keeps one row per message and hour.
func TestLogRingDigestKeepsWarningsPastTickChatter(t *testing.T) {
	r := NewLogRing()
	hour := time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)
	r.Append(LogEntry{TS: hour.Add(time.Minute), Level: "WARN", Driver: "pixii", Msg: "write failed reg=40", Attrs: "err=timeout"})
	for i := 0; i < 5000; i++ {
		r.Append(LogEntry{TS: hour.Add(2 * time.Minute), Level: "INFO", Driver: "pixii", Msg: "Pixii: setpoint ems_w=" + fmt.Sprint(i)})
	}
	r.Append(LogEntry{TS: hour.Add(3 * time.Minute), Level: "WARN", Driver: "pixii", Msg: "write failed reg=41", Attrs: "err=reset"})
	r.Append(LogEntry{TS: hour.Add(4 * time.Minute), Level: "ERROR", Msg: "optimizer: deadline exceeded"})
	r.Append(LogEntry{TS: hour.Add(5 * time.Minute), Level: "DEBUG", Msg: "noise"})

	rows := r.TakeDigest(hour.Add(10 * time.Minute))
	if len(rows) != 2 {
		t.Fatalf("digest has %d rows, want the warning pattern and the error: %+v", len(rows), rows)
	}
	w := rows[0]
	if w.Level != "WARN" || w.Driver != "pixii" || w.Pattern != "write failed reg=#" || w.Count != 2 {
		t.Errorf("warning row = %+v, want one pattern seen twice", w)
	}
	if w.Msg != "write failed reg=41" || w.Attrs != "err=reset" {
		t.Errorf("warning row keeps %q %q, want the latest text", w.Msg, w.Attrs)
	}
	if !w.Hour.Equal(hour) || !w.First.Equal(hour.Add(time.Minute)) || !w.Last.Equal(hour.Add(3*time.Minute)) {
		t.Errorf("warning row times = %v %v %v", w.Hour, w.First, w.Last)
	}
	if rows[1].Level != "ERROR" || rows[1].Count != 1 {
		t.Errorf("error row = %+v", rows[1])
	}

	// A second take sends only what arrived since, so the store can add.
	if again := r.TakeDigest(hour.Add(11 * time.Minute)); len(again) != 0 {
		t.Fatalf("nothing new, got %+v", again)
	}
	r.Append(LogEntry{TS: hour.Add(12 * time.Minute), Level: "WARN", Driver: "pixii", Msg: "write failed reg=7"})
	delta := r.TakeDigest(hour.Add(13 * time.Minute))
	if len(delta) != 1 || delta[0].Count != 1 || delta[0].Pattern != "write failed reg=#" {
		t.Fatalf("delta = %+v, want one more of the same pattern", delta)
	}

	// Once the hour has been sent and has passed, it is dropped from memory.
	r.TakeDigest(hour.Add(2 * time.Hour))
	r.mu.RLock()
	left := len(r.digest)
	r.mu.RUnlock()
	if left != 0 {
		t.Errorf("%d digest rows still held after their hour was sent", left)
	}
}

// Messages that differ in more than numbers would each be a new row. Past
// the hourly cap they are counted in one overflow row instead, so a driver
// that puts a timestamp in every warning cannot grow the store.
func TestLogRingDigestCapsDistinctMessagesPerHour(t *testing.T) {
	r := NewLogRing()
	hour := time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)
	for i := 0; i < digestPatternsPerHour+50; i++ {
		r.Append(LogEntry{TS: hour.Add(time.Second), Level: "WARN", Msg: "odd " + string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	rows := r.TakeDigest(hour.Add(time.Minute))
	if len(rows) != digestPatternsPerHour+1 {
		t.Fatalf("got %d rows, want %d patterns plus one overflow row", len(rows), digestPatternsPerHour+1)
	}
	var overflow *DigestEntry
	for i := range rows {
		if rows[i].Pattern == digestOverflowPattern {
			overflow = &rows[i]
		}
	}
	if overflow == nil || overflow.Count != 50 {
		t.Fatalf("overflow row = %+v, want 50 messages", overflow)
	}
}

func TestLogPatternFoldsNumbers(t *testing.T) {
	for in, want := range map[string]string{
		"write failed reg=40":           "write failed reg=#",
		"read 12 of 3.5 kW at 02:13:59": "read # of # kW at #",
		"no numbers":                    "no numbers",
	} {
		if got := logPattern(in); got != want {
			t.Errorf("logPattern(%q) = %q, want %q", in, got, want)
		}
	}
}
