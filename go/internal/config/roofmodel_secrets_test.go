package config

import "testing"

// The STAC catalog password — for Lantmäteriet, the operator's own Geotorget
// account password, since no OAuth is offered for those APIs. It must never
// come back out of the API, and — the failure that actually bites — saving
// the settings form must not wipe it, because the form only ever sends back
// the blank it was given.

func TestRoofModelMaskSecretsHidesThePasswordButSaysOneExists(t *testing.T) {
	c := Config{RoofModel: &RoofModel{
		Enabled:      true,
		StacUsername: "operator@example.com",
		StacPassword: "gt_secret_value",
	}}
	m := c.MaskSecrets()

	if m.RoofModel.StacPassword != "" {
		t.Errorf("password leaked through the API: %q", m.RoofModel.StacPassword)
	}
	if !m.RoofModel.HasStacPassword {
		t.Error("UI cannot tell a stored password from a missing one")
	}
	// The username is not a secret, and blanking it would make the form look
	// empty when it is not.
	if m.RoofModel.StacUsername != "operator@example.com" {
		t.Errorf("username got blanked: %q", m.RoofModel.StacUsername)
	}
	if c.RoofModel.StacPassword != "gt_secret_value" {
		t.Error("masking mutated the original config")
	}
}

// A config written before the basic-auth redesign stores the same secret
// under geotorget_token. It must mask just as hard, and the masked copy must
// present the canonical stac_* shape so the UI only ever reads one thing.
func TestRoofModelMaskSecretsFoldsTheLegacyKeys(t *testing.T) {
	c := Config{RoofModel: &RoofModel{
		Enabled:           true,
		GeotorgetUsername: "operator",
		GeotorgetToken:    "legacy_secret",
	}}
	m := c.MaskSecrets()

	if m.RoofModel.GeotorgetToken != "" || m.RoofModel.StacPassword != "" {
		t.Errorf("secret leaked: token=%q password=%q",
			m.RoofModel.GeotorgetToken, m.RoofModel.StacPassword)
	}
	if !m.RoofModel.HasStacPassword {
		t.Error("a legacy-stored secret was reported as absent")
	}
	if m.RoofModel.StacUsername != "operator" || m.RoofModel.GeotorgetUsername != "" {
		t.Errorf("masked copy not folded to stac_*: stac=%q legacy=%q",
			m.RoofModel.StacUsername, m.RoofModel.GeotorgetUsername)
	}
}

func TestRoofModelMaskSecretsReportsNoPasswordWhenUnset(t *testing.T) {
	for _, pw := range []string{"", "   "} {
		c := Config{RoofModel: &RoofModel{Enabled: true, StacPassword: pw}}
		if c.MaskSecrets().RoofModel.HasStacPassword {
			t.Errorf("password %q reported as stored", pw)
		}
	}
}

// Saving any unrelated setting round-trips the whole config, so an empty
// password from the UI means "unchanged", not "delete it".
func TestRoofModelPreserveMaskedSecretsKeepsTheStoredPassword(t *testing.T) {
	existing := &Config{RoofModel: &RoofModel{
		Enabled: true, StacUsername: "operator", StacPassword: "gt_secret_value",
	}}
	incoming := &Config{RoofModel: &RoofModel{
		Enabled: true, StacUsername: "operator", StacPassword: "", RadiusM: 60,
	}}

	incoming.PreserveMaskedSecrets(existing)

	if incoming.RoofModel.StacPassword != "gt_secret_value" {
		t.Errorf("password = %q, want it preserved", incoming.RoofModel.StacPassword)
	}
	if incoming.RoofModel.RadiusM != 60 {
		t.Error("the edit being saved was lost")
	}
}

// A secret stored under the legacy key survives a save and migrates to the
// canonical key, which is how old configs move forward without the operator
// retyping anything.
func TestRoofModelPreserveMaskedSecretsMigratesALegacyToken(t *testing.T) {
	existing := &Config{RoofModel: &RoofModel{GeotorgetToken: "legacy_secret"}}
	incoming := &Config{RoofModel: &RoofModel{}}

	incoming.PreserveMaskedSecrets(existing)

	if incoming.RoofModel.StacPassword != "legacy_secret" {
		t.Errorf("password = %q, want the legacy secret under the new key",
			incoming.RoofModel.StacPassword)
	}
}

// Pasting a new password has to replace the old one, or a rotated credential
// could never be entered.
func TestRoofModelPreserveMaskedSecretsAcceptsANewPassword(t *testing.T) {
	existing := &Config{RoofModel: &RoofModel{StacPassword: "old_password"}}
	incoming := &Config{RoofModel: &RoofModel{StacPassword: "new_password"}}

	incoming.PreserveMaskedSecrets(existing)

	if incoming.RoofModel.StacPassword != "new_password" {
		t.Errorf("password = %q, want the newly entered one", incoming.RoofModel.StacPassword)
	}
}

// Enabling the module for the first time has no existing section to copy from.
func TestRoofModelPreserveMaskedSecretsSurvivesAMissingSection(t *testing.T) {
	incoming := &Config{RoofModel: &RoofModel{StacPassword: "first_password"}}
	incoming.PreserveMaskedSecrets(&Config{})
	if incoming.RoofModel.StacPassword != "first_password" {
		t.Errorf("password = %q", incoming.RoofModel.StacPassword)
	}

	none := &Config{}
	none.PreserveMaskedSecrets(&Config{RoofModel: &RoofModel{StacPassword: "x"}})
	if none.RoofModel != nil {
		t.Error("a section the operator never configured was invented")
	}
}

// Both credential spellings resolve through the accessors, stac_* winning.
func TestRoofModelCredentialAccessors(t *testing.T) {
	r := &RoofModel{GeotorgetUsername: "legacy-u", GeotorgetToken: "legacy-p"}
	if r.StacUser() != "legacy-u" || r.StacPass() != "legacy-p" {
		t.Errorf("legacy keys not readable: %q %q", r.StacUser(), r.StacPass())
	}
	r.StacUsername, r.StacPassword = "new-u", "new-p"
	if r.StacUser() != "new-u" || r.StacPass() != "new-p" {
		t.Errorf("stac_* keys must win: %q %q", r.StacUser(), r.StacPass())
	}
}
