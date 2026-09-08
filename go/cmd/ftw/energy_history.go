package main

import (
	"encoding/json"
	"log/slog"
	"math"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

const observedConsumerAssetID = "site/observed-consumer"

type energyIdentityLookup func(string) state.Device

// A known serial must be confirmed by the running driver before a weaker
// alias can write counters again. Another MAC or a newly reported serial is
// a replacement, not evidence that it owns the old device's counter history.
func confirmedEnergyDeviceID(current state.Device, known []state.Device) string {
	id := state.ResolveDeviceID(current.Make, current.Serial, current.MAC, current.Endpoint)
	if id == "" || state.ResolveDeviceID(current.Make, current.Serial, "", "") != "" {
		return id
	}
	macID := state.ResolveDeviceID("", "", current.MAC, "")
	for _, previous := range known {
		if macID != "" {
			previousMAC := state.ResolveDeviceID("", "", previous.MAC, "")
			sameEndpoint := current.Endpoint != "" && previous.Endpoint == current.Endpoint
			// ARP and serial discovery can leave their evidence in separate rows.
			// Follow the known MAC's old endpoint even after an address change.
			for _, alias := range known {
				if previous.Endpoint != "" && alias.Endpoint == previous.Endpoint &&
					state.ResolveDeviceID("", "", alias.MAC, "") == macID {
					sameEndpoint = true
				}
			}
			if state.ResolveDeviceID(previous.Make, previous.Serial, "", "") != "" &&
				(previousMAC == macID || (previousMAC == "" && sameEndpoint)) {
				return ""
			}
		} else if previous.Endpoint == current.Endpoint &&
			state.ResolveDeviceID(previous.Make, previous.Serial, previous.MAC, previous.Endpoint) != id {
			return ""
		}
	}
	return id
}

// buildEnergyObservations translates current telemetry into independent,
// unsigned ledger directions. It does not attribute PV to individual loads:
// the observed household consumer remains a site-balance observation only.
func buildEnergyObservations(st *state.Store, tel *telemetry.Store, ctrl *control.State, hp state.HistoryPoint, identity energyIdentityLookup) []state.EnergyObservation {
	if st == nil || tel == nil || ctrl == nil {
		return nil
	}
	out := make([]state.EnergyObservation, 0, 16)
	known, err := st.AllDevices()
	if err != nil {
		slog.Warn("energy device identity unavailable; skipping device counters", "err", err)
		identity = nil
	}
	ids := make(map[string]string)
	usable := func(driver string) bool {
		h := tel.DriverHealth(driver)
		return h != nil && h.Status != telemetry.StatusOffline
	}
	asset := func(driver string, kind state.EnergyAssetKind) (string, string, bool) {
		id, resolved := ids[driver]
		if !resolved && identity != nil {
			id = confirmedEnergyDeviceID(identity(driver), known)
			ids[driver] = id
		}
		if id == "" {
			return "", "", false
		}
		return state.HardwareEnergyAssetID(id, kind), id, true
	}
	appendDirection := func(assetID, deviceID string, kind state.EnergyAssetKind, label string,
		flow state.EnergyFlow, atMS int64, powerW float64, counter *float64, readOnly bool) {
		p := math.Max(0, powerW)
		out = append(out, state.EnergyObservation{
			AssetID: assetID, DeviceID: deviceID, AssetKind: kind, Label: label,
			ReadOnly: readOnly, Flow: flow, AtMs: atMS, PowerW: &p, CounterWh: counter,
		})
	}

	// The configured site meter is the grid boundary. Other meter-capable
	// devices remain available in generic telemetry but must not be summed into
	// system import/export.
	if driver := ctrl.SiteMeterDriver; driver != "" && usable(driver) {
		if r := tel.Get(driver, telemetry.DerMeter); r != nil {
			if id, deviceID, ok := asset(driver, state.AssetGridMeter); ok {
				data := energyNumbers(r.Data)
				appendDirection(id, deviceID, state.AssetGridMeter, driver,
					state.FlowGridImport, r.UpdatedAt.UnixMilli(), math.Max(r.RawW, 0), firstEnergyNumber(data, "import_wh", "total_import_wh"), false)
				appendDirection(id, deviceID, state.AssetGridMeter, driver,
					state.FlowGridExport, r.UpdatedAt.UnixMilli(), math.Max(-r.RawW, 0), firstEnergyNumber(data, "export_wh", "total_export_wh"), false)
			}
		}
	}

	for _, r := range tel.ReadingsByType(telemetry.DerBattery) {
		if !usable(r.Driver) {
			continue
		}
		id, deviceID, ok := asset(r.Driver, state.AssetBattery)
		if !ok {
			continue
		}
		data := energyNumbers(r.Data)
		appendDirection(id, deviceID, state.AssetBattery, r.Driver,
			state.FlowBatteryCharge, r.UpdatedAt.UnixMilli(), math.Max(r.RawW, 0), firstEnergyNumber(data, "charge_wh", "total_charge_wh"), false)
		appendDirection(id, deviceID, state.AssetBattery, r.Driver,
			state.FlowBatteryDischarge, r.UpdatedAt.UnixMilli(), math.Max(-r.RawW, 0), firstEnergyNumber(data, "discharge_wh", "total_discharge_wh"), false)
	}

	for _, r := range tel.ReadingsByType(telemetry.DerPV) {
		if !usable(r.Driver) {
			continue
		}
		id, deviceID, ok := asset(r.Driver, state.AssetPV)
		if !ok {
			continue
		}
		data := energyNumbers(r.Data)
		appendDirection(id, deviceID, state.AssetPV, r.Driver,
			state.FlowPVGeneration, r.UpdatedAt.UnixMilli(), math.Max(-r.RawW, 0),
			firstEnergyNumber(data, "generation_wh", "total_generation_wh", "lifetime_wh"), false)
	}

	for _, r := range tel.ReadingsByType(telemetry.DerEV) {
		if !usable(r.Driver) {
			continue
		}
		id, deviceID, ok := asset(r.Driver, state.AssetVehicleCharger)
		if !ok {
			continue
		}
		data := energyNumbers(r.Data)
		appendDirection(id, deviceID, state.AssetVehicleCharger, r.Driver,
			state.FlowVehicleCharge, r.UpdatedAt.UnixMilli(), math.Max(r.RawW, 0), firstEnergyNumber(data, "session_wh", "total_charge_wh"), false)
	}

	for _, r := range tel.ReadingsByType(telemetry.DerV2X) {
		if !usable(r.Driver) {
			continue
		}
		id, deviceID, ok := asset(r.Driver, state.AssetVehicleCharger)
		if !ok {
			continue
		}
		data := energyNumbers(r.Data)
		appendDirection(id, deviceID, state.AssetVehicleCharger, r.Driver,
			state.FlowVehicleCharge, r.UpdatedAt.UnixMilli(), math.Max(r.RawW, 0), firstEnergyNumber(data, "total_charge_wh", "session_charge_wh"), false)
		appendDirection(id, deviceID, state.AssetVehicleCharger, r.Driver,
			state.FlowVehicleDischarge, r.UpdatedAt.UnixMilli(), math.Max(-r.RawW, 0), firstEnergyNumber(data, "total_discharge_wh", "session_discharge_wh"), false)
	}

	// A consumer is observation-only: it has no driver command, control target,
	// or mutable config identity. It is the residual site balance after storage,
	// PV, EV and V2X and is never allocated back to an energy source.
	if ctrl.SiteMeterDriver != "" && usable(ctrl.SiteMeterDriver) {
		meter := tel.Get(ctrl.SiteMeterDriver, telemetry.DerMeter)
		if meter == nil {
			return out
		}
		appendDirection(observedConsumerAssetID, "", state.AssetObservedConsumer, "Observed load",
			state.FlowConsumerUse, meter.UpdatedAt.UnixMilli(), math.Max(hp.LoadW, 0), nil, true)
	}
	return out
}

func energyNumbers(raw json.RawMessage) map[string]float64 {
	var values map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &values) != nil {
		return nil
	}
	out := make(map[string]float64, len(values))
	for k, value := range values {
		if n, ok := value.(float64); ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
			out[k] = n
		}
	}
	return out
}

func firstEnergyNumber(values map[string]float64, names ...string) *float64 {
	for _, name := range names {
		if value, ok := values[name]; ok {
			v := value
			return &v
		}
	}
	return nil
}
