// Driver-debug surface: per-driver detail (health + last reading + live
// metric snapshots), recent log lines from the in-memory ring, and a
// gzipped support bundle for sending to the developers.
//
// Wired in main.go via Deps.LogRing. Without a LogRing the log + dump
// endpoints return 503; the detail endpoint still works (no logs).
package api

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

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
	writeJSON(w, 200, resp)
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
	if err := reg.Add(ctx, cfg); err != nil {
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

// GET /api/support/dump — gzipped tarball with everything a developer
// needs to triage a remote incident: redacted config, full driver
// health JSON, recent global + per-driver logs, last 1 h of TS samples
// per (driver, metric), and a manifest. SQLite is NOT included; the
// dump is intended to be small enough to email or paste-link.
func (s *Server) handleSupportDump(w http.ResponseWriter, r *http.Request) {
	if s.deps.LogRing == nil {
		writeJSON(w, 503, map[string]string{"error": "log ring not configured"})
		return
	}
	now := time.Now().UTC()
	stamp := now.Format("20060102-150405")
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="ftw-support-`+stamp+`.tar.gz"`)
	w.Header().Set("Cache-Control", "no-store")

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	addFile := func(name string, body []byte) {
		hdr := &tar.Header{
			Name:    "ftw-support-" + stamp + "/" + name,
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: now,
		}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write(body)
	}

	// Manifest first so a curious recipient can `tar -xOzf … manifest.json`
	// and see what they've got without unpacking the whole bundle.
	manifest := map[string]any{
		"generated_at": now.Format(time.RFC3339),
		"version":      s.deps.Version,
		"go_version":   runtime.Version(),
		"goos":         runtime.GOOS,
		"goarch":       runtime.GOARCH,
		"hostname":     hostnameOrEmpty(),
		"contents": []string{
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

	// Logs.
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
		b.WriteString(e.Msg)
		if e.Attrs != "" {
			b.WriteByte(' ')
			b.WriteString(e.Attrs)
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
	keys := []string{"password", "secret", "token", "api_key", "apikey", "private_key", "client_secret"}
	for _, s := range keys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}
