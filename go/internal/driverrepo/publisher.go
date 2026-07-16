package driverrepo

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/components"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// PublicationOptions turns the monorepo's drivers directory into the static,
// independently releasable artifact set consumed by Manager. PrivateKey is
// never persisted; only the signed envelope and public driver contents are
// written below OutputDir.
type PublicationOptions struct {
	DriversDir        string
	OutputDir         string
	BaseURL           string
	Repository        string
	Commit            string
	KeyID             string
	PrivateKey        ed25519.PrivateKey
	ExpectedPublicKey ed25519.PublicKey
	GeneratedAt       time.Time
	Unsigned          bool
}

type Publication struct {
	Manifest      Manifest
	ManifestPath  string
	PayloadPath   string
	ArtifactPaths []string
	PublicKey     ed25519.PublicKey
}

var explicitHostAPIRE = regexp.MustCompile(`(?m)^\s*host_api_(?:min|max)\s*=\s*[0-9]+`)

// BuildPublication validates every Lua driver, creates immutable hash-named
// artifacts, signs the exact compact JSON payload, and atomically replaces the
// requested staging directory.
func BuildPublication(opts PublicationOptions) (Publication, error) {
	if opts.DriversDir == "" {
		return Publication{}, errors.New("drivers directory is required")
	}
	if err := validatePublicationOutput(opts.OutputDir); err != nil {
		return Publication{}, err
	}
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return Publication{}, errors.New("publication base URL must be absolute HTTPS")
	}
	if opts.Repository == "" {
		return Publication{}, errors.New("repository URL is required")
	}
	if !opts.Unsigned {
		if opts.KeyID == "" {
			return Publication{}, errors.New("signing key ID is required")
		}
		if len(opts.PrivateKey) != ed25519.PrivateKeySize {
			return Publication{}, errors.New("Ed25519 private key is required")
		}
	}

	entries, err := os.ReadDir(opts.DriversDir)
	if err != nil {
		return Publication{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	parent := filepath.Dir(filepath.Clean(opts.OutputDir))
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return Publication{}, err
	}
	staging, err := os.MkdirTemp(parent, ".driver-publication-*")
	if err != nil {
		return Publication{}, err
	}
	defer os.RemoveAll(staging)

	manifest := Manifest{
		SchemaVersion: components.ComponentManifestSchemaVersion,
		Repository:    opts.Repository,
		Commit:        opts.Commit,
		GeneratedAt:   opts.GeneratedAt.UTC(),
	}
	if manifest.GeneratedAt.IsZero() {
		manifest.GeneratedAt = time.Now().UTC()
	}
	seenIDs := make(map[string]string)
	var artifactNames []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".lua" {
			continue
		}
		sourcePath := filepath.Join(opts.DriversDir, entry.Name())
		raw, err := os.ReadFile(sourcePath)
		if err != nil {
			return Publication{}, err
		}
		metadata, err := drivers.ParseCatalogFile(sourcePath)
		if err != nil {
			return Publication{}, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if count := len(explicitHostAPIRE.FindAll(raw, -1)); count != 2 {
			return Publication{}, fmt.Errorf("%s must explicitly declare host_api_min and host_api_max", entry.Name())
		}
		if previous, exists := seenIDs[metadata.ID]; exists {
			return Publication{}, fmt.Errorf("duplicate driver id %q in %s and %s", metadata.ID, previous, entry.Name())
		}
		seenIDs[metadata.ID] = entry.Name()

		sum := sha256.Sum256(raw)
		digest := hex.EncodeToString(sum[:])
		artifactName := fmt.Sprintf("driver-%s-v%s-%s.lua", safeSegment(metadata.ID), metadata.Version, digest[:16])
		artifactPath := filepath.Join(staging, artifactName)
		if err := atomicWrite(artifactPath, raw, 0o644); err != nil {
			return Publication{}, err
		}
		logicalPath := filepath.ToSlash(filepath.Join("drivers", entry.Name()))
		metadata.Path = logicalPath
		metadata.Filename = entry.Name()
		metadata.Source = "upstream"
		driver := ManifestDriver{
			ID: metadata.ID, Path: logicalPath, Filename: entry.Name(), Version: metadata.Version,
			SHA256: digest, URL: baseURL + "/" + artifactName,
			HostAPI:  components.CompatibleRange{Min: metadata.HostAPIMin, Max: metadata.HostAPIMax},
			Metadata: metadata,
		}
		if err := validateLuaArtifact(artifactPath, driver); err != nil {
			return Publication{}, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		manifest.Drivers = append(manifest.Drivers, driver)
		artifactNames = append(artifactNames, artifactName)
	}
	if len(manifest.Drivers) == 0 {
		return Publication{}, errors.New("drivers directory contains no Lua drivers")
	}
	if err := validateManifest(manifest, false); err != nil {
		return Publication{}, err
	}

	payload, err := json.Marshal(manifest)
	if err != nil {
		return Publication{}, err
	}
	manifestBytes := payload
	var publicKey ed25519.PublicKey
	if !opts.Unsigned {
		publicKey = append(ed25519.PublicKey(nil), opts.PrivateKey.Public().(ed25519.PublicKey)...)
		if len(opts.ExpectedPublicKey) > 0 && !publicKey.Equal(opts.ExpectedPublicKey) {
			return Publication{}, errors.New("signing private key does not match expected public key")
		}
		envelope := ManifestEnvelope{
			SchemaVersion: components.ComponentManifestSchemaVersion,
			KeyID:         opts.KeyID, Payload: payload,
			Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(opts.PrivateKey, payload)),
		}
		manifestBytes, err = json.Marshal(envelope)
		if err != nil {
			return Publication{}, err
		}
	}
	prettyPayload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Publication{}, err
	}
	if err := atomicWrite(filepath.Join(staging, "manifest.json"), append(manifestBytes, '\n'), 0o644); err != nil {
		return Publication{}, err
	}
	if err := atomicWrite(filepath.Join(staging, "manifest.payload.json"), append(prettyPayload, '\n'), 0o644); err != nil {
		return Publication{}, err
	}

	if err := os.RemoveAll(opts.OutputDir); err != nil {
		return Publication{}, err
	}
	if err := os.Rename(staging, opts.OutputDir); err != nil {
		return Publication{}, err
	}
	artifactPaths := make([]string, 0, len(artifactNames))
	for _, name := range artifactNames {
		artifactPaths = append(artifactPaths, filepath.Join(opts.OutputDir, name))
	}
	return Publication{
		Manifest: manifest, ManifestPath: filepath.Join(opts.OutputDir, "manifest.json"),
		PayloadPath:   filepath.Join(opts.OutputDir, "manifest.payload.json"),
		ArtifactPaths: artifactPaths, PublicKey: publicKey,
	}, nil
}

