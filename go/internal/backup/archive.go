// Package backup creates and restores complete, portable FTW backups.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

const (
	Format        = "ftw-full-backup"
	SchemaVersion = 1
	manifestPath  = "manifest.json"
)

var ensureBackupDiskSpace = state.EnsureDiskSpace

// ComponentInventory records the independently versioned runtime that created
// the backup. Driver artifacts themselves are also captured under data/.
type ComponentInventory struct {
	Core      ComponentVersion  `json:"core"`
	Optimizer *ComponentVersion `json:"optimizer,omitempty"`
	Drivers   []DriverVersion   `json:"drivers,omitempty"`
}

type ComponentVersion struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol,omitempty"`
}

type DriverVersion struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

type FileEntry struct {
	Path       string `json:"path"`
	Type       string `json:"type"` // file | symlink
	Mode       uint32 `json:"mode"`
	Size       int64  `json:"size,omitempty"`
	SHA256     string `json:"sha256"`
	LinkTarget string `json:"link_target,omitempty"`
}

type Manifest struct {
	Format        string             `json:"format"`
	SchemaVersion int                `json:"schema_version"`
	CreatedAt     time.Time          `json:"created_at"`
	DatabaseFile  string             `json:"database_file"`
	DatabaseEntry string             `json:"database_entry"`
	Components    ComponentInventory `json:"components"`
	Files         []FileEntry        `json:"files"`
}

type CreateOptions struct {
	ConfigPath  string // Defaults to config.yaml inside DataDir.
	State       *state.Store
	StatePath   string
	DataDir     string
	OutputDir   string
	Components  ComponentInventory
	Maintenance *sync.Mutex
	Progress    func(state.BackupProgress)
	Now         time.Time
}

type Info struct {
	ID        string    `json:"id"`
	Path      string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	Verified  bool      `json:"verified"`
}

type RestoreResult struct {
	DataDir   string `json:"data_dir"`
	SafetyDir string `json:"safety_dir,omitempty"`
}

// Inspect performs a full verification and returns stable archive metadata.
func Inspect(ctx context.Context, archivePath string) (Manifest, Info, error) {
	manifest, err := Verify(archivePath)
	if err != nil {
		return Manifest{}, Info{}, err
	}
	stat, err := os.Stat(archivePath)
	if err != nil {
		return Manifest{}, Info{}, err
	}
	sum, err := hashFile(ctx, archivePath)
	if err != nil {
		return Manifest{}, Info{}, err
	}
	return manifest, Info{
		ID: filepath.Base(archivePath), Path: archivePath, CreatedAt: manifest.CreatedAt,
		SizeBytes: stat.Size(), SHA256: sum, Verified: true,
	}, nil
}

type sourceEntry struct {
	archivePath string
	sourcePath  string
	entry       FileEntry
	isDatabase  bool
}

