package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/srcfl/ftw/go/internal/backup"
	"github.com/srcfl/ftw/go/internal/state"
)

const (
	// These are planning thresholds only. This endpoint never enforces them.
	defaultDryRunBudgetBytes int64 = 1500 << 20
	filesystemReserveBytes   int64 = 500 << 20
)

type storageFootprint struct {
	Bytes            int64 `json:"bytes"`
	Files            int64 `json:"files"`
	LatestObservedMs int64 `json:"latest_observed_ms,omitempty"`
	OnDevice         bool  `json:"on_device"`
}

type parquetFootprint struct {
	storageFootprint
	DiagnosticsBytes int64 `json:"diagnostics_bytes"`
	DiagnosticsFiles int64 `json:"diagnostics_files"`
	RetentionDays    int   `json:"retention_days"`
}

type storageFileInventory struct {
	Parquet           parquetFootprint `json:"parquet"`
	RecoverySnapshot  storageFootprint `json:"recovery_snapshot"`
	RollbackSnapshots storageFootprint `json:"rollback_snapshots"`
	FullBackups       storageFootprint `json:"full_backups"`
	OtherData         storageFootprint `json:"other_data"`
}

type storageFilesystem struct {
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

type storageMaintenance struct {
	LastParquetSuccessMs          int64            `json:"last_parquet_success_ms,omitempty"`
	LastRecoverySnapshotSuccessMs int64            `json:"last_recovery_snapshot_success_ms,omitempty"`
	LastRollbackSnapshotSuccessMs int64            `json:"last_rollback_snapshot_success_ms,omitempty"`
	LastFullBackupVerifiedMs      int64            `json:"last_full_backup_verified_ms,omitempty"`
	LastRecoveryEvent             *state.HealEvent `json:"last_recovery_event,omitempty"`
}

type storageAdvice struct {
	Category      string `json:"category"`
	ObservedBytes int64  `json:"observed_bytes"`
	WouldConsider bool   `json:"would_consider"`
	Action        string `json:"action"`
}

type storageAdvisor struct {
	Mode                   string          `json:"mode"`
	Status                 string          `json:"status"`
	BudgetBytes            int64           `json:"budget_bytes"`
	ManagedBytes           int64           `json:"managed_bytes"`
	BudgetRemainingBytes   int64           `json:"budget_remaining_bytes"`
	OverBudgetBytes        int64           `json:"over_budget_bytes"`
	FilesystemReserveBytes int64           `json:"filesystem_reserve_bytes"`
	ReserveRemainingBytes  int64           `json:"reserve_remaining_bytes"`
	ReadOnly               bool            `json:"read_only"`
	Candidates             []storageAdvice `json:"candidates"`
}

type storageInventoryResponse struct {
	GeneratedAtMs        int64                 `json:"generated_at_ms"`
	Databases            state.SQLiteInventory `json:"databases"`
	Files                storageFileInventory  `json:"files"`
	Filesystem           storageFilesystem     `json:"filesystem"`
	Maintenance          storageMaintenance    `json:"maintenance"`
	Advisor              storageAdvisor        `json:"advisor"`
	IncompleteCategories []string              `json:"incomplete_categories,omitempty"`
}

type footprintScan struct {
	storageFootprint
	latestSuccessMs int64
}

// handleStorageInventory performs only SQLite metadata queries and filesystem
// stats. It does not checkpoint WAL, create directories, or run maintenance.
func (s *Server) handleStorageInventory(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil || s.deps.StatePath == "" || s.deps.DataDir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "storage inventory unavailable"})
		return
	}

	dbs, err := s.deps.State.SQLiteInventory(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage inventory unavailable"})
		return
	}

	incomplete := make([]string, 0)
	data, err := scanFootprint(r.Context(), s.deps.DataDir, nil, nil)
	if err != nil {
		incomplete = append(incomplete, "data_dir")
	}

	parquet, err := scanFootprint(r.Context(), s.deps.ColdDir,
		func(_ string, d fs.DirEntry) bool {
			return !d.IsDir() && strings.EqualFold(filepath.Ext(d.Name()), ".parquet")
		},
		func(_ string, d fs.DirEntry) bool {
			return !d.IsDir() && strings.EqualFold(filepath.Ext(d.Name()), ".parquet")
		})
	if err != nil {
		incomplete = append(incomplete, "parquet")
	}
	diagnosticsDir := ""
	if s.deps.ColdDir != "" {
		diagnosticsDir = filepath.Join(s.deps.ColdDir, "diagnostics")
	}
	diagnostics, err := scanFootprint(r.Context(), diagnosticsDir,
		func(_ string, d fs.DirEntry) bool {
			return !d.IsDir() && strings.EqualFold(filepath.Ext(d.Name()), ".parquet")
		}, nil)
	if err != nil {
		incomplete = append(incomplete, "parquet_diagnostics")
	}

	recovery := scanSingleFile(s.deps.StatePath + ".snapshot")
	rollbacks, err := scanFootprint(r.Context(), s.deps.SnapshotDir, nil, nil)
	if err != nil {
		incomplete = append(incomplete, "rollback_snapshots")
	}
	snapshots, snapshotErrs := listSnapshots(s.deps.SnapshotDir)
	if len(snapshots) > 0 {
		rollbacks.latestSuccessMs = snapshots[0].CreatedAt.UnixMilli()
	}
	if len(snapshotErrs) > 0 {
		incomplete = append(incomplete, "rollback_snapshot_metadata")
	}
	backups, err := scanFootprint(r.Context(), s.deps.BackupDir, backupInventoryFile, nil)
	if err != nil {
		incomplete = append(incomplete, "full_backups")
	}
	backups.latestSuccessMs = latestVerifiedBackupMs(s.deps.BackupDir)

	parquet.OnDevice = pathWithin(s.deps.DataDir, s.deps.ColdDir)
	diagnostics.OnDevice = parquet.OnDevice
	recovery.OnDevice = pathWithin(s.deps.DataDir, s.deps.StatePath+".snapshot")
	rollbacks.OnDevice = pathWithin(s.deps.DataDir, s.deps.SnapshotDir)
	backups.OnDevice = pathWithin(s.deps.DataDir, s.deps.BackupDir)
	data.OnDevice = true

	knownBytes := dbs.State.PhysicalBytes() + dbs.Cache.PhysicalBytes()
	for _, fp := range []storageFootprint{parquet.storageFootprint, recovery.storageFootprint, rollbacks.storageFootprint, backups.storageFootprint} {
		if fp.OnDevice {
			knownBytes += fp.Bytes
		}
	}
	otherBytes := data.Bytes - knownBytes
	if otherBytes < 0 {
		otherBytes = 0
	}

	filesystem := storageFilesystem{}
	filesystemKnown := false
	if usage, usageErr := disk.UsageWithContext(r.Context(), s.deps.DataDir); usageErr == nil {
		filesystem = storageFilesystem{
			TotalBytes: usage.Total, UsedBytes: usage.Used, AvailableBytes: usage.Free, UsedPercent: usage.UsedPercent,
		}
		filesystemKnown = true
	} else {
		incomplete = append(incomplete, "filesystem")
	}

	retentionDays := s.coldRetentionDays()
	files := storageFileInventory{
		Parquet: parquetFootprint{
			storageFootprint: parquet.storageFootprint,
			DiagnosticsBytes: diagnostics.Bytes, DiagnosticsFiles: diagnostics.Files,
			RetentionDays: retentionDays,
		},
		RecoverySnapshot:  recovery.storageFootprint,
		RollbackSnapshots: rollbacks.storageFootprint,
		FullBackups:       backups.storageFootprint,
		OtherData:         storageFootprint{Bytes: otherBytes, OnDevice: true},
	}

	sort.Strings(incomplete)
	maintenance := storageMaintenance{
		LastParquetSuccessMs:          parquet.latestSuccessMs,
		LastRecoverySnapshotSuccessMs: recovery.latestSuccessMs,
		LastRollbackSnapshotSuccessMs: rollbacks.latestSuccessMs,
		LastFullBackupVerifiedMs:      backups.latestSuccessMs,
		LastRecoveryEvent:             latestHealEvent(s.deps.State.HealEvents()),
	}
	advisor := buildStorageAdvisor(data.Bytes, filesystem.AvailableBytes, filesystemKnown, files, incomplete)

	writeJSON(w, http.StatusOK, storageInventoryResponse{
		GeneratedAtMs: time.Now().UnixMilli(), Databases: dbs, Files: files,
		Filesystem: filesystem, Maintenance: maintenance, Advisor: advisor,
		IncompleteCategories: incomplete,
	})
}

