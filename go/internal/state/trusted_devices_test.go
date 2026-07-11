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
	const pk = "af11bb22cc33dd44ee55ff66007788990011223344556677889900aabbccddeeff0011223344556677889900aabbccddeeff00112233445566778899aabbccdd"
	dev := TrustedDevice{
		CredentialID: []byte("zap"), PublicKey: []byte("k"), FriendlyName: "x", DevicePubkey: pk,
	}
	_ = s.SaveTrustedDevice(dev)
	if err := s.DeleteTrustedDevice(dev.CredentialID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err := s.LookupTrustedDevice(dev.CredentialID)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("want sql.ErrNoRows after delete, got %v", err)
	}
	if pks, err := s.TrustedDevicePubkeys(); err != nil || len(pks) != 0 {
		t.Fatalf("browser keys should be deleted with credential, pks=%v err=%v", pks, err)
	}
}

func TestSaveRejectsMissingRequiredFields(t *testing.T) {
	s := openTempStore(t)
	cases := []TrustedDevice{
		{PublicKey: []byte("k"), FriendlyName: "x"},         // no credential_id
		{CredentialID: []byte("c"), FriendlyName: "x"},      // no public_key
		{CredentialID: []byte("c"), PublicKey: []byte("k")}, // no friendly_name
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

// C4: the device_pubkey minted at LAN enrollment round-trips through save +
// load + lookup, and is exposed in the published set (C1).
func TestTrustedDeviceDevicePubkeyRoundTrip(t *testing.T) {
	s := openTempStore(t)
	const pk = "aa11bb22cc33dd44ee55ff66007788990011223344556677889900aabbccddeeff0011223344556677889900aabbccddeeff00112233445566778899aabbccdd"
	dev := TrustedDevice{
		CredentialID: []byte("c-dk"), PublicKey: []byte("k"),
		FriendlyName: "phone", DevicePubkey: pk,
	}
	if err := s.SaveTrustedDevice(dev); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.LookupTrustedDevice(dev.CredentialID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.DevicePubkey != pk {
		t.Fatalf("device_pubkey = %q, want %q", got.DevicePubkey, pk)
	}
	list, _ := s.LoadTrustedDevices()
	if len(list) != 1 || list[0].DevicePubkey != pk {
		t.Fatalf("load device_pubkey mismatch: %+v", list)
	}

	// Published set contains it.
	pks, err := s.TrustedDevicePubkeys()
	if err != nil {
		t.Fatalf("pubkeys: %v", err)
	}
	if len(pks) != 1 || pks[0] != pk {
		t.Fatalf("TrustedDevicePubkeys = %v, want [%q]", pks, pk)
	}

	// Lookup-by-pubkey resolves the same credential.
	byPk, err := s.LookupTrustedDeviceByPubkey(pk)
	if err != nil {
		t.Fatalf("lookup by pubkey: %v", err)
	}
	if string(byPk.CredentialID) != "c-dk" {
		t.Fatalf("lookup by pubkey returned wrong credential: %q", byPk.CredentialID)
	}
}

// A credential enrolled before the device-key feature has an empty device_pubkey;
// it must never appear in the published set, never resolve by-pubkey for "", and
// can be upgraded in place by SetTrustedDevicePubkey.
func TestTrustedDevicePubkeyEmptyAndUpgrade(t *testing.T) {
	s := openTempStore(t)
	if err := s.SaveTrustedDevice(TrustedDevice{
		CredentialID: []byte("legacy"), PublicKey: []byte("k"), FriendlyName: "old phone",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	// Empty key: not published, not resolvable.
	if pks, _ := s.TrustedDevicePubkeys(); len(pks) != 0 {
		t.Fatalf("expected empty published set, got %v", pks)
	}
	if _, err := s.LookupTrustedDeviceByPubkey(""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty pubkey must not resolve, got %v", err)
	}

	const pk = "bb11bb22cc33dd44ee55ff66007788990011223344556677889900aabbccddeeff0011223344556677889900aabbccddeeff00112233445566778899aabbccdd"
	// Upgrade fills the empty slot.
	if err := s.SetTrustedDevicePubkey([]byte("legacy"), pk, false); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	got, _ := s.LookupTrustedDevice([]byte("legacy"))
	if got.DevicePubkey != pk {
		t.Fatalf("upgrade did not persist: %q", got.DevicePubkey)
	}

	// A second no-overwrite upgrade with a DIFFERENT key adds another browser key
	// for the same synced passkey, but must NOT clobber the legacy single-key slot.
	const pk2 = "cc11bb22cc33dd44ee55ff66007788990011223344556677889900aabbccddeeff0011223344556677889900aabbccddeeff00112233445566778899aabbccdd"
	if err := s.SetTrustedDevicePubkey([]byte("legacy"), pk2, false); err != nil {
		t.Fatalf("no-overwrite upgrade returned err: %v", err)
	}
	got2, _ := s.LookupTrustedDevice([]byte("legacy"))
	if got2.DevicePubkey != pk {
		t.Fatalf("no-overwrite upgrade clobbered legacy pinned key: %q", got2.DevicePubkey)
	}
	pks2, err := s.TrustedDevicePubkeys()
	if err != nil {
		t.Fatalf("pubkeys after second browser key: %v", err)
	}
	wantPks2 := []string{pk, pk2}
	if !reflect.DeepEqual(pks2, wantPks2) {
		t.Fatalf("TrustedDevicePubkeys after second browser key = %v, want %v", pks2, wantPks2)
	}
	byPk2, err := s.LookupTrustedDeviceByPubkey(pk2)
	if err != nil {
		t.Fatalf("lookup second browser key: %v", err)
	}
	if string(byPk2.CredentialID) != "legacy" {
		t.Fatalf("second browser key returned wrong credential: %q", byPk2.CredentialID)
	}

	// Upgrading an unknown credential reports ErrNoRows.
	if err := s.SetTrustedDevicePubkey([]byte("ghost"), pk, false); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("upgrade of unknown credential: want sql.ErrNoRows, got %v", err)
	}
}

func TestTrustedDevicePubkeyRecordsTouchAndDelete(t *testing.T) {
	s := openTempStore(t)
	const pk1 = "dd11bb22cc33dd44ee55ff66007788990011223344556677889900aabbccddeeff0011223344556677889900aabbccddeeff00112233445566778899aabbccdd"
	const pk2 = "ee11bb22cc33dd44ee55ff66007788990011223344556677889900aabbccddeeff0011223344556677889900aabbccddeeff00112233445566778899aabbccdd"
	if err := s.SaveTrustedDevice(TrustedDevice{
		CredentialID: []byte("cred"), PublicKey: []byte("k"), FriendlyName: "sync passkey",
		CreatedAtMs: 1000, LastUsedMs: 2000, DevicePubkey: pk1,
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.SetTrustedDevicePubkey([]byte("cred"), pk2, false); err != nil {
		t.Fatalf("add second browser key: %v", err)
	}
	if err := s.TouchTrustedDevicePubkey(pk2, 9000); err != nil {
		t.Fatalf("touch: %v", err)
	}
	recs, err := s.TrustedDevicePubkeyRecords()
	if err != nil {
		t.Fatalf("records: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("records len = %d, want 2: %+v", len(recs), recs)
	}
	var sawTouched bool
	for _, r := range recs {
		if r.DevicePubkey == pk2 && r.LastUsedMs == 9000 && string(r.CredentialID) == "cred" {
			sawTouched = true
		}
	}
	if !sawTouched {
		t.Fatalf("touched browser key not reflected: %+v", recs)
	}

	if err := s.DeleteTrustedDevicePubkey(pk1); err != nil {
		t.Fatalf("delete legacy/browser key: %v", err)
	}
	dev, err := s.LookupTrustedDevice([]byte("cred"))
	if err != nil {
		t.Fatalf("lookup credential after key delete: %v", err)
	}
	if dev.DevicePubkey != "" {
		t.Fatalf("legacy slot should be cleared after deleting pk1, got %q", dev.DevicePubkey)
	}
	pks, err := s.TrustedDevicePubkeys()
	if err != nil {
		t.Fatalf("pubkeys after delete: %v", err)
	}
	if !reflect.DeepEqual(pks, []string{pk2}) {
		t.Fatalf("pubkeys after delete = %v, want [%q]", pks, pk2)
	}
}

// An old state.db whose trusted_devices table predates the device_pubkey column
// must upgrade cleanly via addColumnIfMissing, with existing rows defaulting to
// the empty key. Simulates the real upgrade: drop the column, re-run migrations.
func TestDevicePubkeyColumnMigratesOldSchema(t *testing.T) {
	s := openTempStore(t)
	// Seed a row, then simulate a pre-column DB by dropping the column.
	if err := s.SaveTrustedDevice(TrustedDevice{
		CredentialID: []byte("pre"), PublicKey: []byte("k"), FriendlyName: "old",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.db.Exec(`ALTER TABLE trusted_devices DROP COLUMN device_pubkey`); err != nil {
		t.Fatalf("drop column to simulate old schema: %v", err)
	}
	// Re-running the column migration must be idempotent and re-add it.
	if err := s.addColumnIfMissing("trusted_devices", "device_pubkey",
		"device_pubkey TEXT NOT NULL DEFAULT ''"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// A second call is a no-op (idempotent).
	if err := s.addColumnIfMissing("trusted_devices", "device_pubkey",
		"device_pubkey TEXT NOT NULL DEFAULT ''"); err != nil {
		t.Fatalf("migrate idempotent: %v", err)
	}
	got, err := s.LookupTrustedDevice([]byte("pre"))
	if err != nil {
		t.Fatalf("lookup after migrate: %v", err)
	}
	if got.DevicePubkey != "" {
		t.Fatalf("migrated row should default to empty device_pubkey, got %q", got.DevicePubkey)
	}
}

// The published set is de-duplicated and sorted so the wire order is stable
// (the relay stores it as a set; a stable order keeps tests + diffs sane).
func TestTrustedDevicePubkeysDedupSorted(t *testing.T) {
	s := openTempStore(t)
	const pkA = "aa00000000000000000000000000000000000000000000000000000000000000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const pkB = "bb00000000000000000000000000000000000000000000000000000000000000bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// Two credentials sharing pkB, one with pkA — expect [pkA, pkB] sorted, deduped.
	mustSave := func(id, pk string) {
		if err := s.SaveTrustedDevice(TrustedDevice{
			CredentialID: []byte(id), PublicKey: []byte("k"), FriendlyName: id, DevicePubkey: pk,
		}); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
	}
	mustSave("c2", pkB)
	mustSave("c1", pkA)
	mustSave("c3", pkB) // duplicate key on a different credential
	pks, err := s.TrustedDevicePubkeys()
	if err != nil {
		t.Fatalf("pubkeys: %v", err)
	}
	want := []string{pkA, pkB}
	if !reflect.DeepEqual(pks, want) {
		t.Fatalf("TrustedDevicePubkeys = %v, want %v", pks, want)
	}
}

func TestTrustedDeviceWalletHandleRoundTrip(t *testing.T) {
	s := openTempStore(t)
	dev := TrustedDevice{
		CredentialID: []byte("c1"), PublicKey: []byte("k"),
		FriendlyName: "phone", WalletHandle: "wallet-abc",
	}
	if err := s.SaveTrustedDevice(dev); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.LookupTrustedDevice(dev.CredentialID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.WalletHandle != "wallet-abc" {
		t.Fatalf("wallet_handle = %q, want wallet-abc", got.WalletHandle)
	}
	list, _ := s.LoadTrustedDevices()
	if len(list) != 1 || list[0].WalletHandle != "wallet-abc" {
		t.Fatalf("load wallet_handle mismatch: %+v", list)
	}
}
