package drivers

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// CatalogEntry describes one available driver discovered in the drivers
// directory. Populated from the DRIVER={…} table each .lua file declares
// at the top. Missing fields are left empty.
type CatalogEntry struct {
	Path               string         `json:"path"`     // portable config lua path
	Filename           string         `json:"filename"` // e.g. "ferroamp.lua"
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Manufacturer       string         `json:"manufacturer,omitempty"`
	Version            string         `json:"version,omitempty"`
	HostAPIMin         int            `json:"host_api_min,omitempty"`
	HostAPIMax         int            `json:"host_api_max,omitempty"`
	Source             string         `json:"source,omitempty"` // local | managed | bundled | upstream
	RepositoryID       string         `json:"repository_id,omitempty"`
	InstalledVersion   string         `json:"installed_version,omitempty"`
	UpstreamVersion    string         `json:"upstream_version,omitempty"`
	UpdateAvailable    bool           `json:"update_available,omitempty"`
	Protocols          []string       `json:"protocols,omitempty"`    // mqtt / modbus / http
	Capabilities       []string       `json:"capabilities,omitempty"` // meter / pv / battery
	HTTPHosts          []string       `json:"http_hosts,omitempty"`
	Description        string         `json:"description,omitempty"`
	Homepage           string         `json:"homepage,omitempty"`
	ConnectionDefaults map[string]any `json:"connection_defaults,omitempty"`
	// ReadOnly means the driver never accepts dispatch commands. The catalog
	// UI uses it to avoid presenting battery capacity as a control opt-in and
	// to enable battery_telemetry_only for gateway-style drivers.
	ReadOnly bool `json:"read_only,omitempty"`

	// Verification: who's actually run this driver against real
	// hardware and how long. Populated from the DRIVER block's optional
	// fields. The UI surfaces this as a badge next to each driver in
	// the catalog picker so operators can distinguish "ported from
	// reference, never proven against hardware" from "running for
	// weeks at multiple sites".
	VerificationStatus string   `json:"verification_status,omitempty"` // experimental | beta | production
	VerifiedBy         []string `json:"verified_by,omitempty"`         // e.g. "frahlg@homelab-rpi:14d"
	VerifiedAt         string   `json:"verified_at,omitempty"`         // ISO date of most recent entry
	VerificationNotes  string   `json:"verification_notes,omitempty"`
	TestedModels       []string `json:"tested_models,omitempty"` // e.g. ["Home", "Charge"]

	// ConfigSecrets lists driver-specific config keys that the Settings
	// UI / setup wizard should render as password inputs and store
	// under config.<key>. Used for things like Auth-Tokens that the
	// operator would otherwise have to drop into config.yaml by hand
	// (e.g. the sonnen JSON-API v2 Auth-Token). The Lua side just
	// reads `config.<key>` like any other entry — this is purely a
	// hint for the UI layer.
	ConfigSecrets []string `json:"config_secrets,omitempty"`
}

// LoadCatalog scans dir (and any direct sub-directories) for .lua driver
// files and extracts their DRIVER metadata table. Files without a DRIVER
// block are still returned — just with ID/Name empty — so operators can
// at least see they exist.
func LoadCatalog(dir string) ([]CatalogEntry, error) {
	return LoadCatalogMulti(dir)
}

type CatalogSource struct {
	Dir    string
	Source string
}

// LoadCatalogMulti scans one or more directories for .lua driver files and
// merges the results. Directories are scanned in order; when the same
// filename appears in more than one directory the first occurrence wins
// (earlier dirs take precedence). This allows a "user" directory passed
// first to shadow bundled drivers of the same name.
//
// Directories that don't exist or can't be read are silently skipped so
// callers don't need to guard against an empty user-drivers dir.
func LoadCatalogMulti(dirs ...string) ([]CatalogEntry, error) {
	sources := make([]CatalogSource, 0, len(dirs))
	for _, dir := range dirs {
		sources = append(sources, CatalogSource{Dir: dir})
	}
	return LoadCatalogSources(sources...)
}

