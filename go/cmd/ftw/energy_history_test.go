package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestBuildEnergyObservationsUsesStableAssetsAndDirectionalCounters(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	deviceID, err := st.RegisterDevice(state.Device{
		DriverName: "mutable-meter-name", Make: "Meter Maker", Serial: "SN-42",
	})
	if err != nil {
		t.Fatal(err)
	}
	evDeviceID, err := st.RegisterDevice(state.Device{
		DriverName: "mutable-ev-name", Make: "Charger Maker", Serial: "EV-42",
	})
	if err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("mutable-meter-name")
	tel.EnsureDriverHealth("mutable-ev-name")
	tel.Update("mutable-meter-name", telemetry.DerMeter, 500, nil,
		json.RawMessage(`{"import_wh":1200,"export_wh":340}`))
	tel.Update("mutable-ev-name", telemetry.DerEV, 3200, nil,
		json.RawMessage(`{"session_wh":800}`))
	ctrl := &control.State{SiteMeterDriver: "mutable-meter-name"}
	observations := buildEnergyObservations(st, tel, ctrl, state.HistoryPoint{LoadW: 450})

	wantAssetID := state.HardwareEnergyAssetID(deviceID, state.AssetGridMeter)
	wantEVAssetID := state.HardwareEnergyAssetID(evDeviceID, state.AssetVehicleCharger)
	var sawImport, sawExport, sawConsumer, sawVehicleCharge bool
	for _, o := range observations {
		switch o.Flow {
		case state.FlowGridImport:
			sawImport = o.AssetID == wantAssetID && o.CounterWh != nil && *o.CounterWh == 1200 && o.PowerW != nil && *o.PowerW == 500
		case state.FlowGridExport:
			sawExport = o.AssetID == wantAssetID && o.CounterWh != nil && *o.CounterWh == 340 && o.PowerW != nil && *o.PowerW == 0
		case state.FlowConsumerUse:
			if o.AssetKind == state.AssetObservedConsumer {
				sawConsumer = o.AssetID == observedConsumerAssetID && o.ReadOnly && o.DeviceID == ""
			} else {
				t.Fatalf("consumer_use must be reserved for observed household load: %+v", o)
			}
		case state.FlowVehicleCharge:
			sawVehicleCharge = o.AssetID == wantEVAssetID && o.CounterWh != nil && *o.CounterWh == 800
		}
	}
	if !sawImport || !sawExport || !sawConsumer || !sawVehicleCharge {
		t.Fatalf("observations missing stable directional assets: %+v", observations)
	}
}
