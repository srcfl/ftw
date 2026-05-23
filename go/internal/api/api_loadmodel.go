package api

import (
	"net/http"

	"github.com/frahlg/forty-two-watts/go/internal/loadmodel"
)

type loadModelStats struct {
	Samples         int64   `json:"samples"`
	MAEW            float64 `json:"mae_w"`
	PeakW           float64 `json:"peak_w"`
	Quality         float64 `json:"quality"`
	LastMs          int64   `json:"last_ms"`
	HeatingWPerDegC float64 `json:"heating_w_per_degc"`
	BucketsWarm     int     `json:"buckets_warm"`
	BucketsTotal    int     `json:"buckets_total"`
}

func (s *Server) handleLoadModel(w http.ResponseWriter, r *http.Request) {
	if s.deps.LoadModel == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	snap := s.deps.LoadModel.Snapshot()
	active := snap.Profiles[snap.ActiveProfile]
	profiles := make(map[loadmodel.Profile]loadModelStats, len(snap.Profiles))
	for _, profile := range loadmodel.Profiles() {
		if m, ok := snap.Profiles[profile]; ok {
			profiles[profile] = loadModelStatsFrom(m)
		}
	}
	stats := loadModelStatsFrom(active)
	writeJSON(w, 200, map[string]any{
		"enabled":            true,
		"profile":            snap.ActiveProfile,
		"active_profile":     snap.ActiveProfile,
		"profiles":           profiles,
		"samples":            stats.Samples,
		"mae_w":              stats.MAEW,
		"peak_w":             stats.PeakW,
		"quality":            stats.Quality,
		"last_ms":            stats.LastMs,
		"heating_w_per_degc": stats.HeatingWPerDegC,
		"buckets_warm":       stats.BucketsWarm,
		"buckets_total":      stats.BucketsTotal,
	})
}

func (s *Server) handleLoadModelProfile(w http.ResponseWriter, r *http.Request) {
	if s.deps.LoadModel == nil {
		writeJSON(w, 400, map[string]string{"error": "loadmodel disabled"})
		return
	}
	var req struct {
		Profile string `json:"profile"`
		Mode    string `json:"mode"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	raw := req.Profile
	if raw == "" {
		raw = req.Mode
	}
	profile, ok := loadmodel.ParseProfile(raw)
	if !ok {
		writeJSON(w, 400, map[string]string{"error": "unknown load profile: " + raw})
		return
	}
	if err := s.deps.LoadModel.SetProfile(profile); err != nil {
		writeJSON(w, 500, map[string]string{"error": "persist failed: " + err.Error()})
		return
	}
	if s.deps.MPC != nil {
		s.deps.MPC.ReplanWithReason(r.Context(), "load_profile_changed")
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "profile": profile})
}

func (s *Server) handleLoadModelReset(w http.ResponseWriter, r *http.Request) {
	if s.deps.LoadModel == nil {
		writeJSON(w, 400, map[string]string{"error": "loadmodel disabled"})
		return
	}
	profile := s.deps.LoadModel.Profile()
	s.deps.LoadModel.Reset()
	if s.deps.MPC != nil {
		s.deps.MPC.ReplanWithReason(r.Context(), "load_profile_reset")
	}
	writeJSON(w, 200, map[string]any{"status": "reset", "profile": profile})
}

func loadModelStatsFrom(m loadmodel.Model) loadModelStats {
	warm := 0
	for i := 0; i < loadmodel.Buckets; i++ {
		if m.Bucket[i].Samples >= loadmodel.MinTrustSamples {
			warm++
		}
	}
	return loadModelStats{
		Samples:         m.Samples,
		MAEW:            m.MAE,
		PeakW:           m.PeakW,
		Quality:         m.Quality(),
		LastMs:          m.LastMs,
		HeatingWPerDegC: m.HeatingW_per_degC,
		BucketsWarm:     warm,
		BucketsTotal:    loadmodel.Buckets,
	}
}
