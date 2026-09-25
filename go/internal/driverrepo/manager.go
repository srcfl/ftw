// Package driverrepo installs signed, independently versioned Lua drivers.
// Refresh is deliberately separate from activation: merely seeing an upstream
// manifest can never alter a running control path.
package driverrepo

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
	lua "github.com/yuin/gopher-lua"
)

const maxManifestBytes = 4 << 20
const maxDriverBytes = 2 << 20

type ManifestEnvelope struct {
	SchemaVersion int             `json:"schema_version"`
	KeyID         string          `json:"key_id"`
	Payload       json.RawMessage `json:"payload"`
	Signature     string          `json:"signature"`
}

type Manifest struct {
	SchemaVersion int              `json:"schema_version"`
	Repository    string           `json:"repository"`
	Commit        string           `json:"commit,omitempty"`
	GeneratedAt   time.Time        `json:"generated_at"`
	Drivers       []ManifestDriver `json:"drivers"`
	// History retains older signed, content-addressed versions. Refresh still
	// never activates anything; operators install a specific version.
	History []ManifestDriver `json:"history,omitempty"`
}

type ManifestDriver struct {
	ID             string                     `json:"id"`
	Path           string                     `json:"path"`
	Filename       string                     `json:"filename"`
	Version        string                     `json:"version"`
	SHA256         string                     `json:"sha256"`
	SizeBytes      int64                      `json:"size_bytes,omitempty"`
	URL            string                     `json:"url"`
	HostAPI        components.CompatibleRange `json:"host_api"`
	Metadata       drivers.CatalogEntry       `json:"metadata"`
	SourceCommit   string                     `json:"source_commit,omitempty"`
	Channel        string                     `json:"channel,omitempty"`
	ControlEnabled bool                       `json:"control_enabled,omitempty"`
	ReadOnly       bool                       `json:"read_only,omitempty"`
	Permissions    []string                   `json:"permissions,omitempty"`
}

