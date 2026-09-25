package driverrepo

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
)

const (
	// The runtime identity drivers.RuntimePolicy requires of a read-only
	// channel artifact.
	readOnlyRuntimeABI     = "gopher-lua-source-v1"
	readOnlyHostAPIProfile = "sourceful.host/ftw-core/v1"

	// Device Support packages were a second driver format that FTW no longer
	// reads. These name what an older install may have left behind.
	retiredPackageFormat   = "sourceful.driver-index/v1"
	retiredPackageEnvelope = "sourceful-package.envelope.json"
)

func repositoryFormat(repo config.DriverRepositorySource) string {
	if repo.Format == "" {
		return config.DriverRepositoryFormatFTWManifestV1
	}
	return repo.Format
}

// retiredPackage reports whether an install came from a Device Support
// package: recorded as one, or an older row with no recorded format whose
// directory still holds the envelope only that format wrote.
func retiredPackage(installed state.DriverRepoInstall) bool {
	switch installed.RepositoryFormat {
	case retiredPackageFormat:
		return true
	case "":
		_, err := os.Lstat(filepath.Join(filepath.Dir(installed.InstalledPath), retiredPackageEnvelope))
		return err == nil
	}
	return false
}

// retirePackages deactivates every active Device Support package through the
// ordinary deactivate path, so its driver resolves to the bundled copy again
// instead of failing to start.
func (m *Manager) retirePackages() {
	if m.store == nil {
		return
	}
	active, err := m.store.ActiveDriverRepoInstalls()
	if err != nil {
		slog.Warn("driver repository: read active state", "err", err)
		return
	}
	for _, installed := range active {
		if !retiredPackage(installed) {
			continue
		}
		if err := m.Deactivate(installed.LogicalPath); err != nil {
			slog.Warn("driver repository: could not deactivate a Device Support package",
				"driver", installed.DriverID, "path", installed.LogicalPath, "err", err)
			continue
		}
		slog.Warn("driver repository: Device Support packages are no longer supported; deactivated one so the bundled driver runs",
			"driver", installed.DriverID, "version", installed.Version, "path", installed.LogicalPath,
			"repository", installed.RepoID)
	}
}

// RuntimePolicy verifies signed metadata for an active managed artifact and
// derives its host permissions without a network call. Local and bundled
// drivers return nil.
func (m *Manager) RuntimePolicy(cfg config.Driver) (*drivers.RuntimePolicy, error) {
	if m.store == nil {
		return nil, nil
	}
	resolved, err := filepath.EvalSymlinks(cfg.Lua)
	installedRoot, rootErr := filepath.EvalSymlinks(filepath.Join(m.root, "installed"))
	if err != nil || rootErr != nil || !pathInside(installedRoot, resolved) {
		return nil, nil
	}
	recordedPath := filepath.Clean(cfg.Lua)
	if target, linkErr := os.Readlink(cfg.Lua); linkErr == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cfg.Lua), target)
		}
		recordedPath = filepath.Clean(target)
	}
	installed, err := m.store.DriverRepoInstallByPath(recordedPath)
	if err != nil {
		return nil, fmt.Errorf("resolve managed driver activation: %w", err)
	}
	if !installed.Active {
		return nil, errors.New("managed driver runtime requires the active artifact")
	}
	if retiredPackage(installed) {
		return nil, errors.New("installed driver is a Device Support package, which FTW no longer runs; use the bundled driver or install one from the FTW driver channel")
	}
	switch installed.RepositoryFormat {
	case "", config.DriverRepositoryFormatFTWManifestV1:
	default:
		return nil, errors.New("installed driver metadata format is unsupported")
	}
	var repo *config.DriverRepositorySource
	if installed.FTWSigned && installed.RepoURL == "https://github.com/srcfl/device-drivers" {
		// The installer recorded this trust identity before config aliases
		// could change. Keep the historical ID only to locate its cache.
		pinned := m.betaRepo
		pinned.ID = installed.RepoID
		repo = &pinned
	} else {
		for i := range m.cfg.Repositories {
			if m.cfg.Repositories[i].ID == installed.RepoID {
				repo = &m.cfg.Repositories[i]
				break
			}
		}
	}
	// InstallChannel uses this pinned trust source without adding it to the
	// stable config list. Bind only its exact recorded repository identity.
	if repo == nil && m.betaRepo.ID != "" && installed.RepoID == m.betaRepo.ID {
		repo = &m.betaRepo
	}
	if repo == nil {
		if installed.RepositoryFormat != config.DriverRepositoryFormatFTWManifestV1 {
			return nil, errors.New("installed driver requires its repository to verify runtime metadata; restore the repository or reinstall the driver")
		}
		// Only a recorded direct-manifest install can retain legacy startup
		// after its source is removed.
		return nil, nil
	}
	if installed.RepositoryFormat != "" && installed.RepositoryFormat != repositoryFormat(*repo) {
		return nil, errors.New("configured repository format does not match the installed driver")
	}
	if installed.RepositoryFormat == "" {
		if err := m.recordDirectManifestFormat(*repo, installed); err != nil {
			return nil, err
		}
	}
	return m.directManifestRuntimePolicy(*repo, installed)
}

