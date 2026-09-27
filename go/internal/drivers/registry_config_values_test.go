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

func TestReloadSeesHTTPWebSocketAndPVCurtailChanges(t *testing.T) {
	base := func() config.Driver {
		return config.Driver{
			Lua: "/app/drivers/nibe_local.lua",
			Capabilities: config.Capabilities{
				HTTP: &config.HTTPCapability{
					AllowedHosts: []string{"192.168.1.20"},
					TLSPinSHA256: "aa11",
					AllowWrite:   true,
				},
				WebSocket: &config.WSCapability{AllowedHosts: []string{"192.168.1.20"}},
			},
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*config.Driver)
	}{
		{"allow_write revoked", func(d *config.Driver) { d.Capabilities.HTTP.AllowWrite = false }},
		{"http hosts tightened", func(d *config.Driver) { d.Capabilities.HTTP.AllowedHosts = []string{"192.168.1.21"} }},
		{"tls pin rotated", func(d *config.Driver) { d.Capabilities.HTTP.TLSPinSHA256 = "bb22" }},
		{"http grant removed", func(d *config.Driver) { d.Capabilities.HTTP = nil }},
		{"websocket hosts changed", func(d *config.Driver) { d.Capabilities.WebSocket.AllowedHosts = nil }},
		{"websocket grant removed", func(d *config.Driver) { d.Capabilities.WebSocket = nil }},
		{"pv curtail opted in", func(d *config.Driver) { d.SupportsPVCurtail = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := base()
			tc.change(&changed)
			if sameDriverConfig(base(), changed) || sameDriverConfig(changed, base()) {
				t.Fatal("reload ignored a change the running driver was built from")
			}
		})
	}

	// A JSON save may send an empty allowlist where YAML holds none; add
	// treats both as "no list", so neither side restarts the driver.
	nilHosts, emptyHosts := base(), base()
	nilHosts.Capabilities.HTTP.AllowedHosts = nil
	emptyHosts.Capabilities.HTTP.AllowedHosts = []string{}
	nilHosts.Capabilities.WebSocket.AllowedHosts = nil
	emptyHosts.Capabilities.WebSocket.AllowedHosts = []string{}
	if !sameDriverConfig(nilHosts, emptyHosts) {
		t.Fatal("nil and empty allowlists restart an unchanged driver")
	}
}
