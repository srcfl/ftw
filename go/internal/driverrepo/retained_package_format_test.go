package driverrepo

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type retainedPathSnapshot struct {
	info   os.FileInfo
	raw    []byte
	target string
}

func snapshotRetainedPath(t *testing.T, path string) *retainedPathSnapshot {
	t.Helper()
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &retainedPathSnapshot{info: info}
	if info.Mode()&os.ModeSymlink != 0 {
		snapshot.target, err = os.Readlink(path)
	} else {
		snapshot.raw, err = os.ReadFile(path)
	}
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertRetainedPathUnchanged(t *testing.T, path string, before *retainedPathSnapshot) {
	t.Helper()
	after := snapshotRetainedPath(t, path)
	if before == nil || after == nil {
		if before != after {
			t.Fatalf("retained path existence changed for %s", path)
		}
		return
	}
	if !os.SameFile(before.info, after.info) {
		t.Errorf("retained path was replaced before install rejection: %s", path)
	}
	if before.info.Mode() != after.info.Mode() || before.target != after.target || !bytes.Equal(before.raw, after.raw) {
		t.Errorf("retained path contents changed before install rejection: %s", path)
	}
}

func directAliasEntryForRetainedSourceful(
	t *testing.T,
	f *installedPolicyFixture,
) (*Manager, config.DriverRepositorySource, Manifest, ManifestDriver) {
	t.Helper()
	manager := f.manager(t, []config.DriverRepositorySource{f.repo}, "1.7.0")
	repo, manifest, entry, err := manager.find(f.repo.ID, f.installed.DriverID, f.installed.Version)
	if err != nil {
		t.Fatal(err)
	}
	repo.Format = config.DriverRepositoryFormatFTWManifestV1
	entry.PackageID = ""
	entry.Target = ""
	entry.ArtifactID = ""
	entry.RuntimeName = ""
	entry.RuntimeSemantics = ""
	entry.RuntimeVersion = ""
	entry.RuntimeABI = ""
	entry.HostAPIProfile = ""
	entry.PackageKeyID = ""
	entry.PackageEnvelopeURL = ""
	entry.PackageEnvelopeSHA256 = ""
	entry.SourceCommit = ""
	entry.Channel = ""
	entry.ControlEnabled = false
	entry.Commands = nil
	entry.DefaultMode = sourcefulDefaultMode{}
	entry.LeasePolicy = sourcefulLeasePolicy{}
	return manager, repo, manifest, entry
}

func TestRetainedSourcefulPackageRejectsDirectAliasBeforeChanges(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mutateEnvelope func(*testing.T, string)
	}{
		{name: "valid envelope"},
		{
			name: "corrupt envelope",
			mutateEnvelope: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("corrupt retained envelope"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "dangling envelope symlink",
			mutateEnvelope: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing-envelope", path); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "missing canonical envelope",
			mutateEnvelope: func(t *testing.T, path string) {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstalledSourcefulControlFixture(t)
			f.setHistoricalUnknownFormat(t)
			manager, repo, manifest, entry := directAliasEntryForRetainedSourceful(t, f)
			envelopePath := filepath.Join(filepath.Dir(f.installed.InstalledPath), sourcefulInstalledPackageEnvelope)
			activePath := filepath.Join(
				manager.ActiveDir(),
				filepath.FromSlash(strings.TrimPrefix(f.installed.LogicalPath, "drivers/")),
			)
			if tc.mutateEnvelope != nil {
				tc.mutateEnvelope(t, envelopePath)
			}

			beforeRow := f.installRow(t)
			beforeLua := snapshotRetainedPath(t, f.installed.InstalledPath)
			beforeEnvelope := snapshotRetainedPath(t, envelopePath)
			beforeActive := snapshotRetainedPath(t, activePath)
			if beforeActive == nil || beforeActive.info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("active logical path is not a symlink before reinstall: %s", activePath)
			}
			_, installErr := manager.installResolved(context.Background(), repo, manifest, entry)

			afterRow := f.installRow(t)
			if installErr == nil {
				t.Error("direct-manifest alias reclassified a retained Sourceful package")
			}
			if afterRow != beforeRow {
				t.Errorf("active install row changed before rejection:\n before: %+v\n  after: %+v", beforeRow, afterRow)
			}
			if afterRow.PreviousInstalledPath != beforeRow.PreviousInstalledPath {
				t.Errorf("previous installed path changed from %q to %q", beforeRow.PreviousInstalledPath, afterRow.PreviousInstalledPath)
			}
			assertRetainedPathUnchanged(t, f.installed.InstalledPath, beforeLua)
			assertRetainedPathUnchanged(t, envelopePath, beforeEnvelope)
			assertRetainedPathUnchanged(t, activePath, beforeActive)
		})
	}
}

func TestHistoricalSourcefulPackageAllowsCanonicalReinstall(t *testing.T) {
	f := newInstalledSourcefulControlFixture(t)
	f.setHistoricalUnknownFormat(t)
	manager := f.manager(t, []config.DriverRepositorySource{f.repo}, "1.7.0")

	installed, err := manager.Install(context.Background(), f.repo.ID, f.installed.DriverID, f.installed.Version)
	if err != nil {
		t.Fatalf("canonical Sourceful reinstall: %v", err)
	}
	if installed.RepositoryFormat != config.DriverRepositoryFormatSourcefulIndexV1 || !installed.Active {
		t.Fatalf("canonical reinstalled row = %+v", installed)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(installed.InstalledPath), sourcefulInstalledPackageEnvelope)); err != nil {
		t.Fatalf("canonical package envelope after reinstall: %v", err)
	}
}

func TestHistoricalDirectManifestAllowsVerifiedReinstallAndDefault(t *testing.T) {
	f := installLegacyDirectFixture(t)
	f.clearFormat(t, nil)
	store, manager := f.reopen(t)
	envelopePath := filepath.Join(filepath.Dir(f.installed.InstalledPath), sourcefulInstalledPackageEnvelope)
	if _, err := os.Lstat(envelopePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy direct install has package envelope: %v", err)
	}

	installed, err := manager.Install(context.Background(), f.repo.ID, f.installed.DriverID, f.installed.Version)
	if err != nil {
		t.Fatalf("verified direct-manifest reinstall: %v", err)
	}
	if installed.RepositoryFormat != config.DriverRepositoryFormatFTWManifestV1 || !installed.Active {
		t.Fatalf("verified direct-manifest row = %+v", installed)
	}
	stored, err := store.ActiveDriverRepoInstall(installed.LogicalPath)
	if err != nil || stored.RepositoryFormat != config.DriverRepositoryFormatFTWManifestV1 {
		t.Fatalf("stored direct-manifest row = %+v, %v", stored, err)
	}

	registry := drivers.NewRegistry(telemetry.NewStore())
	registry.RuntimePolicyResolver = manager.RuntimePolicy
	defer registry.ShutdownAll()
	driver := f.legacyDriver()
	if err := registry.Add(context.Background(), driver); err != nil {
		t.Fatalf("start verified direct-manifest driver: %v", err)
	}
	if err := registry.SendDefault(context.Background(), driver.Name); err != nil {
		t.Fatalf("run verified direct-manifest default: %v", err)
	}
}
