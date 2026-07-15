// Package nova is the opt-in federation client that publishes
// FTW telemetry into Sourceful's Nova Core backend.
//
// # Design thesis
//
// FTW uses a clean, physics-consistent, snake_case, site-
// convention data model (see ../../../docs/site-convention.md). Nova's
// current wire format has divergences from that model — mixed-case field
// names (SoC_nom_fract, L1_V, heatsink_C), opposite battery sign
// (negative W for charging), and a mismatched DER type vocabulary
// (solar/ev_port vs pv/ev).
//
// Rather than adopt Nova's debt, this package holds FTW's
// native clean payload (payload.go) and a single adapter (adapter.go)
// that translates it to the current Nova wire shape at the MQTT
// publisher boundary. When Nova ships the unified schema (see
// docs/nova-integration.md and the tracking issue), flip
// `nova.schema_mode: unified` in config and the adapter is bypassed;
// once legacy fleets migrate, the adapter can be deleted in one go
// with no change to any driver, the telemetry store, or the HTTP API.
//
// Topic
//
//	gateways/{gateway_serial}/devices/{hardware_id}/ders/{der_name}/telemetry/json/v1
//
// Identity mapping
//
//   - Nova hardware_id  ← FTW device_id verbatim
//     (make:serial / mac:aabb... / ep:host:port)
//   - Nova der_name     ← "{driver_name}-{der_type}" (e.g. solis-battery)
//   - Nova der_id       ← server-generated der-{uuid7}, stored locally
//     in state.nova_ders purely for diagnostics
//   - Gateway identity  ← ES256 keypair generated on first run
//
// # Runtime layout
//
// Start() mirrors internal/ha.Start: it owns its own paho MQTT client,
// reads snapshots from telemetry.Store on a ticker, translates via the
// adapter, and publishes. It also runs the nightly reconcile loop against
// Nova's REST API and kicks off startup provisioning of device+DER
// records via POST /devices/provision.
package nova
