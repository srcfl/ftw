package fleetping

import (
	"path"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
)

// FactsFromConfig reads one box's shape out of its configuration.
//
// The only field here that needs care is the driver list. A configured driver
// has two names: `name`, which the operator types and which reads
// "sungrow-vasagatan-12" or "pappas-laddbox" on real sites, and `lua`, which
// points at the catalogue file the driver came from. Only the second one is a
// type rather than a household detail, so it is the one used — and even then
// only the file's own name, never the path it was found at, since a user
// drivers directory sits under a home directory.
func FactsFromConfig(cfg *config.Config, version string, catalogue []string, installedAt time.Time) Facts {
	f := Facts{
		Version:     version,
		Catalogue:   catalogue,
		InstalledAt: installedAt,
	}
	if cfg == nil {
		return f
	}

	for _, d := range cfg.Drivers {
		if d.Disabled {
			continue
		}
		if name := driverFileType(d.Lua); name != "" {
			f.Drivers = append(f.Drivers, name)
		}
		// Every battery at the site, including the ones another party
		// actuates: the question is how much storage FTW sees, not how much
		// it commands.
		f.BatteryWh += d.BatteryCapacityWh
	}
	if cfg.Price != nil {
		f.PriceZone = cfg.Price.Zone
	}
	return f
}

// RunningDrivers is the configured drivers that delivered a reading in the
// last day, by type, each with the version its file declares. lastSuccess and
// version are the box's telemetry and catalogue, so this package stays free
// of both.
func RunningDrivers(cfg *config.Config, now time.Time, lastSuccess func(name string) (time.Time, bool), version func(lua string) string) map[string]string {
	out := map[string]string{}
	if cfg == nil {
		return out
	}
	for _, d := range cfg.Drivers {
		if d.Disabled {
			continue
		}
		at, ok := lastSuccess(d.Name)
		if !ok || now.Sub(at) > 24*time.Hour {
			continue
		}
		if name := driverFileType(d.Lua); name != "" {
			out[name] = version(d.Lua)
		}
	}
	return out
}

// driverFileType is the catalogue name behind a configured `lua:` path —
// "drivers/sungrow.lua" and "/home/pi/.ftw/drivers/sungrow.lua" are both
// "sungrow". The directory is dropped rather than reported, because the path a
// driver was installed at says where the box is and who set it up.
func driverFileType(luaPath string) string {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(luaPath), `\`, "/"))
	name := strings.TrimSuffix(base, ".lua")
	if name == "." || name == "/" || name == "" {
		return ""
	}
	return name
}

// ShippedCatalogue is the driver types this build bundles.
//
// Compiled in from drivers/BUNDLED_SOURCE.json by genshipped, so the set is
// decided when the binary is built and every box running this release has the
// same one. Nothing on a running box can add to it: not a file in a directory,
// not a config field naming a directory, not a repository. That is the whole
// reason it is a generated constant rather than a scan — a directory is
// something a config can point at, and this list is what tells a household's
// own driver name from a type the fleet has in common.
func ShippedCatalogue() []string {
	return append(make([]string, 0, len(shippedDrivers)), shippedDrivers[:]...)
}

// CatalogueNames is the set of driver types the fleet has in common, taken
// from catalogue filenames.
//
// Never a user drivers directory, and never a whole directory of any kind: a
// driver the operator wrote themselves is not a type the fleet has in common,
// and its filename is whatever they felt like calling it. Which files the
// caller may draw on is decided in go/cmd/ftw's fleetCatalogue, per file
// rather than per directory.
func CatalogueNames(filenames []string) []string {
	out := make([]string, 0, len(filenames))
	for _, f := range filenames {
		if name := driverFileType(f); name != "" {
			out = append(out, name)
		}
	}
	return out
}
