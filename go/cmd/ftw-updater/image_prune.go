package main

import (
	"context"
	"log/slog"
	"strings"
)

// Image cleanup after a verified update (#1305).
//
// Every update pulls a new image and used to leave the replaced one behind.
// A Raspberry Pi that followed the beta channel for months held 12 GB of
// unused Core, updater and optimizer images. A tester without SSH cannot
// reclaim them, and a full disk breaks the next update itself: the pull
// fails and the rollback point cannot be written.

const (
	canonicalOptimizerImage = "ghcr.io/srcfl/ftw-optimizer"
	// Compatibility mirror for installs that predate the srcfl namespace.
	legacyMainImage = "ghcr.io/frahlg/forty-two-watts"
)

// prunedImageRepositories lists the repositories the updater installs and may
// therefore clean. Other repositories and user-built images are never touched.
var prunedImageRepositories = []string{
	canonicalMainImage, canonicalUpdaterImage, canonicalOptimizerImage,
	legacyMainImage, legacyUpdaterImage,
}

// imageTag is one entry of `docker images` for a repository.
type imageTag struct {
	// ID is the full sha256 image ID, the form `docker inspect` reports.
	ID string
	// Ref is repository:tag, or the ID when the tag has been moved away.
	Ref string
}

// pruneReplacedImages removes tags in FTW's own repositories whose image no
// container uses and which is not an image this site could roll back to.
// Docker refuses to remove an image a container still references, so a stale
// inventory cannot delete a running image; the filter only avoids noisy
// failures. Problems are logged and never fail the finished update.
func (s *server) pruneReplacedImages(ctx context.Context, keep map[string]bool) []string {
	if s.listImages == nil || s.usedImageIDs == nil {
		return nil
	}
	used, err := s.usedImageIDs(ctx)
	if err != nil {
		slog.Warn("image cleanup skipped; containers could not be listed", "err", err)
		return nil
	}
	var removed []string
	for _, repository := range prunedImageRepositories {
		tags, err := s.listImages(ctx, repository)
		if err != nil {
			slog.Warn("image cleanup skipped for one repository", "repository", repository, "err", err)
			continue
		}
		for _, tag := range tags {
			if tag.ID == "" || used[tag.ID] || keep[tag.ID] {
				continue
			}
			if err := s.runner(ctx, nil, "image", "rm", tag.Ref); err != nil {
				slog.Warn("replaced image not removed", "image", tag.Ref, "err", err)
				continue
			}
			removed = append(removed, tag.Ref)
		}
	}
	if len(removed) > 0 {
		slog.Info("replaced images removed", "count", len(removed), "images", removed)
	}
	return removed
}

// dockerImagesInRepository lists every tag of one repository with its full ID.
func dockerImagesInRepository(ctx context.Context, repository string) ([]imageTag, error) {
	out, err := dockerOutput(ctx, "images", "--no-trunc", "--format", "{{.ID}}\t{{.Repository}}:{{.Tag}}", repository)
	if err != nil {
		return nil, err
	}
	return parseImageTags(out), nil
}

func parseImageTags(out string) []imageTag {
	var tags []imageTag
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) != 2 || !strings.HasPrefix(fields[0], "sha256:") {
			continue
		}
		ref := fields[1]
		if strings.Contains(ref, "<none>") {
			ref = fields[0]
		}
		tags = append(tags, imageTag{ID: fields[0], Ref: ref})
	}
	return tags
}

// dockerUsedImageIDs returns the image of every container, running or not.
func dockerUsedImageIDs(ctx context.Context) (map[string]bool, error) {
	out, err := dockerOutput(ctx, "ps", "-aq")
	if err != nil {
		return nil, err
	}
	used := map[string]bool{}
	ids := containerIDsFromDockerPS(out)
	if len(ids) == 0 {
		return used, nil
	}
	out, err = dockerOutput(ctx, append([]string{"inspect", "--format", "{{.Image}}"}, ids...)...)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		if id := strings.TrimSpace(line); strings.HasPrefix(id, "sha256:") {
			used[id] = true
		}
	}
	return used, nil
}