// Create writes a complete backup into OutputDir, verifies the finished
// archive including SQLite quick_check, and only then publishes its final
// filename. The archive is mode 0600 because config and state contain secrets.
func Create(ctx context.Context, opts CreateOptions) (Info, error) {
	if opts.State == nil {
		return Info{}, errors.New("backup: state store unavailable")
	}
	if opts.StatePath == "" || opts.DataDir == "" || opts.OutputDir == "" {
		return Info{}, errors.New("backup: state path, data dir and output dir are required")
	}
	dataDir, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return Info{}, err
	}
	statePath, err := filepath.Abs(opts.StatePath)
	if err != nil {
		return Info{}, err
	}
	outputDir, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return Info{}, err
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return Info{}, fmt.Errorf("backup: create output dir: %w", err)
	}
	if opts.Progress != nil {
		opts.Progress(state.BackupProgress{Phase: "waiting_for_maintenance"})
	}
	resume, err := opts.State.PauseHistoryMaintenance(ctx)
	if err != nil {
		return Info{}, err
	}
	defer resume()
	if opts.Maintenance != nil {
		for !opts.Maintenance.TryLock() {
			select {
			case <-ctx.Done():
				return Info{}, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
		defer opts.Maintenance.Unlock()
	}
	importedHistory, err := opts.State.ImportedHistoryFiles(ctx)
	if err != nil {
		return Info{}, err
	}
	sources, err := collectSources(dataDir, statePath, outputDir, importedHistory)
	if err != nil {
		return Info{}, err
	}
	extraBytes, err := backupExtraBytes(sources)
	if err != nil {
		return Info{}, err
	}
	if err := ensureBackupDiskSpace(outputDir, state.BackupArchiveScratch(opts.State.BackupSourceBytes(), extraBytes)); err != nil {
		return Info{}, err
	}

	created := opts.Now.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	id := "ftw-full-backup-" + created.Format("20060102T150405.000Z") + ".ftwbak"
	finalPath := filepath.Join(outputDir, id)
	if _, err := os.Stat(finalPath); err == nil {
		return Info{}, fmt.Errorf("backup: destination already exists: %s", finalPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Info{}, err
	}

	stageDir, err := os.MkdirTemp(outputDir, ".ftw-backup-stage-")
	if err != nil {
		return Info{}, fmt.Errorf("backup: create staging dir: %w", err)
	}
	defer os.RemoveAll(stageDir)
	databaseGzip := filepath.Join(stageDir, "database.gz")
	stored, hasStored, err := opts.State.BackupWithConfigurationContext(ctx, databaseGzip, opts.Progress)
	if err != nil {
		return Info{}, err
	}

	databaseFile, err := filepath.Rel(dataDir, statePath)
	if err != nil || databaseFile == ".." || strings.HasPrefix(databaseFile, ".."+string(filepath.Separator)) {
		databaseFile = filepath.Base(statePath)
	}
	databaseFile = filepath.ToSlash(databaseFile)
	databaseEntry := "data/" + databaseFile + ".gz"
	if hasStored {
		configPath := opts.ConfigPath
		if configPath == "" {
			configPath = filepath.Join(dataDir, "config.yaml")
		}
		configPath, err = filepath.Abs(configPath)
		if err != nil {
			return Info{}, err
		}
		configFile, err := filepath.Rel(dataDir, configPath)
		if err != nil || configFile == ".." || strings.HasPrefix(configFile, ".."+string(filepath.Separator)) {
			return Info{}, errors.New("backup config is outside data directory")
		}
		databaseRef, err := filepath.Rel(filepath.Dir(configPath), filepath.Join(dataDir, databaseFile))
		if err != nil {
			return Info{}, err
		}
		exported := filepath.Join(stageDir, "config.yaml")
		if err := config.ExportStored(exported, stored, databaseRef); err != nil {
			return Info{}, err
		}
		archivePath := "data/" + filepath.ToSlash(configFile)
		filtered := sources[:0]
		for _, source := range sources {
			if source.archivePath != archivePath {
				filtered = append(filtered, source)
			}
		}
		sources = append(filtered, sourceEntry{archivePath: archivePath, sourcePath: exported})
	}
	sources = append(sources, sourceEntry{
		archivePath: databaseEntry,
		sourcePath:  databaseGzip,
		isDatabase:  true,
	})
	var sourceBytes int64
	for _, source := range sources {
		info, err := os.Lstat(source.sourcePath)
		if err != nil {
			return Info{}, err
		}
		if info.Mode().IsRegular() {
			sourceBytes += info.Size()
		}
	}
	var checkedBytes int64
	if opts.Progress != nil {
		opts.Progress(state.BackupProgress{Phase: "checking_sources", TotalBytes: sourceBytes})
	}
	for i := range sources {
		entry, err := describeSource(ctx, dataDir, sources[i])
		if err != nil {
			return Info{}, err
		}
		sources[i].entry = entry
		checkedBytes += entry.Size
		if opts.Progress != nil {
			opts.Progress(state.BackupProgress{Phase: "checking_sources", CompletedBytes: checkedBytes, TotalBytes: sourceBytes})
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].archivePath < sources[j].archivePath })
	files := make([]FileEntry, len(sources))
	for i := range sources {
		files[i] = sources[i].entry
	}
	manifest := Manifest{
		Format: Format, SchemaVersion: SchemaVersion, CreatedAt: created,
		DatabaseFile: databaseFile, DatabaseEntry: databaseEntry,
		Components: opts.Components, Files: files,
	}
	if err := validateManifest(manifest); err != nil {
		return Info{}, err
	}

	tmpPath := filepath.Join(outputDir, "."+id+".tmp")
	if err := writeArchiveWithProgress(ctx, tmpPath, manifest, sources, !opts.State.OfflineBackup(), opts.Progress); err != nil {
		_ = os.Remove(tmpPath)
		return Info{}, err
	}
	if _, err := verifyInWorkspaceProgressContext(ctx, tmpPath, outputDir, opts.Progress); err != nil {
		_ = os.Remove(tmpPath)
		return Info{}, fmt.Errorf("backup: verify finished archive: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return Info{}, fmt.Errorf("backup: publish archive: %w", err)
	}
	if err := syncDir(outputDir); err != nil {
		return Info{}, err
	}
	info, err := os.Stat(finalPath)
	if err != nil {
		return Info{}, err
	}
	sum, err := hashFile(ctx, finalPath)
	if err != nil {
		return Info{}, err
	}
	return Info{ID: id, Path: finalPath, CreatedAt: created, SizeBytes: info.Size(), SHA256: sum, Verified: true}, nil
}

// Count regular source bytes without following symlinks or hashing the archive.
func backupExtraBytes(sources []sourceEntry) (int64, error) {
	var total int64
	for _, source := range sources {
		info, err := os.Lstat(source.sourcePath)
		if err != nil {
			return 0, err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total, nil
}

func collectSources(dataDir, statePath, outputDir string, importedHistory map[string]bool) ([]sourceEntry, error) {
	stateRel, _ := filepath.Rel(dataDir, statePath)
	cacheRel, _ := filepath.Rel(dataDir, filepath.Join(filepath.Dir(statePath), "cache.db"))
	outputRel, outputInside := filepath.Rel(dataDir, outputDir)
	if outputInside != nil || outputRel == ".." || strings.HasPrefix(outputRel, ".."+string(filepath.Separator)) {
		outputRel = ""
	}
	var out []sourceEntry
	err := filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == dataDir {
			return nil
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil {
			return err
		}
		// Aggregate compaction scratch is derived from the retained source;
		// restoring it is unnecessary and it may have an open SQLite journal.
		if strings.HasPrefix(d.Name(), ".ftw-buckets-") || strings.HasPrefix(d.Name(), ".ftw-archive-") || strings.HasPrefix(d.Name(), ".ftw-samples-") || strings.HasPrefix(d.Name(), ".ftw-summary-") || strings.HasPrefix(d.Name(), ".ftw-parquet-") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		first := strings.Split(rel, string(filepath.Separator))[0]
		if first == "snapshots" || first == "backups" || isRestoreInternalName(first) || strings.HasPrefix(first, ".ftw-backup-") ||
			(outputRel != "" && (rel == outputRel || strings.HasPrefix(rel, outputRel+string(filepath.Separator)))) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Primary history is exported from one read transaction into the
		// SQLite backup above. Never copy a live database file or WAL.
		historyRel, _ := filepath.Rel(dataDir, state.HistoryDatabasePath(statePath))
		if rel == historyRel || rel == historyRel+"-wal" || rel == historyRel+"-shm" || strings.HasPrefix(rel, historyRel+".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Beta originals remain on disk for recovery; the converter verified
		// their rows into the canonical history exported above.
		betaRel, _ := filepath.Rel(dataDir, state.BetaHistoryDatabasePath(statePath))
		if rel == betaRel || rel == betaRel+".wal" || first == state.HotHistoryFilename || first == state.HotHistoryFilename+"-wal" || first == state.HotHistoryFilename+"-shm" {
			return nil
		}

		if importedHistory[p] {
			return nil
		}
		if rel == stateRel || rel == stateRel+"-wal" || rel == stateRel+"-shm" || rel == stateRel+"-journal" {
			return nil
		}
		// Derived files beside state.db. The recovery snapshot copies state.db,
		// whose rows the export above already carries. It is rewritten through
		// a temp database and its journal two minutes after every start, so a
		// backup taken right after an update walked into files that vanished.
		// cache.db holds only re-fetchable data. Restore moves all of them
		// aside anyway.
		if strings.HasPrefix(rel, stateRel+".snapshot") || rel == cacheRel || strings.HasPrefix(rel, cacheRel+"-") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			out = append(out, sourceEntry{archivePath: "data/" + filepath.ToSlash(rel), sourcePath: p})
			return nil
		}
		return fmt.Errorf("backup: unsupported persistent file type: %s", p)
	})
	return out, err
}

func describeSource(ctx context.Context, dataDir string, source sourceEntry) (FileEntry, error) {
	info, err := os.Lstat(source.sourcePath)
	if err != nil {
		return FileEntry{}, err
	}
	entry := FileEntry{Path: source.archivePath, Mode: uint32(info.Mode().Perm())}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source.sourcePath)
		if err != nil {
			return FileEntry{}, err
		}
		resolved := target
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(source.sourcePath), resolved)
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil || !pathInside(dataDir, resolved) {
			return FileEntry{}, fmt.Errorf("backup: symlink escapes data dir: %s -> %s", source.sourcePath, target)
		}
		// Managed driver activations use absolute paths in the running
		// container. Store an internal relative link so restore can relocate
		// data without referring back to /app/data or the original machine.
		// Do not follow the link; validate the complete archive graph below.
		if filepath.IsAbs(target) {
			rootPrefix := dataDir + string(filepath.Separator)
			if !strings.HasPrefix(target, rootPrefix) {
				return FileEntry{}, fmt.Errorf("backup: absolute symlink is outside data dir: %s", source.sourcePath)
			}
			back, relErr := filepath.Rel(filepath.Dir(source.sourcePath), dataDir)
			if relErr != nil {
				return FileEntry{}, relErr
			}
			// Preserve .. until archive graph validation has expanded links.
			target = back + string(filepath.Separator) + strings.TrimPrefix(target, rootPrefix)
		}
		target = filepath.ToSlash(target)
		entry.Type, entry.LinkTarget = "symlink", target
		h := sha256.Sum256([]byte(target))
		entry.SHA256 = hex.EncodeToString(h[:])
		return entry, nil
	}
	entry.Type, entry.Size = "file", info.Size()
	entry.SHA256, err = hashFile(ctx, source.sourcePath)
	return entry, err
}