func (s *Server) coldRetentionDays() int {
	if s.deps.Cfg == nil {
		return 0
	}
	if s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
		defer s.deps.CfgMu.RUnlock()
	}
	if s.deps.Cfg.State == nil {
		return 0
	}
	return s.deps.Cfg.State.ColdRetentionDays
}

func buildStorageAdvisor(managedBytes int64, availableBytes uint64, filesystemKnown bool, files storageFileInventory, incomplete []string) storageAdvisor {
	remaining := defaultDryRunBudgetBytes - managedBytes
	over := int64(0)
	if remaining < 0 {
		over = -remaining
	}
	reserveRemaining := int64(0)
	if filesystemKnown {
		reserveRemaining = int64(availableBytes) - filesystemReserveBytes
	}
	pressure := remaining < 0 || (filesystemKnown && reserveRemaining < 0)
	status := "ok"
	switch {
	case pressure:
		status = "action_needed"
	case len(incomplete) > 0:
		status = "incomplete"
	case remaining < defaultDryRunBudgetBytes/7 || (filesystemKnown && int64(availableBytes) < 2*filesystemReserveBytes):
		status = "watch"
	}

	candidates := []storageAdvice{
		{
			Category: "full_backups", ObservedBytes: files.FullBackups.Bytes,
			WouldConsider: pressure && files.FullBackups.OnDevice && files.FullBackups.Bytes > 0,
			Action:        "move verified full backups off-device before applying any local backup retention",
		},
		{
			Category: "parquet", ObservedBytes: files.Parquet.Bytes,
			WouldConsider: pressure && files.Parquet.OnDevice && files.Parquet.Bytes > 0,
			Action:        "set state.cold_retention_days only after validating history and model-evaluation requirements",
		},
		{
			Category: "planner_diagnostics", ObservedBytes: files.Parquet.DiagnosticsBytes,
			WouldConsider: pressure && files.Parquet.OnDevice && files.Parquet.DiagnosticsBytes > 0,
			Action:        "evaluate a shorter future diagnostics retention independently from telemetry history",
		},
		{
			Category: "rollback_snapshots", ObservedBytes: files.RollbackSnapshots.Bytes,
			WouldConsider: pressure && files.RollbackSnapshots.OnDevice && files.RollbackSnapshots.Bytes > 0,
			Action:        "review bounded local rollback points after confirming a verified off-device full backup",
		},
	}
	return storageAdvisor{
		Mode: "dry_run", Status: status, BudgetBytes: defaultDryRunBudgetBytes,
		ManagedBytes: managedBytes, BudgetRemainingBytes: remaining, OverBudgetBytes: over,
		FilesystemReserveBytes: filesystemReserveBytes, ReserveRemainingBytes: reserveRemaining,
		ReadOnly: true, Candidates: candidates,
	}
}

