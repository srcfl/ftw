package api

import (
	"net/http"

	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/energyreport"
)

// What this box would add to FTW's daily fleet totals.
//
// The endpoint exists so the Settings tab can render the real message rather
// than a paragraph promising what the message contains. It is built by the
// same call the sender uses, so the two cannot drift: if the payload ever
// grew a field it should not have, this screen would show it.

type fleetPingView struct {
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"`
	// FTWRelay lets Settings state FTW's retention rules only when the running
	// process is using the endpoint those rules govern.
	FTWRelay bool `json:"ftw_relay"`
	// ThroughRelay is true when the report goes to FTW Cloud through the
	// Oblivious HTTP relay, so FTW Cloud never sees the box's address.
	ThroughRelay bool `json:"through_relay"`
	// WaitsForRelay is a box that shares by default, never having said yes:
	// its report goes to FTW Cloud only through the relay, and this build
	// names none, so it waits.
	WaitsForRelay bool `json:"waits_for_relay"`
	// Payload is the whole message, exactly as it would be posted.
	Payload any `json:"payload"`
	// Energy is the daily energy report under the same answer, built by the
	// call its sender uses. Absent on a box without one.
	Energy *energyreport.Preview `json:"energy,omitempty"`
}

func (s *Server) handleFleetPing(w http.ResponseWriter, r *http.Request) {
	if s.deps.FleetPing == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the fleet ping is not running on this box",
		})
		return
	}

	// Enabled is whether the next report goes, read the way the sender reads
	// it: the box's one statistics answer, and no direct post to FTW Cloud
	// from a box that never said yes. The endpoint is fixed when the pinger
	// starts. Report that active endpoint rather than a newly saved value
	// that needs a restart before it can be used.
	enabled := s.deps.FleetPing.Enabled()

	endpoint := s.deps.FleetPing.Endpoint()
	view := fleetPingView{
		Enabled:      enabled,
		Endpoint:     endpoint,
		FTWRelay:     endpoint == config.DefaultFleetPingEndpoint || s.deps.FleetPing.ThroughRelay(),
		ThroughRelay: s.deps.FleetPing.ThroughRelay(),
		Payload:      s.deps.FleetPing.Payload(),
	}
	shares := s.deps.State != nil && cloudlink.SharesStatistics(s.deps.State)
	if s.deps.Cloud != nil {
		shares = s.deps.Cloud.Status().Statistics
	}
	view.WaitsForRelay = !enabled && !view.ThroughRelay && endpoint == config.DefaultFleetPingEndpoint && shares
	if s.deps.EnergyReport != nil {
		p := s.deps.EnergyReport.Preview(r.Context())
		view.Energy = &p
	}
	writeJSON(w, http.StatusOK, view)
}
