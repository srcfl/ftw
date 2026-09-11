package devtools

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/units"
)

func TestBackfillBatSoCIsFraction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := Backfill(s, BackfillConfig{Days: 1, Step: 15 * time.Minute, Seed: 42}, log); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadHistory(0, time.Now().UnixMilli()+1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 50 {
		t.Fatalf("backfill wrote %d rows, want dozens", len(rows))
	}
	sawMid := false
	for _, p := range rows {
		if !units.ValidFraction(p.BatSoC) {
			t.Fatalf("BatSoC %v is not a 0–1 fraction (history used to store 0–100 percent)", p.BatSoC)
		}
		if p.BatSoC > 0.2 && p.BatSoC < 0.8 {
			sawMid = true
		}
		if p.BatSoC > 2 {
			t.Fatalf("BatSoC %v looks like percent, not fraction", p.BatSoC)
		}
	}
	if !sawMid {
		t.Fatal("expected some samples in (0.2, 0.8); pack stuck at empty/full suggests the integrator used percent")
	}
}
