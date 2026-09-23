package state

import (
	"context"
	"time"
)

type backupReportKey struct{}

func reportBackupRows(ctx context.Context, phase, table string, rows int64) {
	if report, ok := ctx.Value(backupReportKey{}).(func(BackupProgress)); ok && report != nil {
		report(BackupProgress{Phase: phase, Table: table, RowsDone: rows})
	}
}

func (s *Store) ReportBackupProgress(p BackupProgress) {
	s.backupStatusMu.Lock()
	defer s.backupStatusMu.Unlock()
	p.UpdatedMS = time.Now().UnixMilli()
	s.backupStatus = p
}

func (s *Store) BackupProgress() BackupProgress {
	if s == nil {
		return BackupProgress{}
	}
	s.backupStatusMu.Lock()
	defer s.backupStatusMu.Unlock()
	return s.backupStatus
}