func scanFootprint(ctx context.Context, root string, include, success func(string, fs.DirEntry) bool) (footprintScan, error) {
	if root == "" {
		return footprintScan{}, nil
	}
	var out footprintScan
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) && path == root {
				return nil
			}
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || (include != nil && !include(path, entry)) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil
		}
		out.Bytes += info.Size()
		out.Files++
		modified := info.ModTime().UnixMilli()
		if modified > out.LatestObservedMs {
			out.LatestObservedMs = modified
		}
		if success != nil && success(path, entry) && modified > out.latestSuccessMs {
			out.latestSuccessMs = modified
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return footprintScan{}, nil
	}
	return out, err
}

func scanSingleFile(path string) footprintScan {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return footprintScan{}
	}
	modified := info.ModTime().UnixMilli()
	return footprintScan{
		storageFootprint: storageFootprint{Bytes: info.Size(), Files: 1, LatestObservedMs: modified},
		latestSuccessMs:  modified,
	}
}

func backupInventoryFile(_ string, entry fs.DirEntry) bool {
	if entry.IsDir() {
		return false
	}
	name := entry.Name()
	if validBackupID(name) {
		return true
	}
	return strings.HasSuffix(name, ".verified.json") && validBackupID(strings.TrimSuffix(name, ".verified.json"))
}

func latestVerifiedBackupMs(dir string) int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	var latest int64
	for _, entry := range entries {
		if entry.IsDir() || !validBackupID(entry.Name()) {
			continue
		}
		archivePath := filepath.Join(dir, entry.Name())
		archiveInfo, err := entry.Info()
		if err != nil {
			continue
		}
		verificationInfo, err := os.Stat(verificationPath(archivePath))
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(verificationPath(archivePath))
		if err != nil {
			continue
		}
		var verified backup.Info
		if json.Unmarshal(raw, &verified) != nil || !verified.Verified ||
			verified.ID != entry.Name() || verified.SizeBytes != archiveInfo.Size() {
			continue
		}
		if modified := verificationInfo.ModTime().UnixMilli(); modified > latest {
			latest = modified
		}
	}
	return latest
}

func latestHealEvent(events []state.HealEvent) *state.HealEvent {
	if len(events) == 0 {
		return nil
	}
	latest := events[0]
	for _, event := range events[1:] {
		if event.AtMs > latest.AtMs {
			latest = event
		}
	}
	return &latest
}
