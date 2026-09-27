package prices

import (
	"reflect"
	"testing"
	"time"
)

func TestMixedPriceResolutionPreservesUncoveredMinutes(t *testing.T) {
	start := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	row := func(offset, minutes int, price float64) RawPrice {
		return RawPrice{SlotStart: start.Add(time.Duration(offset) * time.Minute), SlotLenMin: minutes, SEKPerKWh: price}
	}
	for _, tc := range []struct {
		name string
		fine []RawPrice
		want []RawPrice
	}{
		{"first quarter", []RawPrice{row(0, 15, 2)}, []RawPrice{row(0, 15, 2), row(15, 45, 1)}},
		{"middle quarter", []RawPrice{row(15, 15, 2)}, []RawPrice{row(0, 15, 1), row(15, 15, 2), row(30, 30, 1)}},
		{"last quarter", []RawPrice{row(45, 15, 2)}, []RawPrice{row(0, 45, 1), row(45, 15, 2)}},
		{"two separated quarters", []RawPrice{row(45, 15, 3), row(0, 15, 2)}, []RawPrice{row(0, 15, 2), row(15, 30, 1), row(45, 15, 3)}},
		{"nested resolutions", []RawPrice{row(0, 30, 2), row(15, 15, 3)}, []RawPrice{row(0, 15, 2), row(15, 15, 3), row(30, 30, 1)}},
		{"touching boundaries", []RawPrice{row(-15, 15, 2), row(60, 15, 3)}, []RawPrice{row(-15, 15, 2), row(0, 60, 1), row(60, 15, 3)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := append([]RawPrice{row(0, 60, 1)}, tc.fine...)
			original := append([]RawPrice(nil), rows...)
			if got := keepFinestRows(rows); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("prices = %+v, want %+v", got, tc.want)
			}
			if !reflect.DeepEqual(rows, original) {
				t.Fatal("input prices changed")
			}
			for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
				rows[i], rows[j] = rows[j], rows[i]
			}
			if got := keepFinestRows(rows); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("reversed prices = %+v, want %+v", got, tc.want)
			}
		})
	}
}
