package config

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/srcfl/ftw/go/internal/state"
)

const driverSecretPrefix = "driver_secret:"

// DriverSecretStateKey is the unwatched KV row for a rotated driver secret.
func DriverSecretStateKey(owner, key string) string {
	return driverSecretPrefix + strings.TrimSpace(owner) + ":" + strings.TrimSpace(key)
}

// SecretOwner is the durable credential id for this driver. An assigned
// credential_owner wins; the display name is only the pre-migration fallback.
func (d Driver) SecretOwner() string {
	if id := strings.TrimSpace(d.CredentialOwner); id != "" {
		return id
	}
	return strings.TrimSpace(d.Name)
}

func assignCredentialOwners(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	seen := make(map[string]string, len(cfg.Drivers))
	for i := range cfg.Drivers {
		d := &cfg.Drivers[i]
		if strings.TrimSpace(d.CredentialOwner) == "" {
			d.CredentialOwner = uuid.NewString()
		}
		owner := strings.TrimSpace(d.CredentialOwner)
		if owner == "" || strings.Contains(owner, ":") {
			return fmt.Errorf("driver %q: invalid credential_owner", d.Name)
		}
		if other, ok := seen[owner]; ok {
			return fmt.Errorf("drivers %q and %q share credential_owner %s", other, d.Name, owner)
		}
		seen[owner] = d.Name
	}
	return nil
}

func previousDriverForSecrets(previous *Config, d Driver) *Driver {
	if previous == nil {
		return nil
	}
	owner := strings.TrimSpace(d.CredentialOwner)
	if owner != "" {
		for i := range previous.Drivers {
			if strings.TrimSpace(previous.Drivers[i].CredentialOwner) == owner {
				return &previous.Drivers[i]
			}
		}
	}
	for i := range previous.Drivers {
		if previous.Drivers[i].Name == d.Name {
			return &previous.Drivers[i]
		}
	}
	return nil
}

func continuesNamedDriver(previous *Driver, d Driver) bool {
	if previous == nil || previous.Name != d.Name {
		return false
	}
	prevOwner := strings.TrimSpace(previous.CredentialOwner)
	return prevOwner == "" || prevOwner == strings.TrimSpace(d.CredentialOwner)
}

func collectDriverSecretCredentials(cfg *Config, previous *Config, stored map[string]string) (map[string]string, error) {
	credentials := map[string]string{}
	if cfg == nil {
		return credentials, nil
	}
	for _, d := range cfg.Drivers {
		prev := previousDriverForSecrets(previous, d)
		// First import has no previous document; still take leftover
		// name-keyed rows for this display name. A later save only
		// migrates when this entry continues that same named driver.
		if previous == nil || continuesNamedDriver(prev, d) {
			prefix := driverSecretPrefix + d.Name + ":"
			for key, value := range stored {
				secretKey, ok := strings.CutPrefix(key, prefix)
				if !ok || secretKey == "" {
					continue
				}
				owned := DriverSecretStateKey(d.SecretOwner(), secretKey)
				if existing, ok := stored[owned]; ok && existing != value {
					return nil, fmt.Errorf("driver %q: ambiguous %s secret ownership", d.Name, secretKey)
				}
				if owned != key {
					credentials[owned] = value
				}
			}
		}
		token, ok := d.Config["refresh_token"].(string)
		if !ok {
			continue
		}
		oldToken := ""
		if prev != nil {
			oldToken, _ = prev.Config["refresh_token"].(string)
		}
		if previous != nil && token != oldToken {
			credentials[DriverSecretStateKey(d.SecretOwner(), "refresh_token")] = token
		}
	}
	return credentials, nil
}

func driversNeedCredentialOwnerBind(cfg *Config, stored map[string]string) bool {
	if cfg == nil {
		return false
	}
	for _, d := range cfg.Drivers {
		if strings.TrimSpace(d.CredentialOwner) == "" {
			return true
		}
		prefix := driverSecretPrefix + d.Name + ":"
		ownerPrefix := driverSecretPrefix + d.SecretOwner() + ":"
		if ownerPrefix == prefix {
			continue
		}
		for key := range stored {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		}
	}
	return false
}

// BindCredentialOwners assigns durable owners and migrates name-keyed secret
// rows into the settings document in one SaveStored transaction.
func BindCredentialOwners(st *state.Store, path string, cfg *Config) error {
	if st == nil || cfg == nil {
		return nil
	}
	stored, err := st.LoadConfigByPrefix(driverSecretPrefix)
	if err != nil {
		return err
	}
	if !driversNeedCredentialOwnerBind(cfg, stored) {
		return nil
	}
	return SaveStored(st, path, cfg)
}
