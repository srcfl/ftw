// Driver fingerprinting: given an open endpoint discovered by a network
// scan (host + port), ask every driver that speaks that protocol whether
// the device is one of its own. Each driver answers true / false / unknown
// via its Lua driver_fingerprint hook (see drivers/fingerprint.go).
//
// This is the auto-detect counterpart to POST /api/drivers/test: test
// validates a driver the operator already picked; fingerprint discovers
// which driver(s) to pick in the first place.
package api

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/scanner"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

type fingerprintReq struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol,omitempty"` // inferred from port when empty
	UnitID   int    `json:"unit_id,omitempty"`  // modbus unit id, default 1
}

type fingerprintResp struct {
	Host      string                `json:"host"`
	Port      int                   `json:"port"`
	Protocol  string                `json:"protocol"`
	UnitID    int                   `json:"unit_id,omitempty"`
	Matches   []drivers.Fingerprint `json:"matches"` // MatchYes only, best first
	Tried     []drivers.Fingerprint `json:"tried"`   // every candidate + verdict
	ElapsedMs int64                 `json:"elapsed_ms"`
}

// fingerprintBudget caps the whole sweep so an unreachable host doesn't
// hang the request through every candidate's dial timeout. Checked between
// candidates (a single in-flight Modbus read can't be cancelled mid-call).
const fingerprintBudget = 20 * time.Second

// POST /api/drivers/fingerprint — probe a Modbus endpoint against every
// catalog driver that speaks Modbus and report which (if any) recognise
// the device. Body: {host, port, protocol?, unit_id?}.
func (s *Server) handleDriverFingerprint(w http.ResponseWriter, r *http.Request) {
	var req fingerprintReq
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	var err error
	req.Host, err = normalizeFingerprintHost(req.Host)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		writeJSON(w, 400, map[string]string{"error": "missing or invalid port"})
		return
	}

	protocol := strings.ToLower(strings.TrimSpace(req.Protocol))
	if protocol == "" {
		protocol = inferProtocol(req.Port)
	}
	if protocol == "" {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("cannot infer protocol for port %d — specify \"protocol\"", req.Port)})
		return
	}
	if !supportedFingerprintProtocol(protocol) {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("fingerprinting supports protocol \"modbus\" or \"http\" (got %q)", protocol)})
		return
	}
	if protocol == "modbus" && s.deps.DriverModbusFactory == nil {
		writeJSON(w, 503, map[string]string{"error": "modbus fingerprinting unavailable"})
		return
	}

	if req.UnitID < 0 || req.UnitID > 255 {
		writeJSON(w, 400, map[string]string{"error": "unit_id must be between 1 and 255"})
		return
	}
	unit := req.UnitID
	if unit == 0 {
		unit = 1
	}

	started := time.Now()
	tried := s.sweepFingerprint(protocol, req.Host, req.Port, unit)
	matches := matchesOf(tried)
	sort.SliceStable(tried, func(i, j int) bool {
		if r := matchRank(tried[i].Match) - matchRank(tried[j].Match); r != 0 {
			return r < 0
		}
		return tried[i].Name < tried[j].Name
	})

	writeJSON(w, 200, fingerprintResp{
		Host:      req.Host,
		Port:      req.Port,
		Protocol:  protocol,
		UnitID:    unit,
		Matches:   matches,
		Tried:     tried,
		ElapsedMs: time.Since(started).Milliseconds(),
	})
}

// sweepFingerprint runs every catalog driver that speaks `protocol`
// against host:port (unit id is only meaningful for Modbus) and returns
// each verdict. Shared by POST /api/drivers/fingerprint and the GET
// /api/scan enrichment. A whole-sweep budget bounds how long an
// unreachable host can stall the caller; candidates that don't fit are
// simply not probed.
func (s *Server) sweepFingerprint(protocol, host string, port, unit int) []drivers.Fingerprint {
	dir := s.deps.DriverDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(s.deps.ConfigPath), "drivers")
	}
	entries, err := drivers.LoadCatalogMulti(s.deps.UserDriverDir, s.managedDriverDir(), dir)
	if err != nil {
		return nil
	}
	deadline := time.Now().Add(fingerprintBudget)
	var tried []drivers.Fingerprint
	for _, e := range entries {
		if !hasProtocol(e.Protocols, protocol) {
			continue
		}
		if time.Now().After(deadline) {
			break // ran out of budget — stop probing further candidates
		}
		luaPath := resolveDriverPath(s.deps.UserDriverDir, dir, e.Filename)
		if luaPath == "" {
			continue
		}
		fp := s.fingerprintOne(luaPath, protocol, host, port, unit)
		fp.Driver = e.Filename
		fp.Name = e.Name
		tried = append(tried, fp)
	}
	return tried
}

// matchesOf filters a sweep down to confirmed matches, best confidence
// first (name as a stable tiebreak).
func matchesOf(tried []drivers.Fingerprint) []drivers.Fingerprint {
	matches := make([]drivers.Fingerprint, 0, len(tried))
	for _, fp := range tried {
		if fp.Match == drivers.MatchYes {
			matches = append(matches, fp)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Confidence != matches[j].Confidence {
			return matches[i].Confidence > matches[j].Confidence
		}
		return matches[i].Name < matches[j].Name
	})
	return matches
}

