package driverrepo

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
)

const (
	sourcefulIndexSchema              = "sourceful.driver-index/v1"
	sourcefulIndexEnvelopeSchema      = "sourceful.driver-index-envelope/v1"
	sourcefulIndexPayloadType         = "application/vnd.sourceful.driver-index.v1+json"
	sourcefulPackageSchema            = "sourceful.driver-package/v1"
	sourcefulPackageEnvelopeSchema    = "sourceful.driver-package-envelope/v1"
	sourcefulPackagePayloadType       = "application/vnd.sourceful.driver-package.v1+json"
	sourcefulCanonicalJSON            = "sourceful.canonical-json/v1"
	sourcefulFTWTarget                = "ftw-core"
	sourcefulFTWHostProduct           = "ftw"
	sourcefulFTWRuntime               = "gopher-lua"
	sourcefulFTWSemantics             = "lua-5.1"
	sourcefulFTWRuntimeVersion        = "1.1.2"
	sourcefulFTWABIV1                 = "gopher-lua-source-v1"
	sourcefulFTWHostAPIProfileV1      = "sourceful.host/ftw-core/v1"
	sourcefulFTWABIV2                 = drivers.ControlRuntimeABIV2
	sourcefulFTWHostAPIProfileV2      = drivers.ControlHostAPIProfileV2
	sourcefulInstalledPackageEnvelope = "sourceful-package.envelope.json"
)

var (
	sourcefulPackageIDRE = regexp.MustCompile(`^com\.sourceful\.driver\.([a-z0-9]+(?:[.-][a-z0-9]+)*)$`)
	sourcefulHashRE      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	sourcefulCommitRE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sourcefulKeyIDRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{2,127}$`)
	sourcefulCommandRE   = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)
)

type sourcefulSignedEnvelope struct {
	SchemaVersion    string          `json:"schema_version"`
	PayloadType      string          `json:"payload_type"`
	Canonicalization string          `json:"canonicalization"`
	KeyID            string          `json:"key_id"`
	Algorithm        string          `json:"algorithm"`
	Payload          json.RawMessage `json:"payload"`
	Signature        string          `json:"signature"`
}

type sourcefulDriverIndex struct {
	SchemaVersion   string                      `json:"schema_version"`
	Channel         string                      `json:"channel"`
	SourceDateEpoch int64                       `json:"source_date_epoch"`
	Packages        []sourcefulDriverPackageRef `json:"packages"`
}

type sourcefulDriverPackageRef struct {
	PackageID      string   `json:"package_id"`
	Version        string   `json:"version"`
	EnvelopeURL    string   `json:"envelope_url"`
	EnvelopeSHA256 string   `json:"envelope_sha256"`
	Targets        []string `json:"targets"`
}

type sourcefulPackage struct {
	SchemaVersion string                   `json:"schema_version"`
	PackageID     string                   `json:"package_id"`
	Version       string                   `json:"version"`
	Channel       string                   `json:"channel"`
	DisplayName   string                   `json:"display_name"`
	Identity      sourcefulIdentity        `json:"identity"`
	Source        sourcefulSource          `json:"source"`
	Provenance    sourcefulProvenance      `json:"provenance"`
	DeviceMatches []sourcefulDeviceMatch   `json:"device_matches"`
	Capabilities  sourcefulCapabilities    `json:"capabilities"`
	Permissions   []string                 `json:"permissions"`
	Telemetry     sourcefulTelemetry       `json:"telemetry"`
	Commands      []sourcefulCommand       `json:"commands"`
	ReadOnly      bool                     `json:"read_only"`
	DefaultMode   sourcefulDefaultMode     `json:"default_mode"`
	LeasePolicy   sourcefulLeasePolicy     `json:"lease_policy"`
	Rollback      sourcefulRollback        `json:"rollback"`
	Compatibility []sourcefulCompatibility `json:"compatibility"`
	Artifacts     []sourcefulArtifact      `json:"artifacts"`
}

type sourcefulIdentity struct {
	Schema               string   `json:"schema"`
	Make                 string   `json:"make"`
	Serial               string   `json:"serial"`
	HostFallbacks        []string `json:"host_fallbacks"`
	PersistentStateOwner string   `json:"persistent_state_owner"`
}

type sourcefulSource struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Path       string `json:"path"`
}

type sourcefulProvenance struct {
	BuilderID       string                        `json:"builder_id"`
	BuildType       string                        `json:"build_type"`
	SourceDateEpoch int64                         `json:"source_date_epoch"`
	Materials       []sourcefulProvenanceMaterial `json:"materials"`
}

type sourcefulProvenanceMaterial struct {
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
}

type sourcefulDeviceMatch struct {
	Manufacturer string   `json:"manufacturer"`
	ModelFamily  string   `json:"model_family"`
	Variants     []string `json:"variants"`
	Regions      []string `json:"regions"`
}

type sourcefulCapabilities struct {
	Telemetry []string `json:"telemetry"`
	Control   []string `json:"control"`
}

type sourcefulTelemetry struct {
	Schema         string                     `json:"schema"`
	SignConvention string                     `json:"sign_convention"`
	Streams        []sourcefulTelemetryStream `json:"streams"`
}

type sourcefulTelemetryStream struct {
	Kind       string `json:"kind"`
	PowerField string `json:"power_field"`
	Meaning    string `json:"meaning"`
}

type sourcefulCommand struct {
	ID            string                  `json:"id"`
	Capability    string                  `json:"capability"`
	RuntimeAction string                  `json:"runtime_action"`
	Description   string                  `json:"description"`
	Inputs        []sourcefulCommandInput `json:"inputs"`
}

type sourcefulCommandInput struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Unit     string `json:"unit"`
	Required bool   `json:"required"`
}

type sourcefulDefaultMode struct {
	Strategy    string `json:"strategy"`
	Entrypoint  string `json:"entrypoint,omitempty"`
	Description string `json:"description"`
}

type sourcefulLeasePolicy struct {
	RequiredForControl       bool   `json:"required_for_control"`
	MaxDurationSeconds       *int64 `json:"max_duration_seconds,omitempty"`
	HeartbeatIntervalSeconds *int64 `json:"heartbeat_interval_seconds,omitempty"`
	ExpiryAction             string `json:"expiry_action"`
}

type sourcefulRollback struct {
	Strategy   string `json:"strategy"`
	StateOwner string `json:"state_owner"`
	Automatic  bool   `json:"automatic"`
}

type sourcefulCompatibility struct {
	Target         string                        `json:"target"`
	ArtifactID     string                        `json:"artifact_id"`
	Host           sourcefulHostCompatibility    `json:"host"`
	Runtime        sourcefulRuntimeCompatibility `json:"runtime"`
	ControlEnabled bool                          `json:"control_enabled"`
	Constraints    *sourcefulConstraints         `json:"constraints,omitempty"`
}

type sourcefulHostCompatibility struct {
	Product             string `json:"product"`
	MinVersion          string `json:"min_version"`
	MaxVersionExclusive string `json:"max_version_exclusive,omitempty"`
}

type sourcefulRuntimeCompatibility struct {
	Name      string                        `json:"name"`
	Semantics string                        `json:"semantics"`
	Version   string                        `json:"version"`
	ABI       string                        `json:"abi"`
	HostAPI   sourcefulHostCompatibilityAPI `json:"host_api"`
}

type sourcefulHostCompatibilityAPI struct {
	Profile string `json:"profile"`
	Min     int    `json:"min"`
	Max     int    `json:"max"`
}

type sourcefulConstraints struct {
	MaxArtifactBytes int64 `json:"max_artifact_bytes,omitempty"`
	MaxInstances     int64 `json:"max_instances,omitempty"`
}

type sourcefulArtifact struct {
	ArtifactID string `json:"artifact_id"`
	Target     string `json:"target"`
	MediaType  string `json:"media_type"`
	Filename   string `json:"filename"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	SizeBytes  int64  `json:"size_bytes"`
}

