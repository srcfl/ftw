package api

import (
	"os"
	"strings"
	"sync"

	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// vehicleOnlyByFile caches the catalog answer per driver file and
// modification time, so a 2 s status poll does not parse Lua each time.
var vehicleOnlyByFile sync.Map // path + "\x00" + mtime -> bool

// statusVehicleOnly reports whether a driver only reads a car. A car away
// from home answers nothing for hours, and after a restart it may not have
// answered at all; that is normal, so the overview leaves such a driver out
// of LIVE unless it reports an error.
//
// The driver's own catalog entry decides when Core can read it: the vehicle
// capability and nothing else, as the cars wiring reads it. Without one, its
// readings decide: vehicle readings and no others.
func statusVehicleOnly(lua string, tel *telemetry.Store, name string) bool {
	if lua != "" {
		if info, err := os.Stat(lua); err == nil && !info.IsDir() {
			key := lua + "\x00" + info.ModTime().String()
			if v, ok := vehicleOnlyByFile.Load(key); ok {
				return v.(bool)
			}
			if entry, err := drivers.ParseCatalogFile(lua); err == nil && len(entry.Capabilities) > 0 {
				only := true
				for _, c := range entry.Capabilities {
					only = only && strings.EqualFold(c, "vehicle")
				}
				vehicleOnlyByFile.Store(key, only)
				return only
			}
		}
	}
	if tel == nil || tel.Get(name, telemetry.DerVehicle) == nil {
		return false
	}
	for _, der := range telemetry.AllDerTypes() {
		if der != telemetry.DerVehicle && tel.Get(name, der) != nil {
			return false
		}
	}
	return true
}
