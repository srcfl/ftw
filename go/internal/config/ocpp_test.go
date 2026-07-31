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
