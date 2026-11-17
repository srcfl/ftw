package nativeupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// TakeImage brings the release a container image carries into the slots and
// returns a line for the log. dir holds the package as the image downloaded
// it: ftw-linux-<arch>.tar.gz, its .sha256 and its release-version.json.
//
// The first start installs that release and makes it current. After that an
// image release newer than the current one is staged as next, so a rebuild
// with a newer FTW_VERSION goes through the same trial and fallback as
// `ftw update`. An image release that is current, pending, the previous one,
// failed here before or older changes nothing: the box may have updated
// itself past its image, and only the slots know that.
func (m Manager) TakeImage(ctx context.Context, dir string) (string, error) {
	tag, err := imageTag(dir)
	if err != nil {
		return "", err
	}
	archive := filepath.Join(dir, "ftw-linux-"+runtime.GOARCH+".tar.gz")
	checksum := archive + ".sha256"
	state, err := m.Read()
	if errors.Is(err, os.ErrNotExist) {
		if err := m.InstallArchive(ctx, tag, archive, checksum); err != nil {
			return "", err
		}
		if err := m.Init(tag); err != nil {
			return "", err
		}
		return "installed " + tag + " from the image", nil
	}
	if err != nil {
		return "", err
	}
	switch {
	case tag == state.Current || tag == state.Next || tag == state.Trial:
		return "", nil
	case tag == state.LastFailed:
		return fmt.Sprintf("the image's %s failed here before; %s stays", tag, state.Current), nil
	case !newerTag(tag, state.Current):
		return fmt.Sprintf("the image's %s is older than %s, which stays", tag, state.Current), nil
	case tag == state.Previous:
		return fmt.Sprintf("this box went back from the image's %s; %s stays", tag, state.Current), nil
	}
	// The archive is already on disk, but its unpacked copy shares the disk
	// with the data. Refuse as an update from the network would.
	if err := m.CheckSpace(state.Current); err != nil {
		return "", err
	}
	if err := m.InstallArchive(ctx, tag, archive, checksum); err != nil {
		return "", err
	}
	if err := m.Prepare(tag); err != nil {
		return "", err
	}
	return "staged " + tag + " from the image; it starts once as a trial", nil
}

func imageTag(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, packageVersionFile))
	if err != nil {
		return "", fmt.Errorf("image release: %w", err)
	}
	var version packageVersion
	if err := json.Unmarshal(data, &version); err != nil {
		return "", fmt.Errorf("image release: %w", err)
	}
	if !ValidTag(version.Version) {
		return "", fmt.Errorf("image release has invalid tag %q", version.Version)
	}
	return version.Version, nil
}

// newerTag reports whether release a comes after release b. Both are valid
// tags; a stable release comes after each of its betas.
func newerTag(a, b string) bool {
	x, y := tagOrder(a), tagOrder(b)
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

func tagOrder(tag string) [4]uint64 {
	parts := releaseTag.FindStringSubmatch(tag)
	var order [4]uint64
	if parts == nil {
		return order
	}
	for i := range order {
		if parts[i+1] == "" {
			order[i] = ^uint64(0) // stable: after every beta
			continue
		}
		order[i], _ = strconv.ParseUint(parts[i+1], 10, 64)
	}
	return order
}
