package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/state"
)

func TestOwnerRemoteAccessEnabledExplicitConfigWinsOverEnv(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		env  string
		want bool
	}{
		{
			name: "default off",
			cfg:  &config.Config{},
			want: false,
		},
		{
			name: "env alone does not opt in",
			cfg:  &config.Config{},
			env:  "true",
			want: false,
		},
		{
			name: "explicit config false stays off with env",
			cfg:  &config.Config{RemoteAccess: &config.RemoteAccess{Enabled: false}},
			env:  "true",
			want: false,
		},
		{
			name: "explicit config true enables remote",
			cfg:  &config.Config{RemoteAccess: &config.RemoteAccess{Enabled: true}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FTW_REMOTE_ACCESS_ENABLED", tt.env)
			if got := ownerRemoteAccessEnabled(tt.cfg); got != tt.want {
				t.Fatalf("ownerRemoteAccessEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOwnerRelayURLIsOfficialDefaultOnlyAfterOptIn(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		env  *string
		want string
	}{
		{
			name: "default off",
			cfg:  &config.Config{},
			want: "",
		},
		{
			name: "env alone does not opt in",
			cfg:  &config.Config{},
			env:  strPtr("https://relay.example.test"),
			want: "",
		},
		{
			name: "enabled uses official relay by default",
			cfg:  &config.Config{RemoteAccess: &config.RemoteAccess{Enabled: true}},
			want: defaultOwnerRelayURL,
		},
		{
			name: "enabled honours custom relay",
			cfg:  &config.Config{RemoteAccess: &config.RemoteAccess{Enabled: true}},
			env:  strPtr("https://relay.example.test/"),
			want: "https://relay.example.test",
		},
		{
			name: "enabled can explicitly disable relay with empty env",
			cfg:  &config.Config{RemoteAccess: &config.RemoteAccess{Enabled: true}},
			env:  strPtr(""),
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == nil {
				t.Setenv("FTW_RELAY_URL", "")
				_ = os.Unsetenv("FTW_RELAY_URL")
			} else {
				t.Setenv("FTW_RELAY_URL", *tt.env)
			}
			if got := ownerRelayURL(tt.cfg); got != tt.want {
				t.Fatalf("ownerRelayURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

func TestDeriveOwnerSiteIDNewInstallIsHighEntropy(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	siteID := deriveOwnerSiteID(st)
	if siteID == "site:Home" {
		t.Fatalf("new installs must not get guessable site_id %q", siteID)
	}
	if !strings.HasPrefix(siteID, "site:") || len(siteID) < len("site:")+32 {
		t.Fatalf("siteID = %q, want high-entropy site:<token>", siteID)
	}
	if got := deriveOwnerSiteID(st); got != siteID {
		t.Fatalf("siteID not persistent: got %q want %q", got, siteID)
	}
	if got, ok := st.LoadConfig(ownerSiteIDKey); !ok || got != siteID {
		t.Fatalf("persisted owner_site_id = %q/%v, want %q", got, ok, siteID)
	}
}

func TestDeriveOwnerSiteIDSavedOpaqueValueWins(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	want := "site:" + strings.Repeat("a", 32)
	if err := st.SaveConfig(ownerSiteIDKey, want); err != nil {
		t.Fatal(err)
	}

	if got := deriveOwnerSiteID(st); got != want {
		t.Fatalf("existing opaque owner_site_id = %q, want %q", got, want)
	}
}

func TestDeriveOwnerSiteIDRotatesGuessableSavedValue(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveConfig(ownerSiteIDKey, "site:kept"); err != nil {
		t.Fatal(err)
	}

	got := deriveOwnerSiteID(st)
	if got == "site:kept" {
		t.Fatalf("guessable owner_site_id was preserved: %q", got)
	}
	if !strings.HasPrefix(got, "site:") || len(got) < len("site:")+32 {
		t.Fatalf("rotated owner_site_id = %q, want high-entropy site:<token>", got)
	}
	if saved, ok := st.LoadConfig(ownerSiteIDKey); !ok || saved != got {
		t.Fatalf("persisted rotated owner_site_id = %q/%v, want %q", saved, ok, got)
	}
}

func TestDeriveOwnerSiteIDRotatesGuessableValueEvenWithTrustedDevices(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveConfig(ownerSiteIDKey, "site:Home"); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTrustedDevice(state.TrustedDevice{
		CredentialID: []byte("cred"),
		PublicKey:    []byte("pub"),
		FriendlyName: "existing owner",
	}); err != nil {
		t.Fatal(err)
	}

	got := deriveOwnerSiteID(st)
	if got == "site:Home" {
		t.Fatalf("trusted devices must not preserve guessable owner_site_id %q", got)
	}
	if !strings.HasPrefix(got, "site:") || len(got) < len("site:")+32 {
		t.Fatalf("rotated owner_site_id = %q, want high-entropy site:<token>", got)
	}
}
