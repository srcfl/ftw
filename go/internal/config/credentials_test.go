package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func oauthSettings(t *testing.T, dir, database string, d Driver) *Config {
	t.Helper()
	cfg, err := Parse([]byte(minimalYAML), dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Drivers = append(cfg.Drivers, d)
	cfg.ConfigDatabase = database
	return cfg
}

func testOAuthDriver(name, owner, token, lua string) Driver {
	return Driver{
		Name:            name,
		CredentialOwner: owner,
		Lua:             lua,
		Capabilities:    Capabilities{Standalone: true},
		Config:          map[string]any{"refresh_token": token},
	}
}

func oauthDriver(t *testing.T, cfg *Config, name string) *Driver {
	t.Helper()
	for i := range cfg.Drivers {
		if cfg.Drivers[i].Name == name {
			return &cfg.Drivers[i]
		}
	}
	t.Fatalf("driver %q missing", name)
	return nil
}

func TestSaveStoredMigratesNameKeyedSecretAndSurvivesRename(t *testing.T) {
	dir := t.TempDir()
	path, database := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.db")
	lua := filepath.Join(dir, "oauth.lua")
	if err := os.WriteFile(lua, []byte("function driver_init() end"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveConfig(DriverSecretStateKey("old-name", "refresh_token"), "rotated-B"); err != nil {
		t.Fatal(err)
	}

	cfg := oauthSettings(t, dir, database, testOAuthDriver("old-name", "", "config-A", lua))
	if err := SaveStored(st, path, cfg); err != nil {
		t.Fatal(err)
	}
	owner := oauthDriver(t, cfg, "old-name").CredentialOwner
	if owner == "" || owner == "old-name" {
		t.Fatalf("credential_owner = %q, want a minted id", owner)
	}
	if got, ok := st.LoadConfig(DriverSecretStateKey(owner, "refresh_token")); !ok || got != "rotated-B" {
		t.Fatalf("migrated secret = %q ok=%v, want rotated-B", got, ok)
	}

	oauthDriver(t, cfg, "old-name").Name = "renamed"
	if err := SaveStored(st, path, cfg); err != nil {
		t.Fatal(err)
	}
	if oauthDriver(t, cfg, "renamed").CredentialOwner != owner {
		t.Fatalf("rename changed credential_owner to %q", oauthDriver(t, cfg, "renamed").CredentialOwner)
	}
	if got, ok := st.LoadConfig(DriverSecretStateKey(owner, "refresh_token")); !ok || got != "rotated-B" {
		t.Fatalf("renamed secret = %q ok=%v, want rotated-B", got, ok)
	}
}

func TestSaveStoredDoesNotGiveReusedNameTheOldSecret(t *testing.T) {
	dir := t.TempDir()
	path, database := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.db")
	lua := filepath.Join(dir, "oauth.lua")
	if err := os.WriteFile(lua, []byte("function driver_init() end"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveConfig(DriverSecretStateKey("old-name", "refresh_token"), "rotated-B"); err != nil {
		t.Fatal(err)
	}

	first := oauthSettings(t, dir, database, testOAuthDriver("old-name", "owner-old", "config-A", lua))
	if err := SaveStored(st, path, first); err != nil {
		t.Fatal(err)
	}

	replaced := oauthSettings(t, dir, database, testOAuthDriver("old-name", "", "config-C", lua))
	replaced.Revision = first.Revision
	if err := SaveStored(st, path, replaced); err != nil {
		t.Fatal(err)
	}
	newOwner := oauthDriver(t, replaced, "old-name").CredentialOwner
	if newOwner == "" || newOwner == "owner-old" {
		t.Fatalf("reused name kept old owner %q", newOwner)
	}
	if got, ok := st.LoadConfig(DriverSecretStateKey(newOwner, "refresh_token")); !ok || got != "config-C" {
		t.Fatalf("reused-name secret = %q ok=%v, want config-C", got, ok)
	}
	if got, ok := st.LoadConfig(DriverSecretStateKey("owner-old", "refresh_token")); ok && got == "config-C" {
		t.Fatalf("reused name overwrote the previous owner's secret: %q", got)
	}
}

func TestSaveStoredWritesExplicitReauthorizationUnderOwner(t *testing.T) {
	dir := t.TempDir()
	path, database := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.db")
	lua := filepath.Join(dir, "oauth.lua")
	if err := os.WriteFile(lua, []byte("function driver_init() end"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := oauthSettings(t, dir, database, testOAuthDriver("myuplink", "owner-1", "token-A", lua))
	if err := SaveStored(st, path, cfg); err != nil {
		t.Fatal(err)
	}
	oauthDriver(t, cfg, "myuplink").Config["refresh_token"] = "token-D"
	if err := SaveStored(st, path, cfg); err != nil {
		t.Fatal(err)
	}
	if got, ok := st.LoadConfig(DriverSecretStateKey("owner-1", "refresh_token")); !ok || got != "token-D" {
		t.Fatalf("reauth secret = %q ok=%v, want token-D", got, ok)
	}
}

func TestSaveStoredRejectsAmbiguousSecretOwnership(t *testing.T) {
	dir := t.TempDir()
	path, database := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.db")
	lua := filepath.Join(dir, "oauth.lua")
	if err := os.WriteFile(lua, []byte("function driver_init() end"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveConfig(DriverSecretStateKey("myuplink", "refresh_token"), "name-keyed"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(DriverSecretStateKey("owner-1", "refresh_token"), "owner-keyed"); err != nil {
		t.Fatal(err)
	}

	cfg := oauthSettings(t, dir, database, testOAuthDriver("myuplink", "owner-1", "config-A", lua))
	if err := SaveStored(st, path, cfg); err == nil {
		t.Fatal("ambiguous name-keyed and owner-keyed secrets were accepted")
	}
}

func TestBindCredentialOwnersMigratesExistingNameKeyedSecret(t *testing.T) {
	dir := t.TempDir()
	path, database := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "state.db")
	lua := filepath.Join(dir, "oauth.lua")
	if err := os.WriteFile(lua, []byte("function driver_init() end"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := oauthSettings(t, dir, database, testOAuthDriver("old-name", "", "config-A", lua))
	if err := SaveStored(st, path, cfg); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveConfig(DriverSecretStateKey("old-name", "refresh_token"), "rotated-B"); err != nil {
		t.Fatal(err)
	}
	if err := BindCredentialOwners(st, path, cfg); err != nil {
		t.Fatal(err)
	}
	owner := oauthDriver(t, cfg, "old-name").CredentialOwner
	if owner == "" || owner == "old-name" {
		t.Fatalf("bind left credential_owner = %q", owner)
	}
	if got, ok := st.LoadConfig(DriverSecretStateKey(owner, "refresh_token")); !ok || got != "rotated-B" {
		t.Fatalf("bound secret = %q ok=%v, want rotated-B", got, ok)
	}
	if err := BindCredentialOwners(st, path, cfg); err != nil {
		t.Fatal(err)
	}
	if oauthDriver(t, cfg, "old-name").CredentialOwner != owner {
		t.Fatalf("second bind reminted owner %q", oauthDriver(t, cfg, "old-name").CredentialOwner)
	}
}

func TestValidateRejectsInvalidCredentialOwner(t *testing.T) {
	dir := t.TempDir()
	lua := filepath.Join(dir, "oauth.lua")
	if err := os.WriteFile(lua, []byte("function driver_init() end"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(minimalYAML), dir)
	if err != nil {
		t.Fatal(err)
	}
	bad := testOAuthDriver("a", "owner:bad", "t", lua)
	bad.IsSiteMeter = true
	cfg.Drivers = []Driver{bad}
	if err := cfg.Validate(); err == nil {
		t.Fatal("credential_owner containing ':' was accepted")
	}
	first, second := testOAuthDriver("a", "same", "t", lua), testOAuthDriver("b", "same", "t", lua)
	first.IsSiteMeter = true
	cfg.Drivers = []Driver{first, second}
	if err := cfg.Validate(); err == nil {
		t.Fatal("duplicate credential_owner was accepted")
	}
}
