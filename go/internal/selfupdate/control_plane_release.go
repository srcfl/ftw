package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const controlPlaneManifestSchema = 1

var (
	sha256DigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	gitRevisionPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type controlPlaneReleaseManifest struct {
	SchemaVersion int    `json:"schema_version"`
	Release       string `json:"release"`
	Revision      string `json:"revision"`
	Components    struct {
		Core    controlPlaneReleaseComponent `json:"core"`
		Updater controlPlaneReleaseComponent `json:"updater"`
	} `json:"components"`
}

type controlPlaneReleaseComponent struct {
	Image  string `json:"image"`
	Digest string `json:"digest"`
}

type verifiedControlPlaneRelease struct {
	Target        string
	Revision      string
	CoreDigest    string
	UpdaterDigest string
}

func (c *Checker) verifyControlPlaneRelease(ctx context.Context, rel ghRelease, targetTag string) (verifiedControlPlaneRelease, bool, error) {
	assetURL := ""
	for _, asset := range rel.Assets {
		if asset.Name == c.cfg.PairManifestAsset {
			assetURL = asset.BrowserDownloadURL
			break
		}
	}
	if assetURL == "" {
		return verifiedControlPlaneRelease{}, false, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return verifiedControlPlaneRelease{}, false, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "FTW-selfupdate")
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return verifiedControlPlaneRelease{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return verifiedControlPlaneRelease{}, false, nil
	}
	if resp.StatusCode >= 400 {
		return verifiedControlPlaneRelease{}, false, fmt.Errorf("pair manifest HTTP %d", resp.StatusCode)
	}
	var manifest controlPlaneReleaseManifest
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return verifiedControlPlaneRelease{}, false, fmt.Errorf("decode pair manifest: %w", err)
	}
	if err := c.validateControlPlaneManifest(manifest, targetTag); err != nil {
		return verifiedControlPlaneRelease{}, false, err
	}

	coreProbe := &registryProbe{httpClient: c.cfg.HTTPClient, base: c.cfg.RegistryBaseURL, repo: c.cfg.Image, service: c.cfg.RegistryService}
	updaterProbe := &registryProbe{httpClient: c.cfg.HTTPClient, base: c.cfg.RegistryBaseURL, repo: c.cfg.PairedImage, service: c.cfg.RegistryService}
	coreDigest, err := coreProbe.manifestDigest(ctx, targetTag)
	if err != nil {
		return verifiedControlPlaneRelease{}, false, fmt.Errorf("Core digest: %w", err)
	}
	updaterDigest, err := updaterProbe.manifestDigest(ctx, targetTag)
	if err != nil {
		return verifiedControlPlaneRelease{}, false, fmt.Errorf("updater digest: %w", err)
	}
	if coreDigest == "" || updaterDigest == "" {
		return verifiedControlPlaneRelease{}, false, nil
	}
	if coreDigest != manifest.Components.Core.Digest {
		return verifiedControlPlaneRelease{}, false, fmt.Errorf("Core tag digest %s does not match release manifest %s", coreDigest, manifest.Components.Core.Digest)
	}
	if updaterDigest != manifest.Components.Updater.Digest {
		return verifiedControlPlaneRelease{}, false, fmt.Errorf("updater tag digest %s does not match release manifest %s", updaterDigest, manifest.Components.Updater.Digest)
	}
	return verifiedControlPlaneRelease{
		Target: targetTag, Revision: manifest.Revision,
		CoreDigest: coreDigest, UpdaterDigest: updaterDigest,
	}, true, nil
}

func (c *Checker) validateControlPlaneManifest(manifest controlPlaneReleaseManifest, targetTag string) error {
	if manifest.SchemaVersion != controlPlaneManifestSchema {
		return fmt.Errorf("unsupported pair manifest schema %d", manifest.SchemaVersion)
	}
	if manifest.Release != targetTag {
		return fmt.Errorf("pair manifest release %q does not match %q", manifest.Release, targetTag)
	}
	if !gitRevisionPattern.MatchString(manifest.Revision) {
		return errors.New("pair manifest has invalid source revision")
	}
	wantCore := c.cfg.Image + ":" + targetTag
	wantUpdater := c.cfg.PairedImage + ":" + targetTag
	if trimRegistryHost(manifest.Components.Core.Image) != wantCore {
		return fmt.Errorf("pair manifest Core image %q does not match %q", manifest.Components.Core.Image, wantCore)
	}
	if trimRegistryHost(manifest.Components.Updater.Image) != wantUpdater {
		return fmt.Errorf("pair manifest updater image %q does not match %q", manifest.Components.Updater.Image, wantUpdater)
	}
	if !sha256DigestPattern.MatchString(manifest.Components.Core.Digest) || !sha256DigestPattern.MatchString(manifest.Components.Updater.Digest) {
		return errors.New("pair manifest contains an invalid image digest")
	}
	return nil
}

func trimRegistryHost(image string) string {
	image = strings.TrimPrefix(image, "https://")
	image = strings.TrimPrefix(image, "http://")
	if slash := strings.IndexByte(image, '/'); slash >= 0 {
		first := image[:slash]
		if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
			return image[slash+1:]
		}
	}
	return image
}
