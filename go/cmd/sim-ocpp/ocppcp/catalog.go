// Package ocppcp is an OCPP charge-point simulator.
//
// The catalog is Evify's published home-charger range as of 2026-09-11
// (https://www.evify.se/jämför-laddboxar/ and each product page, all marked
// in stock). Every model that speaks OCPP can dial FTW's built-in Central
// System; Tesla Wall Connector is listed so the inventory is complete and
// skipped, because it has no OCPP and already has a local HTTP driver.
//
// Quirks are only the ones FTW has already had to defend against in
// go/internal/ocpp — Charge Amps RemoteStop, connector-0 refusal on dual-
// socket Aura, Absolute profiles with no start, Zaptec appending its serial
// as the identity. Other vendors run as spec-compliant 1.6J (DEFA Power as
// 2.0.1, which is what Evify advertises for it).
package ocppcp

import "strings"

// Protocol is how a catalog model talks to FTW.
type Protocol string

const (
	ProtocolOCPP16  Protocol = "ocpp1.6"
	ProtocolOCPP201 Protocol = "ocpp2.0.1"
	ProtocolHTTP    Protocol = "http-local"
)

const (
	// SiteVoltage is the voltage FTW's EV commands assume.
	SiteVoltage = 230.0
)

// Quirks are vendor disagreements with the specification that change what
// the simulator does with a real OCPP message. Unset means spec-compliant.
type Quirks struct {
	// RejectConnectorZero refuses a TxDefaultProfile on connector 0. Dual-
	// socket Charge Amps Aura units (and some others) read connector 0 as
	// ChargePointMaxProfile-only; FTW retries on connector 1.
	RejectConnectorZero bool
	// IgnoreRemoteStop acknowledges RemoteStopTransaction and keeps charging.
	// Charge Amps hardware does this in the field, which is why FTW pauses
	// with a 0 A profile instead.
	IgnoreRemoteStop bool
	// IgnoreAbsoluteWithoutStart answers Accepted to an Absolute schedule
	// that has no startSchedule, then does not apply it. A charger that
	// parses the missing timestamp strictly treats the profile as not yet
	// active and charges on at full rate.
	IgnoreAbsoluteWithoutStart bool
	// IdentityIsSerial makes the charge-point id the hardware serial.
	// Zaptec appends the serial to the backend URL; operators must enter
	// the URL without it.
	IdentityIsSerial bool
}

// Model is one charger in the Evify inventory.
type Model struct {
	// ID is the CLI slug and, unless IdentityIsSerial, the OCPP identity.
	ID       string
	Vendor   string
	Name     string
	Serial   string
	Firmware string
	Protocol Protocol
	// MaxW is the advertised maximum charge power.
	MaxW float64
	// Phases is the installed supply. Evify's range is 1-or-3; the sim
	// runs 3-phase, which is the common Swedish install.
	Phases     int
	Connectors int
	RFID       bool
	Tethered   bool
	SourceURL  string
	Quirks     Quirks
}

// DialID is the last URL segment this model connects with.
func (m Model) DialID() string {
	if m.Quirks.IdentityIsSerial {
		return m.Serial
	}
	return m.ID
}

// MaxAmps is the per-phase current at MaxW on SiteVoltage.
func (m Model) MaxAmps() float64 {
	phases := m.Phases
	if phases <= 0 {
		phases = 3
	}
	return m.MaxW / (SiteVoltage * float64(phases))
}

// SpeaksOCPP reports whether this model can dial FTW's OCPP server.
func (m Model) SpeaksOCPP() bool {
	return m.Protocol == ProtocolOCPP16 || m.Protocol == ProtocolOCPP201
}

func chargeAmpsQuirks(aura bool) Quirks {
	q := Quirks{
		IgnoreRemoteStop:           true,
		IgnoreAbsoluteWithoutStart: true,
	}
	if aura {
		q.RejectConnectorZero = true
	}
	return q
}

func zaptecQuirks() Quirks {
	return Quirks{IdentityIsSerial: true}
}

