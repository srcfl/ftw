package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/srcfl/ftw/go/internal/fleetstats"
)

func (r *Relay) fleetHeartbeat(w http.ResponseWriter, req *http.Request) {
	if r.Fleet == nil {
		http.NotFound(w, req)
		return
	}
	if r.FleetLimit != nil && !r.FleetLimit.Allow(r.offerClientIP(req)) {
		http.Error(w, "fleet heartbeat rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	decoder := json.NewDecoder(io.LimitReader(req.Body, maxControlBodyBytes))
	decoder.DisallowUnknownFields()
	var payload fleetstats.Payload
	if err := decoder.Decode(&payload); err != nil {
		http.Error(w, "invalid fleet heartbeat", http.StatusBadRequest)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid fleet heartbeat", http.StatusBadRequest)
		return
	}
	if err := r.Fleet.Record(payload); err != nil {
		http.Error(w, "invalid fleet heartbeat", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Relay) fleetAggregate(w http.ResponseWriter, req *http.Request) {
	if r.Fleet == nil {
		http.NotFound(w, req)
		return
	}
	provided := bearerGrant(req)
	if r.FleetAdminToken == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(r.FleetAdminToken)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="FTW fleet statistics"`)
		http.Error(w, "missing or invalid fleet statistics token", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(r.Fleet.Aggregate())
}
