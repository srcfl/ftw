package api

import (
	"context"
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
	installed, err := s.deps.DriverRepository.Install(r.Context(), body.RepositoryID, r.PathValue("id"), body.Version)
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	originals, restartErr := s.restartManagedDrivers(r.Context(), installed)
	if restartErr != nil {
		rolledBack, rollbackErr := s.deps.DriverRepository.Rollback(installed.LogicalPath)
		if rollbackErr == nil {
			if _, recoveryErr := s.restartManagedDrivers(context.Background(), rolledBack); recoveryErr != nil {
				rollbackErr = fmt.Errorf("previous artifact reactivated but did not recover: %w", recoveryErr)
			}
		} else {
			// First managed activation: no managed predecessor exists, so remove
			// the active symlink and restore the bundled configs captured above.
			deactivateErr := s.deps.DriverRepository.Deactivate(installed.LogicalPath)
			restoreErr := s.restoreDriverConfigs(context.Background(), originals)
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
		writeJSON(w, 502, map[string]string{"error": message})
		return
	}
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
	rolledBack, err := s.deps.DriverRepository.Rollback(body.LogicalPath)
	if err != nil {
		writeJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	if _, err := s.restartManagedDrivers(r.Context(), rolledBack); err != nil {
		// Roll the rollback forward if the older artifact no longer initializes
		// against current hardware/config.
		recoveryMessage := ""
		if recovered, recoveryErr := s.deps.DriverRepository.Rollback(rolledBack.LogicalPath); recoveryErr == nil {
			if _, restartErr := s.restartManagedDrivers(context.Background(), recovered); restartErr != nil {
				recoveryMessage = "; roll-forward restart failed: " + restartErr.Error()
			}
		} else {
			recoveryMessage = "; roll-forward activation failed: " + recoveryErr.Error()
		}
		writeJSON(w, 502, map[string]string{"error": err.Error() + recoveryMessage})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "rolled_back", "artifact": rolledBack})
}

// restartManagedDrivers updates only config entries using the activated
// logical Lua filename. Registry.Restart sends DefaultMode before replacing the
// VM. The stable active symlink means config remains portable.
func (s *Server) restartManagedDrivers(ctx context.Context, artifact state.DriverRepoInstall) ([]config.Driver, error) {
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
	for _, driver := range affected {
		if err := s.deps.Registry.Restart(ctx, driver); err != nil {
			return originals, fmt.Errorf("restart driver %s: %w", driver.Name, err)
		}
		if err := s.awaitDriverTelemetry(ctx, driver.Name); err != nil {
			return originals, err
		}
	}
	return originals, nil
}

func (s *Server) restoreDriverConfigs(ctx context.Context, originals []config.Driver) error {
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
		if err := s.awaitDriverTelemetry(ctx, original.Name); err != nil {
			return err
		}
	}
	return nil
}

func pathWithin(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Server) awaitDriverTelemetry(ctx context.Context, name string) error {
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
				return nil
			}
		}
	}
}