func repositoryFormat(repo config.DriverRepositorySource) string {
	if repo.Format == "" {
		return config.DriverRepositoryFormatFTWManifestV1
	}
	return repo.Format
}

func (m *Manager) refreshSourceful(ctx context.Context, repo config.DriverRepositorySource) error {
	raw, err := m.fetch(ctx, repo.ManifestURL, maxManifestBytes, repo.AllowInsecure)
	if err != nil {
		m.recordError(repo, err)
		return err
	}
	packageCache := make(map[string][]byte)
	manifest, keyID, err := m.sourcefulManifest(repo, raw, func(ref sourcefulDriverPackageRef) ([]byte, error) {
		packageRaw, err := m.fetch(ctx, ref.EnvelopeURL, maxManifestBytes, repo.AllowInsecure)
		if err == nil {
			packageCache[ref.EnvelopeSHA256] = packageRaw
		}
		return packageRaw, err
	})
	if err != nil {
		m.recordError(repo, err)
		return err
	}
	for hash, packageRaw := range packageCache {
		if err := atomicWrite(m.sourcefulPackageCachePath(repo, hash), packageRaw, 0o600); err != nil {
			m.recordError(repo, err)
			return err
		}
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

func (m *Manager) cachedSourcefulManifest(repo config.DriverRepositorySource, raw []byte) (Manifest, string, error) {
	return m.sourcefulManifest(repo, raw, func(ref sourcefulDriverPackageRef) ([]byte, error) {
		return os.ReadFile(m.sourcefulPackageCachePath(repo, ref.EnvelopeSHA256))
	})
}

func (m *Manager) sourcefulPackageCachePath(repo config.DriverRepositorySource, hash string) string {
	return filepath.Join(m.root, "cache", safeSegment(repo.ID), "packages", hash+".json")
}

func (m *Manager) sourcefulManifest(
	repo config.DriverRepositorySource,
	indexRaw []byte,
	resolve func(sourcefulDriverPackageRef) ([]byte, error),
) (Manifest, string, error) {
	payloadRaw, keyID, err := verifySourcefulEnvelope(
		indexRaw, repo, sourcefulIndexEnvelopeSchema, sourcefulIndexPayloadType,
	)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("verify Sourceful index: %w", err)
	}
	var index sourcefulDriverIndex
	if err := strictJSON(payloadRaw, &index); err != nil {
		return Manifest{}, "", fmt.Errorf("decode signed Sourceful index: %w", err)
	}
	if err := validateSourcefulIndex(index, repo.AllowInsecure); err != nil {
		return Manifest{}, "", err
	}

	byDriver := make(map[string][]ManifestDriver)
	for _, ref := range index.Packages {
		if !containsString(ref.Targets, sourcefulFTWTarget) {
			continue
		}
		packageRaw, err := resolve(ref)
		if err != nil {
			return Manifest{}, "", fmt.Errorf("resolve %s@%s: %w", ref.PackageID, ref.Version, err)
		}
		sum := sha256.Sum256(packageRaw)
		if got := hex.EncodeToString(sum[:]); got != ref.EnvelopeSHA256 {
			return Manifest{}, "", fmt.Errorf("package envelope %s@%s sha256 %s, want %s", ref.PackageID, ref.Version, got, ref.EnvelopeSHA256)
		}
		packagePayloadRaw, packageKeyID, err := verifySourcefulEnvelope(
			packageRaw, repo, sourcefulPackageEnvelopeSchema, sourcefulPackagePayloadType,
		)
		if err != nil {
			return Manifest{}, "", fmt.Errorf("verify package %s@%s: %w", ref.PackageID, ref.Version, err)
		}
		var pkg sourcefulPackage
		if err := strictJSON(packagePayloadRaw, &pkg); err != nil {
			return Manifest{}, "", fmt.Errorf("decode package %s@%s: %w", ref.PackageID, ref.Version, err)
		}
		driver, compatible, err := m.sourcefulPackageDriver(pkg, ref, index.Channel, repo.AllowInsecure)
		if err != nil {
			return Manifest{}, "", fmt.Errorf("package %s@%s: %w", ref.PackageID, ref.Version, err)
		}
		if compatible {
			driver.PackageKeyID = packageKeyID
			byDriver[driver.ID] = append(byDriver[driver.ID], driver)
		}
	}

	manifest := Manifest{
		SchemaVersion: components.ComponentManifestSchemaVersion,
		Repository:    repo.ManifestURL,
		GeneratedAt:   time.Unix(index.SourceDateEpoch, 0).UTC(),
	}
	ids := make([]string, 0, len(byDriver))
	for id := range byDriver {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		versions := byDriver[id]
		sort.Slice(versions, func(i, j int) bool {
			return compareSemver(versions[i].Version, versions[j].Version) > 0
		})
		manifest.Drivers = append(manifest.Drivers, versions[0])
		manifest.History = append(manifest.History, versions[1:]...)
	}
	if err := validateManifest(manifest, repo.AllowInsecure); err != nil {
		return Manifest{}, "", fmt.Errorf("adapt Sourceful index: %w", err)
	}
	return manifest, keyID, nil
}

func validateSourcefulIndex(index sourcefulDriverIndex, allowInsecure bool) error {
	if index.SchemaVersion != sourcefulIndexSchema {
		return fmt.Errorf("Sourceful index schema %q is unsupported", index.SchemaVersion)
	}
	if index.Channel != "beta" && index.Channel != "stable" {
		return fmt.Errorf("Sourceful index channel %q is invalid", index.Channel)
	}
	if index.SourceDateEpoch < 0 {
		return errors.New("Sourceful index source_date_epoch is negative")
	}
	seen := make(map[string]bool, len(index.Packages))
	for _, ref := range index.Packages {
		if sourcefulPackageIDRE.FindStringSubmatch(ref.PackageID) == nil || !semverRE.MatchString(ref.Version) {
			return fmt.Errorf("Sourceful index has invalid package identity %s@%s", ref.PackageID, ref.Version)
		}
		identity := ref.PackageID + "\x00" + ref.Version
		if seen[identity] {
			return fmt.Errorf("Sourceful index has duplicate package %s@%s", ref.PackageID, ref.Version)
		}
		seen[identity] = true
		if !sourcefulHashRE.MatchString(ref.EnvelopeSHA256) {
			return fmt.Errorf("Sourceful index package %s has invalid envelope sha256", ref.PackageID)
		}
		if err := validateSourcefulURL(ref.EnvelopeURL, allowInsecure); err != nil {
			return fmt.Errorf("Sourceful index package %s envelope URL: %w", ref.PackageID, err)
		}
		if len(ref.Targets) == 0 {
			return fmt.Errorf("Sourceful index package %s has no targets", ref.PackageID)
		}
		targets := make(map[string]bool, len(ref.Targets))
		for _, target := range ref.Targets {
			if target != "ftw-core" && target != "blixt-l1" && target != "zap-firmware" {
				return fmt.Errorf("Sourceful index package %s has unknown target %q", ref.PackageID, target)
			}
			if targets[target] {
				return fmt.Errorf("Sourceful index package %s repeats target %q", ref.PackageID, target)
			}
			targets[target] = true
		}
	}
	return nil
}

func (m *Manager) sourcefulPackageDriver(
	pkg sourcefulPackage,
	ref sourcefulDriverPackageRef,
	channel string,
	allowInsecure bool,
) (ManifestDriver, bool, error) {
	match := sourcefulPackageIDRE.FindStringSubmatch(pkg.PackageID)
	if pkg.SchemaVersion != sourcefulPackageSchema || match == nil || pkg.PackageID != ref.PackageID ||
		pkg.Version != ref.Version || pkg.Channel != channel || !semverRE.MatchString(pkg.Version) {
		return ManifestDriver{}, false, errors.New("signed package identity, version, or channel does not match the index")
	}
	if pkg.DisplayName == "" || pkg.Identity.Schema != "sourceful.hardware-identity/v1" ||
		pkg.Identity.Make != "driver_reported" || pkg.Identity.PersistentStateOwner != "host" ||
		!sourcefulCommitRE.MatchString(pkg.Source.Commit) ||
		pkg.Provenance.BuildType != sourcefulPackageSchema || pkg.Provenance.SourceDateEpoch < 0 {
		return ManifestDriver{}, false, errors.New("signed package identity or provenance contract is invalid")
	}
	if err := validateSourcefulPackageMetadata(pkg); err != nil {
		return ManifestDriver{}, false, err
	}
	if err := validateSourcefulURL(pkg.Source.Repository, allowInsecure); err != nil {
		return ManifestDriver{}, false, fmt.Errorf("source repository: %w", err)
	}
	if len(pkg.DeviceMatches) == 0 || len(pkg.Capabilities.Telemetry) == 0 || len(pkg.Telemetry.Streams) == 0 ||
		pkg.Telemetry.Schema != "sourceful.telemetry/v2" ||
		pkg.Telemetry.SignConvention != "sourceful.site-import-positive/v1" {
		return ManifestDriver{}, false, errors.New("signed package device or telemetry contract is invalid")
	}
	var target *sourcefulCompatibility
	for i := range pkg.Compatibility {
		if pkg.Compatibility[i].Target != sourcefulFTWTarget {
			continue
		}
		if target != nil {
			return ManifestDriver{}, false, errors.New("duplicate ftw-core compatibility entries")
		}
		target = &pkg.Compatibility[i]
	}
	if target == nil {
		return ManifestDriver{}, false, errors.New("index advertises ftw-core but package has no ftw-core compatibility")
	}
	if target.Host.Product != sourcefulFTWHostProduct || !semverRE.MatchString(target.Host.MinVersion) ||
		(target.Host.MaxVersionExclusive != "" && !semverRE.MatchString(target.Host.MaxVersionExclusive)) {
		return ManifestDriver{}, false, errors.New("invalid FTW host compatibility")
	}
	if !semverRE.MatchString(m.hostVersion) {
		return ManifestDriver{}, false, fmt.Errorf("running FTW version %q is not a release SemVer", m.hostVersion)
	}
	if compareSemver(m.hostVersion, target.Host.MinVersion) < 0 ||
		(target.Host.MaxVersionExclusive != "" && compareSemver(m.hostVersion, target.Host.MaxVersionExclusive) >= 0) {
		return ManifestDriver{}, false, nil
	}
	controlV2 := !pkg.ReadOnly
	if pkg.ReadOnly {
		if err := validateSourcefulReadOnlyContract(pkg, *target); err != nil {
			return ManifestDriver{}, false, err
		}
	} else {
		// A staged control source with control_enabled=false stays out of the
		// FTW catalog. It cannot be installed or activated by accident.
		if !target.ControlEnabled {
			return ManifestDriver{}, false, nil
		}
		if err := validateSourcefulControlContract(pkg); err != nil {
			return ManifestDriver{}, false, err
		}
	}

	runtime := target.Runtime
	wantABI, wantProfile, wantAPI := sourcefulFTWABIV1, sourcefulFTWHostAPIProfileV1, 1
	if controlV2 {
		wantABI, wantProfile, wantAPI = sourcefulFTWABIV2, sourcefulFTWHostAPIProfileV2, 2
	}
	if runtime.Name != sourcefulFTWRuntime || runtime.Semantics != sourcefulFTWSemantics ||
		runtime.Version != sourcefulFTWRuntimeVersion || runtime.ABI != wantABI ||
		runtime.HostAPI.Profile != wantProfile || runtime.HostAPI.Min != wantAPI || runtime.HostAPI.Max != wantAPI ||
		wantAPI < components.DriverHostAPIMinVersion || wantAPI > components.DriverHostAPIVersion {
		return ManifestDriver{}, false, nil
	}

	artifacts := make(map[string]sourcefulArtifact, len(pkg.Artifacts))
	for _, artifact := range pkg.Artifacts {
		if _, exists := artifacts[artifact.ArtifactID]; exists {
			return ManifestDriver{}, false, fmt.Errorf("duplicate artifact id %q", artifact.ArtifactID)
		}
		if artifact.Target != "ftw-core" && artifact.Target != "blixt-l1" && artifact.Target != "zap-firmware" {
			return ManifestDriver{}, false, fmt.Errorf("artifact %q has unknown target %q", artifact.ArtifactID, artifact.Target)
		}
		if !sourcefulHashRE.MatchString(artifact.SHA256) || artifact.SizeBytes <= 0 {
			return ManifestDriver{}, false, fmt.Errorf("artifact %q has invalid hash or size", artifact.ArtifactID)
		}
		if err := validateSourcefulURL(artifact.URL, allowInsecure); err != nil {
			return ManifestDriver{}, false, fmt.Errorf("artifact %q URL: %w", artifact.ArtifactID, err)
		}
		artifacts[artifact.ArtifactID] = artifact
	}
	artifact, ok := artifacts[target.ArtifactID]
	if !ok {
		return ManifestDriver{}, false, errors.New("FTW compatibility references a missing artifact")
	}
	if artifact.Target != sourcefulFTWTarget || artifact.MediaType != "application/vnd.sourceful.lua.source" ||
		!sourcefulHashRE.MatchString(artifact.SHA256) || artifact.SizeBytes <= 0 || artifact.SizeBytes > maxDriverBytes {
		return ManifestDriver{}, false, errors.New("FTW artifact metadata is incompatible")
	}
	if target.Constraints != nil && target.Constraints.MaxArtifactBytes > 0 && artifact.SizeBytes > target.Constraints.MaxArtifactBytes {
		return ManifestDriver{}, false, errors.New("FTW artifact exceeds package target constraint")
	}
	if err := validateSourcefulURL(artifact.URL, allowInsecure); err != nil {
		return ManifestDriver{}, false, fmt.Errorf("FTW artifact URL: %w", err)
	}
	artifactURL, _ := url.Parse(artifact.URL)
	if filepath.Base(artifactURL.Path) != artifact.Filename {
		return ManifestDriver{}, false, errors.New("FTW artifact URL does not end in its signed filename")
	}

	id := match[1]
	if safeSegment(id) != id {
		return ManifestDriver{}, false, fmt.Errorf("package id maps to unsafe FTW driver id %q", id)
	}
	metadata := drivers.CatalogEntry{
		Path: "drivers/" + id + ".lua", Filename: id + ".lua", ID: id,
		Name: pkg.DisplayName, Manufacturer: pkg.DeviceMatches[0].Manufacturer,
		Version: pkg.Version, HostAPIMin: runtime.HostAPI.Min, HostAPIMax: runtime.HostAPI.Max,
		Source: "upstream", Protocols: sourcefulProtocols(pkg.Permissions),
		Capabilities: sourcefulFTWCapabilities(pkg.Capabilities.Telemetry),
		Description:  "Signed Sourceful driver package from drivers.sourceful.energy.",
		ReadOnly:     pkg.ReadOnly, VerificationStatus: "experimental",
		TestedModels: sourcefulTestedModels(pkg.DeviceMatches),
	}
	return ManifestDriver{
		ID: id, Path: metadata.Path, Filename: metadata.Filename, Version: pkg.Version,
		SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes, URL: artifact.URL,
		HostAPI:  components.CompatibleRange{Min: runtime.HostAPI.Min, Max: runtime.HostAPI.Max},
		Metadata: metadata, PackageID: pkg.PackageID, Target: sourcefulFTWTarget,
		ArtifactID: target.ArtifactID, RuntimeName: runtime.Name, RuntimeSemantics: runtime.Semantics,
		RuntimeVersion: runtime.Version, RuntimeABI: runtime.ABI, HostAPIProfile: runtime.HostAPI.Profile,
		PackageEnvelopeURL:    ref.EnvelopeURL,
		PackageEnvelopeSHA256: ref.EnvelopeSHA256, SourceCommit: pkg.Source.Commit, Channel: pkg.Channel,
		ControlEnabled: target.ControlEnabled, ReadOnly: pkg.ReadOnly,
		Permissions: append([]string(nil), pkg.Permissions...), Commands: append([]sourcefulCommand(nil), pkg.Commands...),
		DefaultMode: pkg.DefaultMode, LeasePolicy: pkg.LeasePolicy,
	}, true, nil
}

// RuntimePolicy verifies signed metadata for an active managed artifact and
// derives its host permissions without a network call. Local and bundled
// drivers return nil.
func (m *Manager) RuntimePolicy(cfg config.Driver) (*drivers.RuntimePolicy, error) {
	if m.store == nil {
		if cfg.Control != nil && cfg.Control.Enabled {
			return nil, errors.New("control opt-in requires the managed driver state store")
		}
		return nil, nil
	}
	resolved, err := filepath.EvalSymlinks(cfg.Lua)
	installedRoot, rootErr := filepath.EvalSymlinks(filepath.Join(m.root, "installed"))
	if err != nil || rootErr != nil || !pathInside(installedRoot, resolved) {
		if cfg.Control != nil && cfg.Control.Enabled {
			return nil, errors.New("control opt-in requires an active managed Device Support artifact")
		}
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
		if cfg.Control != nil && cfg.Control.Enabled {
			return nil, errors.New("control opt-in requires the active managed artifact")
		}
		return nil, nil
	}
	var repo *config.DriverRepositorySource
	for i := range m.cfg.Repositories {
		if m.cfg.Repositories[i].ID == installed.RepoID {
			repo = &m.cfg.Repositories[i]
			break
		}
	}
	if repo == nil {
		if cfg.Control != nil && cfg.Control.Enabled {
			return nil, errors.New("control opt-in requires a configured Device Support trust root")
		}
		return nil, nil
	}
	if repositoryFormat(*repo) != config.DriverRepositoryFormatSourcefulIndexV1 {
		return m.directManifestRuntimePolicy(cfg, *repo, installed)
	}
	packageRaw, err := readLimitedFile(filepath.Join(filepath.Dir(resolved), sourcefulInstalledPackageEnvelope), maxManifestBytes)
	if err != nil {
		if cfg.Control != nil && cfg.Control.Enabled {
			return nil, fmt.Errorf("read installed signed package envelope: %w", err)
		}
		return nil, nil
	}
	payloadRaw, _, err := verifySourcefulEnvelope(packageRaw, *repo, sourcefulPackageEnvelopeSchema, sourcefulPackagePayloadType)
	if err != nil {
		return nil, fmt.Errorf("verify installed signed package envelope: %w", err)
	}
	var pkg sourcefulPackage
	if err := strictJSON(payloadRaw, &pkg); err != nil {
		return nil, fmt.Errorf("decode installed signed package: %w", err)
	}
	envelopeHash := sha256.Sum256(packageRaw)
	ref := sourcefulDriverPackageRef{
		PackageID: pkg.PackageID, Version: pkg.Version,
		EnvelopeURL:    "https://installed.invalid/" + sourcefulInstalledPackageEnvelope,
		EnvelopeSHA256: hex.EncodeToString(envelopeHash[:]), Targets: []string{sourcefulFTWTarget},
	}
	entry, compatible, err := m.sourcefulPackageDriver(pkg, ref, pkg.Channel, repo.AllowInsecure)
	if err != nil {
		return nil, fmt.Errorf("validate installed signed package: %w", err)
	}
	if !compatible {
		if cfg.Control != nil && cfg.Control.Enabled {
			return nil, errors.New("control opt-in targets an artifact without an enabled FTW v2 control target")
		}
		return nil, nil
	}
	if !strings.EqualFold(entry.SHA256, installed.SHA256) || entry.Version != installed.Version || entry.ID != installed.DriverID {
		return nil, errors.New("installed artifact does not match its signed package envelope")
	}
	permissions := make(map[string]bool, len(entry.Permissions))
	for _, permission := range entry.Permissions {
		permissions[permission] = true
	}
	if entry.ReadOnly {
		if cfg.Control != nil && cfg.Control.Enabled {
			return nil, errors.New("control opt-in targets a read-only signed artifact")
		}
		return &drivers.RuntimePolicy{
			PackageID: entry.PackageID, Version: entry.Version, ArtifactSHA256: strings.ToLower(entry.SHA256),
			RuntimeABI: entry.RuntimeABI, HostAPIProfile: entry.HostAPIProfile,
			ReadOnly: true, Permissions: permissions,
		}, nil
	}
	if !entry.ControlEnabled {
		if cfg.Control != nil && cfg.Control.Enabled {
			return nil, errors.New("control opt-in targets an artifact without an enabled FTW v2 control target")
		}
		return nil, nil
	}

	siteEnabled := false
	if cfg.Control != nil && cfg.Control.Enabled {
		if cfg.Control.PackageID != entry.PackageID || cfg.Control.Version != entry.Version ||
			!strings.EqualFold(strings.TrimSpace(cfg.Control.ArtifactSHA256), entry.SHA256) {
			return nil, errors.New("control opt-in package/version/hash pin does not match the active signed artifact")
		}
		siteEnabled = true
	}
	commands := make(map[string]drivers.RuntimeCommand, len(entry.Commands))
	for _, command := range entry.Commands {
		inputs := make(map[string]drivers.RuntimeCommandInput, len(command.Inputs))
		for _, input := range command.Inputs {
			inputs[input.Name] = drivers.RuntimeCommandInput{Type: input.Type, Required: input.Required}
		}
		commands[command.ID] = drivers.RuntimeCommand{
			ID: command.ID, RuntimeAction: command.RuntimeAction, Inputs: inputs,
		}
	}
	return &drivers.RuntimePolicy{
		PackageID: entry.PackageID, Version: entry.Version, ArtifactSHA256: strings.ToLower(entry.SHA256),
		RuntimeABI: entry.RuntimeABI, HostAPIProfile: entry.HostAPIProfile,
		Permissions: permissions, Commands: commands, DefaultMode: entry.DefaultMode.Entrypoint,
		Lease: drivers.RuntimeLeasePolicy{
			MaxDuration:       time.Duration(*entry.LeasePolicy.MaxDurationSeconds) * time.Second,
			HeartbeatInterval: time.Duration(*entry.LeasePolicy.HeartbeatIntervalSeconds) * time.Second,
			ExpiryAction:      entry.LeasePolicy.ExpiryAction,
		},
		SiteEnabled: siteEnabled, MaxWrites: 128,
	}, nil
}

func (m *Manager) directManifestRuntimePolicy(
	cfg config.Driver,
	repo config.DriverRepositorySource,
	installed state.DriverRepoInstall,
) (*drivers.RuntimePolicy, error) {
	if cfg.Control != nil && cfg.Control.Enabled {
		return nil, errors.New("control opt-in requires a signed Device Support control package")
	}
	manifest, err := m.manifestFor(repo)
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
	if !matched.ReadOnly || !matched.Metadata.ReadOnly || matched.ControlEnabled {
		// This preserves the former runtime for old direct manifests. The new
		// public channel marks both manifest and Lua metadata as read-only.
		if installed.RepoURL == "https://github.com/srcfl/device-drivers" {
			return nil, errors.New("public FTW driver lacks signed read-only policy")
		}
		return nil, nil
	}
	permissions := make(map[string]bool, len(matched.Permissions))
	for _, permission := range matched.Permissions {
		permissions[permission] = true
	}
	return &drivers.RuntimePolicy{
		PackageID:      "com.sourceful.driver." + matched.ID,
		Version:        matched.Version,
		ArtifactSHA256: strings.ToLower(matched.SHA256),
		RuntimeABI:     sourcefulFTWABIV1,
		HostAPIProfile: sourcefulFTWHostAPIProfileV1,
		ReadOnly:       true,
		Permissions:    permissions,
	}, nil
}

func validateSourcefulReadOnlyContract(pkg sourcefulPackage, target sourcefulCompatibility) error {
	if !pkg.ReadOnly || target.ControlEnabled || len(pkg.Commands) != 0 || len(pkg.Capabilities.Control) != 0 ||
		pkg.DefaultMode.Strategy != "not_applicable" || pkg.DefaultMode.Entrypoint != "" ||
		pkg.LeasePolicy.RequiredForControl || pkg.LeasePolicy.MaxDurationSeconds != nil ||
		pkg.LeasePolicy.HeartbeatIntervalSeconds != nil || pkg.LeasePolicy.ExpiryAction != "not_applicable" {
		return errors.New("read-only package has a control contract")
	}
	for _, permission := range pkg.Permissions {
		switch permission {
		case "http.get", "modbus.read", "mqtt.subscribe", "serial.read":
		default:
			return fmt.Errorf("read-only package requests write-capable permission %q", permission)
		}
	}
	return nil
}

func validateSourcefulControlContract(pkg sourcefulPackage) error {
	if pkg.ReadOnly || len(pkg.Commands) == 0 || len(pkg.Capabilities.Control) == 0 ||
		pkg.DefaultMode.Strategy != "vendor_autonomous" || pkg.DefaultMode.Entrypoint != "driver_default_mode_v2" ||
		!pkg.LeasePolicy.RequiredForControl || pkg.LeasePolicy.MaxDurationSeconds == nil ||
		pkg.LeasePolicy.HeartbeatIntervalSeconds == nil || pkg.LeasePolicy.ExpiryAction != "return_to_default" {
		return errors.New("control package lacks the FTW v2 lifecycle contract")
	}
	maxSeconds, heartbeatSeconds := *pkg.LeasePolicy.MaxDurationSeconds, *pkg.LeasePolicy.HeartbeatIntervalSeconds
	if maxSeconds < 1 || maxSeconds > 300 || heartbeatSeconds < 1 || heartbeatSeconds > maxSeconds {
		return errors.New("control package lease bounds are invalid")
	}
	writePermission := false
	for _, permission := range pkg.Permissions {
		switch permission {
		case "modbus.read", "modbus.write":
		default:
			return fmt.Errorf("FTW control v2 only supports Modbus permissions, got %q", permission)
		}
		if permission == "modbus.write" {
			writePermission = true
		}
	}
	if !writePermission {
		return errors.New("control package has no write permission")
	}
	permissions := make(map[string]bool, len(pkg.Permissions))
	for _, permission := range pkg.Permissions {
		permissions[permission] = true
	}
	if permissions["modbus.write"] && !permissions["modbus.read"] {
		return errors.New("control package modbus writes require readback permission")
	}
	controlCapabilities := make(map[string]bool, len(pkg.Capabilities.Control))
	for _, capability := range pkg.Capabilities.Control {
		if !sourcefulCommandRE.MatchString(capability) || controlCapabilities[capability] {
			return errors.New("control package capabilities are invalid or duplicated")
		}
		controlCapabilities[capability] = true
	}
	commands := make(map[string]bool, len(pkg.Commands))
	runtimeActions := make(map[string]bool, len(pkg.Commands))
	for _, command := range pkg.Commands {
		if !sourcefulCommandRE.MatchString(command.ID) || !sourcefulCommandRE.MatchString(command.RuntimeAction) ||
			commands[command.ID] || runtimeActions[command.RuntimeAction] || !controlCapabilities[command.Capability] {
			return errors.New("control package command identity is invalid or duplicated")
		}
		commands[command.ID] = true
		runtimeActions[command.RuntimeAction] = true
		inputs := make(map[string]bool, len(command.Inputs))
		for _, input := range command.Inputs {
			if !sourcefulCommandRE.MatchString(input.Name) || inputs[input.Name] ||
				(input.Type != "number" && input.Type != "boolean" && input.Type != "string") {
				return fmt.Errorf("control package command %q has an invalid input", command.ID)
			}
			inputs[input.Name] = true
		}
	}
	return nil
}

func validateSourcefulPackageMetadata(pkg sourcefulPackage) error {
	if pkg.Identity.Serial != "driver_reported_when_available" && pkg.Identity.Serial != "unavailable" {
		return errors.New("signed package serial identity contract is invalid")
	}
	if len(pkg.Identity.HostFallbacks) == 0 {
		return errors.New("signed package has no identity fallback")
	}
	fallbacks := make(map[string]bool, len(pkg.Identity.HostFallbacks))
	for _, fallback := range pkg.Identity.HostFallbacks {
		if (fallback != "mac" && fallback != "endpoint") || fallbacks[fallback] {
			return errors.New("signed package identity fallbacks are invalid")
		}
		fallbacks[fallback] = true
	}
	if pkg.Source.Path == "" || strings.HasPrefix(pkg.Source.Path, "/") {
		return errors.New("signed package source path is invalid")
	}
	for _, segment := range strings.Split(filepath.ToSlash(pkg.Source.Path), "/") {
		if segment == ".." {
			return errors.New("signed package source path traverses its repository")
		}
	}
	builder, err := url.Parse(pkg.Provenance.BuilderID)
	if err != nil || builder.Scheme == "" || len(pkg.Provenance.Materials) == 0 {
		return errors.New("signed package provenance builder or materials are invalid")
	}
	materials := make(map[string]bool, len(pkg.Provenance.Materials))
	for _, material := range pkg.Provenance.Materials {
		if material.URI == "" || !sourcefulHashRE.MatchString(material.SHA256) || materials[material.URI] {
			return errors.New("signed package provenance material is invalid or duplicated")
		}
		materials[material.URI] = true
	}
	if pkg.Rollback.Strategy != "install_previous_verified_package" || pkg.Rollback.StateOwner != "host" {
		return errors.New("signed package rollback contract is invalid")
	}
	knownCapability := func(value string) bool {
		switch value {
		case "battery", "evse", "meter", "pv", "v2x_charger":
			return true
		default:
			return false
		}
	}
	capabilities := make(map[string]bool)
	for _, capability := range pkg.Capabilities.Telemetry {
		if !knownCapability(capability) || capabilities[capability] {
			return errors.New("signed package telemetry capabilities are invalid")
		}
		capabilities[capability] = true
	}
	for _, stream := range pkg.Telemetry.Streams {
		if !knownCapability(stream.Kind) || stream.PowerField == "" || stream.Meaning == "" {
			return errors.New("signed package telemetry stream is invalid")
		}
	}
	permissions := make(map[string]bool, len(pkg.Permissions))
	for _, permission := range pkg.Permissions {
		if permissions[permission] {
			return fmt.Errorf("signed package repeats permission %q", permission)
		}
		permissions[permission] = true
	}
	targets := make(map[string]bool, len(pkg.Compatibility))
	for _, compatibility := range pkg.Compatibility {
		if targets[compatibility.Target] {
			return fmt.Errorf("signed package repeats compatibility target %q", compatibility.Target)
		}
		targets[compatibility.Target] = true
	}
	for _, match := range pkg.DeviceMatches {
		if match.Manufacturer == "" || match.ModelFamily == "" {
			return errors.New("signed package device match is invalid")
		}
	}
	return nil
}

func verifySourcefulEnvelope(
	raw []byte,
	repo config.DriverRepositorySource,
	wantSchema string,
	wantPayloadType string,
) ([]byte, string, error) {
	if repo.AllowUnsigned {
		return nil, "", errors.New("Sourceful contracts cannot be unsigned")
	}
	if _, err := canonicalJSON(raw); err != nil {
		return nil, "", fmt.Errorf("invalid JSON contract: %w", err)
	}
	var envelope sourcefulSignedEnvelope
	if err := strictJSON(raw, &envelope); err != nil {
		return nil, "", err
	}
	if envelope.SchemaVersion != wantSchema || envelope.PayloadType != wantPayloadType ||
		envelope.Canonicalization != sourcefulCanonicalJSON || envelope.Algorithm != "Ed25519" ||
		!sourcefulKeyIDRE.MatchString(envelope.KeyID) {
		return nil, "", errors.New("unsupported Sourceful signed envelope contract")
	}
	encodedKey, ok := repo.TrustedKeys[envelope.KeyID]
	if !ok {
		return nil, "", fmt.Errorf("Sourceful envelope key_id %q is not trusted", envelope.KeyID)
	}
	key, err := decodeBase64(encodedKey)
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, "", fmt.Errorf("invalid Ed25519 public key %q", envelope.KeyID)
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, "", errors.New("invalid Sourceful signature encoding")
	}
	canonical, err := canonicalJSON(envelope.Payload)
	if err != nil {
		return nil, "", err
	}
	if !ed25519.Verify(ed25519.PublicKey(key), canonical, signature) {
		return nil, "", errors.New("Sourceful signature verification failed")
	}
	return envelope.Payload, envelope.KeyID, nil
}

