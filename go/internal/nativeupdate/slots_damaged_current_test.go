package nativeupdate

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A damaged current release with a healthy, same-schema previous release
// beside it runs the previous release once as a trial.
func TestSelectFallsBackWhenCurrentReleaseIsDamaged(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := committed(t, clock) // current v0.132.0, previous v0.131.0, same schema
	// Past the one-hour probation window, as on a box that ran for days.
	clock.t = clock.t.Add(48 * time.Hour)
	if err := os.Remove(filepath.Join(m.Root, "releases", "v0.132.0", "web", "index.html")); err != nil {
		t.Fatal(err)
	}
	_, tag, trial, err := m.Select()
	if err != nil || tag != "v0.131.0" || !trial {
		t.Fatalf("select %s trial=%v %v", tag, trial, err)
	}
	state, err := m.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Trial != "v0.131.0" || state.LastFailed != "v0.132.0" || state.Current != "v0.132.0" {
		t.Fatalf("slots after fallback: %+v", state)
	}
	if err := m.Commit("v0.131.0"); err != nil {
		t.Fatal(err)
	}
	if got := startTag(t, m); got != "v0.131.0" {
		t.Fatalf("after commit started %s", got)
	}
}

// A fallback trial that dies is not tried again; the box keeps failing on
// the damaged release as before instead of looping between the two.
func TestSelectFallsBackOnlyOnce(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := committed(t, clock)
	clock.t = clock.t.Add(48 * time.Hour)
	if err := os.Remove(filepath.Join(m.Root, "releases", "v0.132.0", "web", "index.html")); err != nil {
		t.Fatal(err)
	}
	if _, _, trial, err := m.Select(); err != nil || !trial {
		t.Fatalf("first fallback trial=%v %v", trial, err)
	}
	// The trial exits before it commits.
	if _, tag, trial, err := m.Select(); err == nil || trial {
		t.Fatalf("second start %s trial=%v err=%v; want the damaged-release error", tag, trial, err)
	}
	state, err := m.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Trial != "" || state.LastFailed != "v0.131.0" {
		t.Fatalf("slots after failed fallback: %+v", state)
	}
}

// Without the current receipt the data schema is unknown, so the launcher
// does not guess.
func TestSelectDoesNotFallBackWithoutCurrentReceipt(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := committed(t, clock)
	clock.t = clock.t.Add(48 * time.Hour)
	if err := os.Remove(filepath.Join(m.Root, "releases", "v0.132.0", receiptFile)); err != nil {
		t.Fatal(err)
	}
	if _, tag, trial, err := m.Select(); err == nil || trial {
		t.Fatalf("start %s trial=%v err=%v; want no fallback", tag, trial, err)
	}
}

func TestSelectDoesNotFallBackAcrossSchemas(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	m := committed(t, clock)
	clock.t = clock.t.Add(48 * time.Hour)
	receipt := `{"tag":"v0.131.0","arch":"` + runtime.GOARCH + `","archive_sha256":"` + string(makeDigest('a')) + `","state_schema":6}`
	if err := os.WriteFile(filepath.Join(m.Root, "releases", "v0.131.0", receiptFile), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(m.Root, "releases", "v0.132.0", "web", "index.html")); err != nil {
		t.Fatal(err)
	}
	if _, tag, trial, err := m.Select(); err == nil || trial {
		t.Fatalf("start %s trial=%v err=%v; want no fallback", tag, trial, err)
	}
}
