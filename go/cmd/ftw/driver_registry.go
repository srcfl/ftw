package main

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func newDriverRegistry(tel *telemetry.Store, st *state.Store) *drivers.Registry {
	reg := drivers.NewRegistry(tel)
	// Install both callbacks before Add can initialize or poll any driver.
	// Rotations keep their own KV rows so they do not apply the whole config
	// or restart the driver that just refreshed its credential.
	driverSecretKey := func(owner, key string) string {
		return config.DriverSecretStateKey(owner, key)
	}
	reg.SecretPersister = func(owner, key, value string) error {
		return st.SaveConfig(driverSecretKey(owner, key), value)
	}
	reg.SecretOverride = func(owner, key string) (string, bool) {
		return st.LoadConfig(driverSecretKey(owner, key))
	}
	return reg
}
