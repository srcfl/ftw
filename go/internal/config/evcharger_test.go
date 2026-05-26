package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestEVChargerNormalizeFoldsLegacyEmail(t *testing.T) {
	e := &EVCharger{Provider: "easee", EmailLegacy: "old@example.com"}
	e.Normalize()
	if e.Username != "old@example.com" {
		t.Errorf("Username: got %q, want %q", e.Username, "old@example.com")
	}
	if e.EmailLegacy != "" {
		t.Errorf("EmailLegacy should be cleared after Normalize, got %q", e.EmailLegacy)
	}
}

func TestEVChargerNormalizePrefersExistingUsername(t *testing.T) {
	e := &EVCharger{Provider: "easee", Username: "new@example.com", EmailLegacy: "old@example.com"}
	e.Normalize()
	if e.Username != "new@example.com" {
		t.Errorf("Username should win over legacy email, got %q", e.Username)
	}
	if e.EmailLegacy != "" {
		t.Errorf("EmailLegacy should be cleared, got %q", e.EmailLegacy)
	}
}

func TestEVChargerYAMLRoundTripEmailAlias(t *testing.T) {
	in := []byte("provider: easee\nemail: rickard@example.com\n")
	var e EVCharger
	if err := yaml.Unmarshal(in, &e); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	e.Normalize()
	if e.Username != "rickard@example.com" {
		t.Errorf("after Normalize Username should be populated from legacy email, got %q", e.Username)
	}

	// Re-marshal — should emit `username:`, not `email:`.
	out, err := yaml.Marshal(&e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "email:") {
		t.Errorf("re-marshaled YAML should not contain legacy email key: %s", s)
	}
	if !strings.Contains(s, "username: rickard@example.com") {
		t.Errorf("re-marshaled YAML missing username: %s", s)
	}
}

func TestEVChargerValidateEasee(t *testing.T) {
	cases := []struct {
		name    string
		e       EVCharger
		wantErr string // empty means no error
	}{
		{"happy", EVCharger{Provider: "easee", Username: "u@x"}, ""},
		{"empty creds allowed (wizard placeholder)", EVCharger{Provider: "easee"}, ""},
		{"modbus block rejected", EVCharger{Provider: "easee", Username: "u@x", Modbus: &EVChargerModbus{Host: "h"}}, "modbus"},
		{"http block allowed", EVCharger{Provider: "easee", Username: "u@x", HTTP: &EVChargerHTTP{BaseURL: "https://staging"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.e.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestEVChargerValidateCTek(t *testing.T) {
	cases := []struct {
		name    string
		e       EVCharger
		wantErr string
	}{
		{"happy", EVCharger{Provider: "ctek", Modbus: &EVChargerModbus{Host: "10.0.0.5"}}, ""},
		{"modbus required", EVCharger{Provider: "ctek"}, "modbus.host"},
		{"host required", EVCharger{Provider: "ctek", Modbus: &EVChargerModbus{Port: 502}}, "modbus.host"},
		{"http block rejected", EVCharger{Provider: "ctek", Modbus: &EVChargerModbus{Host: "h"}, HTTP: &EVChargerHTTP{}}, "http"},
		{"auth rejected", EVCharger{Provider: "ctek", Modbus: &EVChargerModbus{Host: "h"}, Username: "u"}, "username/password"},
		{"unit_id range high", EVCharger{Provider: "ctek", Modbus: &EVChargerModbus{Host: "h", UnitID: 248}}, "unit_id"},
		{"unit_id range negative", EVCharger{Provider: "ctek", Modbus: &EVChargerModbus{Host: "h", UnitID: -1}}, "unit_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.e.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestEVChargerValidateUnknownProvider(t *testing.T) {
	e := &EVCharger{Provider: "made-up"}
	err := e.Validate()
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error should mention 'not supported': %v", err)
	}
}

func TestEVChargerValidateEmptyProvider(t *testing.T) {
	e := &EVCharger{}
	err := e.Validate()
	if err == nil {
		t.Fatal("expected error for empty provider")
	}
}

func TestEVChargerValidateNilIsNoop(t *testing.T) {
	var e *EVCharger
	if err := e.Validate(); err != nil {
		t.Errorf("nil receiver should return nil error, got %v", err)
	}
}