// canonicalJSON implements Sourceful Canonical JSON v1 for the contract's
// integer-only number domain. Parsing before encoding rejects duplicate keys;
// object keys are sorted by encoding/json and HTML escaping is disabled to
// match the Device Support Python producer.
func canonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeJSONValue(decoder)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("JSON has trailing values")
		}
		return nil, err
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

func decodeJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("JSON object key is not a string")
				}
				if _, exists := object[key]; exists {
					return nil, fmt.Errorf("duplicate JSON object key %q", key)
				}
				child, err := decodeJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				object[key] = child
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return nil, errors.New("unterminated JSON object")
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for decoder.More() {
				child, err := decodeJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				array = append(array, child)
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return nil, errors.New("unterminated JSON array")
			}
			return array, nil
		default:
			return nil, errors.New("unexpected JSON delimiter")
		}
	case json.Number:
		integer, err := strconv.ParseInt(value.String(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("Sourceful contract number %q is not an integer", value)
		}
		return integer, nil
	default:
		return value, nil
	}
}

func strictJSON(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON has trailing values")
		}
		return err
	}
	return nil
}

func validateSourcefulURL(rawURL string, allowInsecure bool) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must be an absolute URL without credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(allowInsecure && u.Scheme == "http") {
		return errors.New("must use https")
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sourcefulProtocols(permissions []string) []string {
	set := make(map[string]bool)
	for _, permission := range permissions {
		protocol, _, _ := strings.Cut(permission, ".")
		set[protocol] = true
	}
	out := make([]string, 0, len(set))
	for protocol := range set {
		out = append(out, protocol)
	}
	sort.Strings(out)
	return out
}

func sourcefulFTWCapabilities(capabilities []string) []string {
	mapping := map[string]string{
		"battery": "battery", "evse": "ev", "meter": "meter", "pv": "pv", "v2x_charger": "v2x",
	}
	set := make(map[string]bool)
	for _, capability := range capabilities {
		if mapped := mapping[capability]; mapped != "" {
			set[mapped] = true
		}
	}
	out := make([]string, 0, len(set))
	for capability := range set {
		out = append(out, capability)
	}
	sort.Strings(out)
	return out
}

func sourcefulTestedModels(matches []sourcefulDeviceMatch) []string {
	set := make(map[string]bool)
	for _, match := range matches {
		if match.ModelFamily != "" {
			set[match.ModelFamily] = true
		}
		for _, variant := range match.Variants {
			set[variant] = true
		}
	}
	out := make([]string, 0, len(set))
	for model := range set {
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}
