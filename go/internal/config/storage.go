package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/srcfl/ftw/go/internal/state"
)

// storedSettings carries the hash outside Config's public JSON shape. The EV
// credential is already part of Config and MaskSecrets removes it from reads.
type storedSettings struct {
	Config          *Config `json:"config"`
	LANPasswordHash string  `json:"lan_password_hash,omitempty"`
}

func decodeStored(c state.Configuration, database, baseDir string) (*Config, error) {
	var doc storedSettings
	if err := json.Unmarshal(c.Document, &doc); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}
	if doc.Config == nil {
		return nil, errors.New("stored settings have no config")
	}
	cfg := doc.Config
	cfg.ConfigDatabase = database
	cfg.Revision = c.Revision
	cfg.LANPasswordHash = doc.LANPasswordHash
	// This is the same typed config that was validated on save. Do not apply new
	// defaults during a storage-only reload: nil, false and zero stay distinct.
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("stored settings: %w", err)
	}
	cfg.ResolveDriverPaths(baseDir)
	return cfg, nil
}

func loadStored(database, baseDir string) (*Config, error) {
	doc, err := state.ReadConfiguration(database)
	if err != nil {
		return nil, err
	}
	return decodeStored(doc, database, baseDir)
}

// InitializeStorage imports YAML once, then records the database location in
// the seed file. If a crash interrupted this last step, reuse the committed
// document instead of importing the old YAML again. A recovered database must
// still contain the exact settings revision loaded before state.Open.
func InitializeStorage(path, database string, cfg *Config, st *state.Store) (*Config, error) {
	database, err := filepath.Abs(database)
	if err != nil {
		return nil, err
	}
	if err := protectSettingsDatabase(database); err != nil {
		return nil, err
	}
	doc, found, err := st.Configuration()
	if err != nil {
		return nil, err
	}
	if cfg.ConfigDatabase != "" {
		if !found || doc.Revision != cfg.Revision {
			return nil, errors.New("database recovery lost current settings; restore a full backup")
		}
		return cfg, nil
	}
	if found {
		cfg, err = decodeStored(doc, database, filepath.Dir(path))
		if err != nil {
			return nil, err
		}
	} else {

		if cfg.EVCharger != nil {
			password, _, err := st.ConfigValue("ev_charger_password")
			if err != nil {
				return nil, err
			}
			cfg.EVCharger.Password = password
		}
		cfg.LANPasswordHash, _, err = st.ConfigValue("lan_auth_password")
		if err != nil {
			return nil, err
		}
		cfg.ConfigDatabase = database
		if err := SaveStored(st, path, cfg); err != nil {
			return nil, err
		}
	}
	// SaveAtomic writes an owner-only seed/export with an explicit authority.
	// After this point Load never uses its old settings as a fallback.
	seed := *cfg
	if relative, err := filepath.Rel(filepath.Dir(path), database); err == nil {
		seed.ConfigDatabase = relative
	}
	if err := SaveAtomic(path, &seed); err != nil {
		return nil, fmt.Errorf("record settings database: %w", err)
	}
	return cfg, nil
}

func SaveStored(st *state.Store, path string, cfg *Config) error {
	if cfg.ConfigDatabase == "" {
		return errors.New("settings database is not initialized")
	}
	if err := protectSettingsDatabase(cfg.ConfigDatabase); err != nil {
		return err
	}
	// Moving history is an offline operation, not a settings save that starts a
	// new, empty database on the next boot.
	var previous *Config
	if current, found, err := st.Configuration(); err != nil {
		return err
	} else if found {
		old, err := decodeStored(current, cfg.ConfigDatabase, filepath.Dir(path))
		if err != nil {
			return err
		}
		previous = old
		oldPath, newPath := "", ""
		if old.State != nil {
			oldPath = old.State.Path
		}
		if cfg.State != nil {
			newPath = cfg.State.Path
		}
		if oldPath != newPath {
			return errors.New("move the state database offline; its path cannot change in Settings")
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	portable := *cfg
	portable.Drivers = append([]Driver(nil), cfg.Drivers...)
	portable.UnresolveDriverPaths(filepath.Dir(path))
	raw, err := json.Marshal(storedSettings{Config: &portable, LANPasswordHash: cfg.LANPasswordHash})
	if err != nil {
		return err
	}
	password := ""
	if cfg.EVCharger != nil {
		password = cfg.EVCharger.Password
	}
	credentials := map[string]string{
		"ev_charger_password": password,
		"lan_auth_password":   cfg.LANPasswordHash,
	}
	for _, d := range cfg.Drivers {
		token, ok := d.Config["refresh_token"].(string)
		if !ok {
			continue
		}
		oldToken := ""
		if previous != nil {
			for _, old := range previous.Drivers {
				if old.Name == d.Name {
					oldToken, _ = old.Config["refresh_token"].(string)
					break
				}
			}
		}
		if previous != nil && token != oldToken {
			credentials["driver_secret:"+d.Name+":refresh_token"] = token
		}
	}
	revision, err := st.SaveConfiguration(raw, cfg.Revision, credentials)
	if err != nil {
		return err
	}
	cfg.Revision = revision
	return nil
}

// ExportStored writes portable YAML from a database snapshot. Its database
// reference is relative to the seed file at the restore destination.
func ExportStored(path string, configuration state.Configuration, database string) error {
	var doc storedSettings
	if err := json.Unmarshal(configuration.Document, &doc); err != nil {
		return err
	}
	if doc.Config == nil {
		return errors.New("stored settings have no config")
	}
	doc.Config.ConfigDatabase = database
	return SaveAtomic(path, doc.Config)
}

func protectSettingsDatabase(database string) error {
	for _, path := range []string{database, database + "-wal", database + "-shm"} {
		if err := restrictConfigFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("protect settings database: %w", err)
		}
	}
	return nil
}
