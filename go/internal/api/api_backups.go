package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/backup"
	"github.com/srcfl/ftw/go/internal/components"
)

type backupListEntry struct {
	backup.Info
	DownloadURL string `json:"download_url"`
	OnDevice    bool   `json:"on_device"`
}

func (s *Server) handleBackups(w http.ResponseWriter, _ *http.Request) {
	dir, ok := s.backupConfig(w)
	if !ok {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	backups := make([]backupListEntry, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ftwbak") || !validBackupID(entry.Name()) {
			continue
		}
		archivePath := filepath.Join(dir, entry.Name())
		stat, err := entry.Info()
		if err != nil {
			continue
		}
		info := backup.Info{ID: entry.Name(), CreatedAt: stat.ModTime().UTC(), SizeBytes: stat.Size()}
		if raw, err := os.ReadFile(verificationPath(archivePath)); err == nil {
			var verified backup.Info
			if json.Unmarshal(raw, &verified) == nil && verified.ID == info.ID && verified.SizeBytes == info.SizeBytes {
				info = verified
			}
		}
		backups = append(backups, backupListEntry{
			Info: info, DownloadURL: "/api/backups/" + entry.Name(),
			OnDevice: s.deps.DataDir != "" && pathWithin(s.deps.DataDir, archivePath),
		})
	}
	sort.Slice(backups, func(i, j int) bool { return backups[i].CreatedAt.After(backups[j].CreatedAt) })
	writeJSON(w, 200, map[string]any{
		"enabled": true, "backups": backups, "dir": dir,
		"on_device": s.deps.DataDir != "" && pathWithin(s.deps.DataDir, dir),
	})
}

func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.backupConfig(w)
	if !ok {
		return
	}
	if s.deps.State == nil || s.deps.StatePath == "" || s.deps.DataDir == "" {
		writeJSON(w, 503, map[string]string{"error": "full backup is not wired to persistent state"})
		return
	}
	for label, persistentPath := range map[string]string{
		"config": s.deps.ConfigPath, "cold history": s.deps.ColdDir, "custom drivers": s.deps.UserDriverDir,
	} {
		if persistentPath != "" && !pathWithin(s.deps.DataDir, persistentPath) {
			writeJSON(w, 409, map[string]string{
				"error": fmt.Sprintf("%s path %s is outside the data directory; refusing to label an incomplete archive as a full backup", label, persistentPath),
			})
			return
		}
	}
	if s.deps.DriverRepository != nil {
		root := s.deps.DriverRepository.Status().RootDir
		if root != "" && !pathWithin(s.deps.DataDir, root) {
			writeJSON(w, 409, map[string]string{
				"error": fmt.Sprintf("managed driver repository %s is outside the data directory; refusing an incomplete full backup", root),
			})
			return
		}
	}
	if !s.backupMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "backup already in progress"})
		return
	}
	defer s.backupMu.Unlock()
	// A Core restart/update can terminate this process mid-archive. Share the
	// component-operation mutex and inspect the sidecar state so a full backup
	// is either completely published or never started.
	if !s.versionUpdateMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "component update or rollback already in progress"})
		return
	}
	defer s.versionUpdateMu.Unlock()
	if s.deps.SelfUpdate != nil && versionUpdateInFlight(s.deps.SelfUpdate.Status().State) {
		writeJSON(w, 409, map[string]string{"error": "component update or rollback already in progress"})
		return
	}
	info, err := backup.Create(r.Context(), backup.CreateOptions{
		State: s.deps.State, StatePath: s.deps.StatePath, DataDir: s.deps.DataDir,
		OutputDir: dir, Components: s.backupComponentInventory(r.Context()),
		Maintenance: s.deps.DataMaintenanceMu,
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	warning := ""
	if err := writeVerification(info.Path, info); err != nil {
		warning = "backup verified, but verification metadata could not be saved: " + err.Error()
	}
	writeJSON(w, 201, map[string]any{
		"status": "created", "backup": backupListEntry{
			Info: info, DownloadURL: "/api/backups/" + info.ID,
			OnDevice: s.deps.DataDir != "" && pathWithin(s.deps.DataDir, info.Path),
		}, "warning": warning,
	})
}

func (s *Server) handleBackupVerify(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.backupConfig(w)
	if !ok {
		return
	}
	if !s.backupMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "backup already in progress"})
		return
	}
	defer s.backupMu.Unlock()
	id := r.PathValue("id")
	if !validBackupID(id) {
		writeJSON(w, 400, map[string]string{"error": "invalid backup id"})
		return
	}
	archivePath := filepath.Join(dir, id)
	_, info, err := backup.Inspect(r.Context(), archivePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, 404, map[string]string{"error": "backup not found"})
		} else {
			writeJSON(w, 422, map[string]string{"error": err.Error()})
		}
		return
	}
	if err := writeVerification(archivePath, info); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "verified", "backup": info})
}

