package selfupdate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
	"github.com/srcfl/ftw/go/internal/nativeupdate"
)

func collectAvailable(bus *events.Bus) func() []events.UpdateAvailable {
	var mu sync.Mutex
	var got []events.UpdateAvailable
	bus.Subscribe(events.KindUpdateAvailable, func(e events.Event) {
		if ev, ok := e.(events.UpdateAvailable); ok {
			mu.Lock()
			got = append(got, ev)
			mu.Unlock()
		}
	})
	return func() []events.UpdateAvailable {
		mu.Lock()
		defer mu.Unlock()
		return append([]events.UpdateAvailable(nil), got...)
	}
}

// readyChecker is a native box running current on schema 7, with its
// release slots in place, so a release on the same schema is one an owner
// can install from the app.
func readyChecker(t *testing.T, root, current, releasesURL string, bus *events.Bus, st Store) *Checker {
	t.Helper()
	return New(Config{
		CurrentVersion: current, CurrentStateSchema: 7, NativeRoot: root,
		ReleasesURL: releasesURL, CheckInterval: time.Hour, Bus: bus,
	}, st)
}

// The owners hear of a version once. A reboot used to announce the same
// version again, since the dedupe lived in memory alone.
func TestUpdateAvailableIsAnnouncedOncePerVersionAcrossRestarts(t *testing.T) {
	root := t.TempDir()
	current := "v0.164.0-beta.1"
	nativeCurrentSlot(t, root, current)
	if err := (nativeupdate.Manager{Root: root}).Init(current); err != nil {
		t.Fatal(err)
	}
	rls := fakeReleasesServer(t, nil, fakeRelease{
		tag: "v0.165.0-beta.1", body: "<!-- ftw-state-schema-v2:7 -->", published: time.Now(), prerelease: true,
	})
	bus := events.NewBus()
	got := collectAvailable(bus)
	st := newMemStore()

	c := readyChecker(t, root, current, rls.URL, bus, st)
	if _, err := c.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	// The next process, on the same stored state.
	again := readyChecker(t, root, current, rls.URL, bus, st)
	if _, err := again.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	evs := got()
	if len(evs) != 1 {
		t.Fatalf("announced %d times, want once: %+v", len(evs), evs)
	}
	if evs[0].Version != "v0.165.0-beta.1" || !evs[0].Installable {
		t.Fatalf("announcement = %+v, want an installable v0.165.0-beta.1", evs[0])
	}
}

// A version that cannot be installed from the app still reaches the box's
// own rules, but says so: the owners' phones stay quiet about it.
func TestUpdateAvailableSaysWhetherTheAppCanInstallIt(t *testing.T) {
	current := "v0.164.0-beta.1"
	for _, tc := range []struct {
		name  string
		body  string
		slots bool
		fail  bool
		full  bool
		want  bool
	}{
		{name: "ready", body: "<!-- ftw-state-schema-v2:7 -->", slots: true, want: true},
		{name: "changes stored data", body: "<!-- ftw-state-schema-v2:8 -->", slots: true},
		{name: "release slots not ready", body: "<!-- ftw-state-schema-v2:7 -->"},
		{name: "failed here before", body: "<!-- ftw-state-schema-v2:7 -->", slots: true, fail: true},
		// TriggerUpdate refuses a download that would fill the disk, so the
		// owners must not hear that a version is ready to install.
		{name: "too little disk space", body: "<!-- ftw-state-schema-v2:7 -->", slots: true, full: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.slots {
				nativeCurrentSlot(t, root, current)
				if err := (nativeupdate.Manager{Root: root}).Init(current); err != nil {
					t.Fatal(err)
				}
			}
			rls := fakeReleasesServer(t, nil, fakeRelease{
				tag: "v0.165.0-beta.1", body: tc.body, published: time.Now(), prerelease: true,
			})
			bus := events.NewBus()
			got := collectAvailable(bus)
			c := readyChecker(t, root, current, rls.URL, bus, newMemStore())
			if tc.fail {
				// What the slots say after a failed trial, read on each check.
				markFailed(t, root, "v0.165.0-beta.1")
			}
			if tc.full {
				fillDisk(t, root, current)
			}
			if _, err := c.Check(context.Background(), true); err != nil {
				t.Fatal(err)
			}
			evs := got()
			if len(evs) != 1 {
				t.Fatalf("announced %d times, want once", len(evs))
			}
			if evs[0].Installable != tc.want {
				t.Fatalf("Installable = %v, want %v", evs[0].Installable, tc.want)
			}
		})
	}
}