// LoadCatalogSources is LoadCatalogMulti with explicit provenance labels.
// Source order is resolver order, so the first matching filename wins.
func LoadCatalogSources(sources ...CatalogSource) ([]CatalogEntry, error) {
	seen := make(map[string]struct{}) // keyed by Filename (e.g. "ferroamp.lua")
	var out []CatalogEntry

	for _, source := range sources {
		dir := source.Dir
		if dir == "" {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			continue // missing or inaccessible — skip silently
		}
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip unreadable, don't fail the whole scan
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".lua") {
				return nil
			}
			filename := filepath.Base(path)
			if _, exists := seen[filename]; exists {
				return nil // earlier dir already claimed this name
			}
			entry, err := parseCatalogEntry(path)
			if err != nil {
				return nil // skip malformed
			}
			rel, _ := filepath.Rel(dir, path)
			entry.Path = filepath.ToSlash(filepath.Join("drivers", rel))
			entry.Filename = filename
			entry.Source = source.Source
			seen[filename] = struct{}{}
			out = append(out, entry)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", dir, err)
		}
	}

	// Stable sort by name (then filename as tiebreaker).
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].Name, out[j].Name
		if a == b {
			return out[i].Filename < out[j].Filename
		}
		if a == "" {
			return false
		}
		if b == "" {
			return true
		}
		return a < b
	})
	return out, nil
}

// parseCatalogEntry opens the .lua file, finds the DRIVER = {…} block,
// and extracts string/list fields via regex. Lightweight — no Lua VM.
func parseCatalogEntry(path string) (CatalogEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CatalogEntry{}, err
	}
	s := string(data)
	block := extractDriverBlock(s)
	e := CatalogEntry{}
	e.ID = pickString(block, "id")
	e.Name = pickString(block, "name")
	e.Manufacturer = pickString(block, "manufacturer")
	e.Version = pickString(block, "version")
	e.HostAPIMin = pickInt(block, "host_api_min")
	e.HostAPIMax = pickInt(block, "host_api_max")
	if e.HostAPIMin == 0 {
		e.HostAPIMin = 1
	}
	if e.HostAPIMax == 0 {
		e.HostAPIMax = e.HostAPIMin
	}
	e.Description = pickString(block, "description")
	e.Homepage = pickString(block, "homepage")
	e.Protocols = pickList(block, "protocols")
	e.Capabilities = pickList(block, "capabilities")
	e.HTTPHosts = pickList(block, "http_hosts")
	e.ConnectionDefaults = pickKVBlock(block, "connection_defaults")
	e.ReadOnly = pickBool(block, "read_only")
	e.VerificationStatus = normalizeVerificationStatus(pickString(block, "verification_status"))
	e.VerifiedBy = pickList(block, "verified_by")
	e.VerifiedAt = pickString(block, "verified_at")
	e.VerificationNotes = pickString(block, "verification_notes")
	e.TestedModels = pickList(block, "tested_models")
	e.ConfigSecrets = pickList(block, "config_secrets")
	return e, nil
}

// ParseCatalogFile exposes the same lightweight metadata parser to the signed
// repository installer, so bundled and downloaded drivers are checked against
// one contract.
func ParseCatalogFile(path string) (CatalogEntry, error) { return parseCatalogEntry(path) }

// IsEVOrVehicleDriver reports whether the driver at luaPath is an EV
// charger (capabilities contains "ev") or a vehicle telemetry source
// (capabilities contains "vehicle"). Returns false when the driver
// isn't in the catalog or declares neither.
//
// The match is by the catalog entry's portable Path (relative to
// drivers/) first, then by Filename — operators reference drivers
// either way in YAML and both forms must work.
//
// This is the source of truth for "is this a non-stationary-battery
// driver" decisions in main.go (battery-pool capacity exclusion +
// operator-warning routing). The Lua DRIVER table's capabilities list
// is the driver's self-declaration; nothing in Go should match on
// filenames or vendor names directly.
func IsEVOrVehicleDriver(catalog []CatalogEntry, luaPath string) bool {
	if luaPath == "" {
		return false
	}
	wantPath := filepath.ToSlash(luaPath)
	wantFilename := filepath.Base(wantPath)
	for _, e := range catalog {
		if strings.EqualFold(e.Path, wantPath) || strings.EqualFold(e.Filename, wantFilename) {
			for _, c := range e.Capabilities {
				if strings.EqualFold(c, "ev") || strings.EqualFold(c, "vehicle") {
					return true
				}
			}
			return false
		}
	}
	return false
}

