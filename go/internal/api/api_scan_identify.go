package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/scanner"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// identifyDriver is the matched driver an identified host was claimed by.
type identifyDriver struct {
	ID           string   `json:"id,omitempty"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer,omitempty"`
	LuaPath      string   `json:"lua_path"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// identifiedDevice is one scanned host plus whatever driver claimed it. When
// Identified is false the host goes into the UI's "Other devices" group.
type identifiedDevice struct {
	scanner.FoundDevice
	Identified        bool            `json:"identified"`
	Driver            *identifyDriver `json:"driver,omitempty"`
	Make              string          `json:"make,omitempty"`
	Model             string          `json:"model,omitempty"`
	Serial            string          `json:"serial,omitempty"`
	BatteryCapacityWh float64         `json:"battery_capacity_wh,omitempty"`
}

// handleScanIdentify: POST /api/scan/identify
//
// Body: a JSON array of scan hits, or {"devices":[...]}, each {ip, port,
// protocol, hostname}. For every host we ask each catalog driver whose
// declared protocol matches "is this you?" via its read-only
// driver_fingerprint. The first driver that matches claims the host and
// reports the capabilities it actually detected on that specific device
// (e.g. a SolarEdge with vs without a revenue meter). Hosts no driver can
// identify — including ones behind auth — come back with identified=false so
// the UI can still surface them under "Other devices".
func (s *Server) handleScanIdentify(w http.ResponseWriter, r *http.Request) {
	if s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "driver registry unavailable"})
		return
	}

	// Accept either a bare array or {"devices":[...]} — read the body once and
	// try the wrapper first, then the bare array.
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB cap
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "read body: " + err.Error()})
		return
	}
	var devices []scanner.FoundDevice
	var wrapped struct {
		Devices []scanner.FoundDevice `json:"devices"`
	}
	if json.Unmarshal(body, &wrapped) == nil && len(wrapped.Devices) > 0 {
		devices = wrapped.Devices
	} else if err := json.Unmarshal(body, &devices); err != nil {
		writeJSON(w, 400, map[string]string{"error": "expected a devices array"})
		return
	}
	if len(devices) == 0 {
		writeJSON(w, 200, map[string]any{"devices": []identifiedDevice{}})
		return
	}

	dir := s.deps.DriverDir
	if dir == "" {
		dir = "drivers"
	}
	catalog, err := drivers.LoadCatalogMulti(s.deps.UserDriverDir, dir)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "load catalog: " + err.Error()})
		return
	}

	baseDir := "."
	if s.deps.ConfigPath != "" {
		baseDir = filepath.Dir(s.deps.ConfigPath)
	}

	// One probe registry shared across all fingerprints — Registry.Fingerprint
	// never registers into the run map, so concurrent calls are independent.
	tel := telemetry.NewStore()
	reg := drivers.NewRegistry(tel)
	reg.MQTTFactory = s.deps.DriverMQTTFactory
	reg.ModbusFactory = s.deps.DriverModbusFactory
	reg.ARPLookup = s.deps.DriverARPLookup

	out := make([]identifiedDevice, len(devices))
	sem := make(chan struct{}, 8) // bound concurrent host probes
	var wg sync.WaitGroup
	for i, dev := range devices {
		wg.Add(1)
		go func(i int, dev scanner.FoundDevice) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = s.identifyOne(r.Context(), reg, catalog, baseDir, dev)
		}(i, dev)
	}
	wg.Wait()

	writeJSON(w, 200, map[string]any{"devices": out})
}

// identifyOne probes a single host against every protocol-matching catalog
// driver and returns the first match (or an unidentified result).
func (s *Server) identifyOne(ctx context.Context, reg *drivers.Registry, catalog []drivers.CatalogEntry, baseDir string, dev scanner.FoundDevice) identifiedDevice {
	res := identifiedDevice{FoundDevice: dev}
	for _, entry := range catalog {
		if !protocolMatches(entry.Protocols, dev.Protocol) {
			continue
		}
		cfg, ok := probeConfigFor(entry, dev, baseDir)
		if !ok {
			continue
		}
		// Per-candidate budget: MQTT fingerprints wait a few seconds for a
		// telemetry frame; Modbus ones return in well under a second.
		fpCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
		fp, err := reg.Fingerprint(fpCtx, cfg)
		cancel()
		if err != nil || !fp.Matched {
			continue
		}

		caps := fp.Capabilities
		if len(caps) == 0 {
			caps = entry.Capabilities // fall back to the driver's static catalog caps
		}
		res.Identified = true
		res.Make = fp.Make
		res.Model = fp.Model
		res.Serial = fp.Serial
		res.BatteryCapacityWh = fp.BatteryCapacityWh
		res.Driver = &identifyDriver{
			ID:           entry.ID,
			Name:         entry.Name,
			Manufacturer: entry.Manufacturer,
			LuaPath:      entry.Path,
			Capabilities: caps,
		}
		return res
	}
	return res
}

// protocolMatches reports whether the catalog entry speaks the scanned port's
// protocol. An entry with no declared protocols matches nothing (we can't
// point it anywhere meaningful).
func protocolMatches(entryProtocols []string, proto string) bool {
	for _, p := range entryProtocols {
		if strings.EqualFold(p, proto) {
			return true
		}
	}
	return false
}

// probeConfigFor builds a throwaway driver config aimed at one scanned host
// for the given catalog entry. Returns ok=false for protocols we can't wire a
// probe transport for (the driver simply won't be tried).
func probeConfigFor(entry drivers.CatalogEntry, dev scanner.FoundDevice, baseDir string) (config.Driver, bool) {
	cfg := config.Driver{
		Name: "__id_" + safeProbeName(entry.Filename) + "_" + strconv.Itoa(dev.Port),
		Lua:  entry.Path,
	}
	switch strings.ToLower(dev.Protocol) {
	case "modbus":
		port := dev.Port
		if port == 0 {
			port = 502
		}
		unit := 1
		if u, ok := connDefaultInt(entry.ConnectionDefaults, "unit_id"); ok && u > 0 {
			unit = u
		}
		cfg.Capabilities.Modbus = &config.ModbusConfig{Host: dev.IP, Port: port, UnitID: unit}
	case "mqtt":
		port := dev.Port
		if port == 0 {
			port = 1883
		}
		cfg.Capabilities.MQTT = &config.MQTTConfig{Host: dev.IP, Port: port}
	default:
		// http / tcp / websocket fingerprints aren't wired yet — those
		// drivers have no driver_fingerprint, so there's nothing to probe.
		return config.Driver{}, false
	}

	resolved := config.Config{Drivers: []config.Driver{cfg}}
	resolved.ResolveDriverPaths(baseDir)
	return resolved.Drivers[0], true
}

// connDefaultInt pulls an int out of a catalog entry's connection_defaults map,
// tolerating the int64/float64/int the lightweight catalog parser may produce.
func connDefaultInt(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}
