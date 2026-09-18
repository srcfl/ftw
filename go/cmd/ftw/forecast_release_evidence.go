package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// forecastReleaseEvidence is only evidence about a curtail_disable command
// that the caller has already seen succeed. Generic default never qualifies.
// Config must contain the resolved Lua paths used by the driver host. Hashing
// runs at configuration time; the returned immutable lookup performs no I/O.
func forecastReleaseEvidence(cfg *config.Config, catalog []drivers.CatalogEntry) func(string) bool {
	return forecastReleaseEvidenceWithDigest(cfg, catalog, forecastReleaseScriptDigest)
}

func forecastReleaseEvidenceWithDigest(cfg *config.Config, catalog []drivers.CatalogEntry, digest func(string) (string, error)) func(string) bool {
	known := map[string]bool{}
	if cfg != nil {
		for _, d := range cfg.Drivers {
			if d.Disabled || d.ObserveOnly || d.BatteryTelemetryOnly || d.Name == "" {
				continue
			}
			e, found := forecastCatalogEntry(catalog, d.Lua)
			if !found || e.ReadOnly || !forecastCapability(e, "pv") {
				continue
			}
			hash, err := digest(d.Lua)
			if err == nil {
				known[d.Name] = forecastReviewedRelease(hash, d.Config)
			}
		}
	}
	return func(name string) bool { return known[name] }
}

func forecastReleaseScriptDigest(path string) (string, error) {
	// A relative lookup could inspect a different script from the one resolved
	// by the host. Unresolved or unreadable source cannot prove release behavior.
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("unresolved driver path")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", fmt.Errorf("invalid driver source size or type")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return "", err
	}
	if n > 1<<20 {
		return "", fmt.Errorf("driver source exceeds size limit")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// These exact public scripts attempt PV release and return write failures.
// Reviewed source and recovery baselines:
// https://github.com/srcfl/device-drivers/tree/0ce2c55db48f3ded7cf1aeeeec29e5caed09454b
// Unknown revisions remain censored until their release branch is reviewed.
// ArtifactSHA256 is deliberately unused: a signed package/archive digest is
// not the digest of the Lua bytes actually loaded by the host.
func forecastReviewedRelease(digest string, settings map[string]any) bool {
	switch digest {
	case "c04d137d595ba50b8c6178c82d917b115dbe9a7cbd2cf671ef2660e871f96de3": // ferroamp.lua
		// Without this value Ferroamp returns success but publishes nothing.
		return forecastReleaseWatts(settings["pplim_release_w"], math.MaxInt32)
	case "a11ddc8de433c527a3d63cd014d243b09f3393ee3afa9b8e1594d4de8de37562", // sungrow.lua
		"466a5f8637e6756fc2e1af4197d4edc1845474231413c0016f0e5900acb7b7ac":
		if settings["pv_curtail_method"] == "feed_in" {
			return forecastReleaseWatts(settings["feed_in_release_w"], math.MaxUint16)
		}
		return true // disable active-power limit, then restore its inert ratio
	case "94b5f18fb42c1413540d7321bca47ed755c8a0a9b069f8d51dbac4384910c496", // solaredge.lua
		"38e8b39254e3171d758cd27fa7a07bd87d62f94e62b74d2d87ad81eb19b53823",
		"8ea6381cdc608939919b8ff55c22c1febe8a8da5a0b81f93cbd341a7d1733467", // solaredge_pv.lua
		"d956b2473c27e52f63504862a85b36f803414327a583464fca087679c428de47",
		"7a590f5f35efc395754f002f0d48aef8b2d46cab525bf2ae863f4fd73b01781b", // solaredge_legacy.lua
		"669aa969467e81c337269ab0f5a2d01d01a0f270deb55491c6a5adc6df848a6f":
		return true // atomic APC enable=0, limit=100%; no nameplate needed
	}
	return false
}

func forecastReleaseWatts(value any, ceiling float64) bool {
	var w float64
	switch v := value.(type) {
	case int:
		w = float64(v)
	case int64:
		w = float64(v)
	case float64:
		w = v
	case json.Number:
		w, _ = v.Float64()
	case string:
		w, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
	default:
		return false
	}
	// Lua floors these settings before sending integer watts. Sub-watt values
	// are zero on the wire, and cannot qualify as a release ceiling.
	return !math.IsNaN(w) && !math.IsInf(w, 0) && w >= 1 && w <= ceiling
}

// Proof is bound to the loaded driver generation. An independent driver
// restart/update cannot inherit release semantics from the previous instance.
func forecastReleaseEvidenceForRegistry(cfg *config.Config, catalog []drivers.CatalogEntry, reg *drivers.Registry) func(string) bool {
	get := func(name string) (uint64, bool) {
		s, ok := reg.ControlStatus(name)
		return s.Generation, ok
	}
	return bindForecastReleaseGeneration(forecastReleaseEvidence(cfg, catalog), reg.Names(), get)
}

func bindForecastReleaseGeneration(evidence func(string) bool, names []string, generation func(string) (uint64, bool)) func(string) bool {
	bound := make(map[string]uint64)
	for _, name := range names {
		if value, ok := generation(name); ok && evidence(name) {
			bound[name] = value
		}
	}
	return func(name string) bool {
		old, known := bound[name]
		current, live := generation(name)
		return known && live && old == current
	}
}
