// Driver-debug surface: per-driver detail (health + last reading + live
// metric snapshots), recent log lines from the in-memory ring, and a
// gzipped support bundle for sending to the developers.
//
// Wired in main.go via Deps.LogRing. Without a LogRing the log + dump
// endpoints return 503; the detail endpoint still works (no logs).
package api

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/assistant"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// driverDetailResp bundles everything an operator wants to see for one
// driver in a single response so the UI doesn't fan-out four parallel
// fetches.
type driverDetailResp struct {
	Name     string                     `json:"name"`
	Health   *telemetry.DriverHealth    `json:"health,omitempty"`
	Readings []readingDTO               `json:"readings"`
	Metrics  []telemetry.MetricSnapshot `json:"metrics"`
	Identity driverIdentityDTO          `json:"identity"`
	// Controls are what this driver says an operator may command. Absent
	// for every driver that only reports.
	Controls []drivers.CatalogControl `json:"controls,omitempty"`
	// Hold is the operator setting in force, if any, and when it ends.
	Hold *controlHold `json:"hold,omitempty"`
	// ControlState says whether Core has confirmed autonomous default mode.
	// It must not call a failed default safe or settled.
	ControlState driverControlStateResp `json:"control_state"`
}

type driverControlStateResp struct {
	State            string `json:"state"`
	Blocked          bool   `json:"blocked"`
	DefaultConfirmed bool   `json:"default_confirmed"`
	RecoveryPending  bool   `json:"recovery_pending"`
}

type readingDTO struct {
	Type      string   `json:"type"`
	RawW      float64  `json:"raw_w"`
	SmoothedW float64  `json:"smoothed_w"`
	SoC       *float64 `json:"soc,omitempty"`
	UpdatedAt int64    `json:"updated_at_ms"`
	Stale     bool     `json:"stale"`
}

