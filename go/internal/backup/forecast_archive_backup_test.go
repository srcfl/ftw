package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

func backupForecastIssue(origin time.Time) forecasting.Issue {
	start := origin.UnixMilli()
	band := forecasting.Band{Method: forecasting.BandMethodColdStart}
	point := forecasting.Point{
		StartMS: start, EndMS: start + int64(time.Hour/time.Millisecond),
		PVW: 12, LoadW: 34, PVKnown: true, LoadKnown: true,
		PVQuality: "ready", LoadQuality: "ready",
		PVBand: band, LoadBand: band, NetBand: band,
	}
	return forecasting.Issue{
		Schema: forecasting.Schema, ID: "backed-up", DecisionID: "decision",
		OriginMS: start, IssuedAtMS: start, LatestInputMS: start, ConfigVersion: "cfg",
		Series: []forecasting.Series{{Name: "champion", ModelVersion: "v1", Points: []forecasting.Point{point}}},
	}
}

func TestFullBackupIncludesConsistentForecastArchive(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "offline"}[offline], func(t *testing.T) {
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(dataDir, "state.db")
			st, err := state.Open(statePath)
			if err != nil {
				t.Fatal(err)
			}
			arch, err := st.OpenForecastLongArchive(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			origin := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
			if err := arch.ArchiveIssues(context.Background(), []state.ForecastArchiveIssue{{Issue: backupForecastIssue(origin)}}); err != nil {
				t.Fatal(err)
			}
			src := st
			if offline {
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
				src, err = state.OpenBackupSource(statePath)
				if err != nil {
					t.Fatal(err)
				}
			}
			defer src.Close()
			info, err := Create(context.Background(), CreateOptions{
				State: src, StatePath: statePath, DataDir: dataDir,
				OutputDir: filepath.Join(root, "backups"),
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest, _, err := Inspect(context.Background(), info.Path)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, file := range manifest.Files {
				if file.Path == "data/forecast-archive.db" {
					found = true
				}
				if file.Path == "data/forecast-archive.db-wal" || file.Path == "data/forecast-archive.db-shm" {
					t.Fatalf("backup contains a live sidecar: %s", file.Path)
				}
			}
			if !found {
				t.Fatal("backup is missing forecast-archive.db")
			}
			restoredDir := filepath.Join(root, "restored")
			if _, err := restoreFresh(info.Path, restoredDir, time.Time{}); err != nil {
				t.Fatal(err)
			}
			restored, err := state.Open(filepath.Join(restoredDir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			restoredArch, err := restored.OpenForecastLongArchive(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := restoredArch.LoadIssue(context.Background(), "backed-up")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Series[0].Points[0].LoadW != 34 {
				t.Fatalf("restored issue = %+v", loaded.Series[0].Points[0])
			}
		})
	}
}

func TestFullBackupSkipsDamagedForecastArchive(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := os.WriteFile(state.ForecastLongArchivePath(statePath), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Copies an earlier heal moved aside stay out of the backup too.
	for _, name := range []string{"forecast-archive.db.corrupt-1", "forecast-archive.db-wal.corrupt-1", "forecast-archive.db-shm.corrupt-1"} {
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte("quarantined"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	info, err := Create(context.Background(), CreateOptions{
		State: st, StatePath: statePath, DataDir: dataDir,
		OutputDir: filepath.Join(root, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(header.Name, "data/forecast-archive.db") {
			t.Fatalf("damaged or quarantined archive was copied: %s", header.Name)
		}
	}
}

func TestBackupPreflightCountsForecastArchive(t *testing.T) {
	root := t.TempDir()
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	archivePath := state.ForecastLongArchivePath(filepath.Join(root, "state.db"))
	const archiveBytes = 1 << 20
	if err := os.WriteFile(archivePath, make([]byte, archiveBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	old := ensureBackupDiskSpace
	t.Cleanup(func() { ensureBackupDiskSpace = old })
	var needed int64
	ensureBackupDiskSpace = func(dir string, n int64) error {
		needed = n
		return os.ErrPermission
	}
	_, err = Create(context.Background(), CreateOptions{
		State: st, StatePath: filepath.Join(root, "state.db"), DataDir: root,
		OutputDir: filepath.Join(root, "backups"),
	})
	if err == nil {
		t.Fatal("preflight did not run")
	}
	minimum := state.BackupArchiveScratch(st.BackupSourceBytes(), archiveBytes)
	if needed < minimum {
		t.Fatalf("space check %d omits the %d byte forecast archive (minimum %d)", needed, archiveBytes, minimum)
	}
}
