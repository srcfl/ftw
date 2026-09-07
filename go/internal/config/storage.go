package config

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/srcfl/ftw/go/internal/state"
	"gopkg.in/yaml.v3"
)

// storedSettings carries the hash outside Config's public JSON shape. The EV
// credential is already part of Config and MaskSecrets removes it from reads.
type storedSettings struct {
	Config          *Config `json:"config"`
	LANPasswordHash string  `json:"lan_password_hash,omitempty"`
	YAMLSourceHash  string  `json:"yaml_source_hash,omitempty"`
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
		current, err := decodeStored(doc, database, filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(cfg, current) {
			return nil, errors.New("database recovery changed current settings; restore a full backup")
		}
		return cfg, nil
	}
	rawSeed, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read config seed: %w", readErr)
	}
	sourceHash := ""
	if readErr == nil {
		sourceHash = fmt.Sprintf("%x", sha256.Sum256(rawSeed))
	}
	var saved storedSettings
	if found {
		if err := json.Unmarshal(doc.Document, &saved); err != nil {
			return nil, fmt.Errorf("decode settings source: %w", err)
		}
	}
	// A rolled-back Core removes the unknown locator when it saves YAML.
	// Distinguish that new save from an import interrupted before publication:
	// the latter still has the exact source bytes committed with the document.
	legacySave := found && saved.YAMLSourceHash != "" && sourceHash != "" && saved.YAMLSourceHash != sourceHash
	if found && !legacySave {
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
		if found {
			cfg.Revision = doc.Revision
		}
		if err := saveStored(st, path, cfg, sourceHash); err != nil {
			return nil, err
		}
	}
	// Preserve fields understood by the old Core so an automatic image rollback
	// can still read its original settings. The new Core only reads the locator.
	seed := *cfg
	if relative, err := filepath.Rel(filepath.Dir(path), database); err == nil {
		seed.ConfigDatabase = relative
	}
	if err := recordSettingsDatabase(path, &seed, rawSeed); err != nil {
		return nil, fmt.Errorf("record settings database: %w", err)
	}
	return cfg, nil
}

func SaveStored(st *state.Store, path string, cfg *Config) error {
	return saveStored(st, path, cfg, "")
}

func saveStored(st *state.Store, path string, cfg *Config, sourceHash string) error {
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
		if sourceHash == "" {
			var saved storedSettings
			if err := json.Unmarshal(current.Document, &saved); err != nil {
				return err
			}
			sourceHash = saved.YAMLSourceHash
		}
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
	raw, err := json.Marshal(storedSettings{Config: &portable, LANPasswordHash: cfg.LANPasswordHash, YAMLSourceHash: sourceHash})
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

func recordSettingsDatabase(path string, cfg *Config, raw []byte) error {
	if len(raw) == 0 {
		return SaveAtomic(path, cfg)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return errors.New("config seed must be a YAML mapping")
	}
	root := document.Content[0]
	value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: cfg.ConfigDatabase}
	found := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "config_database" {
			root.Content[i+1], found = value, true
			break
		}
	}
	if !found {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "config_database"}, value)
	}
	data, err := yaml.Marshal(&document)
	if err != nil {
		return err
	}
	data = append([]byte("# Settings live in SQLite. Use FTW Settings to change them.\n# This file keeps the original import for an older Core after rollback.\n"), data...)
	return writeConfigAtomic(defaultDurableWriter, path, data)
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
