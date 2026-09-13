package drivers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Load the actual recovery bundle through the same default-hook gate as Add.
// This proves startup contract compatibility, not physical device recovery.
func TestBundledDriversMeetStartupContract(t *testing.T) {
	root := "../../../drivers"
	data, err := os.ReadFile(filepath.Join(root, "BUNDLED_SOURCE.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pin struct {
		Drivers []string `json:"drivers"`
	}
	if err := json.Unmarshal(data, &pin); err != nil {
		t.Fatal(err)
	}
	if len(pin.Drivers) == 0 {
		t.Fatal("recovery bundle has no drivers")
	}
	for _, name := range pin.Drivers {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(root, name+".lua")
			d, err := NewLuaDriver(path, NewHostEnv(name, telemetry.NewStore()))
			if err != nil {
				t.Fatal(err)
			}
			defer d.L.Close()
			required, err := legacyDriverRequiresDefaultMode(path, d.hasEntrypoint("driver_command"))
			if err != nil {
				t.Fatal(err)
			}
			if required && !d.hasEntrypoint("driver_default_mode") {
				t.Fatal("pinned control driver would fail the startup default-mode gate")
			}
		})
	}
}
