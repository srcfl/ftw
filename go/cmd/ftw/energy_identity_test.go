package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestEnergyIdentityWaitsForSerialWithoutReusingItForAReplacement(t *testing.T) {
	old := state.Device{DriverName: "old-name", Make: "inverter", Serial: "one", MAC: "aa:bb:cc:dd:ee:ff", Endpoint: "tcp://site"}
	for _, tc := range []struct {
		name string
		live state.Device
		want string
	}{
		{"no live identity", state.Device{}, ""},
		{"same MAC after rename", state.Device{DriverName: "new-name", MAC: "AABBCCDDEEFF"}, ""},
		{"endpoint before ARP", state.Device{Endpoint: old.Endpoint}, ""},
		{"confirmed serial", old, "inverter:one"},
		{"new serial at same endpoint and MAC", state.Device{Make: "inverter", Serial: "two", MAC: old.MAC, Endpoint: old.Endpoint}, "inverter:two"},
		{"different MAC at same endpoint", state.Device{MAC: "11:22:33:44:55:66", Endpoint: old.Endpoint}, "mac:112233445566"},
		{"new endpoint only", state.Device{Endpoint: "tcp://new"}, "ep:tcp://new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := confirmedEnergyDeviceID(tc.live, []state.Device{old}); got != tc.want {
				t.Fatalf("identity = %q, want %q", got, tc.want)
			}
		})
	}
	if got := confirmedEnergyDeviceID(state.Device{MAC: old.MAC}, nil); got != "mac:aabbccddeeff" {
		t.Fatalf("new MAC-only device = %q", got)
	}
}

func TestStartupAliasCannotReplayCountersWhileRawHistoryKeepsWriting(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	serial := state.Device{DriverName: "meter", Make: "inverter", Serial: "one", MAC: "aa:bb:cc:dd:ee:ff"}
	if _, err := st.RegisterDevice(serial); err != nil {
		t.Fatal(err)
	}
	mac := state.Device{DriverName: "meter", MAC: serial.MAC}
	if _, err := st.RegisterDevice(mac); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	oldAt := now.Add(-24 * time.Hour).UnixMilli()
	oldCounter := 100.0
	macAsset := state.HardwareEnergyAssetID("mac:aabbccddeeff", state.AssetGridMeter)
	if err := st.EnqueueTelemetryTick(nil, nil, []state.EnergyObservation{{
		AssetID: macAsset, DeviceID: "mac:aabbccddeeff", AssetKind: state.AssetGridMeter,
		Flow: state.FlowGridImport, AtMs: oldAt, CounterWh: &oldCounter,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}

	tel := telemetry.NewStore()
	tel.EnsureDriverHealth("meter")
	tel.Update("meter", telemetry.DerMeter, 500, nil, json.RawMessage(`{"import_wh":7300}`))
	tel.RecordDriverSuccess("meter")
	ctrl := &control.State{SiteMeterDriver: "meter"}
	live := mac
	lookup := func(string) state.Device { return live }
	if _, err := persistTelemetryTick(st, tel, ctrl, now.UnixMilli(), time.Minute, lookup); err != nil {
		t.Fatal(err)
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	history, err := st.LoadHistory(now.Add(-time.Second).UnixMilli(), now.Add(time.Second).UnixMilli(), 0)
	if err != nil || len(history) != 1 {
		t.Fatalf("raw history=%v err=%v", history, err)
	}
	samples, err := st.LoadSeries("meter", "meter_w", 0, time.Now().Add(time.Second).UnixMilli(), 0)
	if err != nil || len(samples) == 0 {
		t.Fatalf("samples=%v err=%v", samples, err)
	}

	// Serial comes from the live host before the periodic devices-table update.
	live = serial
	if _, err := persistTelemetryTick(st, tel, ctrl, now.Add(time.Second).UnixMilli(), time.Minute, lookup); err != nil {
		t.Fatal(err)
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	assets, err := st.EnergyAssets()
	if err != nil {
		t.Fatal(err)
	}
	confirmed := false
	for _, asset := range assets {
		if asset.AssetID == macAsset && asset.LastSeenMS != oldAt {
			t.Fatalf("revived old MAC counter: %+v", asset)
		}
		if asset.DeviceID == "inverter:one" {
			confirmed = true
		}
	}
	if !confirmed {
		t.Fatalf("live serial not used: %+v", assets)
	}
	start := oldAt / state.EnergyLedgerBucketMS * state.EnergyLedgerBucketMS
	points, _, err := st.LoadEnergyHistory(state.EnergyHistoryQuery{AssetID: macAsset, SinceMS: start, UntilMS: now.Add(time.Minute).UnixMilli(), BucketMS: state.EnergyLedgerBucketMS, Limit: 300})
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range points {
		if point.EnergyWh != 0 {
			t.Fatalf("duplicate MAC energy: %+v", point)
		}
	}
}
