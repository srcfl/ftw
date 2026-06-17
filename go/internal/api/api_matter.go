package api

import "net/http"

// Matter sidecar admin endpoints — the one-time pairing-code join and
// node listing. Per drivers/matter.lua's onboarding flow: a device is
// commissioned by whatever controller it shipped with, then "shared"
// into 42W via that controller's multi-admin flow, which mints a pairing
// code. POST /api/matter/commission joins it and hands back the small
// logical node_id to paste into the driver's config.node_id field.
//
// Per the api/CLAUDE.md split convention, this lives in its own file and
// is registered via routes() in api.go.

type matterCommissionRequest struct {
	PairingCode string `json:"pairing_code"`
}

type matterCommissionResponse struct {
	NodeID int `json:"node_id"`
}

// handleMatterCommission joins a shared device using a pairing code minted
// by its original controller.
func (s *Server) handleMatterCommission(w http.ResponseWriter, r *http.Request) {
	if s.deps.Matter == nil {
		writeJSON(w, 503, map[string]string{"error": "matter sidecar not configured"})
		return
	}
	var req matterCommissionRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.PairingCode == "" {
		writeJSON(w, 400, map[string]string{"error": "pairing_code is required"})
		return
	}
	nodeID, err := s.deps.Matter.Commission(req.PairingCode)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, matterCommissionResponse{NodeID: nodeID})
}

// handleMatterNodes lists every node the sidecar has joined so far, so an
// operator can confirm a node_id before pasting it into driver config.
func (s *Server) handleMatterNodes(w http.ResponseWriter, r *http.Request) {
	if s.deps.Matter == nil {
		writeJSON(w, 503, map[string]string{"error": "matter sidecar not configured"})
		return
	}
	nodes, err := s.deps.Matter.ListNodes()
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, nodes)
}
