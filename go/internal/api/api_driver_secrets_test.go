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

func TestMaskDriverConfigSecretsFailsClosedWhenCatalogMissing(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Lua:  "drivers/sonnen.lua",
		Config: map[string]any{
			"api_token": "secret-token",
			"host":      "192.168.1.10",
		},
	}}}
	maskDriverConfigSecrets(cfg, nil)
	if got := cfg.Drivers[0].Config["api_token"]; got != maskedPlaceholder {
		t.Fatalf("api_token = %q, want masked when catalog is unreadable", got)
	}
	if got := cfg.Drivers[0].Config["host"]; got != maskedPlaceholder {
		t.Fatalf("host = %q, want masked when catalog is unreadable", got)
	}

	incoming := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Config: map[string]any{
			"api_token": maskedPlaceholder,
			"host":      maskedPlaceholder,
		},
	}}}
	existing := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Config: map[string]any{
			"api_token": "secret-token",
			"host":      "192.168.1.10",
		},
	}}}
	restoreDriverConfigSecrets(incoming, existing, nil)
	if got := incoming.Drivers[0].Config["api_token"]; got != "secret-token" {
		t.Fatalf("restored api_token = %q", got)
	}
	if got := incoming.Drivers[0].Config["host"]; got != "192.168.1.10" {
		t.Fatalf("restored host = %q", got)
	}
}

func TestMaskDriverConfigSecretsFailsClosedWhenCatalogSkipsDriver(t *testing.T) {
	cfg := &config.Config{Drivers: []config.Driver{{
		Name: "sonnen",
		Lua:  "drivers/sonnen.lua",
		Config: map[string]any{
			"api_token": "secret-token",
		},
	}}}
	// Another driver loaded; this lua file was skipped.
	maskDriverConfigSecrets(cfg, map[string][]string{"drivers/other.lua": {"api_token"}})
	if got := cfg.Drivers[0].Config["api_token"]; got != maskedPlaceholder {
		t.Fatalf("api_token = %q, want masked when the catalog skipped this driver", got)
	}

	emptyCatalog := &config.Config{Drivers: []config.Driver{{
		Name:   "sonnen",
		Lua:    "drivers/sonnen.lua",
		Config: map[string]any{"api_token": "secret-token"},
	}}}
	maskDriverConfigSecrets(emptyCatalog, map[string][]string{})
	if got := emptyCatalog.Drivers[0].Config["api_token"]; got != maskedPlaceholder {
		t.Fatalf("api_token = %q, want masked when the catalog loaded nothing", got)
	}
}

func TestRejectUnsafeProbeHost(t *testing.T) {
	if err := rejectUnsafeProbeHost("127.0.0.1"); err == nil {
		t.Fatal("loopback should be refused")
	}
	if err := rejectUnsafeProbeHost("::1"); err == nil {
		t.Fatal("IPv6 loopback should be refused")
	}
	if err := rejectUnsafeProbeHost("[::1]"); err == nil {
		t.Fatal("bracketed IPv6 loopback should be refused")
	}
	if err := rejectUnsafeProbeHost("localhost"); err == nil {
		t.Fatal("localhost should be refused")
	}
	if err := rejectUnsafeProbeHost("169.254.1.1"); err == nil {
		t.Fatal("link-local should be refused")
	}
	if err := rejectUnsafeProbeHost("fe80::1"); err == nil {
		t.Fatal("IPv6 link-local should be refused")
	}
	if err := rejectUnsafeProbeHost("0.0.0.0"); err == nil {
		t.Fatal("unspecified should be refused")
	}
	if err := rejectUnsafeProbeHost("192.168.1.10"); err != nil {
		t.Fatalf("private IP refused: %v", err)
	}
	if err := rejectUnsafeProbeHost("zap.local"); err != nil {
		t.Fatalf("hostname refused: %v", err)
	}
}

func TestRejectUnsafeProbeTargetsCoversHTTP(t *testing.T) {
	cfg := config.Driver{
		Config: map[string]any{"host": "127.0.0.1"},
		Capabilities: config.Capabilities{
			HTTP: &config.HTTPCapability{AllowedHosts: []string{"127.0.0.1"}},
		},
	}
	if err := rejectUnsafeProbeTargets(cfg); err == nil {
		t.Fatal("HTTP loopback probe should be refused")
	}
	cfg.Config["host"] = "192.168.1.10"
	cfg.Capabilities.HTTP.AllowedHosts = []string{"inverter.local"}
	if err := rejectUnsafeProbeTargets(cfg); err != nil {
		t.Fatalf("LAN HTTP probe refused: %v", err)
	}
}

