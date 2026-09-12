package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestDoRolloffPrunesFixedColumnHistory(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	oldMs := time.Now().Add(-state.HotRetention - 24*time.Hour).UnixMilli()
	pts := make([]state.HistoryPoint, 20)
	for i := range pts {
		pts[i] = state.HistoryPoint{
			TsMs:  oldMs + int64(i)*1000,
			GridW: float64(100 + i),
			JSON:  "{}",
		}
	}
	if err := st.BulkRecordHistory(pts); err != nil {
		t.Fatal(err)
	}

	doRolloff(context.Background(), st, filepath.Join(dir, "cold"))

	hot, warm, _, err := st.HistoryCounts()
	if err != nil {
		t.Fatal(err)
	}
	if hot != 0 || warm == 0 {
		t.Fatalf("history counts after rolloff = hot:%d warm:%d, want hot:0 warm:>0", hot, warm)
	}
}