// fingerprintOne wires the protocol-appropriate capability into a probe
// HostEnv for one candidate driver, runs its driver_fingerprint, and tears
// the connection down. A capability/connection error becomes an unknown
// verdict carrying the error so the operator can see why a candidate was
// skipped (e.g. host unreachable).
func (s *Server) fingerprintOne(luaPath, protocol, host string, port, unit int) drivers.Fingerprint {
	target := drivers.FingerprintTarget{Host: host, Port: port, Protocol: protocol}
	switch protocol {
	case "modbus":
		mbCfg := &config.ModbusConfig{Host: host, Port: port, UnitID: unit}
		cap, err := s.deps.DriverModbusFactory("__fingerprint", mbCfg)
		if err != nil {
			return drivers.Fingerprint{Match: drivers.MatchUnknown, Err: err.Error()}
		}
		defer cap.Close()
		env := drivers.NewHostEnv("__fingerprint", telemetry.NewStore()).WithModbus(cap)
		env.SetEndpoint(fmt.Sprintf("modbus://%s:%d", host, port))
		fp, _ := drivers.RunFingerprint(luaPath, env, target)
		return fp
	case "http":
		// HTTP is a built-in host capability (no factory). Scope the
		// allowlist to exactly the scanned host so a fingerprint can't be
		// tricked into probing some other address via a crafted base_url.
		allowedEndpoint := net.JoinHostPort(host, strconv.Itoa(port))
		env := drivers.NewHostEnv("__fingerprint", telemetry.NewStore()).
			WithHTTP().WithHTTPAllowedHosts([]string{allowedEndpoint})
		env.SetEndpoint(fmt.Sprintf("http://%s:%d", host, port))
		fp, _ := drivers.RunFingerprint(luaPath, env, target)
		return fp
	default:
		return drivers.Fingerprint{Match: drivers.MatchUnknown, Err: "unsupported protocol: " + protocol}
	}
}

// normalizeFingerprintHost accepts a bare IP address or DNS/mDNS hostname.
// URL components, userinfo, and embedded ports are rejected so the target
// table and HTTP allowlist always describe exactly one endpoint.
func normalizeFingerprintHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", fmt.Errorf("missing host")
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	if len(host) > 253 || strings.ContainsAny(host, "/\\@?#:") {
		return "", fmt.Errorf("invalid host")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid host")
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
				return "", fmt.Errorf("invalid host")
			}
		}
	}
	return strings.ToLower(host), nil
}

// scannedDevice augments a discovered open port with fingerprint matches.
// scanner.FoundDevice is embedded so the default (no-fingerprint)
// /api/scan response stays byte-for-byte unchanged — Matches is omitted
// when empty.
type scannedDevice struct {
	scanner.FoundDevice
	Matches []drivers.Fingerprint `json:"matches,omitempty"`
}

// enrichScanWithFingerprints fingerprints every fingerprintable device in
// a scan result (Modbus on 502, HTTP on 80), attaching the confirmed
// matches. Devices on protocols we can't fingerprint — and Modbus devices
// when no Modbus factory is wired — pass through untouched. This is what
// turns "port 502 is open on 10.0.0.7" into "that's a SolarEdge".
func (s *Server) enrichScanWithFingerprints(devices []scanner.FoundDevice) []scannedDevice {
	out := make([]scannedDevice, len(devices))
	for i, d := range devices {
		sd := scannedDevice{FoundDevice: d}
		switch {
		case d.Protocol == "modbus" && s.deps.DriverModbusFactory != nil:
			sd.Matches = matchesOf(s.sweepFingerprint("modbus", d.IP, d.Port, 1))
		case d.Protocol == "http":
			sd.Matches = matchesOf(s.sweepFingerprint("http", d.IP, d.Port, 1))
		}
		out[i] = sd
	}
	return out
}

// supportedFingerprintProtocol reports whether the sweep knows how to wire
// a probe capability for this protocol. MQTT isn't here: identifying a
// device over MQTT means subscribing and waiting for a retained/periodic
// message, which doesn't fit the synchronous request/response probe shape.
func supportedFingerprintProtocol(protocol string) bool {
	return protocol == "modbus" || protocol == "http"
}

// inferProtocol maps a well-known scanner port to the protocol drivers use
// on it. Mirrors scanner.wellKnownPorts for the ports we can fingerprint.
func inferProtocol(port int) string {
	switch port {
	case 502:
		return "modbus"
	case 80:
		return "http"
	default:
		return ""
	}
}

func hasProtocol(protocols []string, want string) bool {
	for _, p := range protocols {
		if strings.EqualFold(p, want) {
			return true
		}
	}
	return false
}

// resolveDriverPath turns a catalog filename into an absolute path,
// honouring the same user-dir-first precedence as LoadCatalogMulti.
func resolveDriverPath(userDir, dir, filename string) string {
	for _, base := range []string{userDir, dir} {
		if base == "" {
			continue
		}
		p := filepath.Join(base, filename)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// matchRank orders verdicts for display: confirmed matches first, then
// definite non-matches, then the inconclusive remainder.
func matchRank(m drivers.MatchState) int {
	switch m {
	case drivers.MatchYes:
		return 0
	case drivers.MatchNo:
		return 1
	default:
		return 2
	}
}
