package api

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

const (
	loadResearchFormatVersion = 1
	loadResearchSiteIDKey     = "research/site_id"
	loadResearchBucketMin     = 15
)

type loadResearchBucket struct {
	startMs int64
	count   int

	gridW         float64
	pvW           float64
	batW          float64
	evW           float64
	houseLoadW    float64
	recordedLoadW float64
	batSoC        float64
}

// GET /api/research/load/dump?days=120
//
// Produces an anonymized, model-oriented tarball: no logs, no raw config, no
// hostnames, no driver names. The CSV is 15-minute site aggregates with EV
// split out so offline analysis can compare "whole-site load" vs house-only
// load using the same convention as loadmodel.Service.sample.
func (s *Server) handleLoadResearchDump(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "state store not configured"})
		return
	}

	days := 120
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 365 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "days must be in [1, 365]"})
			return
		}
		days = n
	}

	now := time.Now().UTC()
	untilMs := now.UnixMilli()
	sinceMs := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	history, err := s.deps.State.LoadHistory(sinceMs, untilMs, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	forecasts, _ := s.deps.State.LoadForecasts(sinceMs, untilMs)
	prices := []state.PricePoint{}
	priceZone := s.priceZone()
	if priceZone != "" {
		prices, _ = s.deps.State.LoadPrices(priceZone, sinceMs, untilMs)
	}

	buckets := buildLoadResearchBuckets(history)
	stamp := now.Format("20060102-150405")
	root := "ftw-load-research-" + stamp

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+root+`.tar.gz"`)
	w.Header().Set("Cache-Control", "no-store")

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	addFile := func(name string, body []byte) {
		hdr := &tar.Header{
			Name:    root + "/" + name,
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: now,
		}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write(body)
	}

	manifest := map[string]any{
		"format":         "forty-two-watts-load-research",
		"format_version": loadResearchFormatVersion,
		"generated_at":   now.Format(time.RFC3339),
		"version":        s.deps.Version,
		"site_id":        s.researchSiteID(),
		"since_ms":       sinceMs,
		"until_ms":       untilMs,
		"days":           days,
		"bucket_min":     loadResearchBucketMin,
		"timezone":       time.Local.String(),
		"privacy":        "no logs, no raw config, no hostnames, no driver names, no device identifiers",
		"load_semantics": "house_load_w = max(grid_w - pv_w - bat_w - ev_w, 0); site convention: import/charge positive, PV/discharge negative",
		"contents": []string{
			"manifest.json",
			"site.json",
			"timeseries_15m.csv",
			"loadmodel_state.json",
		},
	}
	manifestBody, _ := json.MarshalIndent(manifest, "", "  ")
	addFile("manifest.json", manifestBody)

	siteBody, _ := json.MarshalIndent(s.researchSiteSummary(), "", "  ")
	addFile("site.json", siteBody)
	addFile("timeseries_15m.csv", []byte(formatLoadResearchCSV(buckets, forecasts, prices)))

	if s.deps.LoadModel != nil {
		modelBody, _ := json.MarshalIndent(s.deps.LoadModel.Snapshot(), "", "  ")
		addFile("loadmodel_state.json", modelBody)
	} else {
		addFile("loadmodel_state.json", []byte("{}\n"))
	}
}

func buildLoadResearchBuckets(rows []state.HistoryPoint) []loadResearchBucket {
	const bucketMs = int64(loadResearchBucketMin * 60 * 1000)
	byStart := make(map[int64]*loadResearchBucket)
	for _, row := range rows {
		start := (row.TsMs / bucketMs) * bucketMs
		b := byStart[start]
		if b == nil {
			b = &loadResearchBucket{startMs: start}
			byStart[start] = b
		}
		evW := historyEVW(row.JSON)
		houseLoadW := row.GridW - row.PVW - row.BatW - evW
		if houseLoadW < 0 {
			houseLoadW = 0
		}
		b.count++
		b.gridW += row.GridW
		b.pvW += row.PVW
		b.batW += row.BatW
		b.evW += evW
		b.houseLoadW += houseLoadW
		b.recordedLoadW += row.LoadW
		b.batSoC += row.BatSoC
	}

	out := make([]loadResearchBucket, 0, len(byStart))
	for _, b := range byStart {
		if b.count <= 0 {
			continue
		}
		n := float64(b.count)
		out = append(out, loadResearchBucket{
			startMs:       b.startMs,
			count:         b.count,
			gridW:         b.gridW / n,
			pvW:           b.pvW / n,
			batW:          b.batW / n,
			evW:           b.evW / n,
			houseLoadW:    b.houseLoadW / n,
			recordedLoadW: b.recordedLoadW / n,
			batSoC:        b.batSoC / n,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].startMs < out[j].startMs })
	return out
}

