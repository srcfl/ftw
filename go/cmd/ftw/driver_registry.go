package main

import (
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func newDriverRegistry(tel *telemetry.Store, st *state.Store) *drivers.Registry {
	reg := drivers.NewRegistry(tel)
	// Install both callbacks before Add can initialize or poll any driver.
	// Rotations keep their own KV rows so they do not apply the whole config
	// or restart the driver that just refreshed its credential.
	driverSecretKey := func(driverName, key string) string {
		return "driver_secret:" + driverName + ":" + key
	}
	reg.SecretPersister = func(driverName, key, value string) error {
		return st.SaveConfig(driverSecretKey(driverName, key), value)
	}
	reg.SecretOverride = func(driverName, key string) (string, bool) {
		return st.LoadConfig(driverSecretKey(driverName, key))
	}
	return reg
}
