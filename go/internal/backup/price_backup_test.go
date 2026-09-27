package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// Savings history is costed from past prices, which no provider sends again.
// A full backup must restore them, whether Core or the offline helper made it.
func TestFullBackupRestoresPastPrices(t *testing.T) {
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
			slot := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
			if err := st.SavePrices([]state.PricePoint{{Zone: "SE3", SlotTsMs: slot, SlotLenMin: 15, SpotOreKwh: 42, TotalOreKwh: 99, Source: "test", FetchedAtMs: slot}}); err != nil {
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
			restoredDir := filepath.Join(root, "restored")
			if _, err := Restore(info.Path, restoredDir, time.Time{}); err != nil {
				t.Fatal(err)
			}
			restored, err := state.Open(filepath.Join(restoredDir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			prices, err := restored.LoadPrices("SE3", slot, slot)
			if err != nil || len(prices) != 1 || prices[0].TotalOreKwh != 99 {
				t.Fatalf("restored prices = %+v err=%v", prices, err)
			}
		})
	}
}
