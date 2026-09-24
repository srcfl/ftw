package nativeupdate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

// committed installs a and b, runs b as a trial and commits it, the way an
// update does.
func committed(t *testing.T, clock *testClock) Manager {
	t.Helper()
	root := t.TempDir()
	release(t, root, "v0.131.0")
	release(t, root, "v0.132.0")
	m := Manager{Root: root, Now: clock.now}
	if err := m.Init("v0.131.0"); err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare("v0.132.0"); err != nil {
		t.Fatal(err)
	}
	if _, _, trial, err := m.Select(); err != nil || !trial {
		t.Fatalf("trial %v %v", trial, err)
	}
	if err := m.Commit("v0.132.0"); err != nil {
		t.Fatal(err)
	}
	return m
}

func startTag(t *testing.T, m Manager) string {
	t.Helper()
	_, tag, trial, err := m.Select()
	if err != nil || trial {
		t.Fatalf("select %s trial=%v %v", tag, trial, err)
	}
	return tag
}

func TestACrashLoopAfterCommitFallsBackToThePreviousRelease(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	m := committed(t, clock)
	state, _ := m.Read()
	if state.Probation == nil || state.Probation.Tag != "v0.132.0" || !state.Probation.Until.Equal(clock.t.Add(ProbationWindow)) {
		t.Fatalf("commit starts no probation: %+v", state.Probation)
	}
	for i := 1; i <= 2; i++ {
		clock.t = clock.t.Add(30 * time.Second)
		if tag := startTag(t, m); tag != "v0.132.0" {
			t.Fatalf("crash %d already fell back to %s", i, tag)
		}
	}
	clock.t = clock.t.Add(30 * time.Second)
	if tag := startTag(t, m); tag != "v0.131.0" {
		t.Fatalf("third crash kept %s", tag)
	}
	state, _ = m.Read()
	if state.Current != "v0.131.0" || state.Previous != "" || state.LastFailed != "v0.132.0" || state.Probation != nil {
		t.Fatalf("after fallback %+v", state)
	}
	if tag := startTag(t, m); tag != "v0.131.0" {
		t.Fatalf("the previous release does not stay: %s", tag)
	}
}

func TestCleanRestartsAndSlowCrashesDoNotFallBack(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	m := committed(t, clock)
	for i := 0; i < 5; i++ {
		if err := m.MarkCleanExit("v0.132.0"); err != nil {
			t.Fatal(err)
		}
		clock.t = clock.t.Add(10 * time.Second)
		if tag := startTag(t, m); tag != "v0.132.0" {
			t.Fatalf("clean restart %d fell back", i)
		}
	}
	// A mark left by another release is not this release's clean stop.
	if err := m.MarkCleanExit("v0.131.0"); err != nil {
		t.Fatal(err)
	}
	startTag(t, m)
	if state, _ := m.Read(); len(state.Probation.Crashes) != 1 {
		t.Fatalf("another release's mark was counted as clean: %+v", state.Probation)
	}
	// Crashes eleven minutes apart never make three within ten minutes.
	for i := 0; i < 3; i++ {
		clock.t = clock.t.Add(11 * time.Minute)
		if tag := startTag(t, m); tag != "v0.132.0" {
			t.Fatalf("slow crash %d fell back", i)
		}
	}
	// After the hour the release is trusted; crashes no longer count.
	clock.t = clock.t.Add(ProbationWindow)
	for i := 0; i < 4; i++ {
		clock.t = clock.t.Add(time.Second)
		if tag := startTag(t, m); tag != "v0.132.0" {
			t.Fatalf("a crash after probation fell back")
		}
	}
	if state, _ := m.Read(); state.Probation != nil {
		t.Fatalf("probation kept after its hour: %+v", state.Probation)
	}
}

func TestNoFallbackWithoutAPreviousReleaseThatReadsTheData(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	m := committed(t, clock)
	// The previous release has been removed.
	if err := os.RemoveAll(filepath.Join(m.Root, "releases", "v0.131.0")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		clock.t = clock.t.Add(10 * time.Second)
		if tag := startTag(t, m); tag != "v0.132.0" {
			t.Fatalf("fell back to a missing release")
		}
	}
}

func TestCheckOwnerAcceptsTheOwner(t *testing.T) {
	if err := (Manager{Root: t.TempDir()}).CheckOwner(); err != nil {
		t.Fatal(err)
	}
}

func TestARollbackKeepsTheFailedReleaseMarked(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)}
	m := committed(t, clock)
	release(t, m.Root, "v0.133.0")
	// v0.133.0 fails its trial.
	if err := m.Prepare("v0.133.0"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.Select(); err != nil {
		t.Fatal(err)
	}
	if tag := startTag(t, m); tag != "v0.132.0" {
		t.Fatalf("a failed trial kept %s", tag)
	}
	// The owner rolls back to v0.131.0; the failed mark must stay.
	if _, err := m.PrepareRollback(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.Select(); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit("v0.131.0"); err != nil {
		t.Fatal(err)
	}
	if state, _ := m.Read(); state.LastFailed != "v0.133.0" {
		t.Fatalf("rollback cleared the failed release: %+v", state)
	}
	// A retry of the failed release that commits clears it.
	if err := m.Prepare("v0.133.0"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.Select(); err != nil {
		t.Fatal(err)
	}
	if err := m.Commit("v0.133.0"); err != nil {
		t.Fatal(err)
	}
	if state, _ := m.Read(); state.LastFailed != "" {
		t.Fatalf("a successful retry kept the mark: %+v", state)
	}
}
