package nativeupdate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// image writes the package a container image carries for tag.
func image(t *testing.T, tag string) string {
	t.Helper()
	dir := t.TempDir()
	writeArchive(t, dir, tag)
	version := fmt.Sprintf(`{"version":%q,"arch":%q,"state_schema":7}`, tag, runtime.GOARCH)
	if err := os.WriteFile(filepath.Join(dir, packageVersionFile), []byte(version), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTakeImageStartsEmptySlotsFromTheImage(t *testing.T) {
	m := Manager{Root: t.TempDir()}
	dir := image(t, "v0.165.0-beta.1")
	note, err := m.TakeImage(context.Background(), dir)
	if err != nil || !strings.Contains(note, "installed v0.165.0-beta.1") {
		t.Fatalf("first start: %q %v", note, err)
	}
	if state, err := m.Read(); err != nil || state != (Slots{Current: "v0.165.0-beta.1"}) {
		t.Fatalf("slots after the first start: %+v %v", state, err)
	}
	// Every later start of the same image changes nothing.
	if note, err := m.TakeImage(context.Background(), dir); err != nil || note != "" {
		t.Fatalf("second start: %q %v", note, err)
	}
}

func TestTakeImageFinishesAnInterruptedFirstStart(t *testing.T) {
	root := t.TempDir()
	m := Manager{Root: root}
	dir := image(t, "v0.165.0-beta.1")
	archive := filepath.Join(dir, "ftw-linux-"+runtime.GOARCH+".tar.gz")
	if err := m.InstallArchive(context.Background(), "v0.165.0-beta.1", archive, archive+".sha256"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.TakeImage(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if state, err := m.Read(); err != nil || state.Current != "v0.165.0-beta.1" {
		t.Fatalf("slots: %+v %v", state, err)
	}
}

func TestTakeImageStagesANewerImageAsATrial(t *testing.T) {
	root := t.TempDir()
	m := Manager{Root: root}
	release(t, root, "v0.165.0-beta.1")
	if err := m.Init("v0.165.0-beta.1"); err != nil {
		t.Fatal(err)
	}
	note, err := m.TakeImage(context.Background(), image(t, "v0.165.0"))
	if err != nil || !strings.Contains(note, "staged v0.165.0 ") {
		t.Fatalf("newer image: %q %v", note, err)
	}
	if state, err := m.Read(); err != nil || state.Next != "v0.165.0" || state.Current != "v0.165.0-beta.1" {
		t.Fatalf("slots: %+v %v", state, err)
	}
	if _, tag, trial, err := m.Select(); err != nil || tag != "v0.165.0" || !trial {
		t.Fatalf("select = %s %t %v", tag, trial, err)
	}
}

func TestTakeImageLeavesSlotsThatMovedPastTheImage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image string
		slots Slots
	}{
		{"older image", "v0.164.0", Slots{Current: "v0.165.0"}},
		{"updated past the image", "v0.165.0-beta.2", Slots{Current: "v0.165.0", Previous: "v0.165.0-beta.2"}},
		{"went back from the image", "v0.166.0", Slots{Current: "v0.165.0", Previous: "v0.166.0"}},
		{"failed here", "v0.166.0", Slots{Current: "v0.165.0", LastFailed: "v0.166.0"}},
		{"already pending", "v0.166.0", Slots{Current: "v0.165.0", Next: "v0.166.0"}},
		{"trial running", "v0.166.0", Slots{Current: "v0.165.0", Trial: "v0.166.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			m := Manager{Root: root}
			for _, tag := range []string{tc.slots.Current, tc.slots.Previous} {
				if tag != "" {
					release(t, root, tag)
				}
			}
			if err := m.locked(func() error { return m.writeLocked(tc.slots) }); err != nil {
				t.Fatal(err)
			}
			if _, err := m.TakeImage(context.Background(), image(t, tc.image)); err != nil {
				t.Fatal(err)
			}
			if state, err := m.Read(); err != nil || state != tc.slots {
				t.Fatalf("slots changed: %+v %v", state, err)
			}
			if _, err := os.Lstat(filepath.Join(root, "releases", tc.image)); tc.image != tc.slots.Previous && err == nil {
				t.Fatal("an image release that is not taken was unpacked")
			}
		})
	}
}

func TestTakeImageRefusesASchemaChange(t *testing.T) {
	root := t.TempDir()
	m := Manager{Root: root}
	release(t, root, "v0.165.0")
	receipt := filepath.Join(root, "releases", "v0.165.0", receiptFile)
	data, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receipt, []byte(strings.Replace(string(data), `"state_schema":7`, `"state_schema":6`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Init("v0.165.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.TakeImage(context.Background(), image(t, "v0.166.0")); err == nil || !strings.Contains(err.Error(), "state schema") {
		t.Fatalf("schema change: %v", err)
	}
	if state, err := m.Read(); err != nil || state != (Slots{Current: "v0.165.0"}) {
		t.Fatalf("slots changed: %+v %v", state, err)
	}
}

func TestTakeImageRefusesWithoutDiskSpace(t *testing.T) {
	root := t.TempDir()
	m := Manager{Root: root}
	release(t, root, "v0.165.0")
	if err := m.Init("v0.165.0"); err != nil {
		t.Fatal(err)
	}
	// A sparse 8 TiB file makes three times the current release more than
	// this disk has free, as in TestCheckSpaceRefusesWhenTheNextReleaseWouldNotFit.
	big, err := os.Create(filepath.Join(root, "releases", "v0.165.0", "web", "big"))
	if err != nil {
		t.Fatal(err)
	}
	defer big.Close()
	if err := big.Truncate(1 << 43); err != nil {
		t.Skipf("file system has no sparse files: %v", err)
	}
	if _, err := m.TakeImage(context.Background(), image(t, "v0.166.0")); err == nil || !strings.Contains(err.Error(), "disk space") {
		t.Fatalf("no space: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "releases", "v0.166.0")); err == nil {
		t.Fatal("unpacked without space")
	}
}

func TestTakeImageRefusesAMislabelledImage(t *testing.T) {
	m := Manager{Root: t.TempDir()}
	dir := image(t, "v0.165.0")
	if err := os.WriteFile(filepath.Join(dir, packageVersionFile), []byte(`{"version":"v0.166.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.TakeImage(context.Background(), dir); err == nil {
		t.Fatal("a package for another tag was installed")
	}
	if _, err := m.Read(); err == nil {
		t.Fatal("slots were written for a mislabelled image")
	}
}

func TestNewerTagOrdersBetasBeforeTheirStable(t *testing.T) {
	order := []string{"v0.9.0", "v0.10.0-beta.1", "v0.10.0-beta.2", "v0.10.0-beta.10", "v0.10.0", "v0.10.1-beta.1", "v1.0.0"}
	for i, a := range order {
		for j, b := range order {
			if got := newerTag(a, b); got != (i > j) {
				t.Errorf("newerTag(%s, %s) = %t", a, b, got)
			}
		}
	}
}