func writeArchive(ctx context.Context, dst string, manifest Manifest, sources []sourceEntry, live bool) error {
	return writeArchiveWithProgress(ctx, dst, manifest, sources, live, nil)
}

func writeArchiveWithProgress(ctx context.Context, dst string, manifest Manifest, sources []sourceEntry, live bool, report func(state.BackupProgress)) error {
	var total int64
	for _, source := range sources {
		if source.entry.Type == "file" {
			total += source.entry.Size
		}
	}
	progress := newArchiveProgress("packing_archive", total, report)
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(dst)
		}
	}()
	zw, err := gzip.NewWriterLevel(state.NewMaintenanceWriterPaced(ctx, f, live), gzip.BestSpeed)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(zw)
	manifestBody, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: manifestPath, Mode: 0o600, Size: int64(len(manifestBody)), ModTime: manifest.CreatedAt}); err != nil {
		return err
	}
	if _, err := tw.Write(manifestBody); err != nil {
		return err
	}
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := source.entry
		header := &tar.Header{Name: e.Path, Mode: int64(e.Mode), ModTime: manifest.CreatedAt}
		if e.Type == "symlink" {
			header.Typeflag, header.Linkname = tar.TypeSymlink, e.LinkTarget
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			continue
		}
		header.Typeflag, header.Size = tar.TypeReg, e.Size
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		in, err := os.Open(source.sourcePath)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, copyErr := copyContext(ctx, io.MultiWriter(tw, h), io.TeeReader(in, progress))
		closeErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
			return fmt.Errorf("backup: %s changed while archiving", e.Path)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	progress.finish()
	if report != nil {
		report(state.BackupProgress{Phase: "syncing_archive"})
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	committed = true
	return nil
}

