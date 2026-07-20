package driverinventory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func writeDriver(t *testing.T, dir, name, id, version string, readOnly bool) (string, string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	readOnlyLua := "false"
	if readOnly {
		readOnlyLua = "true"
	}
	body := []byte("DRIVER = {\n  id = \"" + id + "\",\n  name = \"Test\",\n  version = \"" + version + "\",\n  read_only = " + readOnlyLua + "\n}\n")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return path, hex.EncodeToString(sum[:])
}

func TestBuildGroupsBundledInstancesWithoutLeakingNames(t *testing.T) {
	dir := t.TempDir()
	path, sourceSHA := writeDriver(t, dir, "sdm630.lua", "sdm630", "1.1.1", true)
	last := time.Now()
	snapshot, err := Build(time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC), Input{
		HostVersion:      "v1.5.0-beta.2",
		BundledDriverDir: dir,
		Drivers: []config.Driver{
			{Name: "customer-meter-one", Lua: path},
			{Name: "customer-meter-two", Lua: path},
		},
		RunningNames: []string{"customer-meter-one"},
		Health: map[string]telemetry.DriverHealth{
			"customer-meter-one": {Status: telemetry.StatusOk, LastSuccess: &last},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Host.UpdateChannel != "beta" || len(snapshot.Drivers) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	row := snapshot.Drivers[0]
	if row.DriverID != "sdm630" || row.Source != "bundled" || row.SourceSHA256 != sourceSHA || row.ConfiguredInstances != 2 || row.RunningInstances != 1 || row.Health.OK != 1 || row.Health.Unknown != 1 {
		t.Fatalf("row = %+v", row)
	}
}

func TestBuildBindsMatchingCanonicalArtifact(t *testing.T) {
	dir := t.TempDir()
	path, artifactSHA := writeDriver(t, dir, "sdm630.lua", "sdm630", "1.1.1", true)
	snapshot, err := Build(time.Now(), Input{
		HostVersion:      "1.5.0",
		ManagedDriverDir: dir,
		Drivers:          []config.Driver{{Name: "meter", Lua: path}},
		RunningNames:     []string{"meter"},
		RepositoryDrivers: []RepositoryArtifact{{
			LogicalPath: "drivers/sdm630.lua", DriverID: "sdm630", Version: "1.1.1",
			SHA256: artifactSHA, RepositoryID: "sourceful", PackageID: "com.sourceful.driver.sdm630",
			PackageChannel: "beta", ControlClass: "read_only",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := snapshot.Drivers[0]
	if row.Source != "managed" || row.PackageID != "com.sourceful.driver.sdm630" || row.ArtifactSHA256 != artifactSHA || row.SourceSHA256 != "" || row.ControlClass != "read_only" {
		t.Fatalf("row = %+v", row)
	}
}

func TestBuildDoesNotGuessControlClassFromMissingMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.lua")
	if err := os.WriteFile(path, []byte("DRIVER = {\n  id = \"old\",\n  version = \"1.0.0\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Build(time.Now(), Input{
		HostVersion: "1.5.0", BundledDriverDir: dir,
		Drivers: []config.Driver{{Name: "old-instance", Lua: path}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Drivers[0].ControlClass; got != "unknown" {
		t.Fatalf("control class = %q, want unknown", got)
	}
}

func TestBuildDowngradesChangedManagedFileToLocalOverride(t *testing.T) {
	dir := t.TempDir()
	path, _ := writeDriver(t, dir, "meter.lua", "meter", "1.0.0", true)
	snapshot, err := Build(time.Now(), Input{
		HostVersion:      "1.5.0",
		ManagedDriverDir: dir,
		Drivers:          []config.Driver{{Name: "meter", Lua: path}},
		RepositoryDrivers: []RepositoryArtifact{{
			LogicalPath: "drivers/meter.lua", DriverID: "meter", Version: "1.0.0",
			SHA256:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RepositoryID: "sourceful", PackageID: "com.sourceful.driver.meter", PackageChannel: "beta",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := snapshot.Drivers[0]
	if row.Source != "local_override" || row.SourceSHA256 == "" || row.PackageID != "" || row.ArtifactSHA256 != "" {
		t.Fatalf("row = %+v", row)
	}
}

func TestBuildMarksOldRepositoryArtifactAsLegacy(t *testing.T) {
	dir := t.TempDir()
	path, artifactSHA := writeDriver(t, dir, "old.lua", "old", "0.9.0", false)
	snapshot, err := Build(time.Now(), Input{
		HostVersion:      "1.5.0",
		ManagedDriverDir: dir,
		Drivers:          []config.Driver{{Name: "old-instance", Lua: path}},
		RepositoryDrivers: []RepositoryArtifact{{
			LogicalPath: "drivers/old.lua", DriverID: "old", Version: "0.9.0",
			SHA256: artifactSHA, RepositoryID: "legacy-ftw",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := snapshot.Drivers[0]
	if row.Source != "legacy_repository" || row.RepositoryID != "legacy-ftw" || row.PackageID != "" || row.ArtifactSHA256 != artifactSHA {
		t.Fatalf("row = %+v", row)
	}
}