// IsReadOnlyDriver reports whether the matched Lua catalog entry explicitly
// declares read_only=true. Control-pool construction uses this as a safety
// boundary for telemetry gateways such as Sourceful Zap.
func IsReadOnlyDriver(catalog []CatalogEntry, luaPath string) bool {
	if luaPath == "" {
		return false
	}
	wantPath := filepath.ToSlash(luaPath)
	wantFilename := filepath.Base(wantPath)
	for _, e := range catalog {
		if strings.EqualFold(e.Path, wantPath) || strings.EqualFold(e.Filename, wantFilename) {
			return e.ReadOnly
		}
	}
	return false
}

// normalizeVerificationStatus coerces the Lua string into one of the
// three canonical values the UI renders badges for. Anything else
// (blank, typo, unknown) falls back to "experimental" — the safest
// default for a driver with no declared provenance.
func normalizeVerificationStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "production":
		return "production"
	case "beta":
		return "beta"
	case "experimental", "":
		return "experimental"
	default:
		return "experimental"
	}
}

var driverBlockRe = regexp.MustCompile(`(?s)DRIVER\s*=\s*\{(.*?)\n\}`)

func extractDriverBlock(src string) string {
	m := driverBlockRe.FindStringSubmatch(src)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// pickString matches `name = "value"` inside the block.
func pickString(block, name string) string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]*)"`)
	m := re.FindStringSubmatch(block)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func pickInt(block, name string) int {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*([0-9]+)`)
	m := re.FindStringSubmatch(block)
	if len(m) < 2 {
		return 0
	}
	v, _ := strconv.Atoi(m[1])
	return v
}

func pickBool(block, name string) bool {
	re := regexp.MustCompile(`(?mi)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*(true|false)`)
	m := re.FindStringSubmatch(block)
	return len(m) >= 2 && strings.EqualFold(m[1], "true")
}

// pickList matches `name = { "a", "b", "c" }` inside the block.
func pickList(block, name string) []string {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `\s*=\s*\{([^}]*)\}`)
	m := re.FindStringSubmatch(block)
	if len(m) < 2 {
		return nil
	}
	items := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it[1])
	}
	return out
}

// kvPairRe matches `key = "string"` or `key = 123` inside a Lua table body.
var kvPairRe = regexp.MustCompile(`(\w+)\s*=\s*(?:"([^"]*)"|([^\s,]+))`)

// pickKVBlock matches a nested Lua table `name = { key = val, ... }`
// and returns key-value pairs as a map. Values can be numbers or quoted
// strings. Returns nil when the block is absent.
func pickKVBlock(block, name string) map[string]any {
	re := regexp.MustCompile(`(?s)` + regexp.QuoteMeta(name) + `\s*=\s*\{([^}]*)\}`)
	m := re.FindStringSubmatch(block)
	if len(m) < 2 {
		return nil
	}
	pairs := kvPairRe.FindAllStringSubmatch(m[1], -1)
	if len(pairs) == 0 {
		return nil
	}
	out := make(map[string]any, len(pairs))
	for _, p := range pairs {
		key := p[1]
		if p[2] != "" {
			out[key] = p[2]
		} else if f, err := strconv.ParseFloat(p[3], 64); err == nil {
			if f == float64(int64(f)) {
				out[key] = int64(f)
			} else {
				out[key] = f
			}
		} else {
			out[key] = p[3]
		}
	}
	return out
}