type archiveProgress struct {
	phase  string
	total  int64
	done   int64
	last   time.Time
	report func(state.BackupProgress)
}

func newArchiveProgress(phase string, total int64, report func(state.BackupProgress)) *archiveProgress {
	p := &archiveProgress{phase: phase, total: total, last: time.Now(), report: report}
	p.publish()
	return p
}

func (p *archiveProgress) Write(data []byte) (int, error) {
	p.done += int64(len(data))
	if time.Since(p.last) >= time.Second || p.done == p.total {
		p.publish()
		p.last = time.Now()
	}
	return len(data), nil
}

func (p *archiveProgress) finish() {
	p.publish()
}

func (p *archiveProgress) publish() {
	if p.report != nil {
		p.report(state.BackupProgress{Phase: p.phase, CompletedBytes: p.done, TotalBytes: p.total})
	}
}

// Verify checks archive structure, every file hash, SQLite quick_check and
// Parquet readability. A matching hash alone cannot detect a damaged source.
func Verify(archivePath string) (Manifest, error) {
	return verifyInWorkspace(archivePath, "")
}

// Creation uses its preflighted output filesystem; restore uses its writable
// target. Inspect/verify prefer the archive filesystem, then the caller's
// temporary workspace (TMPDIR) when the source directory is read-only.
func verifyInWorkspace(archivePath, workspace string) (Manifest, error) {
	return verifyInWorkspaceContext(context.Background(), archivePath, workspace)
}

func verifyInWorkspaceContext(ctx context.Context, archivePath, workspace string) (Manifest, error) {
	return verifyInWorkspaceProgressContext(ctx, archivePath, workspace, nil)
}

