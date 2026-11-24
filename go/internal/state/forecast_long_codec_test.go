package state

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func TestForecastLongCodecRoundTrip(t *testing.T) {
	values := []float64{0, 1, 1500.5, math.MaxFloat32, -math.MaxFloat32, math.NaN()}
	encoded, err := encodeF32(values)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 4*len(values) {
		t.Fatalf("encoded length %d", len(encoded))
	}
	got, err := decodeF32(encoded, len(values))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range values {
		if math.IsNaN(want) {
			if !math.IsNaN(got[i]) {
				t.Fatalf("value %d: want NaN, got %v", i, got[i])
			}
			continue
		}
		if float32(got[i]) != float32(want) {
			t.Fatalf("value %d: got %v want %v", i, got[i], want)
		}
	}
	if _, err := decodeF32(encoded, len(values)-1); err == nil {
		t.Fatal("length mismatch was accepted")
	}
	bytes, err := encodeU8([]byte{0, 1, 255})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeU8(bytes, 2); err == nil {
		t.Fatal("byte length mismatch was accepted")
	}
}

func TestForecastLongCodecRejectsNonFinite(t *testing.T) {
	if _, err := encodeF32([]float64{math.Inf(1)}); err == nil {
		t.Fatal("positive infinity was accepted")
	}
	if _, err := encodeF32([]float64{math.Inf(-1)}); err == nil {
		t.Fatal("negative infinity was accepted")
	}
	if _, err := encodeF32([]float64{math.MaxFloat32 * 2}); err == nil {
		t.Fatal("value outside float32 was accepted")
	}
}

func TestForecastLongIrregularGridAndPartialPoint(t *testing.T) {
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC).UnixMilli()
	points := []forecasting.Point{
		{StartMS: start, EndMS: start + 15*60*1000, PredictionStartMS: start + 60*1000},
		{StartMS: start + 30*60*1000, EndMS: start + 90*60*1000},
	}
	layout, err := layoutSeries(points)
	if err != nil {
		t.Fatal(err)
	}
	if layout.predictionStartMS != start+60*1000 {
		t.Fatalf("partial first point = %d", layout.predictionStartMS)
	}
	if len(layout.starts) != 16 {
		t.Fatalf("irregular grid bytes = %d, want 16", len(layout.starts))
	}
	starts, ends, err := expandLayout(layout)
	if err != nil {
		t.Fatal(err)
	}
	if starts[0] != points[0].StartMS || ends[0] != points[0].EndMS || starts[1] != points[1].StartMS || ends[1] != points[1].EndMS {
		t.Fatalf("grid = %v %v", starts, ends)
	}
	points[1].PredictionStartMS = points[1].StartMS + 1000
	if _, err := layoutSeries(points); err == nil {
		t.Fatal("partial point after the first was accepted")
	}
}