func formatLoadResearchCSV(buckets []loadResearchBucket, forecasts []state.ForecastPoint, prices []state.PricePoint) string {
	var b strings.Builder
	b.WriteString("bucket_start_ms,bucket_end_ms,local_weekday,local_hour,local_minute,n_samples,grid_w,pv_w,bat_w,ev_w,house_load_w,recorded_load_w,bat_soc,temp_c,cloud_pct,total_ore_kwh,spot_ore_kwh\n")
	for _, row := range buckets {
		start := time.UnixMilli(row.startMs)
		local := start.In(time.Local)
		midMs := row.startMs + int64(loadResearchBucketMin)*60*1000/2
		tempC, tempOK, cloudPct, cloudOK := forecastAt(forecasts, midMs)
		totalOre, totalOK, spotOre, spotOK := priceAt(prices, midMs)
		fmt.Fprintf(&b, "%d,%d,%d,%d,%d,%d,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n",
			row.startMs,
			row.startMs+int64(loadResearchBucketMin)*60*1000,
			int(local.Weekday()),
			local.Hour(),
			local.Minute(),
			row.count,
			researchFloat(row.gridW, true),
			researchFloat(row.pvW, true),
			researchFloat(row.batW, true),
			researchFloat(row.evW, true),
			researchFloat(row.houseLoadW, true),
			researchFloat(row.recordedLoadW, true),
			researchFloat(row.batSoC, true),
			researchFloat(tempC, tempOK),
			researchFloat(cloudPct, cloudOK),
			researchFloat(totalOre, totalOK),
			researchFloat(spotOre, spotOK),
		)
	}
	return b.String()
}

func historyEVW(js string) float64 {
	if js == "" {
		return 0
	}
	var payload struct {
		EVW     *float64                      `json:"ev_w"`
		Drivers map[string]map[string]float64 `json:"drivers"`
	}
	if err := json.Unmarshal([]byte(js), &payload); err != nil {
		return 0
	}
	if payload.EVW != nil {
		return *payload.EVW
	}
	var evW float64
	for _, vals := range payload.Drivers {
		evW += vals["ev_w"]
	}
	if math.Abs(evW) < 1 {
		return 0
	}
	return evW
}

func forecastAt(rows []state.ForecastPoint, tsMs int64) (tempC float64, tempOK bool, cloudPct float64, cloudOK bool) {
	for _, r := range rows {
		slotLen := r.SlotLenMin
		if slotLen <= 0 {
			slotLen = 60
		}
		end := r.SlotTsMs + int64(slotLen)*60*1000
		if tsMs < r.SlotTsMs || tsMs >= end {
			continue
		}
		if r.TempC != nil {
			tempC, tempOK = *r.TempC, true
		}
		if r.CloudCoverPct != nil {
			cloudPct, cloudOK = *r.CloudCoverPct, true
		}
		return tempC, tempOK, cloudPct, cloudOK
	}
	return 0, false, 0, false
}

func priceAt(rows []state.PricePoint, tsMs int64) (total float64, totalOK bool, spot float64, spotOK bool) {
	for _, r := range rows {
		slotLen := r.SlotLenMin
		if slotLen <= 0 {
			slotLen = 60
		}
		end := r.SlotTsMs + int64(slotLen)*60*1000
		if tsMs < r.SlotTsMs || tsMs >= end {
			continue
		}
		return r.TotalOreKwh, true, r.SpotOreKwh, true
	}
	return 0, false, 0, false
}

func researchFloat(v float64, ok bool) string {
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func (s *Server) researchSiteID() string {
	if s.deps.State != nil {
		if id, ok := s.deps.State.LoadConfig(loadResearchSiteIDKey); ok && id != "" {
			return id
		}
	}
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "ephemeral-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	id := hex.EncodeToString(buf[:])
	if s.deps.State != nil {
		_ = s.deps.State.SaveConfig(loadResearchSiteIDKey, id)
	}
	return id
}

func (s *Server) priceZone() string {
	if s == nil || s.deps.Cfg == nil {
		return ""
	}
	if s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
		defer s.deps.CfgMu.RUnlock()
	}
	if s.deps.Cfg.Price == nil {
		return ""
	}
	return s.deps.Cfg.Price.Zone
}

func (s *Server) researchSiteSummary() map[string]any {
	out := map[string]any{}
	if s == nil || s.deps.Cfg == nil {
		return out
	}
	if s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
		defer s.deps.CfgMu.RUnlock()
	}
	cfg := s.deps.Cfg
	out["fuse_max_amps"] = cfg.Fuse.MaxAmps
	out["fuse_phases"] = cfg.Fuse.Phases
	out["fuse_voltage"] = cfg.Fuse.Voltage
	out["fuse_max_power_w"] = cfg.Fuse.MaxPowerW()
	out["driver_count"] = len(cfg.Drivers)
	out["loadpoint_count"] = len(cfg.Loadpoints)
	out["has_ev"] = len(cfg.Loadpoints) > 0 || cfg.EVCharger != nil || (cfg.OCPP != nil && cfg.OCPP.Enabled)

	var batteryWh float64
	loadpointDrivers := make(map[string]bool, len(cfg.Loadpoints))
	for _, lp := range cfg.Loadpoints {
		loadpointDrivers[lp.DriverName] = true
	}
	for _, d := range cfg.Drivers {
		if !loadpointDrivers[d.Name] {
			batteryWh += d.BatteryCapacityWh
		}
	}
	out["battery_capacity_wh_total"] = batteryWh
	if cfg.Price != nil {
		out["price_zone"] = cfg.Price.Zone
		out["price_provider"] = cfg.Price.Provider
	}
	if cfg.Weather != nil {
		out["weather_provider"] = cfg.Weather.Provider
		out["pv_rated_w"] = cfg.Weather.PVRatedW
		out["heating_w_per_degc"] = cfg.Weather.HeatingWPerDegC
		out["pv_array_count"] = len(cfg.Weather.PVArrays)
	}
	if cfg.Planner != nil {
		out["planner_enabled"] = cfg.Planner.Enabled
		out["planner_mode"] = cfg.Planner.Mode
	}
	return out
}
