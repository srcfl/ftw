package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// driverFile writes a minimal driver whose DRIVER table carries a version and
// whose body can be varied to change the file's bytes.
func driverFile(t *testing.T, dir, name, version, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := "DRIVER = {\n  id = \"" + strings.TrimSuffix(name, ".lua") + "\",\n" +
		"  version = \"" + version + "\",\n}\n\nfunction driver_poll()\n" + body + "\nend\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapshots(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	oldDir := filepath.Join(root, "old")
	newDir := filepath.Join(root, "new")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return oldDir, newDir
}

// The failure this gate exists for: a driver's bytes move between two pins
// while its version stands still. A gateway offered that driver cannot tell
// the two apart, and the signed channel refuses to publish changed bytes
// under a version it already published.
func TestChangedBytesWithoutAVersionBumpFail(t *testing.T) {
	oldDir, newDir := snapshots(t)
	driverFile(t, oldDir, "pixii.lua", "2.1.0", "  return 5000")
	driverFile(t, newDir, "pixii.lua", "2.1.0", "  return 1000")

	err := checkVersionsAcrossDirs(oldDir, newDir)
	if err == nil {
		t.Fatal("changed bytes under an unchanged version were accepted")
	}
	if !strings.Contains(err.Error(), "pixii.lua") {
		t.Errorf("error does not name the driver: %v", err)
	}
}

func TestChangedBytesWithABumpPass(t *testing.T) {
	oldDir, newDir := snapshots(t)
	driverFile(t, oldDir, "pixii.lua", "2.1.0", "  return 5000")
	driverFile(t, newDir, "pixii.lua", "2.1.1", "  return 1000")

	if err := checkVersionsAcrossDirs(oldDir, newDir); err != nil {
		t.Fatalf("a bumped driver was rejected: %v", err)
	}
}

func TestUnchangedDriversPass(t *testing.T) {
	oldDir, newDir := snapshots(t)
	driverFile(t, oldDir, "pixii.lua", "2.1.0", "  return 5000")
	driverFile(t, newDir, "pixii.lua", "2.1.0", "  return 5000")

	if err := checkVersionsAcrossDirs(oldDir, newDir); err != nil {
		t.Fatalf("an untouched driver was rejected: %v", err)
	}
}

// A driver added to the pin's bundle list has no previous bytes to compare
// against. That is not a version question.
func TestNewlyBundledDriverPasses(t *testing.T) {
	oldDir, newDir := snapshots(t)
	driverFile(t, newDir, "atmoce.lua", "1.0.0", "  return 5000")

	if err := checkVersionsAcrossDirs(oldDir, newDir); err != nil {
		t.Fatalf("a newly bundled driver was rejected: %v", err)
	}
}

// Dropping a driver from the bundle list is a coverage decision, not a
// version one, so a file present only in the old snapshot is ignored.
func TestDroppedDriverIsIgnored(t *testing.T) {
	oldDir, newDir := snapshots(t)
	driverFile(t, oldDir, "zap.lua", "1.0.0", "  return 5000")
	driverFile(t, newDir, "pixii.lua", "2.1.0", "  return 5000")
	driverFile(t, oldDir, "pixii.lua", "2.1.0", "  return 5000")

	if err := checkVersionsAcrossDirs(oldDir, newDir); err != nil {
		t.Fatalf("a dropped driver was treated as a failure: %v", err)
	}
}

// Every driver is reported, not just the first bad one, so a pin move that
// missed several bumps is fixed in one pass rather than one per run.
func TestAllOffendersAreReported(t *testing.T) {
	oldDir, newDir := snapshots(t)
	for _, name := range []string{"pixii.lua", "sungrow.lua", "kostal.lua"} {
		driverFile(t, oldDir, name, "1.0.0", "  return 5000")
		driverFile(t, newDir, name, "1.0.0", "  return 1000")
	}

	err := checkVersionsAcrossDirs(oldDir, newDir)
	if err == nil {
		t.Fatal("expected failures")
	}
	for _, name := range []string{"pixii.lua", "sungrow.lua", "kostal.lua"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s missing from the report: %v", name, err)
		}
	}
}
