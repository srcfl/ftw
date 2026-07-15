package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

func TestDriverSecretKeysIncludePortableDriverPathAlias(t *testing.T) {
	driverDir := filepath.Join(t.TempDir(), "custom-driver-dir")
	if err := os.MkdirAll(driverDir, 0755); err != nil {
		t.Fatal(err)
	}
	luaPath := filepath.Join(driverDir, "sonnen.lua")
	if err := os.WriteFile(luaPath, []byte(`
DRIVER = {
  id = "sonnen",
  name = "sonnen",
  protocols = { "http" },
  config_secrets = { "api_token" },
}
`), 0644); err != nil {
		t.Fatal(err)
	}

	srv := New(&Deps{
		DriverDir:  driverDir,
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
	})
	secrets := srv.driverSecretKeys()
	cfg := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Lua:  "drivers/sonnen.lua",
		Config: map[string]any{
			"api_token": "secret-token",
		},
	}}}

	maskDriverConfigSecrets(cfg, secrets)

	if got := cfg.Drivers[0].Config["api_token"]; got != maskedPlaceholder {
		t.Fatalf("api_token = %q, want masked placeholder", got)
	}

	incoming := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Lua:  "drivers/sonnen.lua",
		Config: map[string]any{
			"api_token": maskedPlaceholder,
		},
	}}}
	existing := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Config: map[string]any{
			"api_token": "secret-token",
		},
	}}}
	restoreDriverConfigSecrets(incoming, existing, secrets)
	if got := incoming.Drivers[0].Config["api_token"]; got != "secret-token" {
		t.Fatalf("restored api_token = %q, want original secret", got)
	}
}
