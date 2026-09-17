package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestBackupRestoresAggregateEvidenceAndDuplicateIdentity(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "source")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.db")
	cold := filepath.Join(dir, "cold")
	st, err := state.OpenWithLegacyHistory(path, cold)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
	for _, sm := range []state.Sample{{Driver: "ev", Metric: "power", TsMs: base - 48*time.Hour.Milliseconds(), Value: 300}, {Driver: "ev", Metric: "power", TsMs: base + 1000, Value: 100}, {Driver: "ev", Metric: "power", TsMs: base + 2000, Value: 500}} {
		if err := st.EnqueueTelemetryTick(nil, []state.Sample{sm}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := st.MaintainAggregateHistory(context.Background(), cold, time.Now()); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(cold, "2026", "09", ".ftw-buckets-01.pending.db"), "resumable scratch, not a backup source")
	info, err := Create(context.Background(), CreateOptions{State: st, StatePath: path, DataDir: dir, OutputDir: filepath.Join(root, "backups")})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Verify(info.Path)
	if err != nil {
		t.Fatal(err)
	}
	archiveFound := false
	for _, f := range manifest.Files {
		if strings.Contains(f.Path, ".ftw-buckets-") {
			t.Fatal("included mutable scratch", f.Path)
		}
		archiveFound = archiveFound || strings.HasSuffix(f.Path, ".buckets.parquet")
	}
	if !archiveFound {
		t.Fatal("missing aggregate Parquet")
	}
	target := filepath.Join(root, "restore")
	if _, err := Restore(info.Path, target, time.Now()); err != nil {
		t.Fatal(err)
	}
	restored, err := state.OpenWithLegacyHistory(filepath.Join(target, "state.db"), filepath.Join(target, "cold"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	if err := restored.EnqueueTelemetryTick(nil, []state.Sample{{Driver: "ev", Metric: "power", TsMs: base + 2000, Value: 999}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := restored.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	points, err := restored.LoadSeriesBucketsOrRaw("ev", "power", base-72*time.Hour.Milliseconds(), base+60000, 0)
	if err != nil || len(points) != 2 || points[0].N != 1 || points[0].V != 300 || points[1].N != 2 || points[1].V != 300 || points[1].Min != 100 || points[1].Max != 500 {
		t.Fatal(points, err)
	}
}
