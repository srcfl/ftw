package config

import "testing"

// The OCPP listener cannot be pinned to one interface — the library builds its
// listen address from the port alone. Basic auth is therefore the only thing
// between an enabled server and any host that can route to it, so an enabled
// section without credentials has to fail rather than start.
func TestOCPPValidate(t *testing.T) {
	tests := []struct {
		name    string
		ocpp    *OCPP
		wantErr bool
	}{
		{
			name: "absent section is fine",
			ocpp: nil,
		},
		{
			name: "disabled without credentials is fine",
			ocpp: &OCPP{Enabled: false},
		},
		{
			name:    "enabled without credentials is rejected",
			ocpp:    &OCPP{Enabled: true},
			wantErr: true,
		},
		{
			name:    "enabled with only a username is rejected",
			ocpp:    &OCPP{Enabled: true, Username: "ftw"},
			wantErr: true,
		},
		{
			name:    "enabled with only a password is rejected",
			ocpp:    &OCPP{Enabled: true, Password: "secret"},
			wantErr: true,
		},
		{
			name: "enabled with both is accepted",
			ocpp: &OCPP{Enabled: true, Username: "ftw", Password: "secret"},
		},
		{
			name:    "port out of range is rejected",
			ocpp:    &OCPP{Enabled: true, Username: "ftw", Password: "secret", Port: 70000},
			wantErr: true,
		},
		{
			name:    "negative heartbeat is rejected",
			ocpp:    &OCPP{Enabled: true, Username: "ftw", Password: "secret", HeartbeatIntervalS: -1},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ocpp.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

// The OCPP password is the only gate in front of a listener reachable on every
// interface. It must never be served over /api/config, and a settings save that
// returns the masked (empty) value must not wipe it.
func TestOCPPPasswordIsMaskedAndPreserved(t *testing.T) {
	stored := &Config{OCPP: &OCPP{
		Enabled:  true,
		Username: "ftw",
		Password: "the-real-secret",
	}}

	masked := stored.MaskSecrets()
	if masked.OCPP == nil {
		t.Fatal("masked config lost the ocpp section")
	}
	if masked.OCPP.Password != "" {
		t.Errorf("password leaked through MaskSecrets: %q", masked.OCPP.Password)
	}
	if masked.OCPP.Username != "ftw" {
		t.Errorf("username should survive masking, got %q", masked.OCPP.Username)
	}
	// Masking must not mutate the original.
	if stored.OCPP.Password != "the-real-secret" {
		t.Errorf("MaskSecrets mutated the source config: %q", stored.OCPP.Password)
	}

	// The UI round-trips the masked config back on save.
	incoming := &Config{OCPP: &OCPP{
		Enabled:  true,
		Username: "ftw",
		Password: "",
	}}
	incoming.PreserveMaskedSecrets(stored)
	if incoming.OCPP.Password != "the-real-secret" {
		t.Errorf("password not preserved on save, got %q", incoming.OCPP.Password)
	}

	// A genuinely new password must still win.
	changed := &Config{OCPP: &OCPP{
		Enabled:  true,
		Username: "ftw",
		Password: "a-new-secret",
	}}
	changed.PreserveMaskedSecrets(stored)
	if changed.OCPP.Password != "a-new-secret" {
		t.Errorf("new password was overwritten, got %q", changed.OCPP.Password)
	}
}

// A disabled or absent OCPP section must not stop the rest of the config from
// validating, and an enabled one without credentials must take the whole
// config down with it rather than being skipped.
func TestConfigValidateCoversOCPP(t *testing.T) {
	// SmoothingAlpha and MaxAmps are normally filled in by defaults, which this
	// test does not run; set them so the only thing under test is OCPP.
	base := func() *Config {
		return &Config{
			Site: Site{SmoothingAlpha: 0.3},
			Fuse: Fuse{MaxAmps: 20, Phases: 3, Voltage: 230},
		}
	}

	c := base()
	if err := c.Validate(); err != nil {
		t.Fatalf("config without an ocpp section should validate, got %v", err)
	}

	c = base()
	c.OCPP = &OCPP{Enabled: true}
	if err := c.Validate(); err == nil {
		t.Fatal("config with an enabled, credential-less ocpp section should fail")
	}

	c = base()
	c.OCPP = &OCPP{Enabled: true, Username: "ftw", Password: "secret"}
	if err := c.Validate(); err != nil {
		t.Fatalf("config with a credentialed ocpp section should validate, got %v", err)
	}
}
