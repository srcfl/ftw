package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
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

func (s *Server) handleDeviceRepositoryCatalog(w http.ResponseWriter, _ *http.Request) {
	if s.deps.DriverRepository == nil {
		writeJSON(w, 503, map[string]string{"error": "device repository disabled"})
		return
	}
	catalog, err := s.deps.DriverRepository.Catalog()
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
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if body.RepositoryID == "" {
		writeJSON(w, 400, map[string]string{"error": "repository_id is required"})
		return
	}
	if !s.driverUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "another driver update is in progress"})
		return
	}
	defer s.driverUpdateMu.Unlock()
	started := time.Now()
	fromVersion := s.activeManagedDriverVersion(r.PathValue("id"))
	installed, err := s.deps.DriverRepository.Install(r.Context(), body.RepositoryID, r.PathValue("id"), body.Version)
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
	s.recordDriverUpdate(r.PathValue("id"), "install", fromVersion, installed.Version, "succeeded", "driver restarted with fresh telemetry", started)
	writeJSON(w, 200, map[string]any{"status": "installed", "artifact": installed})
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
	if body.LogicalPath == "" {
		body.LogicalPath = "drivers/" + filepath.Base(r.PathValue("id")) + ".lua"
	}
	if !s.driverUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "another driver update is in progress"})
		return
	}
	defer s.driverUpdateMu.Unlock()
	started := time.Now()
	fromVersion := s.activeManagedDriverVersion(r.PathValue("id"))
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
	s.recordDriverUpdate(r.PathValue("id"), "rollback", fromVersion, rolledBack.Version, "succeeded", "driver restarted with fresh telemetry", started)
	writeJSON(w, 200, map[string]any{"status": "rolled_back", "artifact": rolledBack})
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
	s.recordDriverUpdate(driverID, "activate", fromVersion, activated.Version, "succeeded", "driver restarted with fresh telemetry", started)
	writeJSON(w, 200, map[string]any{"status": "activated", "artifact": activated})
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

type managedDriverRestartState struct {
	Originals   []config.Driver
	ExpectedIDs map[string]string
}

// restartManagedDrivers updates only config entries using the activated
// logical Lua filename. Registry.Restart sends DefaultMode before replacing the
// VM. The stable active symlink means config remains portable.
func (s *Server) restartManagedDrivers(ctx context.Context, artifact state.DriverRepoInstall) (managedDriverRestartState, error) {
	return s.restartManagedDriversExpected(ctx, artifact, nil)
}

func (s *Server) restartManagedDriversExpected(ctx context.Context, artifact state.DriverRepoInstall, expectedIDs map[string]string) (managedDriverRestartState, error) {
	activePath := filepath.Join(s.deps.DriverRepository.ActiveDir(), filepath.FromSlash(strings.TrimPrefix(artifact.LogicalPath, "drivers/")))
	s.deps.CfgMu.Lock()
	var affected []config.Driver
	var originals []config.Driver
	for i := range s.deps.Cfg.Drivers {
		if filepath.Base(s.deps.Cfg.Drivers[i].Lua) != filepath.Base(activePath) {
			continue
		}
		// A custom/local absolute path owns its file and always shadows the
		// repository. Only switch bundled or already-managed copies.
		current := s.deps.Cfg.Drivers[i].Lua
		if !pathWithin(s.deps.DriverDir, current) && !pathWithin(s.deps.DriverRepository.ActiveDir(), current) {
			continue
		}
		originals = append(originals, s.deps.Cfg.Drivers[i])
		s.deps.Cfg.Drivers[i].Lua = activePath
		affected = append(affected, s.deps.Cfg.Drivers[i])
	}
	s.deps.CfgMu.Unlock()
	if expectedIDs == nil {
		expectedIDs = make(map[string]string, len(affected))
		for _, driver := range affected {
			expectedIDs[driver.Name] = s.runningDriverIdentity(driver.Name)
		}
	}
	restartState := managedDriverRestartState{Originals: originals, ExpectedIDs: expectedIDs}
	for _, driver := range affected {
		if err := s.deps.Registry.Restart(ctx, driver); err != nil {
			return restartState, fmt.Errorf("restart driver %s: %w", driver.Name, err)
		}
		if err := s.awaitDriverTelemetry(ctx, driver.Name, expectedIDs[driver.Name]); err != nil {
			return restartState, err
		}
	}
	return restartState, nil
}

func (s *Server) restoreDriverConfigs(ctx context.Context, originals []config.Driver, expectedIDs map[string]string) error {
	for _, original := range originals {
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
		return nil
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
