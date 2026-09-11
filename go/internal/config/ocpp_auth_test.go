package config

import "testing"

func enabledOCPP(mut func(o *OCPP)) *OCPP {
	o := &OCPP{Enabled: true, Username: "ftw", Password: "shared-secret"}
	if mut != nil {
		mut(o)
	}
	return o
}

func TestOCPPBindValidation(t *testing.T) {
	tests := []struct {
		name    string
		bind    string
		wantErr bool
	}{
		{name: "empty means every interface", bind: ""},
		{name: "unspecified", bind: "0.0.0.0"},
		{name: "a LAN address", bind: "192.168.1.10"},
		{name: "IPv6", bind: "::1"},
		// A hostname would silently fall back to "every interface", which is
		// the opposite of what the operator asked for.
		{name: "hostname", bind: "ftw.local", wantErr: true},
		{name: "address with a port", bind: "192.168.1.10:8887", wantErr: true},
		{name: "nonsense", bind: "everywhere", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := enabledOCPP(func(o *OCPP) { o.Bind = tc.bind }).Validate()
			if tc.wantErr && err == nil {
				t.Errorf("bind %q accepted, want rejected", tc.bind)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("bind %q rejected: %v", tc.bind, err)
			}
		})
	}
}

func TestOCPPTLSValidation(t *testing.T) {
	tests := []struct {
		name    string
		tls     *OCPPTLS
		wantErr bool
	}{
		{name: "absent"},
		{name: "empty section"},
		{name: "cert and key", tls: &OCPPTLS{CertFile: "c.pem", KeyFile: "k.pem"}},
		{name: "mutual TLS", tls: &OCPPTLS{CertFile: "c.pem", KeyFile: "k.pem", ClientCAFile: "ca.pem"}},
		{name: "cert without key", tls: &OCPPTLS{CertFile: "c.pem"}, wantErr: true},
		{name: "key without cert", tls: &OCPPTLS{KeyFile: "k.pem"}, wantErr: true},
		// Client certificates are only verified on a TLS listener, so this
		// would look like mutual TLS and be plaintext.
		{name: "client CA alone", tls: &OCPPTLS{ClientCAFile: "ca.pem"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := enabledOCPP(func(o *OCPP) { o.TLS = tc.tls })
			if tc.name == "empty section" {
				o.TLS = &OCPPTLS{}
			}
			err := o.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("%+v accepted, want rejected", tc.tls)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("%+v rejected: %v", tc.tls, err)
			}
		})
	}
}

func TestOCPPPerChargerValidation(t *testing.T) {
	tests := []struct {
		name     string
		chargers []OCPPCharger
		wantErr  bool
	}{
		{name: "none"},
		{name: "one", chargers: []OCPPCharger{{ID: "garage", Password: "x"}}},
		{name: "two", chargers: []OCPPCharger{{ID: "garage", Password: "x"}, {ID: "carport", Password: "y"}}},
		{name: "no id", chargers: []OCPPCharger{{Password: "x"}}, wantErr: true},
		// An entry with no password would quietly fall back to the shared
		// one while the operator believes it is pinned.
		{name: "no password", chargers: []OCPPCharger{{ID: "garage"}}, wantErr: true},
		{name: "duplicate id", chargers: []OCPPCharger{{ID: "garage", Password: "x"}, {ID: "garage", Password: "y"}}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := enabledOCPP(func(o *OCPP) { o.Chargers = tc.chargers }).Validate()
			if tc.wantErr && err == nil {
				t.Errorf("%+v accepted, want rejected", tc.chargers)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("%+v rejected: %v", tc.chargers, err)
			}
		})
	}
}

// Per-charger passwords are the same secret as the shared one with a narrower
// blast radius, so they get the same treatment: never served, never wiped by a
// settings save that returns the masked value.
func TestOCPPPerChargerPasswordsMaskedAndPreserved(t *testing.T) {
	stored := &Config{OCPP: enabledOCPP(func(o *OCPP) {
		o.Chargers = []OCPPCharger{
			{ID: "garage", Password: "garage-secret"},
			{ID: "carport", Password: "carport-secret"},
		}
	})}

	masked := stored.MaskSecrets()
	for i, c := range masked.OCPP.Chargers {
		if c.Password != "" {
			t.Errorf("charger %d (%s) leaked its password: %q", i, c.ID, c.Password)
		}
		if c.ID == "" {
			t.Errorf("charger %d lost its id to masking", i)
		}
	}
	// The struct copy shares its backing array with the live config, so this
	// is the assertion that catches blanking in place.
	if stored.OCPP.Chargers[0].Password != "garage-secret" {
		t.Errorf("MaskSecrets mutated the source config: %q", stored.OCPP.Chargers[0].Password)
	}

	// A save that round-trips the masked values must not wipe them, and must
	// match by id — the settings UI may reorder or drop entries, and
	// restoring by position would hand one charger another's credential.
	incoming := &Config{OCPP: enabledOCPP(func(o *OCPP) {
		o.Chargers = []OCPPCharger{
			{ID: "carport", Password: ""},
			{ID: "garage", Password: ""},
		}
	})}
	incoming.PreserveMaskedSecrets(stored)
	if got := incoming.OCPP.Chargers[0].Password; got != "carport-secret" {
		t.Errorf("carport got %q, want carport-secret", got)
	}
	if got := incoming.OCPP.Chargers[1].Password; got != "garage-secret" {
		t.Errorf("garage got %q, want garage-secret", got)
	}

	// A genuinely new password still wins.
	changed := &Config{OCPP: enabledOCPP(func(o *OCPP) {
		o.Chargers = []OCPPCharger{{ID: "garage", Password: "rotated"}}
	})}
	changed.PreserveMaskedSecrets(stored)
	if got := changed.OCPP.Chargers[0].Password; got != "rotated" {
		t.Errorf("rotated password not kept, got %q", got)
	}

	// A charger with no stored secret stays empty rather than inheriting one.
	fresh := &Config{OCPP: enabledOCPP(func(o *OCPP) {
		o.Chargers = []OCPPCharger{{ID: "driveway", Password: ""}}
	})}
	fresh.PreserveMaskedSecrets(stored)
	if got := fresh.OCPP.Chargers[0].Password; got != "" {
		t.Errorf("an unknown charger inherited a secret: %q", got)
	}
}

func TestOCPPChargerSecrets(t *testing.T) {
	var nilOCPP *OCPP
	if got := nilOCPP.ChargerSecrets(); got != nil {
		t.Errorf("nil section: got %v, want nil", got)
	}
	o := enabledOCPP(func(o *OCPP) {
		o.Chargers = []OCPPCharger{{ID: "garage", Password: "x"}}
	})
	secrets := o.ChargerSecrets()
	if len(secrets) != 1 || secrets["garage"] != "x" {
		t.Errorf("got %v, want one garage entry", secrets)
	}
}
