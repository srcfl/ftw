package main

import (
	"encoding/json"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/ocpp"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A missing or stale cloud response is not an unplug. Keep the last session
// until a fresh, valid reading reports the connection has ended.
func currentEVSample(r *telemetry.DerReading, health *telemetry.DriverHealth, watchdog time.Duration, now time.Time, ocppOnline bool, deviceID string) (loadpoint.EVSample, bool) {
	if watchdog <= 0 {
		watchdog = time.Minute
	}
	if r == nil || r.UpdatedAt.IsZero() || now.Sub(r.UpdatedAt) > watchdog {
		return loadpoint.EVSample{}, false
	}
	var d struct {
		ConnectionGeneration uint64  `json:"connection_generation"`
		ConnectionUnknown    bool    `json:"connection_unknown"`
		Connected            *bool   `json:"connected"`
		SessionWh            float64 `json:"session_wh"`
		RequestActive        *bool   `json:"request_active"`
		SessionID            string  `json:"session_id"`
	}
	if json.Unmarshal(r.Data, &d) != nil || d.SessionWh < 0 {
		return loadpoint.EVSample{}, false
	}
	if d.ConnectionUnknown {
		return loadpoint.EVSample{ConnectionUnknown: true, ConnectionGeneration: d.ConnectionGeneration}, true
	}
	if d.Connected == nil || (!ocppOnline && (health == nil || !health.TelemetryLive())) {
		return loadpoint.EVSample{}, false
	}
	active := true
	if d.RequestActive != nil {
		active = *d.RequestActive
	}
	return loadpoint.EVSample{ConnectionGeneration: d.ConnectionGeneration, PowerW: r.SmoothedW, SessionWh: d.SessionWh,
		Connected: *d.Connected, RequestActive: active, DeviceID: deviceID, SessionID: d.SessionID}, true
}

// OCPP has no driver registry entry. Only the current adopted charger's
// BootNotification supplies hardware identity; its dialled name is an address.
func currentOCPPDeviceID(h *ocpp.Handler, name string) string {
	ident, ok := h.CurrentIdentity(name)
	if !ok || ident.Serial == "" {
		return ""
	}
	return state.ResolveDeviceID(ident.Vendor, ident.Serial, "", "")
}
