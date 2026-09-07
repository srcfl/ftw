package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
)

func (s *Server) managedDriverDir() string {
	if s.deps.DriverRepository == nil {
		return ""
	}
	return s.deps.DriverRepository.ActiveDir()
}

func (s *Server) handleDeviceRepositoryStatus(w http.ResponseWriter, _ *http.Request) {
	if s.deps.DriverRepository == nil {
		writeJSON(w, 503, map[string]string{"error": "device repository disabled"})
		return
	}
	writeJSON(w, 200, s.deps.DriverRepository.Status())
}

func (s *Server) handleDeviceRepositoryCatalog(w http.ResponseWriter, r *http.Request) {
	if s.deps.DriverRepository == nil {
		writeJSON(w, 503, map[string]string{"error": "device repository disabled"})
		return
	}
	channel := r.URL.Query().Get("channel")
	var catalog any
	var err error
	if channel == "" {
		catalog, err = s.deps.DriverRepository.Catalog()
	} else {
		catalog, err = s.deps.DriverRepository.ChannelCatalog(r.Context(), channel)
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"entries": catalog})
}

func (s *Server) handleDeviceRepositoryRefresh(w http.ResponseWriter, r *http.Request) {
	if s.deps.DriverRepository == nil {
		writeJSON(w, 503, map[string]string{"error": "device repository disabled"})
		return
	}
	var body struct {
		RepositoryID string `json:"repository_id,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	if err := s.deps.DriverRepository.Refresh(r.Context(), body.RepositoryID); err != nil {
		writeJSON(w, 502, map[string]any{"error": err.Error(), "status": s.deps.DriverRepository.Status()})
		return
	}
	writeJSON(w, 200, s.deps.DriverRepository.Status())
}

func (s *Server) handleDeviceRepositoryInstall(w http.ResponseWriter, r *http.Request) {
	if s.deps.DriverRepository == nil || s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "device repository or registry unavailable"})
		return
	}
	var body struct {
		RepositoryID string `json:"repository_id"`
		Version      string `json:"version,omitempty"`
		Channel      string `json:"channel,omitempty"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if body.RepositoryID == "" && body.Channel == "" {
		writeJSON(w, 400, map[string]string{"error": "repository_id or channel is required"})
		return
	}
	if !s.driverUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "another driver update is in progress"})
		return
	}
	defer s.driverUpdateMu.Unlock()
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	started := time.Now()
	fromVersion := s.activeManagedDriverVersion(r.PathValue("id"))
	var installed state.DriverRepoInstall
	var err error
	if body.Channel != "" {
		installed, err = s.deps.DriverRepository.InstallChannel(r.Context(), body.Channel, r.PathValue("id"), body.Version)
	} else {
		installed, err = s.deps.DriverRepository.Install(r.Context(), body.RepositoryID, r.PathValue("id"), body.Version)
	}
	if err != nil {
		s.recordDriverUpdate(r.PathValue("id"), "install", fromVersion, body.Version, "failed", err.Error(), started)
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	restartState, restartErr := s.restartManagedDrivers(r.Context(), installed)
	if restartErr != nil {
		rolledBack, rollbackErr := s.deps.DriverRepository.Rollback(installed.LogicalPath)
		if rollbackErr == nil {
			if _, recoveryErr := s.restartManagedDriversExpected(context.Background(), rolledBack, restartState.ExpectedIDs); recoveryErr != nil {
				rollbackErr = fmt.Errorf("previous artifact reactivated but did not recover: %w", recoveryErr)
			}
		} else {
			// First managed activation: no managed predecessor exists, so remove
			// the active symlink and restore the bundled configs captured above.
			deactivateErr := s.deps.DriverRepository.Deactivate(installed.LogicalPath)
			restoreErr := s.restoreDriverConfigs(context.Background(), restartState.Originals, restartState.ExpectedIDs)
			if deactivateErr == nil && restoreErr == nil {
				rollbackErr = nil
			} else {
				rollbackErr = fmt.Errorf("managed rollback unavailable (%v); bundled restore: deactivate=%v restart=%v", rollbackErr, deactivateErr, restoreErr)
			}
		}
		message := restartErr.Error()
		if rollbackErr != nil {
			message += "; automatic rollback failed: " + rollbackErr.Error()
		}
		s.recordDriverUpdate(r.PathValue("id"), "install", fromVersion, installed.Version, "failed", message, started)
		writeJSON(w, 502, map[string]string{"error": message})
		return
	}
	s.recordDriverUpdate(r.PathValue("id"), "install", fromVersion, installed.Version, "succeeded", restartState.message(), started)
	writeJSON(w, 200, restartState.response("installed", installed))
}

