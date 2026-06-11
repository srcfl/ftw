package nova

import (
	"fmt"
	"strings"
)

// DerName derives the stable human name forty-two-watts uses for a DER
// in Nova. Nova stores this on the DER row (unique per device) and the
// publisher puts it into every telemetry topic path, so stability is
// load-bearing: a rename orphans every metric in VictoriaMetrics.
//
//	{driverName}-{derKind}
//
// Example: ferroamp-battery, sungrow-pv, p1-meter. Satisfies Nova's
// DER-name regex (alphanumeric + dash/underscore, ≤64 chars) as long
// as the driver name in config.yaml does.
func DerName(driverName, derKind string) string {
	return fmt.Sprintf("%s-%s", driverName, derKind)
}

// TopicFor returns the MQTT topic path Nova expects for a single DER's
// telemetry. Subject format:
//
//	gateways/{gateway_serial}/devices/{hardware_id}/ders/{der_name}/telemetry/json/v1
//
// Nova's MQTT adapter translates slashes to NATS-subject dots, so any
// '.' in hardware_id would create accidental extra levels in the
// subject tree and break routing. sanitizeTopicSegment converts such
// characters to '_' at the boundary.
func TopicFor(gatewaySerial, hardwareID, derName string) string {
	return fmt.Sprintf("gateways/%s/devices/%s/ders/%s/telemetry/json/v1",
		sanitizeTopicSegment(gatewaySerial),
		sanitizeTopicSegment(hardwareID),
		sanitizeTopicSegment(derName))
}

// sanitizeTopicSegment makes a string safe to use as one level of an
// MQTT topic that will be translated to a NATS subject. Preserves
// alphanumerics, dash, and underscore; replaces everything else
// (including '.', '/', '+', '#', ':') with '_'.
func sanitizeTopicSegment(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// DeviceTypeFor chooses a Nova device_type string from the set of
// DER kinds one forty-two-watts driver reports. Nova expects one of
// a fixed vocabulary (checked against its device_types table); we
// pick the most-specific single value consistent with what the driver
// actually emits:
//
//   - emits any battery reading           → "inverter" (hybrid covers
//     battery + pv + meter cases we see in the field)
//   - emits only pv                        → "inverter"
//   - emits only meter                     → "meter"
//   - emits only ev/v2x                    → "charger"
//
// derKinds is the set of forty-two-watts clean DER kinds (pv, battery,
// meter, ev, v2x_charger) observed for this device.
func DeviceTypeFor(derKinds []string) string {
	has := map[string]bool{}
	for _, k := range derKinds {
		has[k] = true
	}
	switch {
	case has[KindBattery], has[KindPV]:
		return "inverter"
	case has[KindEV], has[KindV2X]:
		return "charger"
	case has[KindMeter]:
		return "meter"
	}
	return "inverter"
}