func (m *Manager) recordDirectManifestFormat(repo config.DriverRepositorySource, installed state.DriverRepoInstall) error {
	if repositoryFormat(repo) != config.DriverRepositoryFormatFTWManifestV1 {
		return errors.New("installed driver metadata format is unsupported")
	}
	// Do not infer an older install's format from a config alias or an
	// in-memory manifest. Reverify the saved bytes against the resolved trust
	// source and match the recorded origin and exact artifact.
	raw, err := readLimitedFile(filepath.Join(m.root, "cache", safeSegment(installed.RepoID)+".json"), maxManifestBytes)
	if err != nil {
		return fmt.Errorf("verify older installed driver format: %w", err)
	}
	// An unsigned source can still perform an explicit new install. It
	// cannot supply missing historical provenance during startup.
	repo.AllowUnsigned = false
	manifest, _, err := verifyManifest(raw, repo)
	if err != nil {
		return fmt.Errorf("verify older installed driver format: %w", err)
	}
	if err := validateManifest(manifest, repo.AllowInsecure); err != nil {
		return fmt.Errorf("validate older installed driver format: %w", err)
	}
	if manifest.Repository != installed.RepoURL {
		return errors.New("older installed driver source does not match its verified manifest")
	}
	for _, entry := range append(append([]ManifestDriver{}, manifest.Drivers...), manifest.History...) {
		if entry.ID == installed.DriverID && entry.Version == installed.Version && strings.EqualFold(entry.SHA256, installed.SHA256) {
			return m.store.RecordDriverRepoInstallFormat(installed.ID, config.DriverRepositoryFormatFTWManifestV1)
		}
	}
	return errors.New("older installed driver is absent from its verified manifest; reinstall the driver")
}

func (m *Manager) directManifestRuntimePolicy(
	repo config.DriverRepositorySource,
	installed state.DriverRepoInstall,
) (*drivers.RuntimePolicy, error) {
	var manifest Manifest
	var err error
	if installed.FTWSigned && installed.RepoURL == "https://github.com/srcfl/device-drivers" {
		// The in-memory cache is keyed only by a configurable repository ID.
		// Another source may now own that ID. Reverify the saved envelope
		// against the pinned official key before applying its runtime policy.
		var raw []byte
		raw, err = readLimitedFile(filepath.Join(m.root, "cache", safeSegment(installed.RepoID)+".json"), maxManifestBytes)
		if err == nil {
			manifest, _, err = verifyManifest(raw, repo)
		}
		if err == nil {
			err = validateManifest(manifest, repo.AllowInsecure)
		}
	} else {
		manifest, err = m.manifestFor(repo)
	}
	if err != nil {
		if installed.RepoURL != "https://github.com/srcfl/device-drivers" {
			return nil, nil
		}
		return nil, fmt.Errorf("verify signed driver manifest for runtime: %w", err)
	}
	var matched *ManifestDriver
	for i := range manifest.Drivers {
		entry := &manifest.Drivers[i]
		if entry.ID == installed.DriverID && entry.Version == installed.Version &&
			strings.EqualFold(entry.SHA256, installed.SHA256) {
			matched = entry
			break
		}
	}
	if matched == nil {
		for i := range manifest.History {
			entry := &manifest.History[i]
			if entry.ID == installed.DriverID && entry.Version == installed.Version &&
				strings.EqualFold(entry.SHA256, installed.SHA256) {
				matched = entry
				break
			}
		}
	}
	if matched == nil {
		// Old FTW releases did not carry runtime policy fields and may not
		// appear in the new public channel history. Keep those installed
		// artifacts on their former v1 behavior. New public installs fail
		// closed if their signed entry disappears.
		if installed.RepoURL == "https://github.com/srcfl/device-drivers" {
			return nil, errors.New("active public driver is absent from its signed manifest")
		}
		return nil, nil
	}
	// read_only and control_enabled are two spellings of one fact. A driver
	// that may control while claiming to be read-only reads as safe to
	// anything that checks only one of them, so refuse the pair outright.
	if matched.ReadOnly == matched.ControlEnabled || matched.ReadOnly != matched.Metadata.ReadOnly {
		if installed.RepoURL == "https://github.com/srcfl/device-drivers" {
			return nil, errors.New("public FTW driver has a contradictory read-only policy")
		}
		return nil, nil
	}
	if matched.ControlEnabled {
		// A driver the catalog marks control: true is published with its
		// control path intact, and runs under the same terms as the copy
		// bundled with this build -- which is the same source. Binding a
		// read-only policy here made one file behave two ways depending on
		// where it came from.
		return nil, nil
	}
	permissions := make(map[string]bool, len(matched.Permissions))
	for _, permission := range matched.Permissions {
		permissions[permission] = true
	}
	return &drivers.RuntimePolicy{
		// drivers.RuntimePolicy requires this identity form of a read-only
		// artifact; it names the driver, not a package.
		PackageID:      "com.sourceful.driver." + matched.ID,
		Version:        matched.Version,
		ArtifactSHA256: strings.ToLower(matched.SHA256),
		RuntimeABI:     readOnlyRuntimeABI,
		HostAPIProfile: readOnlyHostAPIProfile,
		ReadOnly:       true,
		Permissions:    permissions,
		// Only a read-only driver can have one, and only the path the signed
		// manifest names. An unsigned or absent value leaves it empty, which
		// is the same as having no exemption at all.
		AuthPostPath:  matched.Metadata.AuthPostPath,
		ConfigSecrets: append([]string(nil), matched.Metadata.ConfigSecrets...),
	}, nil
}