type driverIdentityDTO struct {
	Make     string `json:"make,omitempty"`
	SN       string `json:"sn,omitempty"`
	MAC      string `json:"mac,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

type driverProbeResp struct {
	Name      string                     `json:"name"`
	OK        bool                       `json:"ok"`
	Error     string                     `json:"error,omitempty"`
	ElapsedMs int64                      `json:"elapsed_ms"`
	Health    *telemetry.DriverHealth    `json:"health,omitempty"`
	Readings  []readingDTO               `json:"readings"`
	Metrics   []telemetry.MetricSnapshot `json:"metrics"`
	Identity  driverIdentityDTO          `json:"identity"`
}

// GET /api/drivers/{name} — composite view: health, last readings,
// live metric snapshots, hardware identity. Cheap; pure RAM read.
func (s *Server) handleDriverDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "missing driver name"})
		return
	}
	resp := driverDetailResp{Name: name}
	if h := s.deps.Tel.DriverHealth(name); h != nil {
		resp.Health = h
	}
	staleAfter := 60 * time.Second
	if s.deps.Cfg != nil && s.deps.Cfg.Site.WatchdogTimeoutS > 0 {
		staleAfter = time.Duration(s.deps.Cfg.Site.WatchdogTimeoutS) * time.Second
	}
	for _, der := range telemetry.AllDerTypes() {
		rd := s.deps.Tel.Get(name, der)
		if rd == nil {
			continue
		}
		resp.Readings = append(resp.Readings, readingDTO{
			Type:      der.String(),
			RawW:      rd.RawW,
			SmoothedW: rd.SmoothedW,
			SoC:       rd.SoC,
			UpdatedAt: rd.UpdatedAt.UnixMilli(),
			Stale:     time.Since(rd.UpdatedAt) > staleAfter,
		})
	}
	resp.Metrics = s.deps.Tel.LatestMetricsByDriver(name)
	sort.Slice(resp.Metrics, func(i, j int) bool { return resp.Metrics[i].Name < resp.Metrics[j].Name })
	if s.deps.Registry != nil {
		if env := s.deps.Registry.Env(name); env != nil {
			make, sn, mac, ep := env.FullIdentity()
			resp.Identity = driverIdentityDTO{Make: make, SN: sn, MAC: mac, Endpoint: ep}
		}
	}
	resp.Controls = s.driverControls(name)
	resp.Hold = s.activeControlHold(name)
	resp.ControlState = s.driverControlState(name, resp.Hold)
	writeJSON(w, 200, resp)
}

func (s *Server) driverControlState(name string, hold *controlHold) driverControlStateResp {
	if s.deps == nil || s.deps.Registry == nil {
		return driverControlStateResp{State: "unknown"}
	}
	status, ok := s.deps.Registry.ControlStatus(name)
	if !ok {
		return driverControlStateResp{State: "unknown"}
	}
	state := "unknown"
	switch {
	case status.Blocked:
		state = "default_recovery"
	case hold != nil:
		state = "held"
	case status.DefaultConfirmed:
		state = "default_confirmed"
	}
	return driverControlStateResp{
		State:            state,
		Blocked:          status.Blocked,
		DefaultConfirmed: status.DefaultConfirmed,
		RecoveryPending:  status.RecoveryPending,
	}
}

// driverControls returns the controls the configured driver `name` declares.
//
// The lookup goes name → configured lua path → catalog entry. A driver that
// is configured but whose file no longer parses returns nothing, which is the
// same answer as a driver that declares nothing. That is deliberate: a parse
// failure belongs in the catalog endpoint, where an operator is looking at
// driver files, not here.
func (s *Server) driverControls(name string) []drivers.CatalogControl {
	cfg, ok := s.configuredDriver(name)
	if !ok || cfg.Lua == "" {
		return nil
	}
	lua := cfg.Lua
	// Config.ResolveDriverPaths normally makes lua absolute. Read that exact
	// file first when it is available: a local overlay may contain the same
	// filename as a deliberately selected managed or bundled driver.
	if info, err := os.Stat(lua); err == nil && !info.IsDir() {
		entry, err := drivers.ParseCatalogFile(lua)
		if err != nil {
			return nil
		}
		return entry.Controls
	}

	dir := s.deps.DriverDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(s.deps.ConfigPath), "drivers")
	}
	entries, err := drivers.LoadCatalogMulti(s.deps.UserDriverDir, s.managedDriverDir(), dir)
	if err != nil {
		return nil
	}
	return drivers.ControlsForDriver(entries, lua)
}

func (s *Server) configuredDriver(name string) (config.Driver, bool) {
	if s.deps == nil || s.deps.Cfg == nil {
		return config.Driver{}, false
	}
	if s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
		defer s.deps.CfgMu.RUnlock()
	}
	for _, d := range s.deps.Cfg.Drivers {
		if d.Name == name {
			return d, true
		}
	}
	return config.Driver{}, false
}

// POST /api/drivers/test — start one short-lived driver instance from the
// posted config, wait briefly for telemetry, and return whatever live values
// it emitted. This lets Settings validate an unsaved driver without writing it
// into config.yaml or disturbing the running registry.
func (s *Server) handleDriverTest(w http.ResponseWriter, r *http.Request) {
	var cfg config.Driver
	if err := readJSON(r, &cfg); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid driver config: " + err.Error()})
		return
	}
	if strings.TrimSpace(cfg.Lua) == "" {
		writeJSON(w, 400, map[string]string{"error": "missing driver lua path"})
		return
	}

	// Match /api/config save semantics: masked/empty secrets in the form are
	// restored from the live config before the probe runs.
	if s.deps.CfgMu != nil && s.deps.Cfg != nil {
		s.deps.CfgMu.RLock()
		existing := *s.deps.Cfg
		s.deps.CfgMu.RUnlock()
		wrapped := config.Config{Drivers: []config.Driver{cfg}}
		wrapped.PreserveMaskedSecrets(&existing)
		restoreDriverConfigSecrets(&wrapped, &existing, s.driverSecretKeys())
		cfg = wrapped.Drivers[0]
	}

	// Resolve UI-relative Lua paths the same way config.Load does.
	baseDir := "."
	if s.deps.ConfigPath != "" {
		baseDir = filepath.Dir(s.deps.ConfigPath)
	}
	resolved := config.Config{Drivers: []config.Driver{cfg}}
	resolved.ResolveDriverPaths(baseDir)
	cfg = resolved.Drivers[0]

	if err := rejectUnsafeProbeTargets(cfg); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if mq := cfg.EffectiveMQTT(); mq != nil {
		if mq.Port == 0 {
			mq.Port = 1883
		}
		if s.deps.DriverMQTTFactory == nil {
			writeJSON(w, 503, map[string]string{"error": "mqtt probe unavailable"})
			return
		}
	}
	if mb := cfg.EffectiveModbus(); mb != nil {
		if mb.Port == 0 {
			mb.Port = 502
		}
		if mb.UnitID == 0 {
			mb.UnitID = 1
		}
		if s.deps.DriverModbusFactory == nil {
			writeJSON(w, 503, map[string]string{"error": "modbus probe unavailable"})
			return
		}
	}

	displayName := strings.TrimSpace(cfg.Name)
	if displayName == "" {
		displayName = filepath.Base(cfg.Lua)
	}
	testName := "__test_" + safeProbeName(displayName) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	cfg.Name = testName
	if cfg.BatteryCapacityWh <= 0 {
		// Probe-only: let battery-capable drivers emit enough to prove the
		// connection even before the operator has entered nameplate capacity.
		cfg.BatteryCapacityWh = 1
	}

	tel := telemetry.NewStore()
	reg := drivers.NewRegistry(tel)
	reg.MQTTFactory = s.deps.DriverMQTTFactory
	reg.ModbusFactory = s.deps.DriverModbusFactory
	reg.ARPLookup = s.deps.DriverARPLookup

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	started := time.Now()
	if err := reg.AddProbe(ctx, cfg); err != nil {
		writeJSON(w, 200, driverProbeResp{
			Name:      displayName,
			OK:        false,
			Error:     err.Error(),
			ElapsedMs: time.Since(started).Milliseconds(),
		})
		return
	}
	defer reg.RemoveProbe(cfg.Name)

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()

	for {
		resp := collectDriverProbe(displayName, cfg.Name, tel, reg, started)
		if len(resp.Readings) > 0 || len(resp.Metrics) > 0 {
			resp.OK = true
			writeJSON(w, 200, resp)
			return
		}
		select {
		case <-ctx.Done():
			resp.Error = ctx.Err().Error()
			writeJSON(w, 200, resp)
			return
		case <-deadline.C:
			resp.Error = "no telemetry received within 8s"
			writeJSON(w, 200, resp)
			return
		case <-ticker.C:
		}
	}
}

// rejectUnsafeProbeTargets checks every host a driver test might dial:
// MQTT, Modbus, config.host / config.url, and HTTP/WS/TCP allowlists.
func rejectUnsafeProbeTargets(cfg config.Driver) error {
	if mq := cfg.EffectiveMQTT(); mq != nil {
		if err := rejectUnsafeProbeHost(mq.Host); err != nil {
			return fmt.Errorf("mqtt host: %w", err)
		}
	}
	if mb := cfg.EffectiveModbus(); mb != nil {
		if err := rejectUnsafeProbeHost(mb.Host); err != nil {
			return fmt.Errorf("modbus host: %w", err)
		}
	}
	if cfg.Config != nil {
		if h, ok := cfg.Config["host"].(string); ok && strings.TrimSpace(h) != "" {
			if err := rejectUnsafeProbeHost(h); err != nil {
				return fmt.Errorf("config.host: %w", err)
			}
		}
		if u, ok := cfg.Config["url"].(string); ok && strings.TrimSpace(u) != "" {
			if host := hostFromProbeURL(u); host != "" {
				if err := rejectUnsafeProbeHost(host); err != nil {
					return fmt.Errorf("config.url: %w", err)
				}
			}
		}
	}
	if httpCap := cfg.Capabilities.HTTP; httpCap != nil {
		for _, h := range httpCap.AllowedHosts {
			if strings.TrimSpace(h) == "" {
				continue
			}
			if err := rejectUnsafeProbeHost(hostFromAllowlistEntry(h)); err != nil {
				return fmt.Errorf("http allowlist: %w", err)
			}
		}
	}
	if ws := cfg.Capabilities.WebSocket; ws != nil {
		for _, h := range ws.AllowedHosts {
			if strings.TrimSpace(h) == "" {
				continue
			}
			if err := rejectUnsafeProbeHost(hostFromAllowlistEntry(h)); err != nil {
				return fmt.Errorf("websocket allowlist: %w", err)
			}
		}
	}
	if tcp := cfg.Capabilities.TCP; tcp != nil {
		for _, h := range tcp.AllowedHosts {
			if strings.TrimSpace(h) == "" {
				continue
			}
			if err := rejectUnsafeProbeHost(hostFromAllowlistEntry(h)); err != nil {
				return fmt.Errorf("tcp allowlist: %w", err)
			}
		}
	}
	return nil
}

func hostFromProbeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	return u.Hostname()
}

func hostFromAllowlistEntry(entry string) string {
	entry = strings.TrimSpace(entry)
	if host, _, err := net.SplitHostPort(entry); err == nil {
		return host
	}
	return entry
}

// rejectUnsafeProbeHost stops a driver test or fingerprint from dialing
// the box itself or link-local/metadata addresses. Hostnames such as
// zap.local still probe unless they resolve to a forbidden address.
func rejectUnsafeProbeHost(host string) error {
	host = strings.TrimSpace(host)
	if i := strings.Index(host, "%"); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		return fmt.Errorf("missing host")
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return fmt.Errorf("loopback, link-local and unspecified addresses are not permitted")
	}
	if ip := net.ParseIP(host); ip != nil {
		return rejectUnsafeProbeIP(ip)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if err := rejectUnsafeProbeIP(a.IP); err != nil {
			return err
		}
	}
	return nil
}

func rejectUnsafeProbeIP(ip net.IP) error {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return fmt.Errorf("loopback, link-local and unspecified addresses are not permitted")
	}
	return nil
}

func collectDriverProbe(displayName, runtimeName string, tel *telemetry.Store, reg *drivers.Registry, started time.Time) driverProbeResp {
	resp := driverProbeResp{Name: displayName, ElapsedMs: time.Since(started).Milliseconds()}
	if h := tel.DriverHealth(runtimeName); h != nil {
		resp.Health = h
		if h.LastError != "" {
			resp.Error = h.LastError
		}
	}
	for _, der := range telemetry.AllDerTypes() {
		rd := tel.Get(runtimeName, der)
		if rd == nil {
			continue
		}
		resp.Readings = append(resp.Readings, readingDTO{
			Type:      der.String(),
			RawW:      rd.RawW,
			SmoothedW: rd.SmoothedW,
			SoC:       rd.SoC,
			UpdatedAt: rd.UpdatedAt.UnixMilli(),
			Stale:     false,
		})
	}
	resp.Metrics = tel.LatestMetricsByDriver(runtimeName)
	sort.Slice(resp.Metrics, func(i, j int) bool { return resp.Metrics[i].Name < resp.Metrics[j].Name })
	if env := reg.Env(runtimeName); env != nil {
		make, sn, mac, ep := env.FullIdentity()
		resp.Identity = driverIdentityDTO{Make: make, SN: sn, MAC: mac, Endpoint: ep}
	}
	return resp
}

func safeProbeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		return "driver"
	}
	if len(s) > 48 {
		return s[:48]
	}
	return s
}

// GET /api/drivers/{name}/logs?limit=N — recent log lines for one
// driver, oldest first. Pulled from the in-memory ring buffer; nothing
// hits disk.
func (s *Server) handleDriverLogs(w http.ResponseWriter, r *http.Request) {
	if s.deps.LogRing == nil {
		writeJSON(w, 503, map[string]string{"error": "log ring not configured"})
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "missing driver name"})
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	entries := s.deps.LogRing.RecentByDriver(name, limit)
	writeJSON(w, 200, map[string]any{
		"driver":  name,
		"limit":   limit,
		"entries": entries,
	})
}

// GET /api/logs?limit=N — global log ring (control loop, MPC, HA,
// etc., plus all driver lines). Same shape as the per-driver endpoint.
func (s *Server) handleGlobalLogs(w http.ResponseWriter, r *http.Request) {
	if s.deps.LogRing == nil {
		writeJSON(w, 503, map[string]string{"error": "log ring not configured"})
		return
	}
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	entries := s.deps.LogRing.RecentGlobal(limit)
	writeJSON(w, 200, map[string]any{
		"limit":   limit,
		"entries": entries,
	})
}

// GET /api/support/dump — zip archive with everything a developer needs
// to triage a support incident: redacted config, full driver health JSON,
// recent global + per-driver logs (secrets stripped at least as strictly
// as Ask why), last 1 h of TS samples per (driver, metric), and a
// manifest. SQLite is NOT included; the dump is intended to be small
// enough to attach to a chat message — measured at ~6 kB on a two-driver
// install.
//
// Zip rather than tar.gz because this file's whole purpose is to be
// handed to somebody else. Windows and every chat client open a zip
// without a second tool; a .tar.gz asks the person you need help from to
// go find one first.
func (s *Server) handleSupportDump(w http.ResponseWriter, r *http.Request) {
	if s.deps.LogRing == nil {
		writeJSON(w, 503, map[string]string{"error": "log ring not configured"})
		return
	}
	now := time.Now().UTC()
	stamp := now.Format("20060102-150405")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="ftw-support-`+stamp+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")

	zw := zip.NewWriter(w)
	defer zw.Close()

	addFile := func(name string, body []byte) {
		hdr := &zip.FileHeader{
			Name:     "ftw-support-" + stamp + "/" + name,
			Method:   zip.Deflate,
			Modified: now,
		}
		hdr.SetMode(0o644)
		f, err := zw.CreateHeader(hdr)
		if err != nil {
			return
		}
		_, _ = f.Write(body)
	}

	// The help report goes in first and is named to sort first, because it
	// is the only file in here most recipients need to read. Everything
	// below it is what you reach for when the report does not settle the
	// question. Shipping them together means the person asking for help
	// sends one file and never has to pick the right one.
	addFile("00-help-report.md", []byte(s.buildSupportReport(r.Context(), time.Now())))

	manifest := map[string]any{
		"generated_at": now.Format(time.RFC3339),
		"version":      s.deps.Version,
		"go_version":   runtime.Version(),
		"goos":         runtime.GOOS,
		"goarch":       runtime.GOARCH,
		"hostname":     hostnameOrEmpty(),
		"read_first":   "00-help-report.md",
		"contents": []string{
			"00-help-report.md",
			"config.redacted.yaml",
			"drivers.json",
			"logs/global.log",
			"logs/<driver>.log",
			"telemetry/<driver>__<metric>.csv",
		},
	}
	manifestBody, _ := json.MarshalIndent(manifest, "", "  ")
	addFile("manifest.json", manifestBody)

	// Redacted config — strip MQTT/HTTP passwords, EV charger creds, Nova
	// signing key paths, etc. Keeps the structure visible without leaking
	// secrets.
	if cfgBytes := s.redactedConfig(); cfgBytes != nil {
		addFile("config.redacted.yaml", cfgBytes)
	}

	// All driver health in one file — easier to scan than one-per-driver.
	healthBody, _ := json.MarshalIndent(s.deps.Tel.AllHealth(), "", "  ")
	addFile("drivers.json", healthBody)

	// Logs. Lua host.log and HTTP error bodies often carry OAuth JSON;
	// redact before the zip leaves the house.
	globalLog := formatLogs(s.deps.LogRing.RecentGlobal(0))
	addFile("logs/global.log", []byte(globalLog))
	for _, d := range s.deps.LogRing.Drivers() {
		entries := s.deps.LogRing.RecentByDriver(d, 0)
		if len(entries) == 0 {
			continue
		}
		addFile("logs/"+sanitizeName(d)+".log", []byte(formatLogs(entries)))
	}

	// Last 1 h of TS samples per (driver, metric) — small enough to
	// keep the bundle email-able. Only metrics the operator actually
	// emitted in this window appear.
	if s.deps.State != nil {
		untilMs := now.UnixMilli()
		sinceMs := untilMs - 3600*1000
		drivers, _ := s.deps.State.DriverNames()
		metrics, _ := s.deps.State.MetricNames()
		for _, d := range drivers {
			for _, m := range metrics {
				series, err := s.deps.State.LoadSeries(d, m, sinceMs, untilMs, 4096)
				if err != nil || len(series) == 0 {
					continue
				}
				var b strings.Builder
				b.WriteString("ts_ms,value\n")
				for _, p := range series {
					fmt.Fprintf(&b, "%d,%g\n", p.TsMs, p.Value)
				}
				addFile("telemetry/"+sanitizeName(d)+"__"+sanitizeName(m)+".csv", []byte(b.String()))
			}
		}
	}
}

