package backup

import (
	"context"
	"github.com/srcfl/ftw/go/internal/state"
	"path/filepath"
	"testing"
	"time"
)

func TestBackupRestoresDashboardEnergyAndCursor(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	cold := filepath.Join(root, "cold")
	st, err := state.OpenWithLegacyHistory(path, cold)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
	for _, offset := range []int64{0, 1000} {
		p := state.HistoryPoint{TsMs: base + offset, GridW: 3600, JSON: "{}"}
		if err := st.EnqueueTelemetryTick(&p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := Create(context.Background(), CreateOptions{State: st, StatePath: path, DataDir: root, OutputDir: filepath.Join(t.TempDir(), "backup")})
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(info.Path, dst, time.Now()); err != nil {
		t.Fatal(err)
	}
	restored, err := state.OpenWithLegacyHistory(filepath.Join(dst, "state.db"), filepath.Join(dst, "cold"))
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	// A repeated timestamp adds neither chart count nor energy after restore.
	for _, offset := range []int64{1000, 2000} {
		p := state.HistoryPoint{TsMs: base + offset, GridW: 3600, JSON: "{}"}
		if err := restored.EnqueueTelemetryTick(&p, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := restored.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	total, err := restored.DailyEnergy(base, base+2000)
	if err != nil || total.ImportWh != 2 {
		t.Fatal(total, err)
	}
	rows, err := restored.LoadHistory(base, base+10000, 0)
	if err != nil || len(rows) != 1 || rows[0].N != 3 {
		t.Fatal(rows, err)
	}
}
