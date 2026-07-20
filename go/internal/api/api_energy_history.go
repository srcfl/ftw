package api

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

const (
	energyHistoryDefaultWindowMS = int64(24 * time.Hour / time.Millisecond)
	energyHistoryMaxWindowMS     = int64(2 * 365 * 24 * time.Hour / time.Millisecond)
	energyHistoryDefaultLimit    = 2000
	energyHistoryMaxLimit        = 5000
	energyHistoryMaxBuckets      = 2000
)

func (s *Server) handleEnergyAssets(w http.ResponseWriter, _ *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "energy history unavailable"})
		return
	}
	assets, err := s.deps.State.EnergyAssets()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": state.EnergyLedgerSchemaVersion,
		"assets":         assets,
	})
}

func (s *Server) handleEnergyHistory(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "energy history unavailable"})
		return
	}
	q, err := parseEnergyHistoryQuery(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	points, truncated, err := s.deps.State.LoadEnergyHistory(q)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	scope := "system"
	if q.AssetID != "" {
		scope = "asset"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version":          state.EnergyLedgerSchemaVersion,
		"scope":                   scope,
		"asset_id":                q.AssetID,
		"since_ms":                q.SinceMS,
		"until_ms":                q.UntilMS,
		"requested_bucket_len_ms": q.BucketMS,
		"limit_unit":              "buckets",
		"truncated":               truncated,
		"points":                  points,
	})
}

func (s *Server) handleEnergyHistoryCSV(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		http.Error(w, "energy history unavailable", http.StatusServiceUnavailable)
		return
	}
	q, err := parseEnergyHistoryQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	points, truncated, err := s.deps.State.LoadEnergyHistory(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ftw-energy-history.csv"`)
	w.Header().Set("X-FTW-History-Limit-Unit", "buckets")
	if truncated {
		w.Header().Set("X-FTW-History-Truncated", "true")
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"schema_version", "asset_id", "flow", "bucket_start_ms", "bucket_len_ms",
		"energy_wh", "source", "quality", "provenance", "sample_count",
	})
	for _, p := range points {
		_ = cw.Write([]string{
			strconv.Itoa(p.SchemaVersion), p.AssetID, string(p.Flow),
			strconv.FormatInt(p.BucketStartMS, 10), strconv.FormatInt(p.BucketLenMS, 10),
			strconv.FormatFloat(p.EnergyWh, 'f', -1, 64), p.Source, p.Quality, p.Provenance,
			strconv.FormatInt(p.SampleCount, 10),
		})
	}
	cw.Flush()
}

func parseEnergyHistoryQuery(r *http.Request) (state.EnergyHistoryQuery, error) {
	nowMS := time.Now().UnixMilli()
	untilMS := nowMS
	if raw := r.URL.Query().Get("until"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return state.EnergyHistoryQuery{}, fmt.Errorf("until must be Unix milliseconds")
		}
		untilMS = parsed
		if untilMS > nowMS {
			untilMS = nowMS
		}
	}
	sinceMS := untilMS - energyHistoryDefaultWindowMS
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			return state.EnergyHistoryQuery{}, fmt.Errorf("since must be Unix milliseconds")
		}
		sinceMS = parsed
	}
	if sinceMS >= untilMS {
		return state.EnergyHistoryQuery{}, fmt.Errorf("since must be before until")
	}
	if untilMS-sinceMS > energyHistoryMaxWindowMS {
		return state.EnergyHistoryQuery{}, fmt.Errorf("history range exceeds two years")
	}

	bucketMS := int64(15 * time.Minute / time.Millisecond)
	if raw := r.URL.Query().Get("bucket"); raw != "" {
		bucketMS = parseRange(raw)
		if bucketMS == 5*60*1000 && raw != "5m" {
			return state.EnergyHistoryQuery{}, fmt.Errorf("invalid bucket")
		}
	}
	if bucketMS < state.EnergyLedgerBucketMS {
		return state.EnergyHistoryQuery{}, fmt.Errorf("bucket must be at least 5m")
	}
	if (untilMS-sinceMS+bucketMS-1)/bucketMS > energyHistoryMaxBuckets {
		return state.EnergyHistoryQuery{}, fmt.Errorf("bucket is too small for the requested range")
	}

	limit := energyHistoryDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			return state.EnergyHistoryQuery{}, fmt.Errorf("limit must be a positive integer")
		}
		limit = parsed
	}
	if limit > energyHistoryMaxLimit {
		limit = energyHistoryMaxLimit
	}

	assetID := r.URL.Query().Get("asset_id")
	if len(assetID) > 512 {
		return state.EnergyHistoryQuery{}, fmt.Errorf("asset_id is too long")
	}
	scope := r.URL.Query().Get("scope")
	if scope == "asset" && assetID == "" {
		return state.EnergyHistoryQuery{}, fmt.Errorf("asset scope requires asset_id")
	}
	if scope != "" && scope != "system" && scope != "asset" {
		return state.EnergyHistoryQuery{}, fmt.Errorf("scope must be system or asset")
	}
	if scope == "system" {
		assetID = ""
	}
	return state.EnergyHistoryQuery{
		AssetID: assetID, SinceMS: sinceMS, UntilMS: untilMS,
		BucketMS: bucketMS, Limit: limit,
	}, nil
}