// Inventory is Evify's published home-charger range. Tesla is included so
// callers that ask "every charger in the warehouse" see the one that cannot
// speak OCPP, rather than silently dropping it.
func Inventory() []Model {
	return []Model{
		{
			ID: "easee-charge-up", Vendor: "Easee", Name: "Charge Up",
			Serial: "EH-UP-22001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 22000, Phases: 3, Connectors: 1, RFID: true,
			SourceURL: "https://www.evify.se/produkter/easee-charge-up/",
		},
		{
			ID: "easee-charge-max", Vendor: "Easee", Name: "Charge Max",
			Serial: "EH-MAX-22001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 22000, Phases: 3, Connectors: 1, RFID: true,
			SourceURL: "https://www.evify.se/produkter/easee-charge-max/",
		},
		{
			ID: "zaptec-go", Vendor: "Zaptec", Name: "Go",
			Serial: "ZAPGO22001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 22000, Phases: 3, Connectors: 1, RFID: true,
			Quirks:    zaptecQuirks(),
			SourceURL: "https://www.evify.se/produkter/zaptec-go/",
		},
		{
			ID: "zaptec-go-2", Vendor: "Zaptec", Name: "Go 2",
			Serial: "ZAPGO222001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 22000, Phases: 3, Connectors: 1, RFID: true,
			Quirks:    zaptecQuirks(),
			SourceURL: "https://www.evify.se/produkter/zaptec-go-2/",
		},
		{
			ID: "nexblue-edge-2", Vendor: "NexBlue", Name: "Edge 2",
			Serial: "NB-EDGE2-22001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 22000, Phases: 3, Connectors: 1, RFID: true,
			SourceURL: "https://www.evify.se/produkter/nexblue-edge-2/",
		},
		{
			ID: "go-e-gemini-flex", Vendor: "go-e", Name: "Gemini Flex 2.0",
			Serial: "GOE-GF-22001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 22000, Phases: 3, Connectors: 1, RFID: true,
			SourceURL: "https://www.evify.se/produkter/go-e-gemini-flex/",
		},
		{
			ID: "charge-amps-luna", Vendor: "Charge Amps", Name: "Luna",
			Serial: "CA-LUNA-22001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 22000, Phases: 3, Connectors: 1, RFID: true,
			Quirks:    chargeAmpsQuirks(false),
			SourceURL: "https://www.evify.se/produkter/charge-amps-luna/",
		},
		{
			ID: "charge-amps-halo", Vendor: "Charge Amps", Name: "Halo",
			Serial: "CA-HALO-11001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 11000, Phases: 3, Connectors: 1, RFID: true, Tethered: true,
			Quirks:    chargeAmpsQuirks(false),
			SourceURL: "https://www.evify.se/produkter/charge-amps-halo/",
		},
		{
			ID: "charge-amps-aura", Vendor: "Charge Amps", Name: "Aura",
			Serial: "CA-AURA-22001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 22000, Phases: 3, Connectors: 2, RFID: true,
			Quirks:    chargeAmpsQuirks(true),
			SourceURL: "https://www.evify.se/produkter/charge-amps-aura/",
		},
		{
			ID: "charge-amps-dawn", Vendor: "Charge Amps", Name: "Dawn",
			Serial: "CA-DAWN-22001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 22000, Phases: 3, Connectors: 1, RFID: true,
			Quirks:    chargeAmpsQuirks(false),
			SourceURL: "https://www.evify.se/produkter/charge-amps-dawn/",
		},
		{
			ID: "wallbox-pulsar-max", Vendor: "Wallbox", Name: "Pulsar Max",
			Serial: "WB-PM-22001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP16, MaxW: 22000, Phases: 3, Connectors: 1, Tethered: true,
			SourceURL: "https://www.evify.se/produkter/wallbox-pulsar-max/",
		},
		{
			ID: "defa-power", Vendor: "DEFA", Name: "Power",
			Serial: "DEFA-PWR-22001", Firmware: "sim-1.0",
			Protocol: ProtocolOCPP201, MaxW: 22000, Phases: 3, Connectors: 1, RFID: true, Tethered: true,
			SourceURL: "https://www.evify.se/produkter/defa-power/",
		},
		{
			ID: "tesla-wall-connector", Vendor: "Tesla", Name: "Wall Connector",
			Serial: "TWC-22001", Firmware: "sim-1.0",
			Protocol: ProtocolHTTP, MaxW: 22000, Phases: 3, Connectors: 1, Tethered: true,
			SourceURL: "https://www.evify.se/produkter/tesla-wall-connector/",
		},
	}
}

// OCPPModels is the subset that can dial FTW over OCPP.
func OCPPModels() []Model {
	all := Inventory()
	out := make([]Model, 0, len(all))
	for _, m := range all {
		if m.SpeaksOCPP() {
			out = append(out, m)
		}
	}
	return out
}

// Lookup finds a model by CLI slug, DialID, or serial. Case-insensitive.
func Lookup(id string) (Model, bool) {
	want := strings.ToLower(strings.TrimSpace(id))
	if want == "" {
		return Model{}, false
	}
	for _, m := range Inventory() {
		if strings.ToLower(m.ID) == want || strings.ToLower(m.DialID()) == want || strings.ToLower(m.Serial) == want {
			return m, true
		}
	}
	return Model{}, false
}