type RepositoryStatus struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Format      string    `json:"format"`
	ManifestURL string    `json:"manifest_url"`
	Enabled     bool      `json:"enabled"`
	LastRefresh time.Time `json:"last_refresh,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Cached      bool      `json:"cached"`
	KeyID       string    `json:"key_id,omitempty"`
	DriverCount int       `json:"driver_count"`
}

type CatalogCandidate struct {
	RepositoryID    string                   `json:"repository_id"`
	Repository      string                   `json:"repository"`
	Driver          ManifestDriver           `json:"driver"`
	Installed       *state.DriverRepoInstall `json:"installed,omitempty"`
	UpdateAvailable bool                     `json:"update_available"`
}

type VersionCandidate struct {
	RepositoryID string `json:"repository_id"`
	// Channel is "beta" for the built-in beta channel and "stable" for a
	// configured repository.
	Channel string `json:"channel"`
	// Repository is the signed manifest's source repository URL; with the
	// entry's source commit it names the history behind this version.
	Repository string                   `json:"repository,omitempty"`
	Driver     ManifestDriver           `json:"driver"`
	Installed  *state.DriverRepoInstall `json:"installed,omitempty"`
}

type Status struct {
	// Warnings are problems a refresh met that did not stop it, such as an
	// unreachable beta channel.
	Warnings     []string                  `json:"warnings,omitempty"`
	Enabled      bool                      `json:"enabled"`
	HostAPI      int                       `json:"driver_host_api"`
	RootDir      string                    `json:"root_dir"`
	Repositories []RepositoryStatus        `json:"repositories"`
	Active       []state.DriverRepoInstall `json:"active"`
}

type Manager struct {
	cfg         config.DeviceRepository
	root        string
	store       *state.Store
	hostVersion string
	client      *http.Client
	betaRepo    config.DriverRepositorySource
	// bundledDir holds the drivers this Core release ships; see SetBundledDir.
	bundledDir string

	mu        sync.Mutex
	manifests map[string]Manifest
	statuses  map[string]RepositoryStatus
}

func New(cfg *config.DeviceRepository, persistentDir string, store *state.Store) *Manager {
	return NewWithHostVersion(cfg, persistentDir, store, "dev")
}

// NewWithHostVersion names the running FTW release in the manager's logs.
func NewWithHostVersion(cfg *config.DeviceRepository, persistentDir string, store *state.Store, hostVersion string) *Manager {
	effective := config.DeviceRepository{}
	if cfg != nil {
		effective = *cfg
	}
	root := effective.RootDir
	if root == "" {
		root = filepath.Join(persistentDir, "driver-repository")
	} else if !filepath.IsAbs(root) {
		root = filepath.Join(persistentDir, root)
	}
	manager := &Manager{
		cfg: effective, root: root, store: store, hostVersion: strings.TrimPrefix(hostVersion, "v"),
		client:    &http.Client{Timeout: 15 * time.Second},
		betaRepo:  officialBetaRepository(effective.Repositories),
		manifests: make(map[string]Manifest), statuses: make(map[string]RepositoryStatus),
	}
	manager.reconcileActive()
	manager.retirePackages()
	manager.rebuildEffective()
	return manager
}

func officialBetaRepository(configured []config.DriverRepositorySource) config.DriverRepositorySource {
	id := config.DefaultDriverRepositoryBetaID
	for {
		available := true
		for _, repo := range configured {
			if repo.ID == id {
				available = false
				id += "-channel"
				break
			}
		}
		if available {
			break
		}
	}
	return config.DriverRepositorySource{
		ID: id, Name: config.DefaultDriverRepositoryBetaName,
		ManifestURL: config.DefaultDriverRepositoryBetaManifestURL, Enabled: true,
		TrustedKeys: map[string]string{
			config.DefaultDriverRepositorySigningKeyID: config.DefaultDriverRepositoryPublicKey,
		},
	}
}

// ftwSigned reports whether an artifact taken from repo can only have arrived
// under a signature made with FTW's own key.
//
// Read against the repository as it stands during the install, and recorded on
// the install row, so it is a fact about what happened rather than a claim the
// config can restate afterwards.
//
// Three conditions, and each rules out a different way a household could put a
// name of its own choosing in front of the box:
//
//   - allow_unsigned means no signature was demanded at all;
//   - allow_insecure drops the transport and URL rules the manifest is fetched
//     under, so what arrived is not what the rules describe;
//   - exactly one trusted key, and that key FTW's, is what makes the
//     verification unambiguous. With a second key listed, the manifest may
//     have been signed by whoever holds it — including the household — and the
//     key id alone cannot say which, because a config names its own key ids.
//
// The public key is a build-time constant. Claiming a repository id, renaming
// a channel or editing an entry cannot reach it.
func ftwSigned(repo config.DriverRepositorySource) bool {
	if repo.AllowUnsigned || repo.AllowInsecure || len(repo.TrustedKeys) != 1 {
		return false
	}
	return repo.TrustedKeys[config.DefaultDriverRepositorySigningKeyID] ==
		config.DefaultDriverRepositoryPublicKey
}

func (m *Manager) ActiveDir() string { return filepath.Join(m.root, "active") }

// reconcileActive closes the tiny crash window between the SQLite activation
// commit and the atomic symlink swap. The path actually used by the driver
// resolver is authoritative; an unknown, missing, or modified artifact is
// deactivated so startup falls back to the release's own driver.
func (m *Manager) reconcileActive() {
	if m.store == nil {
		return
	}
	active, err := m.store.ActiveDriverRepoInstalls()
	if err != nil {
		slog.Warn("driver repository: read active state", "err", err)
		return
	}
	for _, recorded := range active {
		rel := strings.TrimPrefix(recorded.LogicalPath, "drivers/")
		activePath := filepath.Join(m.ActiveDir(), filepath.FromSlash(rel))
		info, statErr := os.Lstat(activePath)
		target, linkErr := os.Readlink(activePath)
		if linkErr == nil && !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(activePath), target)
		}
		target = filepath.Clean(target)

		candidate := recorded
		if linkErr == nil && filepath.Clean(recorded.InstalledPath) != target {
			candidate, err = m.store.DriverRepoInstallByPath(target)
		} else {
			err = linkErr
		}
		valid := statErr == nil && info.Mode()&os.ModeSymlink != 0 && err == nil &&
			candidate.LogicalPath == recorded.LogicalPath &&
			pathInside(filepath.Join(m.root, "installed"), candidate.InstalledPath) &&
			validateInstalledFile(candidate) == nil
		if valid {
			if candidate.InstalledPath != recorded.InstalledPath {
				if _, activateErr := m.store.ActivateDriverRepoInstall(candidate); activateErr != nil {
					slog.Warn("driver repository: reconcile activation", "path", recorded.LogicalPath, "err", activateErr)
				} else {
					continue
				}
			} else {
				continue
			}
		}

		_ = os.Remove(activePath)
		if err := m.store.DeactivateDriverRepoInstall(recorded.LogicalPath); err != nil {
			slog.Warn("driver repository: deactivate inconsistent artifact", "path", recorded.LogicalPath, "err", err)
		} else {
			slog.Warn("driver repository: inconsistent activation removed", "path", recorded.LogicalPath)
		}
	}
}

func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	statuses := make([]RepositoryStatus, 0, len(m.cfg.Repositories))
	for _, repo := range m.cfg.Repositories {
		st := m.statuses[repo.ID]
		st.ID, st.Name, st.Format, st.ManifestURL, st.Enabled = repo.ID, repo.Name, repositoryFormat(repo), repo.ManifestURL, repo.Enabled
		statuses = append(statuses, st)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	active, _ := m.store.ActiveDriverRepoInstalls()
	return Status{Enabled: m.cfg.Enabled, HostAPI: components.DriverHostAPIVersion, RootDir: m.root, Repositories: statuses, Active: active}
}

// Refresh downloads and verifies manifests but never installs, activates, or
// restarts a driver. On failure the last-good cache remains available.
func (m *Manager) Refresh(ctx context.Context, repositoryID string) error {
	if !m.cfg.Enabled {
		return errors.New("device repository is disabled")
	}
	var errs []error
	for _, repo := range m.cfg.Repositories {
		if !repo.Enabled || (repositoryID != "" && repo.ID != repositoryID) {
			continue
		}
		if err := m.refreshOne(ctx, repo); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo.ID, err))
		}
	}
	if repositoryID != "" && len(errs) == 0 {
		found := false
		for _, repo := range m.cfg.Repositories {
			found = found || (repo.ID == repositoryID && repo.Enabled)
		}
		if !found {
			return fmt.Errorf("repository %q not found or disabled", repositoryID)
		}
	}
	return errors.Join(errs...)
}

// RefreshAll is an owner's "check for new versions": every configured
// source and the beta channel. A beta that cannot be reached is a warning,
// not a failure, so the versions list still redraws from what did refresh.
// The periodic refresh stays on Refresh, which reads only configured sources.
func (m *Manager) RefreshAll(ctx context.Context) (warnings []string, err error) {
	if err := m.Refresh(ctx, ""); err != nil {
		return nil, err
	}
	if betaErr := m.refreshOne(ctx, m.betaRepo); betaErr != nil {
		warnings = append(warnings, "beta channel: "+betaErr.Error())
	}
	return warnings, nil
}

func (m *Manager) refreshOne(ctx context.Context, repo config.DriverRepositorySource) error {
	raw, err := m.fetch(ctx, repo.ManifestURL, maxManifestBytes, repo.AllowInsecure)
	if err != nil {
		m.recordError(repo, err)
		return err
	}
	manifest, keyID, err := verifyManifest(raw, repo)
	if err != nil {
		m.recordError(repo, err)
		return err
	}
	if err := validateManifest(manifest, repo.AllowInsecure); err != nil {
		m.recordError(repo, err)
		return err
	}
	cachePath := filepath.Join(m.root, "cache", safeSegment(repo.ID)+".json")
	if err := atomicWrite(cachePath, raw, 0o600); err != nil {
		m.recordError(repo, err)
		return err
	}
	m.mu.Lock()
	m.manifests[repo.ID] = manifest
	m.statuses[repo.ID] = RepositoryStatus{
		ID: repo.ID, Name: repo.Name, Format: repositoryFormat(repo), ManifestURL: repo.ManifestURL, Enabled: repo.Enabled,
		LastRefresh: time.Now(), Cached: true, KeyID: keyID, DriverCount: len(manifest.Drivers),
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) recordError(repo config.DriverRepositorySource, err error) {
	m.mu.Lock()
	st := m.statuses[repo.ID]
	st.ID, st.Name, st.Format, st.ManifestURL, st.Enabled = repo.ID, repo.Name, repositoryFormat(repo), repo.ManifestURL, repo.Enabled
	st.LastRefresh, st.LastError = time.Now(), err.Error()
	_, cacheErr := os.Stat(filepath.Join(m.root, "cache", safeSegment(repo.ID)+".json"))
	st.Cached = cacheErr == nil
	m.statuses[repo.ID] = st
	m.mu.Unlock()
}

func (m *Manager) Catalog() ([]CatalogCandidate, error) {
	if m.store == nil {
		return nil, errors.New("device repository store unavailable")
	}
	active, err := m.store.ActiveDriverRepoInstalls()
	if err != nil {
		return nil, err
	}
	activeByID := make(map[string]state.DriverRepoInstall, len(active))
	for _, in := range active {
		activeByID[in.RepoID+"\x00"+in.DriverID] = in
	}
	var out []CatalogCandidate
	for _, repo := range m.cfg.Repositories {
		if !repo.Enabled {
			continue
		}
		manifest, err := m.manifestFor(repo)
		if err != nil {
			continue
		}
		for _, d := range manifest.Drivers {
			candidate := CatalogCandidate{RepositoryID: repo.ID, Repository: manifest.Repository, Driver: d}
			if in, ok := activeByID[repo.ID+"\x00"+d.ID]; ok {
				copy := in
				candidate.Installed = &copy
				candidate.UpdateAvailable = compareSemver(d.Version, in.Version) > 0
			}
			out = append(out, candidate)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Driver.ID == out[j].Driver.ID {
			return out[i].RepositoryID < out[j].RepositoryID
		}
		return out[i].Driver.ID < out[j].Driver.ID
	})
	return out, nil
}

// ChannelCatalog reads the official signed beta channel without changing the
// configured stable source. This lets an operator inspect or install one beta
// driver while every other driver stays on its current artifact.
func (m *Manager) ChannelCatalog(ctx context.Context, channel string) ([]CatalogCandidate, error) {
	if m.store == nil {
		return nil, errors.New("device repository store unavailable")
	}
	if !m.cfg.Enabled {
		return nil, errors.New("device repository is disabled")
	}
	if channel != "beta" {
		return nil, fmt.Errorf("unsupported driver channel %q", channel)
	}
	repo := m.betaRepo
	if err := m.refreshOne(ctx, repo); err != nil {
		return nil, err
	}
	manifest, err := m.manifestFor(repo)
	if err != nil {
		return nil, err
	}
	active, err := m.store.ActiveDriverRepoInstalls()
	if err != nil {
		return nil, err
	}
	activeByID := make(map[string]state.DriverRepoInstall, len(active))
	for _, installed := range active {
		activeByID[installed.DriverID] = installed
	}
	out := make([]CatalogCandidate, 0, len(manifest.Drivers))
	for _, driver := range manifest.Drivers {
		candidate := CatalogCandidate{RepositoryID: repo.ID, Repository: manifest.Repository, Driver: driver}
		if installed, ok := activeByID[driver.ID]; ok {
			copy := installed
			candidate.Installed = &copy
			candidate.UpdateAvailable = compareSemver(driver.Version, installed.Version) > 0
		}
		out = append(out, candidate)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Driver.ID < out[j].Driver.ID })
	return out, nil
}

// EnrichCatalog overlays installed/upstream versions onto the resolver's
// local/managed/bundled catalog without changing its precedence.
func (m *Manager) EnrichCatalog(entries []drivers.CatalogEntry) []drivers.CatalogEntry {
	candidates, _ := m.Catalog()
	active, _ := m.store.ActiveDriverRepoInstalls()
	activeByLogical := make(map[string]state.DriverRepoInstall, len(active))
	for _, in := range active {
		activeByLogical[in.LogicalPath] = in
	}
	for i := range entries {
		// A local file is an operator's own copy and is not a managed install,
		// so none of the install metadata below applies to it. It still needs
		// the upstream comparison: an override shadows the channel silently,
		// and without this the operator is never told a newer version exists.
		if in, ok := activeByLogical[entries[i].Path]; ok && entries[i].Source == "managed" {
			entries[i].InstalledVersion = in.Version
			entries[i].RepositoryID = in.RepoID
			for _, candidate := range candidates {
				driver := candidate.Driver
				if candidate.RepositoryID != in.RepoID || driver.ID != in.DriverID || driver.Version != in.Version ||
					!strings.EqualFold(driver.SHA256, in.SHA256) {
					continue
				}
				entries[i].PackageChannel = driver.Channel
				entries[i].ArtifactSHA256 = strings.ToLower(driver.SHA256)
				break
			}
		}
		local := entries[i].Source == "local"
		for _, candidate := range candidates {
			if candidate.Driver.ID != entries[i].ID {
				continue
			}
			if entries[i].UpstreamVersion == "" || compareSemver(candidate.Driver.Version, entries[i].UpstreamVersion) > 0 {
				entries[i].UpstreamVersion = candidate.Driver.Version
				// A local file came from the operator, not from a repository.
				// Naming one here would read as provenance it does not have.
				if !local {
					entries[i].RepositoryID = candidate.RepositoryID
				}
			}
		}
		base := entries[i].Version
		if entries[i].InstalledVersion != "" {
			base = entries[i].InstalledVersion
		}
		// Only claim an update when both sides are real versions that can be
		// ordered. A local driver often carries no version, or something like
		// "local", and comparing that as a string would announce an update
		// on nothing better than alphabetical luck.
		entries[i].UpdateAvailable = entries[i].UpstreamVersion != "" &&
			isSemver(entries[i].UpstreamVersion) && isSemver(base) &&
			compareSemver(entries[i].UpstreamVersion, base) > 0
	}
	return entries
}

func (m *Manager) manifestFor(repo config.DriverRepositorySource) (Manifest, error) {
	m.mu.Lock()
	manifest, ok := m.manifests[repo.ID]
	m.mu.Unlock()
	if ok {
		return manifest, nil
	}
	raw, err := os.ReadFile(filepath.Join(m.root, "cache", safeSegment(repo.ID)+".json"))
	if err != nil {
		return Manifest{}, err
	}
	manifest, keyID, err := verifyManifest(raw, repo)
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifest(manifest, repo.AllowInsecure); err != nil {
		return Manifest{}, err
	}
	m.mu.Lock()
	m.manifests[repo.ID] = manifest
	st := m.statuses[repo.ID]
	st.Cached, st.KeyID, st.DriverCount = true, keyID, len(manifest.Drivers)
	m.statuses[repo.ID] = st
	m.mu.Unlock()
	return manifest, nil
}

// Install downloads and validates an artifact, stores it content-addressed,
// then atomically points the stable active path at it. The caller owns the
// affected runtime restart so hardware safety policy stays in core.
func (m *Manager) Install(ctx context.Context, repositoryID, driverID, version string) (state.DriverRepoInstall, error) {
	repo, manifest, entry, err := m.find(repositoryID, driverID, version)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	return m.installResolved(ctx, repo, manifest, entry)
}

// InstallChannel installs one artifact from the pinned official beta channel.
// The caller cannot supply a URL or trust root, and the configured stable
// repository remains unchanged.
func (m *Manager) InstallChannel(ctx context.Context, channel, driverID, version string) (state.DriverRepoInstall, error) {
	if m.store == nil {
		return state.DriverRepoInstall{}, errors.New("device repository store unavailable")
	}
	if !m.cfg.Enabled {
		return state.DriverRepoInstall{}, errors.New("device repository is disabled")
	}
	if channel != "beta" {
		return state.DriverRepoInstall{}, fmt.Errorf("unsupported driver channel %q", channel)
	}
	repo := m.betaRepo
	if err := m.refreshOne(ctx, repo); err != nil {
		return state.DriverRepoInstall{}, err
	}
	manifest, err := m.manifestFor(repo)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	entry, err := findManifestDriver(manifest, driverID, version)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	return m.installResolved(ctx, repo, manifest, entry)
}

func (m *Manager) installResolved(ctx context.Context, repo config.DriverRepositorySource, manifest Manifest, entry ManifestDriver) (state.DriverRepoInstall, error) {
	raw, err := m.fetch(ctx, entry.URL, maxDriverBytes, repo.AllowInsecure)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	sum := sha256.Sum256(raw)
	gotHash := hex.EncodeToString(sum[:])
	if !strings.EqualFold(gotHash, entry.SHA256) {
		return state.DriverRepoInstall{}, fmt.Errorf("driver sha256 %s, want %s", gotHash, entry.SHA256)
	}
	if entry.SizeBytes > 0 && int64(len(raw)) != entry.SizeBytes {
		return state.DriverRepoInstall{}, fmt.Errorf("driver size %d, want %d", len(raw), entry.SizeBytes)
	}
	installPath := filepath.Join(m.root, "installed", safeSegment(repo.ID), safeSegment(entry.ID), entry.Version, strings.ToLower(entry.SHA256), entry.Filename)
	retained, err := m.store.DriverRepoInstallByPath(installPath)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state.DriverRepoInstall{}, err
	}
	if err == nil {
		if retiredPackage(retained) {
			return state.DriverRepoInstall{}, errors.New("a retired Device Support package is retained at this path; install from a repository with another id")
		}
		if retained.RepositoryFormat != "" && retained.RepositoryFormat != repositoryFormat(repo) {
			return state.DriverRepoInstall{}, errors.New("retained driver metadata format cannot change")
		}
		if retained.RepositoryFormat == "" {
			// Missing metadata does not prove v1. Recover the old format only
			// from signed metadata bound to that install's source and artifact.
			if err := m.recordDirectManifestFormat(repo, retained); err != nil {
				return state.DriverRepoInstall{}, err
			}
		}
	}
	if err := atomicWrite(installPath, raw, 0o600); err != nil {
		return state.DriverRepoInstall{}, err
	}
	if err := validateLuaArtifact(installPath, entry); err != nil {
		return state.DriverRepoInstall{}, err
	}
	logical := filepath.ToSlash(entry.Path)
	installed := state.DriverRepoInstall{
		RepoURL: manifest.Repository, RepoID: repo.ID, DriverID: entry.ID,
		RepositoryFormat: repositoryFormat(repo),
		LogicalPath:      logical, Version: entry.Version, SHA256: strings.ToLower(entry.SHA256), InstalledPath: installPath,
		// Recorded here and nowhere else, because here is where it happened.
		FTWSigned: ftwSigned(repo),
	}
	commitSymlink, cleanupSymlink, err := m.prepareSymlink(logical, installPath)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	defer cleanupSymlink()
	previous, previousErr := m.store.ActiveDriverRepoInstall(logical)
	var replaced *state.DriverRepoInstall
	if previousErr == nil {
		replaced = &previous
	}
	activated, err := m.store.ActivateDriverRepoInstall(installed)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	if err := commitSymlink(); err != nil {
		if previousErr == nil {
			_, _ = m.store.ActivateDriverRepoInstall(previous)
		} else {
			_ = m.store.DeactivateDriverRepoInstall(logical)
		}
		return state.DriverRepoInstall{}, err
	}
	m.recordChoice(activated, replaced)
	return activated, nil
}

func (m *Manager) Rollback(logicalPath string) (state.DriverRepoInstall, error) {
	logicalPath, err := safeLogicalPath(logicalPath)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	current, err := m.store.ActiveDriverRepoInstall(logicalPath)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	if current.PreviousInstalledPath == "" {
		// Going back to the bundled copy is a different operation, not a step
		// between managed artifacts: see UseBundled. Rolling back here would
		// deactivate the only active row, leaving the caller's own recovery
		// path -- a second Rollback -- with nothing to restore.
		return state.DriverRepoInstall{}, errors.New("driver has no previous managed artifact")
	}
	previous, err := m.store.DriverRepoInstallByPath(current.PreviousInstalledPath)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	if retiredPackage(previous) {
		return state.DriverRepoInstall{}, errors.New("the previous artifact is a retired Device Support package")
	}
	if err := validateInstalledFile(previous); err != nil {
		return state.DriverRepoInstall{}, err
	}
	commitSymlink, cleanupSymlink, err := m.prepareSymlink(logicalPath, previous.InstalledPath)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	defer cleanupSymlink()
	previous.LogicalPath = logicalPath
	activated, err := m.store.ActivateDriverRepoInstall(previous)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	if err := commitSymlink(); err != nil {
		_, _ = m.store.ActivateDriverRepoInstall(current)
		return state.DriverRepoInstall{}, err
	}
	m.restoreChoice(logicalPath)
	return activated, nil
}

// InstalledVersions lists locally retained, content-addressed artifacts for a
// single driver. Remote refresh is not required, so an operator can return to
// a previously validated driver even when the repository is unavailable.
func (m *Manager) InstalledVersions(driverID string) ([]state.DriverRepoInstall, error) {
	if m.store == nil {
		return nil, errors.New("device repository store unavailable")
	}
	if safeSegment(driverID) != driverID || driverID == "" {
		return nil, fmt.Errorf("unsafe driver id %q", driverID)
	}
	retained, err := m.store.DriverRepoInstallsByDriver(driverID)
	if err != nil {
		return nil, err
	}
	// A retired Device Support package stays on disk but is never offered or
	// activated again.
	out := retained[:0]
	for _, installed := range retained {
		if !retiredPackage(installed) {
			out = append(out, installed)
		}
	}
	return out, nil
}

// AvailableVersions merges the signed remote history with locally retained
// artifacts. It is read-only and does not download or activate code.
func (m *Manager) AvailableVersions(driverID string) ([]VersionCandidate, error) {
	installed, err := m.InstalledVersions(driverID)
	if err != nil {
		return nil, err
	}
	// An install is matched by content, not by the channel it came from: a
	// beta file that stable later publishes byte for byte is the file that
	// runs, whichever row lists it.
	installedByContent := make(map[string]state.DriverRepoInstall, len(installed))
	for _, artifact := range installed {
		key := artifact.Version + "\x00" + strings.ToLower(artifact.SHA256)
		if prior, ok := installedByContent[key]; !ok || (artifact.Active && !prior.Active) {
			installedByContent[key] = artifact
		}
	}
	listed := make(map[int64]bool)
	var out []VersionCandidate
	seen := make(map[string]bool)
	offered := make(map[string]bool) // by content, so a promoted beta file is listed once
	sources := make([]config.DriverRepositorySource, 0, len(m.cfg.Repositories)+1)
	for _, repo := range m.cfg.Repositories {
		if repo.Enabled {
			sources = append(sources, repo)
		}
	}
	if m.cfg.Enabled {
		// Cached only: listing versions never reaches the network.
		sources = append(sources, m.betaRepo)
	}
	newestStable := ""
	for _, repo := range sources {
		manifest, err := m.manifestFor(repo)
		if err != nil {
			continue
		}
		channel := "stable"
		if repo.ID == m.betaRepo.ID {
			channel = "beta"
		}
		for _, driver := range append(append([]ManifestDriver{}, manifest.Drivers...), manifest.History...) {
			if driver.ID != driverID {
				continue
			}
			if channel == "stable" && (newestStable == "" || compareSemver(driver.Version, newestStable) > 0) {
				newestStable = driver.Version
			}
			// Beta is where the next version waits. Its history repeats
			// versions stable has passed, which would only be noise here.
			if channel == "beta" && newestStable != "" && compareSemver(driver.Version, newestStable) <= 0 {
				continue
			}
			key := repo.ID + "\x00" + driver.Version + "\x00" + strings.ToLower(driver.SHA256)
			if seen[key] || (channel == "beta" && offered[strings.ToLower(driver.SHA256)]) {
				continue
			}
			seen[key] = true
			offered[strings.ToLower(driver.SHA256)] = true
			candidate := VersionCandidate{RepositoryID: repo.ID, Channel: channel, Repository: manifest.Repository, Driver: driver}
			if artifact, ok := installedByContent[driver.Version+"\x00"+strings.ToLower(driver.SHA256)]; ok {
				copy := artifact
				candidate.Installed = &copy
				listed[artifact.ID] = true
			}
			out = append(out, candidate)
		}
	}
	// Retained artifacts remain selectable even if the signed cache is
	// temporarily unavailable. They were verified before installation and are
	// re-hashed again by ActivateInstalled; no network metadata is required to
	// return to a known local version.
	for _, artifact := range installed {
		key := artifact.RepoID + "\x00" + artifact.Version + "\x00" + strings.ToLower(artifact.SHA256)
		if seen[key] || listed[artifact.ID] {
			continue
		}
		seen[key] = true
		copy := artifact
		channel := "stable"
		if artifact.RepoID == m.betaRepo.ID {
			channel = "beta"
		}
		out = append(out, VersionCandidate{
			RepositoryID: artifact.RepoID,
			Channel:      channel,
			Driver: ManifestDriver{
				ID: artifact.DriverID, Path: artifact.LogicalPath,
				Filename: filepath.Base(artifact.InstalledPath), Version: artifact.Version,
				SHA256: artifact.SHA256,
			},
			Installed: &copy,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		cmp := compareSemver(out[i].Driver.Version, out[j].Driver.Version)
		if cmp != 0 {
			return cmp > 0
		}
		return out[i].RepositoryID < out[j].RepositoryID
	})
	return out, nil
}

// ActivateInstalled switches to any exact locally retained version. SHA256 is
// required when the same version exists from more than one signed repository.
func (m *Manager) ActivateInstalled(driverID, version, sha256 string) (state.DriverRepoInstall, error) {
	if version == "" {
		return state.DriverRepoInstall{}, errors.New("driver version is required")
	}
	versions, err := m.InstalledVersions(driverID)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	var matches []state.DriverRepoInstall
	for _, candidate := range versions {
		if candidate.Version == version && (sha256 == "" || strings.EqualFold(candidate.SHA256, sha256)) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return state.DriverRepoInstall{}, fmt.Errorf("driver %s version %s is not retained locally", driverID, version)
	}
	if len(matches) > 1 {
		return state.DriverRepoInstall{}, errors.New("multiple retained artifacts match; specify sha256")
	}
	target := matches[0]
	if !pathInside(filepath.Join(m.root, "installed"), target.InstalledPath) {
		return state.DriverRepoInstall{}, errors.New("retained artifact is outside the managed repository")
	}
	if err := validateInstalledFile(target); err != nil {
		return state.DriverRepoInstall{}, err
	}
	current, currentErr := m.store.ActiveDriverRepoInstall(target.LogicalPath)
	commitSymlink, cleanupSymlink, err := m.prepareSymlink(target.LogicalPath, target.InstalledPath)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	defer cleanupSymlink()
	activated, err := m.store.ActivateDriverRepoInstall(target)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	if err := commitSymlink(); err != nil {
		if currentErr == nil {
			_, _ = m.store.ActivateDriverRepoInstall(current)
		} else {
			_ = m.store.DeactivateDriverRepoInstall(target.LogicalPath)
		}
		return state.DriverRepoInstall{}, err
	}
	var replaced *state.DriverRepoInstall
	if currentErr == nil {
		replaced = &current
	}
	m.recordChoice(activated, replaced)
	return activated, nil
}

// UseBundled drops the managed entry so the driver resolves to the copy that
// shipped with this build. It is the only way back once a channel version has
// been installed over a bundled driver, since the bundled copy is not an
// install and cannot be activated by version.
//
// bundledPath is checked before anything is deactivated: not every driver the
// channel offers is bundled, and taking the managed entry away when there is
// nothing behind it would stop the driver instead of reverting it.
func (m *Manager) UseBundled(logicalPath, bundledPath string) (state.DriverRepoInstall, error) {
	logicalPath, err := safeLogicalPath(logicalPath)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	current, err := m.store.ActiveDriverRepoInstall(logicalPath)
	if err != nil {
		return state.DriverRepoInstall{}, err
	}
	if bundledPath == "" {
		return state.DriverRepoInstall{}, errors.New("no bundled copy of this driver to fall back to")
	}
	if _, err := os.Stat(bundledPath); err != nil {
		return state.DriverRepoInstall{}, fmt.Errorf("no bundled copy of this driver to fall back to: %w", err)
	}
	if err := m.Deactivate(logicalPath); err != nil {
		return state.DriverRepoInstall{}, err
	}
	// Returned so the caller can put this exact artifact back if the bundled
	// driver fails to start.
	return current, nil
}

// Deactivate removes the managed resolver entry. It is used when the first
// ever managed activation fails and there is no earlier managed artifact;
// core can then restart the release's own driver.
func (m *Manager) Deactivate(logicalPath string) error {
	logicalPath, err := safeLogicalPath(logicalPath)
	if err != nil {
		return err
	}
	rel := strings.TrimPrefix(logicalPath, "drivers/")
	activePath := filepath.Join(m.ActiveDir(), filepath.FromSlash(rel))
	if err := os.Remove(activePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := m.store.DeactivateDriverRepoInstall(logicalPath); err != nil {
		return err
	}
	m.forgetChoice(logicalPath)
	return nil
}

// Superseded is a managed selection this release's own copy has overtaken.
type Superseded struct {
	DriverID       string `json:"driver_id"`
	LogicalPath    string `json:"logical_path"`
	Version        string `json:"version"`
	BundledVersion string `json:"bundled_version"`
}

// SetBundledDir names the directory of drivers this Core release ships.
func (m *Manager) SetBundledDir(dir string) { m.bundledDir = dir }

// bundledVersion is the SemVer of the release's own copy at logicalPath, or
// "" when the release has none there.
func (m *Manager) bundledVersion(logicalPath string) string {
	if m.bundledDir == "" {
		return ""
	}
	rel := strings.TrimPrefix(logicalPath, "drivers/")
	entry, err := drivers.ParseCatalogFile(filepath.Join(m.bundledDir, filepath.FromSlash(rel)))
	if err != nil || !semverRE.MatchString(entry.Version) {
		return ""
	}
	return entry.Version
}

// ReleaseVersion is the version this Core release bundles at logicalPath, or
// "" when it bundles none there.
func (m *Manager) ReleaseVersion(logicalPath string) string { return m.bundledVersion(logicalPath) }

// Chosen reports whether the managed selection at logicalPath runs version
// because the owner went back to it from a newer one; such a choice stays
// across Core updates.
func (m *Manager) Chosen(logicalPath, version string) bool {
	if m.store == nil || version == "" {
		return false
	}
	pinned, _ := m.store.LoadConfig(pinKey(logicalPath))
	return pinned == version
}

// pinKey holds the version of a managed selection the owner chose over a
// newer one that was running. previousPinKey keeps the value it replaced, so
// undoing an activation also undoes its effect on the choice.
func pinKey(logicalPath string) string { return "driver_repository/pinned/" + logicalPath }

func previousPinKey(logicalPath string) string { return pinKey(logicalPath) + "/previous" }

// runs reports whether a managed selection runs rather than the release's
// own copy: when the release has none, when it is at least as new, or when
// the owner chose it over something newer.
func (m *Manager) runs(installed state.DriverRepoInstall) (bool, string) {
	bundled := m.bundledVersion(installed.LogicalPath)
	if bundled == "" || !semverRE.MatchString(installed.Version) || compareSemver(installed.Version, bundled) >= 0 {
		return true, bundled
	}
	pinned, _ := m.store.LoadConfig(pinKey(installed.LogicalPath))
	return pinned == installed.Version, bundled
}

// recordChoice remembers whether an activation went back to a version older
// than the one that was running, which is a choice that stays across Core
// updates. Moving to the same or a newer version is early access and clears
// the mark. previous is the selection this activation replaced, if any.
func (m *Manager) recordChoice(installed state.DriverRepoInstall, previous *state.DriverRepoInstall) {
	if m.store == nil {
		return
	}
	running := m.bundledVersion(installed.LogicalPath)
	if previous != nil {
		if ok, _ := m.runs(*previous); ok && (running == "" || compareSemver(previous.Version, running) > 0) {
			running = previous.Version
		}
	}
	pinned := ""
	if running != "" && semverRE.MatchString(installed.Version) && compareSemver(installed.Version, running) < 0 {
		pinned = installed.Version
	}
	m.savePin(installed.LogicalPath, pinned)
	m.rebuildEffective()
}

// restoreChoice undoes the last recordChoice at logicalPath: a rollback puts
// back the selection that ran before, and with it the choice that went with
// it.
func (m *Manager) restoreChoice(logicalPath string) {
	if m.store == nil {
		return
	}
	previous, _ := m.store.LoadConfig(previousPinKey(logicalPath))
	m.savePin(logicalPath, previous)
	m.rebuildEffective()
}

func (m *Manager) forgetChoice(logicalPath string) {
	if m.store == nil {
		return
	}
	m.savePin(logicalPath, "")
	m.rebuildEffective()
}

func (m *Manager) savePin(logicalPath, pinned string) {
	current, _ := m.store.LoadConfig(pinKey(logicalPath))
	if err := m.store.SaveConfig(previousPinKey(logicalPath), current); err != nil {
		slog.Warn("driver repository: record chosen version", "path", logicalPath, "err", err)
	}
	if err := m.store.SaveConfig(pinKey(logicalPath), pinned); err != nil {
		slog.Warn("driver repository: record chosen version", "path", logicalPath, "err", err)
	}
}

// EffectiveDir holds the managed drivers that run, and is what paths resolve
// through. ActiveDir records what the owner selected and is never changed by
// a Core start; this directory is derived from it and the release's own
// copies whenever either changes. An update, a failed trial or a rollback to
// an older Core therefore never loses a selection: an older release simply
// derives it again.
func (m *Manager) EffectiveDir() string { return filepath.Join(m.root, "effective") }

// ApplyBundled derives EffectiveDir for this release's drivers and returns
// the selections the release has overtaken. Call it after SetBundledDir.
func (m *Manager) ApplyBundled() []Superseded {
	superseded := m.rebuildEffective()
	for _, s := range superseded {
		slog.Info("driver repository: this release's driver runs instead of an older selection",
			"driver", s.DriverID, "path", s.LogicalPath, "selected_version", s.Version,
			"release_version", s.BundledVersion, "release", m.hostVersion)
	}
	return superseded
}

func (m *Manager) rebuildEffective() []Superseded {
	if m.store == nil {
		return nil
	}
	active, err := m.store.ActiveDriverRepoInstalls()
	if err != nil {
		slog.Warn("driver repository: read active state", "err", err)
		return nil
	}
	want := make(map[string]string, len(active))
	var superseded []Superseded
	for _, installed := range active {
		rel := filepath.FromSlash(strings.TrimPrefix(installed.LogicalPath, "drivers/"))
		if ok, bundled := m.runs(installed); !ok {
			superseded = append(superseded, Superseded{
				DriverID: installed.DriverID, LogicalPath: installed.LogicalPath,
				Version: installed.Version, BundledVersion: bundled,
			})
			continue
		}
		want[rel] = installed.InstalledPath
	}
	dir := m.EffectiveDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		slog.Warn("driver repository: effective drivers", "err", err)
		return superseded
	}
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if _, keep := want[rel]; !keep {
			_ = os.Remove(path)
		}
		return nil
	})
	for rel, target := range want {
		link := filepath.Join(dir, rel)
		if current, err := os.Readlink(link); err == nil && current == target {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
			slog.Warn("driver repository: effective drivers", "path", link, "err", err)
			continue
		}
		tmp := link + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		if err := os.Symlink(target, tmp); err != nil {
			slog.Warn("driver repository: effective drivers", "path", link, "err", err)
			continue
		}
		if err := os.Rename(tmp, link); err != nil {
			_ = os.Remove(tmp)
			slog.Warn("driver repository: effective drivers", "path", link, "err", err)
		}
	}
	return superseded
}

func (m *Manager) find(repositoryID, driverID, version string) (config.DriverRepositorySource, Manifest, ManifestDriver, error) {
	for _, repo := range m.cfg.Repositories {
		if repo.ID != repositoryID || !repo.Enabled {
			continue
		}
		manifest, err := m.manifestFor(repo)
		if err != nil {
			return repo, Manifest{}, ManifestDriver{}, err
		}
		driver, err := findManifestDriver(manifest, driverID, version)
		return repo, manifest, driver, err
	}
	return config.DriverRepositorySource{}, Manifest{}, ManifestDriver{}, fmt.Errorf("repository %q not found or disabled", repositoryID)
}

func findManifestDriver(manifest Manifest, driverID, version string) (ManifestDriver, error) {
	for _, driver := range append(append([]ManifestDriver{}, manifest.Drivers...), manifest.History...) {
		if driver.ID == driverID && (version == "" || driver.Version == version) {
			return driver, nil
		}
	}
	return ManifestDriver{}, fmt.Errorf("driver %q version %q not found", driverID, version)
}

func (m *Manager) prepareSymlink(logicalPath, target string) (commit func() error, cleanup func(), err error) {
	logicalPath, err = safeLogicalPath(logicalPath)
	if err != nil {
		return nil, nil, err
	}
	rel := strings.TrimPrefix(logicalPath, "drivers/")
	destination := filepath.Join(m.ActiveDir(), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return nil, nil, err
	}
	tmp := destination + ".tmp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.Symlink(target, tmp); err != nil {
		return nil, nil, err
	}
	cleanup = func() { _ = os.Remove(tmp) }
	commit = func() error {
		if err := os.Rename(tmp, destination); err != nil {
			return err
		}
		return nil
	}
	return commit, cleanup, nil
}

func (m *Manager) fetch(ctx context.Context, rawURL string, limit int64, allowInsecure bool) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && !(allowInsecure && (u.Scheme == "http" || u.Scheme == "file")) {
		return nil, fmt.Errorf("repository URL must use https")
	}
	if u.Scheme == "file" {
		return readLimitedFile(u.Path, limit)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "FTW-driver-repository")
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	return readLimited(resp.Body, limit)
}

func verifyManifest(raw []byte, repo config.DriverRepositorySource) (Manifest, string, error) {
	var envelope ManifestEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return Manifest{}, "", fmt.Errorf("decode manifest envelope: %w", err)
	}
	if len(envelope.Payload) == 0 {
		if !repo.AllowUnsigned {
			return Manifest{}, "", errors.New("manifest is unsigned")
		}
		var manifest Manifest
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return Manifest{}, "", err
		}
		return manifest, "unsigned", nil
	}
	if envelope.SchemaVersion != components.ComponentManifestSchemaVersion {
		return Manifest{}, "", fmt.Errorf("manifest envelope schema %d, want %d", envelope.SchemaVersion, components.ComponentManifestSchemaVersion)
	}
	encodedKey, ok := repo.TrustedKeys[envelope.KeyID]
	if !ok {
		return Manifest{}, "", fmt.Errorf("manifest key_id %q is not trusted", envelope.KeyID)
	}
	key, err := decodeBase64(encodedKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return Manifest{}, "", fmt.Errorf("invalid Ed25519 public key %q", envelope.KeyID)
	}
	signature, err := decodeBase64(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Manifest{}, "", errors.New("invalid manifest signature encoding")
	}
	if !ed25519.Verify(ed25519.PublicKey(key), envelope.Payload, signature) {
		return Manifest{}, "", errors.New("manifest signature verification failed")
	}
	var manifest Manifest
	if err := json.Unmarshal(envelope.Payload, &manifest); err != nil {
		return Manifest{}, "", fmt.Errorf("decode signed manifest payload: %w", err)
	}
	return manifest, envelope.KeyID, nil
}

func validateManifest(manifest Manifest, allowInsecure bool) error {
	if manifest.SchemaVersion != components.ComponentManifestSchemaVersion {
		return fmt.Errorf("manifest schema %d, want %d", manifest.SchemaVersion, components.ComponentManifestSchemaVersion)
	}
	seen := make(map[string]struct{}, len(manifest.Drivers))
	for _, d := range manifest.Drivers {
		if err := validateManifestDriver(d, allowInsecure); err != nil {
			return err
		}
		if _, ok := seen[d.ID]; ok {
			return fmt.Errorf("duplicate driver id %q", d.ID)
		}
		seen[d.ID] = struct{}{}
	}
	historySeen := make(map[string]struct{}, len(manifest.History))
	for _, d := range manifest.History {
		if err := validateManifestDriver(d, allowInsecure); err != nil {
			return fmt.Errorf("driver history: %w", err)
		}
		key := d.ID + "\x00" + d.Version + "\x00" + strings.ToLower(d.SHA256)
		if _, ok := historySeen[key]; ok {
			return fmt.Errorf("duplicate driver history artifact %s@%s", d.ID, d.Version)
		}
		historySeen[key] = struct{}{}
	}
	return nil
}

func validateManifestDriver(d ManifestDriver, allowInsecure bool) error {
	if d.ID == "" || d.Version == "" || d.URL == "" {
		return errors.New("manifest driver requires id, version, and url")
	}
	if safeSegment(d.ID) != d.ID {
		return fmt.Errorf("driver has unsafe id %q", d.ID)
	}
	if !semverRE.MatchString(d.Version) {
		return fmt.Errorf("driver %s has invalid semver %q", d.ID, d.Version)
	}
	logical, err := safeLogicalPath(d.Path)
	if err != nil {
		return fmt.Errorf("driver %s: %w", d.ID, err)
	}
	if d.Filename != filepath.Base(logical) || !strings.HasSuffix(d.Filename, ".lua") {
		return fmt.Errorf("driver %s filename does not match path", d.ID)
	}
	decoded, err := hex.DecodeString(d.SHA256)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("driver %s has invalid sha256", d.ID)
	}
	if d.HostAPI.Min <= 0 || d.HostAPI.Max < d.HostAPI.Min {
		return fmt.Errorf("driver %s has invalid host API range %d..%d", d.ID, d.HostAPI.Min, d.HostAPI.Max)
	}
	if d.SizeBytes < 0 || d.SizeBytes > maxDriverBytes {
		return fmt.Errorf("driver %s has invalid size %d", d.ID, d.SizeBytes)
	}
	if !d.HostAPI.OverlapsDriverHost() {
		return fmt.Errorf("driver %s host API range %d..%d excludes host range %d..%d", d.ID, d.HostAPI.Min, d.HostAPI.Max, components.DriverHostAPIMinVersion, components.DriverHostAPIVersion)
	}
	u, err := url.Parse(d.URL)
	if err != nil || (u.Scheme != "https" && !(allowInsecure && (u.Scheme == "http" || u.Scheme == "file"))) {
		return fmt.Errorf("driver %s URL must use https", d.ID)
	}
	return nil
}

func validateLuaArtifact(path string, manifest ManifestDriver) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	L := lua.NewState()
	defer L.Close()
	if _, err := L.Load(strings.NewReader(string(raw)), manifest.Filename); err != nil {
		return fmt.Errorf("compile Lua driver: %w", err)
	}
	metadata, err := drivers.ParseCatalogFile(path)
	if err != nil {
		return err
	}
	if metadata.ID != manifest.ID || metadata.Version != manifest.Version {
		return fmt.Errorf("driver metadata id/version %s@%s, want %s@%s", metadata.ID, metadata.Version, manifest.ID, manifest.Version)
	}
	source := string(raw)
	if !regexp.MustCompile(`(?m)^\s*host_api_min\s*=\s*[0-9]+`).MatchString(source) ||
		!regexp.MustCompile(`(?m)^\s*host_api_max\s*=\s*[0-9]+`).MatchString(source) {
		return errors.New("managed driver must declare host_api_min and host_api_max")
	}
	if metadata.HostAPIMin > components.DriverHostAPIVersion || metadata.HostAPIMax < components.DriverHostAPIMinVersion {
		return fmt.Errorf("driver metadata host API range %d..%d is incompatible", metadata.HostAPIMin, metadata.HostAPIMax)
	}
	return nil
}

func validateInstalledFile(in state.DriverRepoInstall) error {
	raw, err := os.ReadFile(in.InstalledPath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), in.SHA256) {
		return errors.New("previous driver artifact hash mismatch")
	}
	return nil
}

func pathInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func safeLogicalPath(path string) (string, error) {
	path = filepath.ToSlash(filepath.Clean(path))
	if !strings.HasPrefix(path, "drivers/") || path == "drivers/." || strings.Contains(path, "..") || filepath.IsAbs(path) {
		return "", fmt.Errorf("unsafe driver path %q", path)
	}
	return path, nil
}

func safeSegment(value string) string {
	out := regexp.MustCompile(`[^a-zA-Z0-9._-]+`).ReplaceAllString(value, "-")
	return strings.Trim(out, ".-")
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return raw, nil
}

func readLimitedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f, limit)
}

func decodeBase64(value string) ([]byte, error) {
	if out, err := base64.StdEncoding.DecodeString(value); err == nil {
		return out, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}

var semverRE = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

// isSemver reports whether a version can be ordered at all. compareSemver
// falls back to comparing strings, which is fine for sorting and wrong for
// deciding whether a newer release exists.
func isSemver(value string) bool {
	return semverRE.MatchString(value)
}

func compareSemver(a, b string) int {
	type version struct {
		core       [3]int
		prerelease string
	}
	parse := func(value string) (version, bool) {
		matches := semverRE.FindStringSubmatch(value)
		if matches == nil {
			return version{}, false
		}
		var out version
		for i := range out.core {
			out.core[i], _ = strconv.Atoi(matches[i+1])
		}
		out.prerelease = matches[4]
		return out, true
	}
	pa, oka := parse(a)
	pb, okb := parse(b)
	if !oka || !okb {
		return strings.Compare(a, b)
	}
	for i := range pa.core {
		if pa.core[i] < pb.core[i] {
			return -1
		}
		if pa.core[i] > pb.core[i] {
			return 1
		}
	}
	if pa.prerelease == "" && pb.prerelease == "" {
		return 0
	}
	if pa.prerelease == "" {
		return 1
	}
	if pb.prerelease == "" {
		return -1
	}
	aParts, bParts := strings.Split(pa.prerelease, "."), strings.Split(pb.prerelease, ".")
	for i := 0; i < len(aParts) && i < len(bParts); i++ {
		if aParts[i] == bParts[i] {
			continue
		}
		ai, aErr := strconv.Atoi(aParts[i])
		bi, bErr := strconv.Atoi(bParts[i])
		switch {
		case aErr == nil && bErr == nil:
			if ai < bi {
				return -1
			}
			return 1
		case aErr == nil:
			return -1
		case bErr == nil:
			return 1
		default:
			return strings.Compare(aParts[i], bParts[i])
		}
	}
	if len(aParts) < len(bParts) {
		return -1
	}
	if len(aParts) > len(bParts) {
		return 1
	}
	return 0
}
