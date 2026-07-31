package main

// Replacing the updater sidecar itself.
//
// Core and the updater are built from the same Core release, but only Core was
// ever recreated by an update. The updater kept running whatever image it was
// installed with, so a fix inside the updater could not reach the installs that
// needed it — the broken updater was the thing that had to run the fix. The
// only way out was a hand-typed `docker compose pull ftw-updater` per site.
//
// The updater cannot recreate its own container directly: compose would stop
// the process mid-command and the recreate may or may not have reached the
// daemon. So it hands the job to a short-lived detached container, started from
// the image the updater is running right now. That image already carries the
// docker CLI and the compose plugin, and it is guaranteed to be on disk.
//
// This runs after Core is updated and verified healthy, and every failure is
// best-effort: a site that cannot replace its updater is left exactly where it
// is today — new Core, old updater — and never worse. Nothing here can fail a
// Core update that already succeeded, and nothing here gates Core on the
// updater's version. That ordering is the whole safety argument.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	canonicalUpdaterImage = "ghcr.io/srcfl/ftw-updater"
	// updaterTagEnv pins the sidecar image the same way mainTagEnv pins Core.
	// These must match the tagEnv values in componentSpec.
	updaterTagEnv = "FTW_UPDATER_IMAGE_TAG"
	mainTagEnv    = "FTW_IMAGE_TAG"
	// selfReplaceContainerName is fixed so a previous attempt's logs survive for
	// diagnosis and a stuck helper can be found by name.
	selfReplaceContainerName = "ftw-updater-self-replace"
	// selfReplaceDelay gives the updater time to finish writing terminal state
	// before the helper stops its container.
	selfReplaceDelay = 3 * time.Second
)

// replaceUpdater brings the updater sidecar to the release Core just moved to.
// It reports an error only so callers can log it; no caller may turn it into a
// failed update.
func (s *server) replaceUpdater(ctx context.Context, target string) error {
	if target == "" {
		return fmt.Errorf("no target release")
	}
	service, err := s.updaterServiceName()
	if err != nil {
		return err
	}

	// Run the helper from the image this process is running. The new image is
	// not needed to issue a compose command, and the current one cannot be
	// missing.
	helperImage, err := s.imageID(ctx, service)
	if err != nil {
		return fmt.Errorf("current updater image: %w", err)
	}

	// Pull and recreate must resolve the same config, or they could disagree
	// about which image the sidecar runs. Both use the host files only.
	env := []string{updaterTagEnv + "=" + target}
	if err := s.runner(ctx, env, s.hostComposeArgs("pull", service)...); err != nil {
		return fmt.Errorf("pull updater %s: %w", target, err)
	}

	// A stuck or finished helper from an earlier update would block the fixed
	// name. Removing it here also means the logs of the last attempt survive
	// until the next one, which is when they are wanted.
	_ = s.runner(ctx, nil, "rm", "-f", selfReplaceContainerName)

	projectDir := filepath.Dir(s.composeFile)
	upArgs := append(s.hostComposeArgs("up", "-d", "--no-deps"), service)

	steps := []string{fmt.Sprintf("sleep %d", int(selfReplaceDelay.Seconds()))}
	// Record the tags this update installed so a later plain `docker compose
	// up -d` on the host does not resolve :latest and undo it. Skipped, with
	// the update left intact, if the current .env cannot be read — overwriting
	// a file we could not see would be worse than the rollback risk.
	if existing, err := readEnvFile(projectDir); err != nil {
		slog.Warn("image tags not persisted; a later `docker compose up` may roll this update back",
			"env", filepath.Join(projectDir, ".env"), "err", err)
	} else {
		// Core and the sidecar are the same release, and this only ever runs
		// after a Core update, so one target pins both.
		steps = append(steps, envPinScript(projectDir, mergeEnvFile(existing, map[string]string{
			mainTagEnv:    target,
			updaterTagEnv: target,
		})))
	}
	steps = append(steps, "exec docker "+strings.Join(upArgs, " "))
	script := strings.Join(steps, "; ")

	runArgs := []string{
		"run", "-d",
		"--name", selfReplaceContainerName,
		"-v", "/var/run/docker.sock:/var/run/docker.sock",
		// The daemon resolves the compose file's relative binds as absolute
		// host paths, so the helper must see the project at its host path for
		// the recreated sidecar to mount the same directories. Writable because
		// the helper persists .env; it already holds the Docker socket.
		"-v", projectDir + ":" + projectDir,
		"-w", projectDir,
		"-e", updaterTagEnv + "=" + target,
	}
	if project := os.Getenv("COMPOSE_PROJECT_NAME"); project != "" {
		runArgs = append(runArgs, "-e", "COMPOSE_PROJECT_NAME="+project)
	}
	runArgs = append(runArgs, "--entrypoint", "sh", helperImage, "-c", script)

	if err := s.runner(ctx, nil, runArgs...); err != nil {
		return fmt.Errorf("start updater replacement helper: %w", err)
	}
	slog.Info("updater replacement handed to detached helper", "service", service, "target", target, "helper", selfReplaceContainerName)
	return nil
}

