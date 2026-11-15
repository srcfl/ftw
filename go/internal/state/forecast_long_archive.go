package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"modernc.org/sqlite"
)

const (
	// ForecastLongArchiveFilename is the long-term forecast archive beside state.db.
	ForecastLongArchiveFilename = "forecast-archive.db"

	// ForecastLongFullDetail keeps every issued run. Older runs are thinned
	// to one per UTC hour. It matches the 30-day issue archive in state.db.
	ForecastLongFullDetail = ForecastIssueRetention
	// ForecastLongRetention is two 365-day years. It is a fixed storage policy,
	// not a setting.
	ForecastLongRetention = 2 * 365 * 24 * time.Hour
	// ForecastLongMaxBytes is the live-page cap. Past it, the oldest days
	// outside the full-detail window are deleted. The figure is a margin
	// around a size estimate, not a measurement from a box.
	ForecastLongMaxBytes = 512 << 20
	// ForecastLongMinFreeBytes is the free-space floor on the data volume.
	// Below it the copier pauses instead of filling the card.
	ForecastLongMinFreeBytes = 1 << 30

	forecastLongVacuumPages  = 256
	forecastLongVacuumBudget = 30 * time.Second
	forecastLongFreeTarget   = 1 << 20
	forecastLongPageTimeout  = 6 * time.Second
	forecastLongWarnGap      = time.Hour
)

// ForecastLongArchiveStatus is the cached view published for /api/health.
// Readers never scan the archive.
type ForecastLongArchiveStatus struct {
	State             string  `json:"state"`
	LiveBytes         int64   `json:"live_bytes"`
	FileBytes         int64   `json:"file_bytes"`
	WALBytes          int64   `json:"wal_bytes"`
	CapBytes          int64   `json:"cap_bytes"`
	OldestOriginMS    int64   `json:"oldest_origin_ms"`
	NewestOriginMS    int64   `json:"newest_origin_ms"`
	Runs              int64   `json:"runs"`
	Coverage7d        float64 `json:"coverage_7d"`
	Coverage30d       float64 `json:"coverage_30d"`
	CoverageAll       float64 `json:"coverage_all"`
	TruthCoverage30d  float64 `json:"truth_coverage_30d"`
	CopyLagMS         int64   `json:"copy_lag_ms"`
	LastCopyMS        int64   `json:"last_copy_ms"`
	LastError         string  `json:"last_error"`
	IssueBytesP50     int64   `json:"issue_bytes_p50"`
	IssueBytesP95     int64   `json:"issue_bytes_p95"`
	IssueGzipBytesP50 int64   `json:"issue_gzip_bytes_p50"`
}

// ForecastArchiveIssue is one issued forecast plus the gzip model states
// already stored in state.db. Passing those bytes avoids compressing them again.
type ForecastArchiveIssue struct {
	Issue     forecasting.Issue
	JSONBytes int
	GzipBytes int
	States    map[string]ForecastModelBlob
}

// ForecastModelBlob is the gzip payload stored for one model-state id.
type ForecastModelBlob struct {
	Expanded int
	Gzip     []byte
}

// ForecastLongArchive is the long-term forecast file. It is opened by the
// background copier, not during boot.
type ForecastLongArchive struct {
	db    *sql.DB
	path  string
	store *Store

	mu      sync.Mutex
	status  ForecastLongArchiveStatus
	rebuilt bool
	warned  map[string]time.Time

	maxBytes      int64
	retention     time.Duration
	fullDetail    time.Duration
	minFree       int64
	maintainEvery time.Duration
}

var forecastLongDiskAvail = diskAvail

// forecastLongBeforeWrite is a test hook. A SQLITE_FULL error pauses copying.
var forecastLongBeforeWrite func() error

// ForecastLongDiskAvailForTest replaces the free-space probe. The returned
// function restores it.
func ForecastLongDiskAvailForTest(fn func(string) (int64, error)) func() {
	prev := forecastLongDiskAvail
	if fn == nil {
		forecastLongDiskAvail = diskAvail
	} else {
		forecastLongDiskAvail = fn
	}
	return func() { forecastLongDiskAvail = prev }
}

// ForecastLongArchivePath is forecast-archive.db beside a state.db path.
func ForecastLongArchivePath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), ForecastLongArchiveFilename)
}

// ReportForecastLongArchive publishes the informational health block. It does
// not change control, dispatch or the top-level box status.
func (s *Store) ReportForecastLongArchive(st ForecastLongArchiveStatus) {
	if s == nil {
		return
	}
	s.forecastLongMu.Lock()
	s.forecastLongStatus = &st
	s.forecastLongMu.Unlock()
}

// ForecastLongArchiveStatus returns the last published block. The second
// result is false until the copier has published one, so health can omit it.
func (s *Store) ForecastLongArchiveStatus() (ForecastLongArchiveStatus, bool) {
	if s == nil {
		return ForecastLongArchiveStatus{}, false
	}
	s.forecastLongMu.Lock()
	defer s.forecastLongMu.Unlock()
	if s.forecastLongStatus == nil {
		return ForecastLongArchiveStatus{}, false
	}
	return *s.forecastLongStatus, true
}