// A skipped version is never announced, and a version announced once stays
// announced after a channel switch.
func TestUpdateAvailableSkipsSkippedVersionsAndSurvivesAChannelSwitch(t *testing.T) {
	root := t.TempDir()
	rls := fakeReleasesServer(t, nil,
		fakeRelease{tag: "v0.165.0-beta.1", published: time.Now(), prerelease: true},
	)
	bus := events.NewBus()
	got := collectAvailable(bus)
	st := newMemStore()
	c := readyChecker(t, root, "v0.164.0-beta.1", rls.URL, bus, st)
	if err := c.Skip("v0.165.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if evs := got(); len(evs) != 0 {
		t.Fatalf("a skipped version was announced: %+v", evs)
	}
	if err := c.Unskip(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetChannel(ChannelStable); err != nil {
		t.Fatal(err)
	}
	if err := c.SetChannel(ChannelBeta); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if evs := got(); len(evs) != 1 {
		t.Fatalf("announced %d times, want once after unskip: %+v", len(evs), evs)
	}
}

// Each version is news once, whichever channel found it. The box used to
// keep only the last version it announced, so beta, then stable, then beta
// again told the owners about the same beta twice, also after a restart.
func TestUpdateAvailableIsAnnouncedOncePerVersionAcrossChannels(t *testing.T) {
	root := t.TempDir()
	current := "v0.164.0-beta.1"
	nativeCurrentSlot(t, root, current)
	if err := (nativeupdate.Manager{Root: root}).Init(current); err != nil {
		t.Fatal(err)
	}
	rls := fakeReleasesServer(t, nil,
		fakeRelease{tag: "v0.165.0-beta.1", body: "<!-- ftw-state-schema-v2:7 -->", published: time.Now(), prerelease: true},
		fakeRelease{tag: "v0.164.0", body: "<!-- ftw-state-schema-v2:7 -->", published: time.Now().Add(-time.Hour)},
	)
	bus := events.NewBus()
	got := collectAvailable(bus)
	st := newMemStore()
	check := func(c *Checker) {
		t.Helper()
		if _, err := c.Check(context.Background(), true); err != nil {
			t.Fatal(err)
		}
	}
	setChannel := func(c *Checker, ch Channel) {
		t.Helper()
		if err := c.SetChannel(ch); err != nil {
			t.Fatal(err)
		}
	}

	c := readyChecker(t, root, current, rls.URL, bus, st)
	check(c) // beta: v0.165.0-beta.1
	setChannel(c, ChannelStable)
	check(c) // stable: v0.164.0
	setChannel(c, ChannelBeta)
	check(c) // beta again: nothing new
	again := readyChecker(t, root, current, rls.URL, bus, st)
	check(again)
	setChannel(again, ChannelStable)
	check(again)

	var versions []string
	for _, ev := range got() {
		versions = append(versions, ev.Version)
	}
	want := []string{"v0.165.0-beta.1", "v0.164.0"}
	if len(versions) != len(want) || versions[0] != want[0] || versions[1] != want[1] {
		t.Fatalf("announced %v, want %v", versions, want)
	}
}

// fillDisk makes the current release look larger than this disk can hold
// three times, as TestCheckSpaceRefusesWhenTheNextReleaseWouldNotFit does.
// The file is sparse, so it takes no space.
func fillDisk(t *testing.T, root, current string) {
	t.Helper()
	f, err := os.Create(filepath.Join(root, "releases", current, "ftw"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(1 << 43); err != nil {
		t.Fatal(err)
	}
}

// markFailed writes tag into the slots as the release that failed here.
func markFailed(t *testing.T, root, tag string) {
	t.Helper()
	path := filepath.Join(root, "slots.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var slots nativeupdate.Slots
	if err := json.Unmarshal(raw, &slots); err != nil {
		t.Fatal(err)
	}
	slots.LastFailed = tag
	if raw, err = json.Marshal(slots); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
