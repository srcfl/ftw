package telemetry

import (
	"encoding/json"
	"testing"
)

func TestBatterySoCCeiling(t *testing.T) {
	for _, tc := range []struct {
		data string
		want float64
		ok   bool
	}{
		{`{"max_soc_fract":0.9}`, 0.9, true},
		{`{"max_soc_fract":1}`, 1, true},
		{`{"max_soc_fract":0}`, 0, false},
		{`{"max_soc_fract":-0.1}`, 0, false},
		{`{"max_soc_fract":90}`, 0, false},
		{`{"max_soc_fract":null}`, 0, false},
		{`{"soc":0.9}`, 0, false},
		{`not json`, 0, false},
		{``, 0, false},
	} {
		got, ok := BatterySoCCeiling(json.RawMessage(tc.data))
		if got != tc.want || ok != tc.ok {
			t.Errorf("BatterySoCCeiling(%s) = %v, %v; want %v, %v", tc.data, got, ok, tc.want, tc.ok)
		}
	}
}
