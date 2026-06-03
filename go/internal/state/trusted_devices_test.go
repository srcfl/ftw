package state

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"database/sql"
)

func openTempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSaveAndLoadTrustedDevice(t *testing.T) {
	s := openTempStore(t)
	dev := TrustedDevice{
		CredentialID: []byte{0x01, 0x02, 0x03},
		PublicKey:    []byte{0x04, 0x05, 0x06},
		SignCount:    7,
		AAGUID:       []byte{0x10, 0x11},
		Transports:   []string{"usb", "internal"},
		FriendlyName: "Fredrik's MacBook",
		CreatedAtMs:  1700_000_000_000,
	}
	if err := s.SaveTrustedDevice(dev); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.LookupTrustedDevice(dev.CredentialID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !reflect.DeepEqual(got.CredentialID, dev.CredentialID) {
		t.Errorf("credential_id mismatch")
	}
	if got.SignCount != dev.SignCount {
		t.Errorf("sign_count: %d want %d", got.SignCount, dev.SignCount)
	}
	if got.FriendlyName != dev.FriendlyName {
		t.Errorf("name: %q want %q", got.FriendlyName, dev.FriendlyName)
	}
	if !reflect.DeepEqual(got.Transports, dev.Transports) {
		t.Errorf("transports: %v want %v", got.Transports, dev.Transports)
	}

	list, err := s.LoadTrustedDevices()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 device, got %d", len(list))
	}
}

func TestUpdateTrustedDeviceSignCount(t *testing.T) {
	s := openTempStore(t)
	dev := TrustedDevice{
		CredentialID: []byte("abc"), PublicKey: []byte("k"),
		FriendlyName: "x", SignCount: 0,
	}
	if err := s.SaveTrustedDevice(dev); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.UpdateTrustedDeviceSignCount(dev.CredentialID, 5, 1700_000_001_000); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := s.LookupTrustedDevice(dev.CredentialID)
	if got.SignCount != 5 {
		t.Errorf("sign_count not updated: %d", got.SignCount)
	}
	if got.LastUsedMs != 1700_000_001_000 {
		t.Errorf("last_used_ms not updated: %d", got.LastUsedMs)
	}
}

func TestUpdateMissingDeviceReturnsErrNoRows(t *testing.T) {
	s := openTempStore(t)
	err := s.UpdateTrustedDeviceSignCount([]byte("does-not-exist"), 1, 0)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows, got %v", err)
	}
}

func TestDeleteTrustedDevice(t *testing.T) {
	s := openTempStore(t)
	dev := TrustedDevice{
		CredentialID: []byte("zap"), PublicKey: []byte("k"), FriendlyName: "x",
	}
	_ = s.SaveTrustedDevice(dev)
	if err := s.DeleteTrustedDevice(dev.CredentialID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := s.LookupTrustedDevice(dev.CredentialID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows after delete, got %v", err)
	}
}

func TestSaveRejectsMissingRequiredFields(t *testing.T) {
	s := openTempStore(t)
	cases := []TrustedDevice{
		{PublicKey: []byte("k"), FriendlyName: "x"},                  // no credential_id
		{CredentialID: []byte("c"), FriendlyName: "x"},               // no public_key
		{CredentialID: []byte("c"), PublicKey: []byte("k")},          // no friendly_name
	}
	for i, dev := range cases {
		if err := s.SaveTrustedDevice(dev); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

// Modern synced passkeys (iCloud Keychain, Google Password Manager) report
// signCount == 0 on every login. Persisting 0 after a prior 0 must be a
// no-corruption no-op, never treated as a clone (a clone is a *decrease*
// from a previously-positive value, handled by the webauthn lib upstream).
func TestUpdateSignCountConstantZeroIsBenign(t *testing.T) {
	s := openTempStore(t)
	cred := []byte("cred-zero")
	if err := s.SaveTrustedDevice(TrustedDevice{
		CredentialID: cred, PublicKey: []byte("k"), SignCount: 0, FriendlyName: "phone",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Two consecutive logins, both reporting 0.
	if err := s.UpdateTrustedDeviceSignCount(cred, 0, 1000); err != nil {
		t.Fatalf("update 1: %v", err)
	}
	if err := s.UpdateTrustedDeviceSignCount(cred, 0, 2000); err != nil {
		t.Fatalf("update 2: %v", err)
	}
	devs, err := s.LoadTrustedDevices()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(devs) != 1 || devs[0].SignCount != 0 {
		t.Fatalf("expected 1 device with signCount 0, got %+v", devs)
	}
	if devs[0].LastUsedMs != 2000 {
		t.Fatalf("expected LastUsedMs updated to 2000, got %d", devs[0].LastUsedMs)
	}
}
