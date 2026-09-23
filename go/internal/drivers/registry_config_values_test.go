package drivers

import (
	"encoding/json"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestDriverConfigJSONRoundTripDoesNotRestart(t *testing.T) {
	fromYAML := config.Driver{Lua: "/app/drivers/sungrow.lua", Config: map[string]any{
		"unit_id": 1, "port": 502, "timeout": 2.5,
		"nested":   map[string]any{"registers": []any{1, 2, 3}, "enabled": true},
		"password": "test-value",
	}}
	data, err := json.Marshal(fromYAML)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON config.Driver
	if err := json.Unmarshal(data, &fromJSON); err != nil {
		t.Fatal(err)
	}
	if !sameDriverConfig(fromYAML, fromJSON) || !sameDriverConfig(fromJSON, fromYAML) {
		t.Fatal("unchanged values restart on API save or file reload")
	}
	for _, tc := range []struct {
		name, key string
		value     any
	}{
		{"changed number", "unit_id", float64(2)},
		{"number became string", "unit_id", "1"},
		{"changed secret", "password", "new-value"},
		{"nested change", "nested", map[string]any{"registers": []any{1, 2, 4}, "enabled": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var changed config.Driver
			if err := json.Unmarshal(data, &changed); err != nil {
				t.Fatal(err)
			}
			changed.Config[tc.key] = tc.value
			if sameDriverConfig(fromYAML, changed) {
				t.Fatal("real setting change ignored")
			}
		})
	}
}
