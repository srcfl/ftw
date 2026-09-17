package state

import (
	"context"
	"testing"
)

func TestHistoryTableReadbackCrossesPages(t *testing.T) {
	s := freshStore(t)
	points := make([]HistoryPoint, 2048*2+1)
	for i := range points {
		points[i] = HistoryPoint{TsMs: int64(i), GridW: float64(i), JSON: "{}"}
	}
	if err := s.BulkRecordHistory(points); err != nil {
		t.Fatal(err)
	}
	var seen int64
	n, err := scanHistoryTable(context.Background(), s.history, "history_hot", func(values []any) error {
		if values[0].(int64) != seen || values[1].(float64) != float64(seen) {
			t.Fatalf("readback skipped or repeated row %d: %v", seen, values)
		}
		seen++
		return nil
	})
	if err != nil || n != int64(len(points)) {
		t.Fatalf("count=%d seen=%d err=%v", n, seen, err)
	}
}