func (s *Server) handleDeviceRepositoryRollback(w http.ResponseWriter, r *http.Request) {
	if s.deps.DriverRepository == nil || s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "device repository or registry unavailable"})
		return
	}
	var body struct {
		LogicalPath string `json:"logical_path"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	if !s.driverUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "another driver update is in progress"})
		return
	}
	defer s.driverUpdateMu.Unlock()
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	started := time.Now()
	fromVersion := s.activeManagedDriverVersion(r.PathValue("id"))
	logicalPath, pathErr := s.activeDriverLogicalPath(r.PathValue("id"), body.LogicalPath)
	if pathErr != nil {
		writeJSON(w, 422, map[string]string{"error": pathErr.Error()})
		return
	}
	body.LogicalPath = logicalPath
	rolledBack, err := s.deps.DriverRepository.Rollback(body.LogicalPath)
	if err != nil {
		s.recordDriverUpdate(r.PathValue("id"), "rollback", fromVersion, "", "failed", err.Error(), started)
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	restartState, restartErr := s.restartManagedDrivers(r.Context(), rolledBack)
	if restartErr != nil {
		// Roll the rollback forward if the older artifact no longer initializes
		// against current hardware/config.
		recoveryMessage := ""
		if recovered, recoveryErr := s.deps.DriverRepository.Rollback(rolledBack.LogicalPath); recoveryErr == nil {
			if _, restartErr := s.restartManagedDriversExpected(context.Background(), recovered, restartState.ExpectedIDs); restartErr != nil {
				recoveryMessage = "; roll-forward restart failed: " + restartErr.Error()
			}
		} else {
			recoveryMessage = "; roll-forward activation failed: " + recoveryErr.Error()
		}
		message := restartErr.Error() + recoveryMessage
		s.recordDriverUpdate(r.PathValue("id"), "rollback", fromVersion, rolledBack.Version, "failed", message, started)
		writeJSON(w, 502, map[string]string{"error": message})
		return
	}
	s.recordDriverUpdate(r.PathValue("id"), "rollback", fromVersion, rolledBack.Version, "succeeded", restartState.message(), started)
	writeJSON(w, 200, restartState.response("rolled_back", rolledBack))
}

// Back to the copy that shipped with this build. Rollback steps between
// managed artifacts and cannot reach the bundled driver, so without this an
// operator who installed one channel version over a bundled driver had no way
// back at all -- which is the first move anyone makes when trying a new one.
func (s *Server) handleDeviceRepositoryUseBundled(w http.ResponseWriter, r *http.Request) {
	if s.deps.DriverRepository == nil || s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "device repository or registry unavailable"})
		return
	}
	var body struct {
		LogicalPath string `json:"logical_path"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &body); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	if !s.driverUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "another driver update is in progress"})
		return
	}
	defer s.driverUpdateMu.Unlock()
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	started := time.Now()
	fromVersion := s.activeManagedDriverVersion(r.PathValue("id"))
	logicalPath, pathErr := s.activeDriverLogicalPath(r.PathValue("id"), body.LogicalPath)
	if pathErr != nil {
		writeJSON(w, 422, map[string]string{"error": pathErr.Error()})
		return
	}
	body.LogicalPath = logicalPath
	bundledPath, err := s.bundledDriverPath(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	replaced, err := s.deps.DriverRepository.UseBundled(body.LogicalPath, bundledPath)
	if err != nil {
		s.recordDriverUpdate(r.PathValue("id"), "rollback", fromVersion, "", "failed", err.Error(), started)
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	// InstalledPath empty tells restartManagedDrivers to point config at the
	// bundled file rather than the symlink UseBundled just removed.
	restartState, restartErr := s.restartManagedDrivers(r.Context(), state.DriverRepoInstall{
		LogicalPath: body.LogicalPath, DriverID: replaced.DriverID,
	})
	if restartErr != nil {
		// Put the managed artifact back. It is still on disk; only the
		// resolver entry was removed, so this is an activation, not a fetch.
		recoveryMessage := ""
		if _, recoveryErr := s.deps.DriverRepository.ActivateInstalled(r.PathValue("id"), replaced.Version, replaced.SHA256); recoveryErr != nil {
			recoveryMessage = "; restoring v" + replaced.Version + " failed: " + recoveryErr.Error()
		} else if _, err := s.restartManagedDriversExpected(context.Background(), replaced, restartState.ExpectedIDs); err != nil {
			recoveryMessage = "; restarting v" + replaced.Version + " failed: " + err.Error()
		}
		message := restartErr.Error() + recoveryMessage
		s.recordDriverUpdate(r.PathValue("id"), "rollback", fromVersion, "", "failed", message, started)
		writeJSON(w, 502, map[string]string{"error": message})
		return
	}
	s.recordDriverUpdate(r.PathValue("id"), "rollback", fromVersion, "bundled", "succeeded", restartState.message(), started)
	writeJSON(w, 200, restartState.response("using_bundled", nil))
}

func (s *Server) handleDeviceRepositoryVersions(w http.ResponseWriter, r *http.Request) {
	if s.deps.DriverRepository == nil {
		writeJSON(w, 503, map[string]string{"error": "device repository disabled"})
		return
	}
	versions, err := s.deps.DriverRepository.InstalledVersions(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	available, availableErr := s.deps.DriverRepository.AvailableVersions(r.PathValue("id"))
	if availableErr != nil {
		writeJSON(w, 500, map[string]string{"error": availableErr.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"driver_id": r.PathValue("id"), "installed": versions, "available": available})
}

func (s *Server) handleDeviceRepositoryActivate(w http.ResponseWriter, r *http.Request) {
	if s.deps.DriverRepository == nil || s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "device repository or registry unavailable"})
		return
	}
	var body struct {
		Version string `json:"version"`
		SHA256  string `json:"sha256,omitempty"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !s.driverUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "another driver update is in progress"})
		return
	}
	defer s.driverUpdateMu.Unlock()
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	driverID := r.PathValue("id")
	started := time.Now()
	fromVersion := s.activeManagedDriverVersion(driverID)
	var original *state.DriverRepoInstall
	if versions, err := s.deps.DriverRepository.InstalledVersions(driverID); err == nil {
		for i := range versions {
			if versions[i].Active {
				copy := versions[i]
				original = &copy
				break
			}
		}
	}
	activated, err := s.deps.DriverRepository.ActivateInstalled(driverID, body.Version, body.SHA256)
	if err != nil {
		s.recordDriverUpdate(driverID, "activate", fromVersion, body.Version, "failed", err.Error(), started)
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	restartState, restartErr := s.restartManagedDrivers(r.Context(), activated)
	if restartErr != nil {
		recoveryErr := error(nil)
		if original != nil {
			var recovered state.DriverRepoInstall
			recovered, recoveryErr = s.deps.DriverRepository.ActivateInstalled(driverID, original.Version, original.SHA256)
			if recoveryErr == nil {
				_, recoveryErr = s.restartManagedDriversExpected(context.Background(), recovered, restartState.ExpectedIDs)
			}
		} else {
			deactivateErr := s.deps.DriverRepository.Deactivate(activated.LogicalPath)
			restoreErr := s.restoreDriverConfigs(context.Background(), restartState.Originals, restartState.ExpectedIDs)
			recoveryErr = errors.Join(deactivateErr, restoreErr)
		}
		message := restartErr.Error()
		if recoveryErr != nil {
			message += "; automatic recovery failed: " + recoveryErr.Error()
		}
		s.recordDriverUpdate(driverID, "activate", fromVersion, activated.Version, "failed", message, started)
		writeJSON(w, 502, map[string]string{"error": message})
		return
	}
	s.recordDriverUpdate(driverID, "activate", fromVersion, activated.Version, "succeeded", restartState.message(), started)
	writeJSON(w, 200, restartState.response("activated", activated))
}

func (s *Server) activeManagedDriverVersion(driverID string) string {
	if s.deps.DriverRepository == nil {
		return ""
	}
	versions, err := s.deps.DriverRepository.InstalledVersions(driverID)
	if err != nil {
		return ""
	}
	for _, version := range versions {
		if version.Active {
			return version.Version
		}
	}
	return ""
}

// A card may still carry the bundled filename after its first install.
// Accept that exact alias only when its metadata declares the requested ID.
func (s *Server) activeDriverLogicalPath(id, requested string) (string, error) {
	versions, err := s.deps.DriverRepository.InstalledVersions(id)
	if err != nil {
		return "", err
	}
	for _, version := range versions {
		if !version.Active {
			continue
		}
		if requested == "" || requested == version.LogicalPath {
			return version.LogicalPath, nil
		}
		if bundled, err := s.bundledDriverPath(id); err == nil {
			rel, _ := filepath.Rel(s.deps.DriverDir, bundled)
			if requested == "drivers/"+filepath.ToSlash(rel) {
				return version.LogicalPath, nil
			}
		}
		return "", fmt.Errorf("path %s does not belong to active driver %s", requested, id)
	}
	return "", fmt.Errorf("no active managed artifact for driver %s", id)
}

type managedDriverRestartState struct {
	Originals     []config.Driver
	ExpectedIDs   map[string]string
	Restarted     []string
	LogicalPath   string
	ConfigChanged bool
}

func (r managedDriverRestartState) message() string {
	if len(r.Restarted) == 0 {
		return "driver artifact selected; no running instances updated"
	}
	return fmt.Sprintf("%d driver instances restarted with fresh telemetry", len(r.Restarted))
}

func (r managedDriverRestartState) response(status string, artifact any) map[string]any {
	names := append([]string{}, r.Restarted...)
	return map[string]any{"status": status, "artifact": artifact,
		"runtime_verified": len(names) > 0, "restarted_drivers": names, "logical_path": r.LogicalPath, "config_changed": r.ConfigChanged}
}

// The repository verifies DRIVER.id against its signed manifest. Bundled
// filenames can differ from that ID; punctuation is not an identity mapping.
func (s *Server) bundledDriverPath(id string) (string, error) {
	var found string
	err := filepath.Walk(s.deps.DriverDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".lua" {
			return nil
		}
		entry, err := drivers.ParseCatalogFile(path)
		if err != nil || id == "" || entry.ID != id {
			return nil
		}
		if found != "" {
			return fmt.Errorf("multiple bundled files declare driver %s", id)
		}
		found = path
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("no bundled file declares driver %s", id)
	}
	return found, nil
}

func (s *Server) restartManagedDrivers(ctx context.Context, artifact state.DriverRepoInstall) (managedDriverRestartState, error) {
	return s.restartManagedDriversExpected(ctx, artifact, nil)
}

func (s *Server) restartManagedDriversExpected(ctx context.Context, artifact state.DriverRepoInstall, expectedIDs map[string]string) (managedDriverRestartState, error) {
	restartState := managedDriverRestartState{ExpectedIDs: expectedIDs, LogicalPath: artifact.LogicalPath}
	rel := filepath.FromSlash(strings.TrimPrefix(artifact.LogicalPath, "drivers/"))
	activePath := filepath.Join(s.managedDriverDir(), rel)
	targetPath := activePath
	if artifact.InstalledPath == "" {
		var err error
		targetPath, err = s.bundledDriverPath(artifact.DriverID)
		if err != nil {
			return restartState, err
		}
		rel, _ := filepath.Rel(s.deps.DriverDir, targetPath)
		restartState.LogicalPath = "drivers/" + filepath.ToSlash(rel)
	}
	s.deps.CfgMu.RLock()
	next := *s.deps.Cfg
	next.Drivers = append([]config.Driver(nil), next.Drivers...)
	s.deps.CfgMu.RUnlock()
	if restartState.ExpectedIDs == nil {
		restartState.ExpectedIDs = make(map[string]string)
	}
	var affected []config.Driver
	changed := false
	for i, current := range next.Drivers {
		// Files outside the bundled and active roots belong to the operator.
		if !pathWithin(s.deps.DriverDir, current.Lua) && !pathWithin(s.managedDriverDir(), current.Lua) {
			continue
		}
		// UseBundled has removed this exact active link. Its verified install
		// record still binds the path to the ID; all other files need metadata.
		if filepath.Clean(current.Lua) != filepath.Clean(activePath) {
			entry, err := drivers.ParseCatalogFile(current.Lua)
			if err != nil || artifact.DriverID == "" || entry.ID != artifact.DriverID {
				continue
			}
		}
		restartState.Originals = append(restartState.Originals, current)
		next.Drivers[i].Lua = targetPath
		changed = changed || current.Lua != targetPath
		if current.Disabled {
			continue
		}
		// Recovery retains the original recipients even if failed init left
		// one absent from the registry. An initial install does not start a
		// stopped or disabled instance just to produce a success claim.
		if expectedIDs == nil {
			if s.deps.Registry.Env(current.Name) == nil {
				continue
			}
			restartState.ExpectedIDs[current.Name] = s.runningDriverIdentity(current.Name)
		} else if _, ok := expectedIDs[current.Name]; !ok {
			continue
		}
		affected = append(affected, next.Drivers[i])
	}
	for _, driver := range affected {
		if err := s.deps.Registry.Restart(ctx, driver); err != nil {
			return restartState, fmt.Errorf("restart driver %s: %w", driver.Name, err)
		}
		if err := s.awaitDriverTelemetry(ctx, driver.Name, restartState.ExpectedIDs[driver.Name]); err != nil {
			return restartState, err
		}
		restartState.Restarted = append(restartState.Restarted, driver.Name)
	}
	// Keep the old config until every recipient has recovered. A save error
	// follows the same artifact/runtime recovery path as an init error.
	if changed {
		if s.deps.SaveConfig == nil {
			return restartState, errors.New("config persistence unavailable")
		}
		if err := s.deps.SaveConfig(s.deps.ConfigPath, &next); err != nil {
			return restartState, fmt.Errorf("save driver selection: %w", err)
		}
		s.deps.CfgMu.Lock()
		s.deps.Cfg.Drivers = next.Drivers
		s.deps.Cfg.Revision = next.Revision
		s.deps.CfgMu.Unlock()
		restartState.ConfigChanged = true
	}
	return restartState, nil
}

func (s *Server) restoreDriverConfigs(ctx context.Context, originals []config.Driver, expectedIDs map[string]string) error {
	for _, original := range originals {
		if _, running := expectedIDs[original.Name]; !running || original.Disabled {
			continue
		}
		s.deps.CfgMu.Lock()
		for i := range s.deps.Cfg.Drivers {
			if s.deps.Cfg.Drivers[i].Name == original.Name {
				s.deps.Cfg.Drivers[i] = original
				break
			}
		}
		s.deps.CfgMu.Unlock()
		if err := s.deps.Registry.Restart(ctx, original); err != nil {
			return err
		}
		if err := s.awaitDriverTelemetry(ctx, original.Name, expectedIDs[original.Name]); err != nil {
			return err
		}
	}
	return nil
}

func pathWithin(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Server) awaitDriverTelemetry(ctx context.Context, name, expectedID string) error {
	interval, ok := s.deps.Registry.PollInterval(name)
	if !ok {
		return fmt.Errorf("driver %s is not running", name)
	}
	window := 2 * interval
	if window < 30*time.Second {
		window = 30 * time.Second
	}
	if window > 5*time.Minute {
		window = 5 * time.Minute
	}
	deadline := time.NewTimer(window)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("driver %s produced no fresh telemetry within %s", name, window)
		case <-ticker.C:
			health := s.deps.Tel.DriverHealth(name)
			if health != nil && health.LastSuccess != nil {
				if expectedID == "" {
					return nil
				}
				actualID := s.runningDriverIdentity(name)
				if actualID == expectedID {
					return nil
				}
				if actualID != "" {
					return fmt.Errorf("driver %s reported hardware identity %s after update, expected %s", name, actualID, expectedID)
				}
			}
		}
	}
}

func (s *Server) runningDriverIdentity(name string) string {
	if s.deps.Registry == nil {
		return ""
	}
	env := s.deps.Registry.Env(name)
	if env == nil {
		return ""
	}
	makeName, serial, mac, endpoint := env.FullIdentity()
	return state.ResolveDeviceID(makeName, serial, mac, endpoint)
}
