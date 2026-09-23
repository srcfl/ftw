package state

import "testing"

func TestDeviceIdentityRefinementRequiresSharedEvidence(t *testing.T) {
	before := Device{MAC: "b8:27:b9:35:8d:1a", Endpoint: "modbus://192.0.2.1:502"}
	serial := Device{Make: "Pixii", Serial: "123", MAC: "b827b9358d1a", Endpoint: before.Endpoint}
	for _, tc := range []struct {
		name          string
		before, after Device
		want          DeviceIDRelation
	}{
		{"same MAC with new serial", before, serial, DeviceIDMatch},
		{"missing MAC cannot use endpoint instead", before, Device{Make: "Pixii", Serial: "123", Endpoint: before.Endpoint}, DeviceIDPending},
		{"wrong MAC with new serial", before, Device{Make: "Pixii", Serial: "123", MAC: "aabbccddeeff", Endpoint: before.Endpoint}, DeviceIDConflict},
		{"wrong endpoint with same MAC", before, Device{Make: "Pixii", Serial: "123", MAC: before.MAC, Endpoint: "modbus://192.0.2.2:502"}, DeviceIDConflict},
		{"endpoint to MAC", Device{Endpoint: before.Endpoint}, before, DeviceIDMatch},
		{"endpoint to serial", Device{Endpoint: before.Endpoint}, serial, DeviceIDMatch},
		{"missing shared endpoint", Device{Endpoint: before.Endpoint}, Device{Make: "Pixii", Serial: "123"}, DeviceIDPending},
		{"changed endpoint to serial", Device{Endpoint: "other"}, serial, DeviceIDConflict},
		{"serial must not change", serial, Device{Make: "Pixii", Serial: "other", MAC: before.MAC}, DeviceIDConflict},
		{"serial still arriving", serial, before, DeviceIDPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RelateDeviceIdentities(tc.before, tc.after); got != tc.want {
				t.Fatalf("relation = %v, want %v", got, tc.want)
			}
		})
	}
}