// hostComposeArgs builds a compose invocation the detached helper can actually
// run: only files that exist on the host, never the updater-owned transient
// override.
//
// composeArgs appends that override, which lives in this container's /tmp and
// is deleted when the job returns. The helper is a separate container and runs
// three seconds later, so it would fail twice over — the path is not in its
// mount namespace, and by then the file is gone. Caught on real hardware:
// "open /tmp/ftw-compose-update-1611897995.yml: no such file or directory".
//
// The override only exists to pin Core's image for deployments that hard-code
// it. Recreating the sidecar does not need it: the tag comes from
// FTW_UPDATER_IMAGE_TAG in the helper's environment.
func (s *server) hostComposeArgs(sub ...string) []string {
	out := []string{"compose", "-f", s.composeFile}
	for _, o := range s.hostComposeFiles() {
		if o != s.composeFile {
			out = append(out, "-f", o)
		}
	}
	return append(out, sub...)
}

// hostComposeFiles is composeFiles minus the transient override, for the same
// reason. The updater service must also be resolved from these files and not
// the merged set: the helper recreates what these declare.
func (s *server) hostComposeFiles() []string {
	return append([]string{s.composeFile}, s.overrideFiles...)
}

// updaterServiceName finds the compose service running the updater image. It
// matches on the image rather than a fixed name so a renamed service, or the
// compatibility GHCR namespace, still resolves.
func (s *server) updaterServiceName() (string, error) {
	files := s.hostComposeFiles()
	var found []string
	for _, name := range composeServiceNames(files) {
		image, ok, err := serviceImageFromComposeFiles(files, name)
		if err != nil || !ok {
			continue
		}
		if isUpdaterImage(image) {
			found = append(found, name)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("expected exactly one updater service in compose, found %v", found)
	}
	return found[0], nil
}

// composeServiceNames lists every service declared across the merged files.
// Unreadable or malformed files are skipped: this is a best-effort lookup on a
// path that must never fail an update.
func composeServiceNames(files []string) []string {
	seen := make(map[string]struct{})
	var names []string
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc struct {
			Services map[string]yaml.Node `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			continue
		}
		for name := range doc.Services {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// isUpdaterImage matches the updater repository while ignoring the tag and any
// registry namespace, so `ghcr.io/srcfl/ftw-updater:v1` and a mirrored
// `example.com/team/ftw-updater:v1` both resolve.
func isUpdaterImage(image string) bool {
	repo := image
	if idx := strings.LastIndex(repo, "@"); idx > 0 {
		repo = repo[:idx]
	}
	// Cut at the FIRST colon after the last slash. Not the last colon: compose
	// images carry unexpanded defaults like `:${FTW_UPDATER_IMAGE_TAG:-latest}`,
	// whose own colon would otherwise be read as the tag separator. Anything
	// before the last slash is the registry, so a host port survives.
	slash := strings.LastIndex(repo, "/")
	if idx := strings.Index(repo[slash+1:], ":"); idx >= 0 {
		repo = repo[:slash+1+idx]
	}
	if repo == canonicalUpdaterImage {
		return true
	}
	return strings.HasSuffix(repo, "/ftw-updater")
}
