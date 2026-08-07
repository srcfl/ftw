package config

import "testing"

// The Geotorget token is the operator's own credential for Lantmateriet. It
// must never come back out of the API, and -- the failure that actually bites --
// saving the settings form must not wipe it, because the form only ever sends
// back the blank it was given.

func TestRoofModelMaskSecretsHidesTheTokenButSaysOneExists(t *testing.T) {
	c := Config{RoofModel: &RoofModel{
		Enabled:           true,
		GeotorgetUsername: "operator@example.com",
		GeotorgetToken:    "gt_secret_value",
	}}
	m := c.MaskSecrets()

	if m.RoofModel.GeotorgetToken != "" {
		t.Errorf("token leaked through the API: %q", m.RoofModel.GeotorgetToken)
	}
	if !m.RoofModel.HasGeotorgetToken {
		t.Error("UI cannot tell a stored token from a missing one")
	}
	// The username is not a secret, and blanking it would make the form look
	// empty when it is not.
	if m.RoofModel.GeotorgetUsername != "operator@example.com" {
		t.Errorf("username got blanked: %q", m.RoofModel.GeotorgetUsername)
	}
	if c.RoofModel.GeotorgetToken != "gt_secret_value" {
		t.Error("masking mutated the original config")
	}
}

func TestRoofModelMaskSecretsReportsNoTokenWhenUnset(t *testing.T) {
	for _, tok := range []string{"", "   "} {
		c := Config{RoofModel: &RoofModel{Enabled: true, GeotorgetToken: tok}}
		if c.MaskSecrets().RoofModel.HasGeotorgetToken {
			t.Errorf("token %q reported as stored", tok)
		}
	}
}

// Saving any unrelated setting round-trips the whole config, so an empty token
// from the UI means "unchanged", not "delete it".
func TestRoofModelPreserveMaskedSecretsKeepsTheStoredToken(t *testing.T) {
	existing := &Config{RoofModel: &RoofModel{
		Enabled: true, GeotorgetUsername: "operator", GeotorgetToken: "gt_secret_value",
	}}
	incoming := &Config{RoofModel: &RoofModel{
		Enabled: true, GeotorgetUsername: "operator", GeotorgetToken: "", RadiusM: 60,
	}}

	incoming.PreserveMaskedSecrets(existing)

	if incoming.RoofModel.GeotorgetToken != "gt_secret_value" {
		t.Errorf("token = %q, want it preserved", incoming.RoofModel.GeotorgetToken)
	}
	if incoming.RoofModel.RadiusM != 60 {
		t.Error("the edit being saved was lost")
	}
}

// Pasting a new token has to replace the old one, or a rotated credential
// could never be entered.
func TestRoofModelPreserveMaskedSecretsAcceptsANewToken(t *testing.T) {
	existing := &Config{RoofModel: &RoofModel{GeotorgetToken: "old_token"}}
	incoming := &Config{RoofModel: &RoofModel{GeotorgetToken: "new_token"}}

	incoming.PreserveMaskedSecrets(existing)

	if incoming.RoofModel.GeotorgetToken != "new_token" {
		t.Errorf("token = %q, want the newly entered one", incoming.RoofModel.GeotorgetToken)
	}
}

// Enabling the module for the first time has no existing section to copy from.
func TestRoofModelPreserveMaskedSecretsSurvivesAMissingSection(t *testing.T) {
	incoming := &Config{RoofModel: &RoofModel{GeotorgetToken: "first_token"}}
	incoming.PreserveMaskedSecrets(&Config{})
	if incoming.RoofModel.GeotorgetToken != "first_token" {
		t.Errorf("token = %q", incoming.RoofModel.GeotorgetToken)
	}

	none := &Config{}
	none.PreserveMaskedSecrets(&Config{RoofModel: &RoofModel{GeotorgetToken: "x"}})
	if none.RoofModel != nil {
		t.Error("a section the operator never configured was invented")
	}
}
