package api

import (
	"net/http"
	"sort"

	"github.com/srcfl/ftw/go/internal/ocpp"
)

// ocppChargerEntry is one charge point in the /api/ocpp/chargers response.
// The embedded view flattens, so the JSON reads as one object per charger.
type ocppChargerEntry struct {
	ID string `json:"id"`
	ocpp.ChargerView
}

// handleOCPPChargers reports the OCPP central system's effective config and
// every charge point it currently knows, for the Settings → Chargers panel.
// OCPP being disabled is a normal state, not an error: enabled=false and an
// empty list, so the UI can render its "how to enable" text from a 200.
func (s *Server) handleOCPPChargers(w http.ResponseWriter, r *http.Request) {
	resp := struct {
		Enabled  bool               `json:"enabled"`
		Port     int                `json:"port,omitempty"`
		PortV201 int                `json:"port_v201,omitempty"`
		Path     string             `json:"path,omitempty"`
		Chargers []ocppChargerEntry `json:"chargers"`
	}{Chargers: []ocppChargerEntry{}}

	if s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
	}
	if s.deps.Cfg != nil && s.deps.Cfg.OCPP != nil && s.deps.Cfg.OCPP.Enabled {
		resp.Enabled = true
		// Mirror ocpp.Config.Defaults so the UI shows the port the listener
		// actually took, not the zero the operator left unset.
		resp.Port = s.deps.Cfg.OCPP.Port
		if resp.Port == 0 {
			resp.Port = 8887
		}
		resp.PortV201 = s.deps.Cfg.OCPP.PortV201
		resp.Path = s.deps.Cfg.OCPP.Path
		if resp.Path == "" {
			resp.Path = "/"
		}
	}
	if s.deps.CfgMu != nil {
		s.deps.CfgMu.RUnlock()
	}

	if s.deps.OCPPChargers != nil {
		snap := s.deps.OCPPChargers()
		ids := make([]string, 0, len(snap))
		for id := range snap {
			ids = append(ids, id)
		}
		// Stable order so the panel does not shuffle between refreshes.
		sort.Strings(ids)
		for _, id := range ids {
			resp.Chargers = append(resp.Chargers, ocppChargerEntry{ID: id, ChargerView: snap[id]})
		}
	}
	writeJSON(w, 200, resp)
}