func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.backupConfig(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !validBackupID(id) {
		writeJSON(w, 400, map[string]string{"error": "invalid backup id"})
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id))
	http.ServeFile(w, r, filepath.Join(dir, id))
}

func (s *Server) handleBackupDelete(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.backupConfig(w)
	if !ok {
		return
	}
	if !s.backupMu.TryLock() {
		writeJSON(w, 409, map[string]string{"error": "backup already in progress"})
		return
	}
	defer s.backupMu.Unlock()
	id := r.PathValue("id")
	if !validBackupID(id) {
		writeJSON(w, 400, map[string]string{"error": "invalid backup id"})
		return
	}
	archivePath := filepath.Join(dir, id)
	if err := os.Remove(archivePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, 404, map[string]string{"error": "backup not found"})
		} else {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
		}
		return
	}
	_ = os.Remove(verificationPath(archivePath))
	writeJSON(w, 200, map[string]any{"ok": true, "deleted": id})
}

func (s *Server) backupConfig(w http.ResponseWriter) (string, bool) {
	if s.deps.BackupDir == "" {
		writeJSON(w, 503, map[string]string{"error": "full backups disabled (no BackupDir)"})
		return "", false
	}
	return s.deps.BackupDir, true
}

func (s *Server) backupComponentInventory(ctx context.Context) backup.ComponentInventory {
	coreVersion := s.deps.Version
	if s.deps.SelfUpdate != nil && s.deps.SelfUpdate.Info().Current != "" {
		coreVersion = s.deps.SelfUpdate.Info().Current
	}
	inventory := backup.ComponentInventory{Core: backup.ComponentVersion{Version: coreVersion}}
	if s.deps.MPC != nil && s.deps.MPC.Optimizer != nil {
		optimizer := backup.ComponentVersion{Version: "unknown", Protocol: components.OptimizerProtocolVersion}
		if health, ok := s.deps.MPC.Optimizer.(optimizerHealth); ok {
			healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			if runtime, err := health.Health(healthCtx); err == nil {
				optimizer.Version = runtime.Version
				optimizer.Protocol = runtime.ProtocolVersion
			}
			cancel()
		}
		inventory.Optimizer = &optimizer
	}
	if s.deps.DriverRepository != nil {
		for _, installed := range s.deps.DriverRepository.Status().Active {
			inventory.Drivers = append(inventory.Drivers, backup.DriverVersion{
				ID: installed.DriverID, Version: installed.Version, SHA256: installed.SHA256,
			})
		}
		sort.Slice(inventory.Drivers, func(i, j int) bool { return inventory.Drivers[i].ID < inventory.Drivers[j].ID })
	}
	return inventory
}

func validBackupID(id string) bool {
	return id != "" && filepath.Base(id) == id && strings.HasPrefix(id, "ftw-full-backup-") && strings.HasSuffix(id, ".ftwbak") && !strings.ContainsAny(id, `/\\`)
}

func verificationPath(archivePath string) string { return archivePath + ".verified.json" }

func writeVerification(archivePath string, info backup.Info) error {
	body, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	tmp := verificationPath(archivePath) + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, verificationPath(archivePath)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
