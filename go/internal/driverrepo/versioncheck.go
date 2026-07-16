package driverrepo

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var luaDriverVersionRE = regexp.MustCompile("(?m)^\\s*version\\s*=\\s*\"([^\"]+)\"")
var hostAPILineRE = regexp.MustCompile("(?m)^[ \\t]*host_api_(?:min|max)[ \\t]*=[ \\t]*[0-9]+,?[ \\t]*\\r?\\n?")

// ValidateDriverVersionChange enforces independent driver release semantics.
// The initial addition/change of host_api_min/max is metadata-only and does
// not force every existing driver to release a new runtime version.
func ValidateDriverVersionChange(filename string, previous, current []byte) error {
	currentVersion, err := driverVersion(current)
	if err != nil {
		return fmt.Errorf("%s: %w", filename, err)
	}
	if len(previous) == 0 {
		return nil
	}
	if bytes.Equal(normalizeHostAPIMetadata(previous), normalizeHostAPIMetadata(current)) {
		return nil
	}
	previousVersion, err := driverVersion(previous)
	if err != nil {
		return fmt.Errorf("%s at base revision: %w", filename, err)
	}
	if compareSemver(currentVersion, previousVersion) <= 0 {
		return fmt.Errorf("%s changed without a version bump (still %s)", filename, currentVersion)
	}
	return nil
}

func driverVersion(raw []byte) (string, error) {
	matches := luaDriverVersionRE.FindSubmatch(raw)
	if matches == nil {
		return "", errors.New("DRIVER.version is required")
	}
	version := string(matches[1])
	if !semverRE.MatchString(version) {
		return "", fmt.Errorf("invalid semantic version %q", version)
	}
	return version, nil
}

func normalizeHostAPIMetadata(raw []byte) []byte {
	normalized := hostAPILineRE.ReplaceAll(raw, nil)
	return []byte(strings.TrimSpace(string(normalized)))
}