func verifyInWorkspaceProgressContext(ctx context.Context, archivePath, workspace string, report func(state.BackupProgress)) (Manifest, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	archiveInfo, err := f.Stat()
	if err != nil {
		return Manifest{}, err
	}
	progress := newArchiveProgress("verifying_archive", archiveInfo.Size(), report)
	zr, err := gzip.NewReader(backupContextReader{ctx, io.TeeReader(f, progress)})
	if err != nil {
		return Manifest{}, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	header, err := tr.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: read manifest header: %w", err)
	}
	if header.Name != manifestPath || header.Typeflag != tar.TypeReg || header.Size > 4<<20 {
		return Manifest{}, errors.New("backup: manifest must be the first regular entry")
	}
	var manifest Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("backup: decode manifest: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	want := make(map[string]FileEntry, len(manifest.Files))
	for _, entry := range manifest.Files {
		want[entry.Path] = entry
	}
	seen := make(map[string]bool, len(want))
	allowFallback := workspace == ""
	if allowFallback {
		workspace = filepath.Dir(archivePath)
	}
	tmpDir, err := os.MkdirTemp(workspace, ".ftw-backup-verify-")
	if err != nil && allowFallback {
		tmpDir, err = os.MkdirTemp("", ".ftw-backup-verify-")
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: create verification workspace: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	dbGzip := filepath.Join(tmpDir, "database.gz")
	for {
		header, err = tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, err
		}
		entry, ok := want[header.Name]
		if !ok || seen[header.Name] {
			return Manifest{}, fmt.Errorf("backup: unexpected or duplicate entry %q", header.Name)
		}
		seen[header.Name] = true
		if header.Mode != int64(entry.Mode) {
			return Manifest{}, fmt.Errorf("backup: mode mismatch for %s", entry.Path)
		}
		if entry.Type == "symlink" {
			if header.Typeflag != tar.TypeSymlink || header.Linkname != entry.LinkTarget {
				return Manifest{}, fmt.Errorf("backup: symlink mismatch for %s", entry.Path)
			}
			h := sha256.Sum256([]byte(header.Linkname))
			if hex.EncodeToString(h[:]) != entry.SHA256 {
				return Manifest{}, fmt.Errorf("backup: symlink hash mismatch for %s", entry.Path)
			}
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size != entry.Size {
			return Manifest{}, fmt.Errorf("backup: file metadata mismatch for %s", entry.Path)
		}
		h := sha256.New()
		var writer io.Writer = h
		var dbFile *os.File
		parquetPath := ""
		if entry.Path == manifest.DatabaseEntry {
			dbFile, err = os.OpenFile(dbGzip, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		} else if strings.HasSuffix(strings.ToLower(entry.Path), ".parquet") {
			// Stage and remove one day at a time, not the entire cold archive.
			parquetPath = filepath.Join(tmpDir, "parquet.tmp")
			dbFile, err = os.OpenFile(parquetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		}
		if err != nil {
			return Manifest{}, err
		}
		if dbFile != nil {
			writer = io.MultiWriter(h, dbFile)
		}
		if _, err := io.Copy(writer, tr); err != nil {
			if dbFile != nil {
				_ = dbFile.Close()
			}
			return Manifest{}, err
		}
		if dbFile != nil {
			if err := dbFile.Close(); err != nil {
				return Manifest{}, err
			}
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != entry.SHA256 {
			return Manifest{}, fmt.Errorf("backup: hash mismatch for %s", entry.Path)
		}
		if parquetPath != "" {
			if err := state.VerifyParquetFile(ctx, parquetPath); err != nil {
				return Manifest{}, fmt.Errorf("backup: unreadable Parquet %s: %w", entry.Path, err)
			}
			if err := os.Remove(parquetPath); err != nil {
				return Manifest{}, err
			}
		}
	}
	if len(seen) != len(want) {
		return Manifest{}, errors.New("backup: archive is missing one or more manifest files")
	}
	progress.finish()
	if report != nil {
		report(state.BackupProgress{Phase: "checking_database"})
	}
	if err := verifyCompressedDatabaseContext(ctx, dbGzip, tmpDir); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func validateManifest(manifest Manifest) error {
	if manifest.Format != Format || manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("backup: unsupported format %q schema %d", manifest.Format, manifest.SchemaVersion)
	}
	if manifest.CreatedAt.IsZero() || !safeDataPath(manifest.DatabaseEntry) || !safeRelativePath(manifest.DatabaseFile) {
		return errors.New("backup: invalid manifest identity or database path")
	}
	seen := make(map[string]bool, len(manifest.Files))
	symlinks := make(map[string]string)
	databaseFound := false
	for _, entry := range manifest.Files {
		if !safeDataPath(entry.Path) || entry.Path == manifestPath || seen[entry.Path] {
			return fmt.Errorf("backup: invalid or duplicate file path %q", entry.Path)
		}
		seen[entry.Path] = true
		if entry.Path == manifest.DatabaseEntry {
			databaseFound = entry.Type == "file"
		}
		if entry.Type != "file" && entry.Type != "symlink" {
			return fmt.Errorf("backup: invalid entry type for %s", entry.Path)
		}
		if entry.Mode > 0o777 {
			return fmt.Errorf("backup: invalid mode for %s", entry.Path)
		}
		if entry.Type == "file" {
			if entry.Size < 0 || len(entry.SHA256) != 64 {
				return fmt.Errorf("backup: invalid file metadata for %s", entry.Path)
			}
		} else {
			if filepath.IsAbs(entry.LinkTarget) || path.IsAbs(entry.LinkTarget) || entry.LinkTarget == "" || len(entry.SHA256) != 64 {
				return fmt.Errorf("backup: unsafe symlink %s", entry.Path)
			}
			// Resolve the link using archive (slash-separated) paths. A link
			// may point sideways within data/, but never above that root. This
			// check is required even when no later archive entry is nested below
			// the link: otherwise Restore could recreate a standalone link to a
			// host path outside the staging directory.
			resolved := path.Clean(path.Join(path.Dir(entry.Path), entry.LinkTarget))
			if !safeDataPath(resolved) {
				return fmt.Errorf("backup: symlink target escapes data root: %s -> %s", entry.Path, entry.LinkTarget)
			}
			symlinks[entry.Path] = entry.LinkTarget
		}
	}
	if !databaseFound {
		return errors.New("backup: compressed database missing from manifest")
	}
	// A lexical path check alone misses `alias/../outside`: .. applies
	// after alias is followed. Resolve each link against archive entries,
	// never the host filesystem, and reject cycles or escape chains.
	for name, target := range symlinks {
		if err := validateArchiveLink(name, target, symlinks); err != nil {
			return err
		}
	}
	for _, entry := range manifest.Files {
		parent := path.Dir(entry.Path)
		for parent != "." && parent != "/" {
			if _, ok := symlinks[parent]; ok {
				return fmt.Errorf("backup: entry %s is nested below symlink %s", entry.Path, parent)
			}
			parent = path.Dir(parent)
		}
	}
	return nil
}

// validateArchiveLink models relative symlink traversal without cleaning away
// .. before symlink expansion. Archive paths start at the data root.
func validateArchiveLink(name, target string, links map[string]string) error {
	parts := strings.Split(strings.TrimPrefix(path.Dir(name), "data/"), "/")
	if path.Dir(name) == "data" {
		parts = nil
	}
	pending := strings.Split(target, "/")
	expansions := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(parts) == 0 {
				return fmt.Errorf("backup: symlink chain escapes data root: %s", name)
			}
			parts = parts[:len(parts)-1]
		default:
			candidate := "data/" + strings.Join(append(append([]string{}, parts...), part), "/")
			if next, ok := links[candidate]; ok {
				expansions++
				if expansions > 40 {
					return fmt.Errorf("backup: cyclic or excessive symlink chain: %s", name)
				}
				pending = append(strings.Split(next, "/"), pending...)
			} else {
				parts = append(parts, part)
			}
		}
	}
	return nil
}

// Restore verifies and extracts the archive into a new directory, then swaps
// it into place while retaining the previous data directory beside it.
// Callers must stop FTW before invoking this function.
func Restore(archivePath, dataDir string, now time.Time) (RestoreResult, error) {
	var err error
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return RestoreResult{}, err
	}
	parent := filepath.Dir(dataDir)
	if _, err := verifyInWorkspace(archivePath, parent); err != nil {
		return RestoreResult{}, err
	}
	staging, err := os.MkdirTemp(parent, ".ftw-restore-stage-")
	if err != nil {
		return RestoreResult{}, err
	}
	keepStaging := false
	defer func() {
		if !keepStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractArchive(archivePath, staging); err != nil {
		return RestoreResult{}, err
	}
	stamp := now.UTC()
	if stamp.IsZero() {
		stamp = time.Now().UTC()
	}
	safetyDir := dataDir + ".pre-restore-" + stamp.Format("20060102T150405Z")
	if _, err := os.Stat(safetyDir); err == nil {
		return RestoreResult{}, fmt.Errorf("backup: safety directory already exists: %s", safetyDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return RestoreResult{}, err
	}
	if _, err := os.Stat(dataDir); err == nil {
		if err := os.Rename(dataDir, safetyDir); err != nil {
			return RestoreResult{}, fmt.Errorf("backup: preserve current data: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return RestoreResult{}, err
	} else {
		safetyDir = ""
	}
	if err := os.Rename(staging, dataDir); err != nil {
		if safetyDir != "" {
			_ = os.Rename(safetyDir, dataDir)
		}
		return RestoreResult{}, fmt.Errorf("backup: activate restored data: %w", err)
	}
	if err := syncDir(parent); err != nil {
		rollbackErr := os.Rename(dataDir, staging)
		if rollbackErr == nil && safetyDir != "" {
			rollbackErr = os.Rename(safetyDir, dataDir)
		}
		rollbackErr = errors.Join(rollbackErr, syncDir(parent))
		return RestoreResult{}, errors.Join(fmt.Errorf("backup: sync activated restore: %w", err), rollbackErr)
	}
	keepStaging = true
	return RestoreResult{DataDir: dataDir, SafetyDir: safetyDir}, nil
}

// RestoreContents restores into an existing persistent mount. Unlike Restore,
// it does not rename dataDir itself (mount roots cannot be renamed). It first
// extracts and verifies the complete archive, then moves the current top-level
// entries into a retained safety directory before activating the restored
// entries. If activation fails, the moves are reversed before returning.
// Callers must stop FTW before invoking this function.
func RestoreContents(archivePath, dataDir string, now time.Time) (RestoreResult, error) {
	var err error
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return RestoreResult{}, err
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		return RestoreResult{}, err
	}
	if !info.IsDir() {
		return RestoreResult{}, fmt.Errorf("backup: restore target is not a directory: %s", dataDir)
	}
	if _, err := verifyInWorkspace(archivePath, dataDir); err != nil {
		return RestoreResult{}, err
	}
	staging, err := os.MkdirTemp(dataDir, ".ftw-restore-stage-")
	if err != nil {
		return RestoreResult{}, err
	}
	defer os.RemoveAll(staging)
	if err := extractArchive(archivePath, staging); err != nil {
		return RestoreResult{}, err
	}
	stamp := now.UTC()
	if stamp.IsZero() {
		stamp = time.Now().UTC()
	}
	safetyDir := filepath.Join(dataDir, ".ftw-pre-restore-"+stamp.Format("20060102T150405Z"))
	if err := os.Mkdir(safetyDir, 0o700); err != nil {
		return RestoreResult{}, fmt.Errorf("backup: create safety directory: %w", err)
	}

	current, err := topLevelEntries(dataDir, staging, safetyDir)
	if err != nil {
		_ = os.Remove(safetyDir)
		return RestoreResult{}, err
	}
	movedCurrent := make([]string, 0, len(current))
	for _, name := range current {
		if err := os.Rename(filepath.Join(dataDir, name), filepath.Join(safetyDir, name)); err != nil {
			rollbackErr := restoreMoveSet(safetyDir, dataDir, movedCurrent)
			_ = os.Remove(safetyDir)
			return RestoreResult{}, errors.Join(fmt.Errorf("backup: preserve current %s: %w", name, err), rollbackErr)
		}
		movedCurrent = append(movedCurrent, name)
	}

	restored, err := topLevelEntries(staging)
	if err != nil {
		rollbackErr := restoreMoveSet(safetyDir, dataDir, movedCurrent)
		return RestoreResult{}, errors.Join(err, rollbackErr)
	}
	activated := make([]string, 0, len(restored))
	for _, name := range restored {
		if err := os.Rename(filepath.Join(staging, name), filepath.Join(dataDir, name)); err != nil {
			rollbackErr := errors.Join(
				restoreMoveSet(dataDir, staging, activated),
				restoreMoveSet(safetyDir, dataDir, movedCurrent),
			)
			return RestoreResult{}, errors.Join(fmt.Errorf("backup: activate restored %s: %w", name, err), rollbackErr)
		}
		activated = append(activated, name)
	}
	if err := syncDir(dataDir); err != nil {
		rollbackErr := errors.Join(
			restoreMoveSet(dataDir, staging, activated),
			restoreMoveSet(safetyDir, dataDir, movedCurrent),
			syncDir(dataDir),
		)
		return RestoreResult{}, errors.Join(fmt.Errorf("backup: sync activated restore: %w", err), rollbackErr)
	}
	return RestoreResult{DataDir: dataDir, SafetyDir: safetyDir}, nil
}

// RevertContents puts a RestoreContents safety directory back into service.
// The rejected restored state is retained under the same safety directory,
// allowing later inspection instead of deleting evidence. FTW must be stopped.
func RevertContents(dataDir, safetyDir string) (RestoreResult, error) {
	var err error
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return RestoreResult{}, err
	}
	safetyDir, err = filepath.Abs(safetyDir)
	if err != nil {
		return RestoreResult{}, err
	}
	if filepath.Dir(safetyDir) != dataDir || !strings.HasPrefix(filepath.Base(safetyDir), ".ftw-pre-restore-") {
		return RestoreResult{}, errors.New("backup: safety directory is not a direct FTW restore point")
	}
	if info, statErr := os.Stat(safetyDir); statErr != nil || !info.IsDir() {
		if statErr != nil {
			return RestoreResult{}, statErr
		}
		return RestoreResult{}, errors.New("backup: safety path is not a directory")
	}
	rejected, err := os.MkdirTemp(dataDir, ".ftw-rejected-restore-")
	if err != nil {
		return RestoreResult{}, err
	}
	keepRejected := false
	defer func() {
		if !keepRejected {
			_ = os.RemoveAll(rejected)
		}
	}()
	current, err := topLevelEntries(dataDir, safetyDir, rejected)
	if err != nil {
		return RestoreResult{}, err
	}
	movedCurrent := make([]string, 0, len(current))
	for _, name := range current {
		if err := os.Rename(filepath.Join(dataDir, name), filepath.Join(rejected, name)); err != nil {
			rollbackErr := restoreMoveSet(rejected, dataDir, movedCurrent)
			return RestoreResult{}, errors.Join(fmt.Errorf("backup: stage rejected %s: %w", name, err), rollbackErr)
		}
		movedCurrent = append(movedCurrent, name)
	}
	previous, err := topLevelEntries(safetyDir)
	if err != nil {
		rollbackErr := restoreMoveSet(rejected, dataDir, movedCurrent)
		return RestoreResult{}, errors.Join(err, rollbackErr)
	}
	restored := make([]string, 0, len(previous))
	for _, name := range previous {
		if err := os.Rename(filepath.Join(safetyDir, name), filepath.Join(dataDir, name)); err != nil {
			rollbackErr := errors.Join(
				restoreMoveSet(dataDir, safetyDir, restored),
				restoreMoveSet(rejected, dataDir, movedCurrent),
			)
			return RestoreResult{}, errors.Join(fmt.Errorf("backup: reactivate previous %s: %w", name, err), rollbackErr)
		}
		restored = append(restored, name)
	}
	if err := os.Remove(safetyDir); err != nil {
		keepRejected = true
		return RestoreResult{DataDir: dataDir, SafetyDir: rejected}, fmt.Errorf("backup: previous data is active but rejected restore remains at %s: %w", rejected, err)
	}
	if err := os.Rename(rejected, safetyDir); err != nil {
		keepRejected = true
		return RestoreResult{DataDir: dataDir, SafetyDir: rejected}, fmt.Errorf("backup: previous data is active but rejected restore remains at %s: %w", rejected, err)
	}
	keepRejected = true
	if err := syncDir(dataDir); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{DataDir: dataDir, SafetyDir: safetyDir}, nil
}

func topLevelEntries(dir string, exclude ...string) ([]string, error) {
	excluded := make(map[string]bool, len(exclude))
	for _, item := range exclude {
		abs, err := filepath.Abs(item)
		if err == nil && filepath.Dir(abs) == dir {
			excluded[filepath.Base(abs)] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !excluded[entry.Name()] && !isRestoreInternalName(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func isRestoreInternalName(name string) bool {
	return strings.HasPrefix(name, ".ftw-restore-stage-") ||
		strings.HasPrefix(name, ".ftw-pre-restore-") ||
		strings.HasPrefix(name, ".ftw-rejected-restore-")
}

func restoreMoveSet(fromDir, toDir string, names []string) error {
	var result error
	for i := len(names) - 1; i >= 0; i-- {
		if err := os.Rename(filepath.Join(fromDir, names[i]), filepath.Join(toDir, names[i])); err != nil {
			result = errors.Join(result, fmt.Errorf("backup: roll back move for %s: %w", names[i], err))
		}
	}
	return result
}

func extractArchive(archivePath, staging string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	header, err := tr.Next()
	if err != nil {
		return fmt.Errorf("backup: read manifest during extraction: %w", err)
	}
	if header.Name != manifestPath || header.Typeflag != tar.TypeReg || header.Size > 4<<20 {
		return errors.New("backup: manifest must be the first regular entry")
	}
	var manifest Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 4<<20)).Decode(&manifest); err != nil {
		return fmt.Errorf("backup: decode manifest during extraction: %w", err)
	}
	if err := validateManifest(manifest); err != nil {
		return err
	}
	want := make(map[string]FileEntry, len(manifest.Files))
	for _, entry := range manifest.Files {
		want[entry.Path] = entry
	}
	seen := make(map[string]bool, len(want))
	for {
		header, err = tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		entry, ok := want[header.Name]
		if !ok || seen[header.Name] {
			return fmt.Errorf("backup: unexpected or duplicate entry %q during extraction", header.Name)
		}
		seen[header.Name] = true
		if header.Mode != int64(entry.Mode) {
			return fmt.Errorf("backup: mode mismatch for %s", entry.Path)
		}
		if entry.Type == "symlink" {
			if header.Typeflag != tar.TypeSymlink || header.Linkname != entry.LinkTarget {
				return fmt.Errorf("backup: symlink mismatch for %s", entry.Path)
			}
			h := sha256.Sum256([]byte(header.Linkname))
			if hex.EncodeToString(h[:]) != entry.SHA256 {
				return fmt.Errorf("backup: symlink hash mismatch for %s", entry.Path)
			}
			rel := strings.TrimPrefix(entry.Path, "data/")
			dst := filepath.Join(staging, filepath.FromSlash(rel))
			if !pathInside(staging, dst) {
				return fmt.Errorf("backup: extraction path escapes staging: %s", entry.Path)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			if err := os.Symlink(entry.LinkTarget, dst); err != nil {
				return err
			}
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size != entry.Size {
			return fmt.Errorf("backup: file metadata mismatch for %s", entry.Path)
		}

		hash := sha256.New()
		if entry.Path == manifest.DatabaseEntry {
			dst := filepath.Join(staging, filepath.FromSlash(manifest.DatabaseFile))
			if !pathInside(staging, dst) {
				return fmt.Errorf("backup: database path escapes staging: %s", manifest.DatabaseFile)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			if err := gunzipTo(io.TeeReader(tr, hash), dst, fs.FileMode(entry.Mode)); err != nil {
				return err
			}
		} else {
			rel := strings.TrimPrefix(entry.Path, "data/")
			dst := filepath.Join(staging, filepath.FromSlash(rel))
			if !pathInside(staging, dst) {
				return fmt.Errorf("backup: extraction path escapes staging: %s", entry.Path)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fs.FileMode(entry.Mode))
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(io.MultiWriter(out, hash), tr)
			syncErr := out.Sync()
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if syncErr != nil {
				return syncErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		if got := hex.EncodeToString(hash.Sum(nil)); got != entry.SHA256 {
			return fmt.Errorf("backup: hash mismatch for %s during extraction", entry.Path)
		}
	}
	if len(seen) != len(want) {
		return errors.New("backup: archive is missing one or more manifest files during extraction")
	}
	return verifyDatabase(filepath.Join(staging, filepath.FromSlash(manifest.DatabaseFile)))
}

func verifyCompressedDatabase(src, workDir string) error {
	return verifyCompressedDatabaseContext(context.Background(), src, workDir)
}

func verifyCompressedDatabaseContext(ctx context.Context, src, workDir string) error {
	tmp, err := os.CreateTemp(workDir, ".ftw-backup-db-*.sqlite")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(name)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := gunzipTo(backupContextReader{ctx, in}, name, 0600); err != nil {
		return fmt.Errorf("backup: database gzip: %w", err)
	}
	return verifyDatabaseContext(ctx, name)
}

func verifyDatabase(dbPath string) error {
	return verifyDatabaseContext(context.Background(), dbPath)
}

func verifyDatabaseContext(ctx context.Context, dbPath string) error {
	db, err := sql.Open("sqlite", state.ReadOnlyDatabaseURI(dbPath))
	if err != nil {
		return err
	}
	defer db.Close()
	var result string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("backup: SQLite quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("backup: SQLite quick_check: %s", result)
	}
	return nil
}

func gunzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return gunzipTo(in, dst, 0o600)
}

func gunzipTo(in io.Reader, dst string, mode fs.FileMode) error {
	zr, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer zr.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, zr)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func hashFile(ctx context.Context, filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := copyContext(ctx, h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != n {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func safeDataPath(p string) bool {
	return strings.HasPrefix(p, "data/") && safeRelativePath(strings.TrimPrefix(p, "data/"))
}

func safeRelativePath(p string) bool {
	return p != "" && p != "." && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../")
}

func pathInside(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, candidateAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Keep cancellation effective while tar/gzip verification reads large files.
type backupContextReader struct {
	ctx context.Context
	io.Reader
}

func (r backupContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