// ---- helpers ----

func hostnameOrEmpty() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}

// sanitizeName replaces filesystem-unfriendly chars in a driver/metric
// name. Driver names today are simple identifiers, but Lua authors are
// free with naming and we'd rather not produce a tarball that won't
// extract on Windows.
func sanitizeName(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

func formatLogs(entries []telemetry.LogEntry) string {
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s %s ", e.TS.UTC().Format(time.RFC3339Nano), e.Level)
		if e.Driver != "" {
			fmt.Fprintf(&b, "[%s] ", e.Driver)
		}
		b.WriteString(redactDumpLog(e.Msg))
		if e.Attrs != "" {
			b.WriteByte(' ')
			b.WriteString(redactDumpLog(e.Attrs))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// redactedConfig returns the in-memory config marshalled to JSON with a
// flat list of sensitive paths replaced by "***". YAML is harder to edit
// in-flight; JSON is good-enough for triage and unambiguous about which
// fields were redacted.
func (s *Server) redactedConfig() []byte {
	if s.deps.Cfg == nil {
		return nil
	}
	s.deps.CfgMu.RLock()
	cfgBytes, err := json.Marshal(s.deps.Cfg)
	s.deps.CfgMu.RUnlock()
	if err != nil {
		return nil
	}
	var asMap any
	if err := json.Unmarshal(cfgBytes, &asMap); err != nil {
		return cfgBytes
	}
	redactSensitive(asMap)
	pretty, err := json.MarshalIndent(asMap, "", "  ")
	if err != nil {
		return cfgBytes
	}
	return pretty
}

// redactSensitive walks an unmarshalled JSON tree and replaces any value
// at a key whose lowercased name matches a sensitive substring. Recursive,
// in-place.
func redactSensitive(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			lk := strings.ToLower(k)
			if isSensitiveKey(lk) {
				x[k] = "***"
				continue
			}
			redactSensitive(val)
		}
	case []any:
		for _, e := range x {
			redactSensitive(e)
		}
	}
}

func isSensitiveKey(k string) bool {
	k = strings.ToLower(strings.TrimSpace(k))
	if k == "" {
		return false
	}
	for _, s := range []string{
		"password", "passwd", "secret", "token", "api_key", "apikey",
		"private_key", "client_secret", "authorization", "credential",
	} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return hasAuthSegment(k)
}

// hasAuthSegment is true for a bare "auth" path segment so auth,
// auth_header and x-auth redact, while oauth and oauth_client_id do not.
func hasAuthSegment(k string) bool {
	start := 0
	for i := 0; i <= len(k); i++ {
		if i < len(k) && ((k[i] >= 'a' && k[i] <= 'z') || (k[i] >= '0' && k[i] <= '9')) {
			continue
		}
		if i-start == 4 && k[start:i] == "auth" {
			return true
		}
		start = i + 1
	}
	return false
}

var (
	dumpJSONSecretRe = regexp.MustCompile(`(?i)("([^"\\]+)"\s*:\s*)("(?:\\.|[^"\\])*")`)
	dumpFormSecretRe = regexp.MustCompile(`(?i)\b([A-Za-z][A-Za-z0-9_-]*)=([^\s&"]+)`)
	dumpBasicAuthRe  = regexp.MustCompile(`(?i)basic\s+[A-Za-z0-9+/=_-]+`)
)

// redactDumpLog applies Ask-why redaction, then also blanks JSON/form
// fields whose keys isSensitiveKey would catch. Stricter on OAuth bodies
// that Lua logs as `HTTP %d: %s`.
func redactDumpLog(s string) string {
	if s == "" {
		return s
	}
	s = assistant.Redact(s)
	s = dumpBasicAuthRe.ReplaceAllString(s, "Basic [omitted]")
	s = dumpJSONSecretRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := dumpJSONSecretRe.FindStringSubmatch(m)
		if len(parts) != 4 || !isSensitiveKey(parts[2]) {
			return m
		}
		return parts[1] + `"***"`
	})
	s = dumpFormSecretRe.ReplaceAllStringFunc(s, func(m string) string {
		parts := dumpFormSecretRe.FindStringSubmatch(m)
		if len(parts) != 3 || !isSensitiveKey(parts[1]) {
			return m
		}
		return parts[1] + "=***"
	})
	return s
}