// The installed copy of a driver can lag its source: the signed channel
// served myuplink 1.2.1 without config_secrets while the bundled source
// declared them, and GET /api/config returned client_secret and
// refresh_token in clear text (#1057). Keys whose names say credential
// are masked, and restored on POST, whether or not the catalog lists them.
func TestMaskDriverConfigSecretsByKeyNameWhenCatalogSilent(t *testing.T) {
	driverDir := filepath.Join(t.TempDir(), "drivers")
	if err := os.MkdirAll(driverDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Catalog knows the driver but declares no secrets.
	if err := os.WriteFile(filepath.Join(driverDir, "myuplink.lua"), []byte(`
DRIVER = {
  id = "myuplink",
  name = "myuplink",
  protocols = { "http" },
}
`), 0644); err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{
		DriverDir:  driverDir,
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
	})
	secrets := srv.driverSecretKeys()
	if keys, known := secrets["drivers/myuplink.lua"]; !known || len(keys) != 0 {
		t.Fatalf("precondition: catalog should know myuplink with no declared secrets, got known=%v keys=%v", known, keys)
	}

	cfg := &config.Config{Drivers: []config.Driver{{
		Name: "myuplink",
		Lua:  "drivers/myuplink.lua",
		Config: map[string]any{
			"client_id":     "public-id",
			"client_secret": "s3cret",
			"refresh_token": "r3fresh",
			"Api-Key":       "k3y",
			"signing_key":   "sign",
			"oauth_scope":   "",
			"poll_s":        30,
		},
	}}}
	maskDriverConfigSecrets(cfg, secrets)
	got := cfg.Drivers[0].Config
	for _, k := range []string{"client_secret", "refresh_token", "Api-Key", "signing_key"} {
		if got[k] != maskedPlaceholder {
			t.Errorf("%s = %q, want masked", k, got[k])
		}
	}
	if got["client_id"] != "public-id" {
		t.Errorf("client_id = %q, want untouched", got["client_id"])
	}
	if got["oauth_scope"] != "" || got["poll_s"] != 30 {
		t.Errorf("non-secret / non-string values changed: %v", got)
	}

	incoming := &config.Config{Drivers: []config.Driver{{
		Name: "myuplink",
		Lua:  "drivers/myuplink.lua",
		Config: map[string]any{
			"client_id":     "public-id",
			"client_secret": maskedPlaceholder,
			"refresh_token": "",
			"Api-Key":       "rotated",
		},
	}}}
	existing := &config.Config{Drivers: []config.Driver{{
		Name: "myuplink",
		Config: map[string]any{
			"client_secret": "s3cret",
			"refresh_token": "r3fresh",
			"Api-Key":       "k3y",
			"signing_key":   "sign",
		},
	}}}
	restoreDriverConfigSecrets(incoming, existing, secrets)
	in := incoming.Drivers[0].Config
	if in["client_secret"] != "s3cret" {
		t.Errorf("masked client_secret should restore, got %q", in["client_secret"])
	}
	if in["refresh_token"] != "r3fresh" {
		t.Errorf("blank refresh_token should restore, got %q", in["refresh_token"])
	}
	if in["Api-Key"] != "rotated" {
		t.Errorf("a new value must win over the stored one, got %q", in["Api-Key"])
	}
	if in["signing_key"] != "sign" {
		t.Errorf("a secret the client omitted should restore, got %q", in["signing_key"])
	}
}

func TestIsDriverSecretKey(t *testing.T) {
	yes := []string{"password", "PASSWORD", "passwd", "client_secret", "refresh_token", "access_token", "api_key", "apiKey", "private_key", "credentials", "signing_key"}
	no := []string{"client_id", "email", "serial", "host", "home_id", "keyboard", "oauth_scope", "base_url"}
	for _, k := range yes {
		if !isDriverSecretKey(k, nil) {
			t.Errorf("%q should count as a secret key", k)
		}
	}
	for _, k := range no {
		if isDriverSecretKey(k, nil) {
			t.Errorf("%q should not count as a secret key", k)
		}
	}
	if !isDriverSecretKey("home_id", []string{"home_id"}) {
		t.Errorf("a catalog-declared key counts whatever its name")
	}
}