func validatePublicationOutput(path string) error {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	parent := filepath.Dir(clean)
	if path == "" || base == "." || base == ".." || clean == string(filepath.Separator) || parent == clean ||
		(filepath.IsAbs(clean) && parent == string(filepath.Separator)) {
		return errors.New("safe publication output directory is required")
	}
	return nil
}

// DecodePrivateKey accepts base64 of either a 32-byte Ed25519 seed or a
// 64-byte private key. CI stores this value in FTW_DRIVER_SIGNING_KEY.
func DecodePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := decodeBase64(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode Ed25519 private key: %w", err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(append([]byte(nil), raw...)), nil
	default:
		return nil, fmt.Errorf("Ed25519 private key is %d bytes, want %d-byte seed or %d-byte key", len(raw), ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

func DecodePublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := decodeBase64(strings.TrimSpace(encoded))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("invalid base64 Ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

// VerifyPublication is shared by the release workflow and tests so publishing
// cannot drift from the runtime client's signature and schema behavior.
func VerifyPublication(raw []byte, keyID, encodedPublicKey string) (Manifest, error) {
	manifest, _, err := verifyManifest(raw, config.DriverRepositorySource{
		TrustedKeys: map[string]string{keyID: encodedPublicKey},
	})
	if err != nil {
		return Manifest{}, err
	}
	if err := validateManifest(manifest, false); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// VerifyPublicationArtifacts checks the local pre-upload files referenced by
// a verified manifest. Release assets are flat, so URL basenames map exactly
// to files in publicationDir.
func VerifyPublicationArtifacts(manifest Manifest, publicationDir string) error {
	for _, driver := range manifest.Drivers {
		u, err := url.Parse(driver.URL)
		if err != nil {
			return err
		}
		path := filepath.Join(publicationDir, filepath.Base(u.Path))
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s: %w", driver.ID, err)
		}
		sum := sha256.Sum256(raw)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), driver.SHA256) {
			return fmt.Errorf("%s artifact hash mismatch", driver.ID)
		}
	}
	return nil
}