// OpenForecastLongArchive opens or creates the archive after startup. A
// damaged file is moved aside and replaced with an empty one. Boot does not
// call this.
func (s *Store) OpenForecastLongArchive(ctx context.Context) (*ForecastLongArchive, error) {
	if s == nil || s.mainDBPath == "" {
		return nil, errors.New("forecast archive: state store unavailable")
	}
	s.forecastLongMu.Lock()
	defer s.forecastLongMu.Unlock()
	if s.forecastLong != nil {
		return s.forecastLong, nil
	}
	path := ForecastLongArchivePath(s.mainDBPath)
	arch, ev, err := openForecastLongFile(ctx, path)
	if err != nil {
		return nil, err
	}
	arch.store = s
	if ev != nil {
		s.RecordHealEvent(*ev)
		arch.rebuilt = true
		arch.status.State = "rebuilt"
	}
	s.forecastLong = arch
	return arch, nil
}

func (s *Store) closeForecastLongArchive() error {
	s.forecastLongMu.Lock()
	arch := s.forecastLong
	s.forecastLong = nil
	s.forecastLongMu.Unlock()
	if arch == nil {
		return nil
	}
	return arch.Close()
}

func openForecastLongFile(ctx context.Context, path string) (*ForecastLongArchive, *HealEvent, error) {
	db, damaged, err := openForecastLongDB(ctx, path)
	if err != nil && !damaged {
		return nil, nil, err
	}
	var ev *HealEvent
	if damaged {
		if db != nil {
			_ = db.Close()
		}
		nowMs := time.Now().UnixMilli()
		if err := quarantine(path, nowMs); err != nil {
			return nil, nil, err
		}
		db, _, err = openForecastLongDB(ctx, path)
		if err != nil {
			return nil, nil, err
		}
		ev = &HealEvent{
			Tier: "forecast_archive", Action: healRebuilt, AtMs: nowMs,
			Detail: "forecast-archive.db was damaged — moved aside and started an empty forecast archive",
		}
	}
	if err := initForecastLongSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	arch := &ForecastLongArchive{
		db: db, path: path,
		status:        ForecastLongArchiveStatus{State: "starting", CapBytes: ForecastLongMaxBytes},
		warned:        map[string]time.Time{},
		maxBytes:      ForecastLongMaxBytes,
		retention:     ForecastLongRetention,
		fullDetail:    ForecastLongFullDetail,
		minFree:       ForecastLongMinFreeBytes,
		maintainEvery: 24 * time.Hour,
	}
	if ev != nil {
		arch.rebuilt = true
		arch.status.State = "rebuilt"
	}
	return arch, ev, nil
}

func openForecastLongDB(ctx context.Context, path string) (*sql.DB, bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		db, err := openRawPragmas(path, forecastLongPragmas)
		if err != nil {
			return nil, false, err
		}
		if err := prepareForecastLongFile(ctx, db); err != nil {
			_ = db.Close()
			return nil, false, err
		}
		return db, false, nil
	}
	db, err := openRawPragmas(path, forecastLongPragmas)
	if err != nil {
		if sqliteCorrupt(err) {
			return nil, true, err
		}
		return nil, false, err
	}
	ok, qerr := quickCheckContext(ctx, db)
	if qerr == nil && ok {
		if err := prepareForecastLongFile(ctx, db); err != nil {
			_ = db.Close()
			return nil, false, err
		}
		return db, false, nil
	}
	if ctx.Err() != nil {
		_ = db.Close()
		return nil, false, ctx.Err()
	}
	if sqliteBusy(qerr) {
		_ = db.Close()
		return nil, false, qerr
	}
	return db, true, qerr
}

const forecastLongPragmas = sqlitePragmas + "&_pragma=journal_size_limit(4194304)&_pragma=auto_vacuum(INCREMENTAL)"

func prepareForecastLongFile(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `PRAGMA journal_size_limit=4194304`); err != nil {
		return err
	}
	var mode, tables int
	if err := db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		// A connection-string pragma can report incremental vacuum before the
		// file has a pointer map. Vacuuming the empty file makes later
		// incremental_vacuum calls actually return pages.
		if mode != 2 {
			if _, err := db.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
				return err
			}
		}
		if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
			return err
		}
	}
	return nil
}

func initForecastLongSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS fa_migrations(name TEXT PRIMARY KEY, completed_at TEXT DEFAULT current_timestamp) STRICT;
CREATE TABLE IF NOT EXISTS fa_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
CREATE TABLE IF NOT EXISTS fa_dict(code INTEGER PRIMARY KEY, kind TEXT NOT NULL, value TEXT NOT NULL, UNIQUE(kind, value)) STRICT;
CREATE TABLE IF NOT EXISTS fa_runs(
  run_id TEXT PRIMARY KEY,
  decision_id TEXT NOT NULL,
  origin_ms INTEGER NOT NULL, issued_at_ms INTEGER NOT NULL, latest_input_ms INTEGER NOT NULL,
  config_version TEXT NOT NULL,
  site_id TEXT, learning_revision TEXT, timezone TEXT,
  has_location INTEGER NOT NULL DEFAULT 0, latitude REAL, longitude REAL,
  models TEXT NOT NULL,
  occupancy BLOB, occupancy_start_ms INTEGER, occupancy_n INTEGER,
  weather_avail TEXT,
  input_hash TEXT NOT NULL,
  thinned INTEGER NOT NULL DEFAULT 0,
  json_bytes INTEGER NOT NULL DEFAULT 0,
  gzip_bytes INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX IF NOT EXISTS fa_runs_origin ON fa_runs(origin_ms);
CREATE INDEX IF NOT EXISTS fa_runs_issued ON fa_runs(issued_at_ms);
CREATE TABLE IF NOT EXISTS fa_series(
  run_id TEXT NOT NULL, series TEXT NOT NULL, model_version TEXT NOT NULL,
  start_ms INTEGER NOT NULL, slot_ms INTEGER NOT NULL, n INTEGER NOT NULL,
  starts BLOB, prediction_start_ms INTEGER,
  pv_w BLOB NOT NULL, load_w BLOB NOT NULL,
  flags BLOB NOT NULL,
  pv_quality BLOB NOT NULL, load_quality BLOB NOT NULL,
  pv_source BLOB, load_source BLOB,
  pv_lo BLOB, pv_hi BLOB, load_lo BLOB, load_hi BLOB, net_lo BLOB, net_hi BLOB,
  band_method BLOB, band_stats BLOB,
  PRIMARY KEY(run_id, series)
) STRICT, WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS fa_weather(
  source TEXT NOT NULL, available_at_ms INTEGER NOT NULL, start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL,
  ghi REAL, cloud REAL, temp REAL, direct_pv REAL, est_pv REAL,
  PRIMARY KEY(source, available_at_ms, start_ms)
) STRICT, WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS fa_run_weather(
  run_id TEXT NOT NULL, available_at_ms INTEGER NOT NULL,
  PRIMARY KEY(run_id, available_at_ms)
) STRICT, WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS fa_observations_15m(
  start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, config_version TEXT NOT NULL,
  available_at_ms INTEGER NOT NULL, pv_w REAL NOT NULL, load_w REAL NOT NULL,
  pv_known INTEGER NOT NULL, load_known INTEGER NOT NULL, quality TEXT NOT NULL,
  PRIMARY KEY(start_ms, end_ms, config_version)
) STRICT, WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS fa_model_states(
  id TEXT PRIMARY KEY, created_at_ms INTEGER NOT NULL, expanded_bytes INTEGER NOT NULL, payload BLOB NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS fa_run_states(
  run_id TEXT NOT NULL, state_id TEXT NOT NULL,
  PRIMARY KEY(run_id, state_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS fa_run_states_ref ON fa_run_states(state_id);
INSERT OR IGNORE INTO fa_migrations(name) VALUES('fa-v1');
`)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT OR IGNORE INTO fa_meta(key, value) VALUES('created_at_ms', ?)`, fmt.Sprint(time.Now().UnixMilli()))
	return err
}

func sqliteBusy(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && (e.Code()&0xff == 5 || e.Code()&0xff == 6)
}

func sqliteFull(err error) bool {
	var code interface{ Code() int }
	return errors.As(err, &code) && code.Code()&0xff == 13
}

// Close releases the archive file. Store.Close does this before cache.db.
func (a *ForecastLongArchive) Close() error {
	if a == nil || a.db == nil {
		return nil
	}
	return a.db.Close()
}

func (a *ForecastLongArchive) warn(kind string, err error) {
	if err == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status.LastError = err.Error()
	now := time.Now()
	if a.warned == nil {
		a.warned = map[string]time.Time{}
	}
	if last, ok := a.warned[kind]; ok && now.Sub(last) < forecastLongWarnGap {
		return
	}
	a.warned[kind] = now
	slog.Warn("forecast long archive", "kind", kind, "err", err)
}

func (a *ForecastLongArchive) publish() {
	a.mu.Lock()
	st := a.status
	a.mu.Unlock()
	if a.store != nil {
		a.store.ReportForecastLongArchive(st)
	}
}

func (a *ForecastLongArchive) setState(state, lastError string) {
	a.mu.Lock()
	a.status.State = state
	if lastError != "" {
		a.status.LastError = lastError
	}
	if state == "ok" || state == "rebuilt" {
		a.status.LastError = lastError
	}
	a.mu.Unlock()
	a.publish()
}

// ArchiveIssues stores one page of issued forecasts. An identical run is left
// untouched. A different payload with the same id is rejected and the stored
// run is kept. Baselines are not stored; they can be rebuilt from observations
// and weather. Quarter-hour truth copied from state.db has load and PV only.
func (a *ForecastLongArchive) ArchiveIssues(ctx context.Context, batch []ForecastArchiveIssue) error {
	if len(batch) == 0 {
		return nil
	}
	if err := a.beforeWrite(); err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	codes := map[string]byte{}
	var immutable error
	for _, item := range batch {
		if err := a.archiveOne(ctx, tx, codes, item); err != nil {
			if errors.Is(err, errForecastArchiveImmutable) {
				a.warn("immutable", err)
				immutable = err
				continue
			}
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return immutable
}

func (a *ForecastLongArchive) beforeWrite() error {
	if forecastLongBeforeWrite != nil {
		if err := forecastLongBeforeWrite(); err != nil {
			return err
		}
	}
	avail, err := forecastLongDiskAvail(filepath.Dir(a.path))
	if err != nil {
		return nil
	}
	if avail < a.minFree {
		return errForecastArchiveDiskFull
	}
	return nil
}

var errForecastArchiveDiskFull = errors.New("forecast archive paused: data volume is below the free-space floor")
var errForecastArchiveImmutable = errors.New("forecast long archive run is immutable")

func (a *ForecastLongArchive) archiveOne(ctx context.Context, tx *sql.Tx, codes map[string]byte, item ForecastArchiveIssue) error {
	issue := item.Issue
	if err := issue.Validate(); err != nil {
		return err
	}
	kept := make([]forecasting.Series, 0, 4)
	for _, series := range issue.Series {
		if forecastLongKeepSeries(series.Name) {
			kept = append(kept, series)
		}
	}
	if len(kept) == 0 {
		a.warn("skip-series", errors.New("issue "+issue.ID+" has none of the stored series"))
		return nil
	}
	states, models, err := forecastLongModelBlobs(issue, item.States)
	if err != nil {
		return err
	}
	issue.Models = models
	hash, err := forecastLongInputHash(issue)
	if err != nil {
		return err
	}
	encoded, err := encodeKeptSeries(kept, func(kind, value string) (byte, error) {
		return forecastLongDict(ctx, tx, codes, kind, value)
	})
	if err != nil {
		return err
	}
	var decision, config, existingHash string
	var origin, issued, latest int64
	err = tx.QueryRowContext(ctx, `SELECT decision_id, origin_ms, issued_at_ms, latest_input_ms, config_version, input_hash
 FROM fa_runs WHERE run_id=?`, issue.ID).Scan(&decision, &origin, &issued, &latest, &config, &existingHash)
	if err == nil {
		same, cmpErr := forecastLongSameSeries(ctx, tx, issue.ID, encoded)
		if cmpErr != nil {
			return cmpErr
		}
		if existingHash != hash || decision != issue.DecisionID || origin != issue.OriginMS || issued != issue.IssuedAtMS ||
			latest != issue.LatestInputMS || config != issue.ConfigVersion || !same {
			return fmt.Errorf("%w: %s", errForecastArchiveImmutable, issue.ID)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for id, blob := range states {
		if len(blob.Gzip) == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fa_model_states(id, created_at_ms, expanded_bytes, payload) VALUES(?,?,?,?)`,
			id, issue.IssuedAtMS, blob.Expanded, blob.Gzip); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fa_run_states(run_id, state_id) VALUES(?,?)`, issue.ID, id); err != nil {
			return err
		}
	}
	if err := insertForecastWeather(ctx, tx, issue); err != nil {
		if !strings.Contains(err.Error(), "is immutable") {
			return err
		}
		a.warn("weather", err)
	}
	occ, occStart, occN, err := encodeOccupancy(issue.Occupancy)
	if err != nil {
		return err
	}
	avail := forecastWeatherAvail(issue.Weather)
	modelsJSON, err := json.Marshal(issue.Models)
	if err != nil {
		return err
	}
	var siteID, learning, tz sql.NullString
	var lat, lon sql.NullFloat64
	hasLoc := 0
	if issue.Site != nil {
		siteID = sql.NullString{String: issue.Site.SiteID, Valid: true}
		learning = sql.NullString{String: issue.Site.LearningRevision, Valid: true}
		tz = sql.NullString{String: issue.Site.Timezone, Valid: true}
		lat = sql.NullFloat64{Float64: issue.Site.Latitude, Valid: true}
		lon = sql.NullFloat64{Float64: issue.Site.Longitude, Valid: true}
		if issue.Site.HasLocation {
			hasLoc = 1
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO fa_runs(
 run_id, decision_id, origin_ms, issued_at_ms, latest_input_ms, config_version,
 site_id, learning_revision, timezone, has_location, latitude, longitude,
 models, occupancy, occupancy_start_ms, occupancy_n, weather_avail, input_hash,
 thinned, json_bytes, gzip_bytes) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?)`,
		issue.ID, issue.DecisionID, issue.OriginMS, issue.IssuedAtMS, issue.LatestInputMS, issue.ConfigVersion,
		siteID, learning, tz, hasLoc, lat, lon, string(modelsJSON), nullableBlob(occ), nullMS(occStart, occN > 0), nullCount(occN),
		nullText(avail), hash, item.JSONBytes, item.GzipBytes)
	if err != nil {
		return err
	}
	for _, series := range encoded {
		if err := insertEncodedSeries(ctx, tx, issue.ID, series); err != nil {
			return err
		}
	}
	return nil
}

func nullMS(v int64, ok bool) any {
	if !ok {
		return nil
	}
	return v
}

func nullCount(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableBlob(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func forecastLongModelBlobs(issue forecasting.Issue, provided map[string]ForecastModelBlob) (map[string]ForecastModelBlob, []forecasting.ModelState, error) {
	out := map[string]ForecastModelBlob{}
	for id, blob := range provided {
		out[id] = blob
	}
	models := append([]forecasting.ModelState(nil), issue.Models...)
	for i := range models {
		model := &models[i]
		if len(model.State) == 0 {
			continue
		}
		sum := sha256.Sum256(model.State)
		id := hex.EncodeToString(sum[:])
		if model.StateID != "" && model.StateID != id {
			return nil, nil, errors.New("forecast model state hash mismatch")
		}
		if _, ok := out[id]; !ok {
			gz, err := gzipForecastModelState(model.State)
			if err != nil {
				return nil, nil, err
			}
			out[id] = ForecastModelBlob{Expanded: len(model.State), Gzip: gz}
		}
		model.StateID = id
		model.State = nil
	}
	return out, models, nil
}

type encodedSeries struct {
	name, version  string
	layout         seriesLayout
	pv, load       []byte
	flags          []byte
	pvQuality      []byte
	loadQuality    []byte
	pvSource       []byte
	loadSource     []byte
	pvLo, pvHi     []byte
	loadLo, loadHi []byte
	netLo, netHi   []byte
	bandMethod     []byte
	bandStats      []byte
}

func encodeKeptSeries(series []forecasting.Series, code func(kind, value string) (byte, error)) ([]encodedSeries, error) {
	out := make([]encodedSeries, 0, len(series))
	for _, s := range series {
		layout, err := layoutSeries(s.Points)
		if err != nil {
			return nil, err
		}
		enc := encodedSeries{name: s.Name, version: s.ModelVersion, layout: layout}
		pv := make([]float64, len(s.Points))
		load := make([]float64, len(s.Points))
		flags := make([]byte, len(s.Points))
		pvQ := make([]byte, len(s.Points))
		loadQ := make([]byte, len(s.Points))
		pvS := make([]byte, len(s.Points))
		loadS := make([]byte, len(s.Points))
		var pvLo, pvHi, loadLo, loadHi, netLo, netHi []float64
		methods := make([]byte, len(s.Points))
		sourceUsed := false
		if s.Name == forecastLongSeriesChampion {
			pvLo = make([]float64, len(s.Points))
			pvHi = make([]float64, len(s.Points))
			loadLo = make([]float64, len(s.Points))
			loadHi = make([]float64, len(s.Points))
			netLo = make([]float64, len(s.Points))
			netHi = make([]float64, len(s.Points))
		}
		for i, p := range s.Points {
			pv[i], load[i] = p.PVW, p.LoadW
			if p.PVKnown {
				flags[i] |= 1
			}
			if p.LoadKnown {
				flags[i] |= 2
			}
			if pvQ[i], err = code("quality", p.PVQuality); err != nil {
				return nil, err
			}
			if loadQ[i], err = code("quality", p.LoadQuality); err != nil {
				return nil, err
			}
			if p.PVSource != "" {
				sourceUsed = true
				if pvS[i], err = code("source", p.PVSource); err != nil {
					return nil, err
				}
			}
			if p.LoadSource != "" {
				sourceUsed = true
				if loadS[i], err = code("source", p.LoadSource); err != nil {
					return nil, err
				}
			}
			if s.Name == forecastLongSeriesChampion {
				pvLo[i], pvHi[i] = p.PVBand.LowW, p.PVBand.HighW
				loadLo[i], loadHi[i] = p.LoadBand.LowW, p.LoadBand.HighW
				netLo[i], netHi[i] = p.NetBand.LowW, p.NetBand.HighW
				methods[i] = packBandMethod(p)
			}
		}
		if enc.pv, err = encodeF32(pv); err != nil {
			return nil, err
		}
		if enc.load, err = encodeF32(load); err != nil {
			return nil, err
		}
		if enc.flags, err = encodeU8(flags); err != nil {
			return nil, err
		}
		if enc.pvQuality, err = encodeU8(pvQ); err != nil {
			return nil, err
		}
		if enc.loadQuality, err = encodeU8(loadQ); err != nil {
			return nil, err
		}
		if sourceUsed {
			if enc.pvSource, err = encodeU8(pvS); err != nil {
				return nil, err
			}
			if enc.loadSource, err = encodeU8(loadS); err != nil {
				return nil, err
			}
		}
		if s.Name == forecastLongSeriesChampion {
			for _, pair := range []struct {
				vals []float64
				dst  *[]byte
			}{
				{pvLo, &enc.pvLo}, {pvHi, &enc.pvHi}, {loadLo, &enc.loadLo}, {loadHi, &enc.loadHi},
				{netLo, &enc.netLo}, {netHi, &enc.netHi},
			} {
				b, err := encodeF32(pair.vals)
				if err != nil {
					return nil, err
				}
				*pair.dst = b
			}
			if enc.bandMethod, err = encodeU8(methods); err != nil {
				return nil, err
			}
			if enc.bandStats, err = encodeBandStats(s.Points); err != nil {
				return nil, err
			}
		}
		out = append(out, enc)
	}
	return out, nil
}

func forecastLongDict(ctx context.Context, tx *sql.Tx, cache map[string]byte, kind, value string) (byte, error) {
	if value == "" {
		return 0, nil
	}
	key := kind + "\x00" + value
	if code, ok := cache[key]; ok {
		return code, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fa_dict(kind, value) VALUES(?, ?) ON CONFLICT(kind, value) DO NOTHING`, kind, value); err != nil {
		return 0, err
	}
	var code int
	if err := tx.QueryRowContext(ctx, `SELECT code FROM fa_dict WHERE kind=? AND value=?`, kind, value).Scan(&code); err != nil {
		return 0, err
	}
	if code < 1 || code > 255 {
		return 0, fmt.Errorf("forecast archive dictionary is full for %s", kind)
	}
	cache[key] = byte(code)
	return byte(code), nil
}

func insertEncodedSeries(ctx context.Context, tx *sql.Tx, runID string, s encodedSeries) error {
	var pred any
	if s.layout.predictionStartMS != 0 {
		pred = s.layout.predictionStartMS
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO fa_series(
 run_id, series, model_version, start_ms, slot_ms, n, starts, prediction_start_ms,
 pv_w, load_w, flags, pv_quality, load_quality, pv_source, load_source,
 pv_lo, pv_hi, load_lo, load_hi, net_lo, net_hi, band_method, band_stats)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		runID, s.name, s.version, s.layout.startMS, s.layout.slotMS, s.layout.n, nullableBlob(s.layout.starts), pred,
		s.pv, s.load, s.flags, s.pvQuality, s.loadQuality, nullableBlob(s.pvSource), nullableBlob(s.loadSource),
		nullableBlob(s.pvLo), nullableBlob(s.pvHi), nullableBlob(s.loadLo), nullableBlob(s.loadHi),
		nullableBlob(s.netLo), nullableBlob(s.netHi), nullableBlob(s.bandMethod), nullableBlob(s.bandStats))
	return err
}

func forecastLongSameSeries(ctx context.Context, tx *sql.Tx, runID string, want []encodedSeries) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT series, model_version, start_ms, slot_ms, n, starts, prediction_start_ms,
 pv_w, load_w, flags, pv_quality, load_quality, pv_source, load_source,
 pv_lo, pv_hi, load_lo, load_hi, net_lo, net_hi, band_method, band_stats
 FROM fa_series WHERE run_id=?`, runID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	got := map[string]encodedSeries{}
	for rows.Next() {
		var s encodedSeries
		var starts, pvSource, loadSource, pvLo, pvHi, loadLo, loadHi, netLo, netHi, method, stats []byte
		var pred sql.NullInt64
		if err := rows.Scan(&s.name, &s.version, &s.layout.startMS, &s.layout.slotMS, &s.layout.n, &starts, &pred,
			&s.pv, &s.load, &s.flags, &s.pvQuality, &s.loadQuality, &pvSource, &loadSource,
			&pvLo, &pvHi, &loadLo, &loadHi, &netLo, &netHi, &method, &stats); err != nil {
			return false, err
		}
		s.layout.starts = starts
		if pred.Valid {
			s.layout.predictionStartMS = pred.Int64
		}
		s.pvSource, s.loadSource = pvSource, loadSource
		s.pvLo, s.pvHi, s.loadLo, s.loadHi, s.netLo, s.netHi = pvLo, pvHi, loadLo, loadHi, netLo, netHi
		s.bandMethod, s.bandStats = method, stats
		got[s.name] = s
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(got) != len(want) {
		return false, nil
	}
	for _, s := range want {
		if !encodedSeriesEqual(got[s.name], s) {
			return false, nil
		}
	}
	return true, nil
}

func encodedSeriesEqual(a, b encodedSeries) bool {
	if a.name != b.name || a.version != b.version || a.layout.startMS != b.layout.startMS || a.layout.slotMS != b.layout.slotMS ||
		a.layout.n != b.layout.n || a.layout.predictionStartMS != b.layout.predictionStartMS {
		return false
	}
	pairs := [][2][]byte{
		{a.layout.starts, b.layout.starts}, {a.pv, b.pv}, {a.load, b.load}, {a.flags, b.flags},
		{a.pvQuality, b.pvQuality}, {a.loadQuality, b.loadQuality}, {a.pvSource, b.pvSource}, {a.loadSource, b.loadSource},
		{a.pvLo, b.pvLo}, {a.pvHi, b.pvHi}, {a.loadLo, b.loadLo}, {a.loadHi, b.loadHi},
		{a.netLo, b.netLo}, {a.netHi, b.netHi}, {a.bandMethod, b.bandMethod}, {a.bandStats, b.bandStats},
	}
	for _, pair := range pairs {
		if !bytesEqualNil(pair[0], pair[1]) {
			return false
		}
	}
	return true
}

func bytesEqualNil(a, b []byte) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func forecastWeatherAvail(rows []forecasting.Weather) string {
	if len(rows) == 0 {
		return ""
	}
	seen := map[int64]bool{}
	var ids []int64
	for _, row := range rows {
		if seen[row.AvailableAtMS] {
			continue
		}
		seen[row.AvailableAtMS] = true
		ids = append(ids, row.AvailableAtMS)
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return ""
	}
	return string(data)
}

func insertForecastWeather(ctx context.Context, tx *sql.Tx, issue forecasting.Issue) error {
	seen := map[int64]bool{}
	for _, row := range issue.Weather {
		if !seen[row.AvailableAtMS] {
			seen[row.AvailableAtMS] = true
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fa_run_weather(run_id, available_at_ms) VALUES(?,?)`, issue.ID, row.AvailableAtMS); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fa_weather(
 source, available_at_ms, start_ms, end_ms, ghi, cloud, temp, direct_pv, est_pv) VALUES(?,?,?,?,?,?,?,?,?)`,
			row.Source, row.AvailableAtMS, row.StartMS, row.EndMS,
			nullFloat(row.GHIWm2), nullFloat(row.CloudPct), nullFloat(row.TempC), nullFloat(row.DirectPVW), nullFloat(row.EstimatedPVW))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			continue
		}
		var end int64
		var ghi, cloud, temp, direct, est sql.NullFloat64
		if err := tx.QueryRowContext(ctx, `SELECT end_ms, ghi, cloud, temp, direct_pv, est_pv FROM fa_weather
 WHERE source=? AND available_at_ms=? AND start_ms=?`, row.Source, row.AvailableAtMS, row.StartMS).Scan(&end, &ghi, &cloud, &temp, &direct, &est); err != nil {
			return err
		}
		if end != row.EndMS || !nullFloatEqual(ghi, row.GHIWm2) || !nullFloatEqual(cloud, row.CloudPct) || !nullFloatEqual(temp, row.TempC) ||
			!nullFloatEqual(direct, row.DirectPVW) || !nullFloatEqual(est, row.EstimatedPVW) {
			return fmt.Errorf("forecast archive weather at %d is immutable", row.AvailableAtMS)
		}
	}
	return nil
}

func nullFloat(v *float64) any {
	if v == nil || math.IsNaN(*v) {
		return nil
	}
	return *v
}

func nullFloatEqual(got sql.NullFloat64, want *float64) bool {
	if want == nil || math.IsNaN(*want) {
		return !got.Valid
	}
	return got.Valid && got.Float64 == *want
}

// LoadIssue rebuilds one stored run. Champion bands are restored. Other series
// get an explicit cold-start band, because only the champion interval was kept.
func (a *ForecastLongArchive) LoadIssue(ctx context.Context, runID string) (forecasting.Issue, error) {
	var issue forecasting.Issue
	var siteID, learning, tz, modelsJSON, weatherAvail sql.NullString
	var lat, lon sql.NullFloat64
	var hasLoc int
	var occ []byte
	var occStart sql.NullInt64
	var occN sql.NullInt64
	err := a.db.QueryRowContext(ctx, `SELECT decision_id, origin_ms, issued_at_ms, latest_input_ms, config_version,
 site_id, learning_revision, timezone, has_location, latitude, longitude, models,
 occupancy, occupancy_start_ms, occupancy_n, weather_avail
 FROM fa_runs WHERE run_id=?`, runID).Scan(
		&issue.DecisionID, &issue.OriginMS, &issue.IssuedAtMS, &issue.LatestInputMS, &issue.ConfigVersion,
		&siteID, &learning, &tz, &hasLoc, &lat, &lon, &modelsJSON, &occ, &occStart, &occN, &weatherAvail)
	if err != nil {
		return forecasting.Issue{}, err
	}
	issue.Schema = forecasting.Schema
	issue.ID = runID
	if siteID.Valid && siteID.String != "" {
		issue.Site = &forecasting.SiteContext{
			SiteID: siteID.String, LearningRevision: learning.String, Timezone: tz.String,
			HasLocation: hasLoc != 0, Latitude: lat.Float64, Longitude: lon.Float64,
		}
	}
	if modelsJSON.Valid && modelsJSON.String != "" {
		if err := json.Unmarshal([]byte(modelsJSON.String), &issue.Models); err != nil {
			return forecasting.Issue{}, err
		}
	}
	nOcc := 0
	if occN.Valid {
		nOcc = int(occN.Int64)
	}
	startOcc := int64(0)
	if occStart.Valid {
		startOcc = occStart.Int64
	}
	if issue.Occupancy, err = decodeOccupancy(occ, startOcc, nOcc); err != nil {
		return forecasting.Issue{}, err
	}
	dict, err := a.loadDict(ctx)
	if err != nil {
		return forecasting.Issue{}, err
	}
	issue.Series, err = a.loadSeries(ctx, runID, dict)
	if err != nil {
		return forecasting.Issue{}, err
	}
	var spanStart, spanEnd int64
	for _, series := range issue.Series {
		for _, p := range series.Points {
			if spanStart == 0 || p.StartMS < spanStart {
				spanStart = p.StartMS
			}
			if p.EndMS > spanEnd {
				spanEnd = p.EndMS
			}
		}
	}
	if issue.Weather, err = a.loadWeather(ctx, weatherAvail.String, spanStart, spanEnd); err != nil {
		return forecasting.Issue{}, err
	}
	if err := issue.Validate(); err != nil {
		return forecasting.Issue{}, err
	}
	return issue, nil
}

func (a *ForecastLongArchive) loadDict(ctx context.Context) (map[byte]string, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT code, value FROM fa_dict`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[byte]string{}
	for rows.Next() {
		var code int
		var value string
		if err := rows.Scan(&code, &value); err != nil {
			return nil, err
		}
		if code >= 0 && code <= 255 {
			out[byte(code)] = value
		}
	}
	return out, rows.Err()
}

func (a *ForecastLongArchive) loadSeries(ctx context.Context, runID string, dict map[byte]string) ([]forecasting.Series, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT series, model_version, start_ms, slot_ms, n, starts, prediction_start_ms,
 pv_w, load_w, flags, pv_quality, load_quality, pv_source, load_source,
 pv_lo, pv_hi, load_lo, load_hi, net_lo, net_hi, band_method, band_stats
 FROM fa_series WHERE run_id=?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []forecasting.Series
	for rows.Next() {
		var name, version string
		var layout seriesLayout
		var starts, pv, load, flags, pvQ, loadQ, pvS, loadS []byte
		var pvLo, pvHi, loadLo, loadHi, netLo, netHi, method, stats []byte
		var pred sql.NullInt64
		if err := rows.Scan(&name, &version, &layout.startMS, &layout.slotMS, &layout.n, &starts, &pred,
			&pv, &load, &flags, &pvQ, &loadQ, &pvS, &loadS, &pvLo, &pvHi, &loadLo, &loadHi, &netLo, &netHi, &method, &stats); err != nil {
			return nil, err
		}
		layout.starts = starts
		if pred.Valid {
			layout.predictionStartMS = pred.Int64
		}
		series, err := decodeStoredSeries(name, version, layout, pv, load, flags, pvQ, loadQ, pvS, loadS, pvLo, pvHi, loadLo, loadHi, netLo, netHi, method, stats, dict)
		if err != nil {
			return nil, err
		}
		out = append(out, series)
	}
	return out, rows.Err()
}

func decodeStoredSeries(name, version string, layout seriesLayout, pvB, loadB, flags, pvQ, loadQ, pvS, loadS, pvLo, pvHi, loadLo, loadHi, netLo, netHi, method, stats []byte, dict map[byte]string) (forecasting.Series, error) {
	starts, ends, err := expandLayout(layout)
	if err != nil {
		return forecasting.Series{}, err
	}
	pv, err := decodeF32(pvB, layout.n)
	if err != nil {
		return forecasting.Series{}, err
	}
	load, err := decodeF32(loadB, layout.n)
	if err != nil {
		return forecasting.Series{}, err
	}
	flagB, err := decodeU8(flags, layout.n)
	if err != nil {
		return forecasting.Series{}, err
	}
	pvQB, err := decodeU8(pvQ, layout.n)
	if err != nil {
		return forecasting.Series{}, err
	}
	loadQB, err := decodeU8(loadQ, layout.n)
	if err != nil {
		return forecasting.Series{}, err
	}
	var pvSB, loadSB, methodB []byte
	if len(pvS) > 0 {
		if pvSB, err = decodeU8(pvS, layout.n); err != nil {
			return forecasting.Series{}, err
		}
	}
	if len(loadS) > 0 {
		if loadSB, err = decodeU8(loadS, layout.n); err != nil {
			return forecasting.Series{}, err
		}
	}
	var loPV, hiPV, loLoad, hiLoad, loNet, hiNet []float64
	var samples, days [][3]int
	champion := name == forecastLongSeriesChampion
	if champion {
		if methodB, err = decodeU8(method, layout.n); err != nil {
			return forecasting.Series{}, err
		}
		for _, pair := range []struct {
			b   []byte
			dst *[]float64
		}{{pvLo, &loPV}, {pvHi, &hiPV}, {loadLo, &loLoad}, {loadHi, &hiLoad}, {netLo, &loNet}, {netHi, &hiNet}} {
			vals, err := decodeF32(pair.b, layout.n)
			if err != nil {
				return forecasting.Series{}, err
			}
			*pair.dst = vals
		}
		if samples, days, err = decodeBandStats(stats, layout.n); err != nil {
			return forecasting.Series{}, err
		}
	}
	points := make([]forecasting.Point, layout.n)
	for i := 0; i < layout.n; i++ {
		p := forecasting.Point{
			StartMS: starts[i], EndMS: ends[i], PVW: pv[i], LoadW: load[i],
			PVKnown: flagB[i]&1 != 0, LoadKnown: flagB[i]&2 != 0,
			PVQuality: dict[pvQB[i]], LoadQuality: dict[loadQB[i]],
		}
		if i == 0 {
			p.PredictionStartMS = layout.predictionStartMS
		}
		if len(pvSB) > 0 {
			p.PVSource = dict[pvSB[i]]
		}
		if len(loadSB) > 0 {
			p.LoadSource = dict[loadSB[i]]
		}
		if champion {
			pvM, loadM, netM := unpackBandMethod(methodB[i])
			p.PVBand = forecasting.Band{LowW: loPV[i], HighW: hiPV[i], Method: pvM, Samples: samples[i][0], Days: days[i][0]}
			p.LoadBand = forecasting.Band{LowW: loLoad[i], HighW: hiLoad[i], Method: loadM, Samples: samples[i][1], Days: days[i][1]}
			p.NetBand = forecasting.Band{LowW: loNet[i], HighW: hiNet[i], Method: netM, Samples: samples[i][2], Days: days[i][2]}
			if p.PVBand.LowW > p.PVBand.HighW {
				p.PVBand.HighW = p.PVBand.LowW
			}
			if p.LoadBand.LowW > p.LoadBand.HighW {
				p.LoadBand.HighW = p.LoadBand.LowW
			}
			if p.NetBand.LowW > p.NetBand.HighW {
				p.NetBand.HighW = p.NetBand.LowW
			}
		} else {
			cold := forecasting.Band{Method: forecasting.BandMethodColdStart}
			p.PVBand, p.LoadBand, p.NetBand = cold, cold, cold
		}
		points[i] = p
	}
	return forecasting.Series{Name: name, ModelVersion: version, Points: points}, nil
}

func (a *ForecastLongArchive) loadWeather(ctx context.Context, availJSON string, spanStart, spanEnd int64) ([]forecasting.Weather, error) {
	if availJSON == "" || spanEnd <= spanStart {
		return nil, nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(availJSON), &ids); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	query := `SELECT source, available_at_ms, start_ms, end_ms, ghi, cloud, temp, direct_pv, est_pv FROM fa_weather
 WHERE available_at_ms IN (` + placeholders(len(ids)) + `) AND start_ms < ? AND end_ms > ? ORDER BY start_ms, source`
	args := make([]any, 0, len(ids)+2)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, spanEnd, spanStart)
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []forecasting.Weather
	for rows.Next() {
		var row forecasting.Weather
		var ghi, cloud, temp, direct, est sql.NullFloat64
		if err := rows.Scan(&row.Source, &row.AvailableAtMS, &row.StartMS, &row.EndMS, &ghi, &cloud, &temp, &direct, &est); err != nil {
			return nil, err
		}
		row.GHIWm2 = floatPtr(ghi)
		row.CloudPct = floatPtr(cloud)
		row.TempC = floatPtr(temp)
		row.DirectPVW = floatPtr(direct)
		row.EstimatedPVW = floatPtr(est)
		out = append(out, row)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*2)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

func floatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid || math.IsNaN(v.Float64) {
		return nil
	}
	f := v.Float64
	return &f
}

// ArchiveObservations stores quarter-hour truth. The 30-day source records
// household load and PV, not grid power, so grid is not invented here.
func (a *ForecastLongArchive) ArchiveObservations(ctx context.Context, rows []forecasting.Observation) error {
	if len(rows) == 0 {
		return nil
	}
	if err := a.beforeWrite(); err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, row := range rows {
		if err := row.Validate(); err != nil {
			return err
		}
		pv, load := 0, 0
		if row.PVKnown {
			pv = 1
		}
		if row.LoadKnown {
			load = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fa_observations_15m(
 start_ms, end_ms, config_version, available_at_ms, pv_w, load_w, pv_known, load_known, quality)
 VALUES(?,?,?,?,?,?,?,?,?)`, row.StartMS, row.EndMS, row.ConfigVersion, row.AvailableAtMS, row.PVW, row.LoadW, pv, load, row.Quality); err != nil {
			return err
		}
	}
	return tx.Commit()
}
