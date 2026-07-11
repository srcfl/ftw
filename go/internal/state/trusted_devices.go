package state

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// TrustedDevice is one passkey the operator has enrolled for owner
// remote access. credential_id is the authenticator-assigned handle
// the browser sends back on every login.
type TrustedDevice struct {
	CredentialID   []byte
	PublicKey      []byte
	SignCount      uint32
	AAGUID         []byte
	Transports     []string
	FriendlyName   string
	CreatedAtMs    int64
	LastUsedMs     int64
	WalletHandle   string // opaque wallet (owner) handle this credential belongs to
	BackupEligible bool   // WebAuthn BE flag — must round-trip or login rejects synced passkeys
	BackupState    bool   // WebAuthn BS flag
	// DevicePubkey is the legacy primary browser key slot for this credential.
	// Additional per-browser keys live in trusted_device_pubkeys so synced passkeys
	// can remember several browsers/devices without clobbering this column.
	DevicePubkey string
}

// TrustedDevicePubkey is one remembered browser-local device key attached to a
// passkey credential. DevicePubkey is public key material; callers can hash it
// before exposing it as a UI handle.
type TrustedDevicePubkey struct {
	DevicePubkey string
	CredentialID []byte
	CreatedAtMs  int64
	LastUsedMs   int64
	Legacy       bool
}

func boolToInt64(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// SaveTrustedDevice inserts a new passkey. The credential_id PK
// guarantees no duplicate enrollment of the same authenticator.
func (s *Store) SaveTrustedDevice(d TrustedDevice) error {
	if len(d.CredentialID) == 0 || len(d.PublicKey) == 0 {
		return errors.New("trusted device requires credential_id and public_key")
	}
	if d.FriendlyName == "" {
		return errors.New("trusted device requires friendly_name")
	}
	if d.CreatedAtMs == 0 {
		d.CreatedAtMs = time.Now().UnixMilli()
	}
	// SQLite STRICT mode rejects a nil for a NOT NULL BLOB even with a
	// non-NULL DEFAULT, because the driver sends explicit NULL bytes.
	// Coerce to empty slice instead.
	if d.AAGUID == nil {
		d.AAGUID = []byte{}
	}
	_, err := s.db.Exec(`
		INSERT INTO trusted_devices
			(credential_id, public_key, sign_count, aaguid, transports, friendly_name, created_at_ms, last_used_ms, wallet_handle, backup_eligible, backup_state, device_pubkey)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.CredentialID, d.PublicKey, int64(d.SignCount), d.AAGUID,
		strings.Join(d.Transports, ","), d.FriendlyName, d.CreatedAtMs, d.LastUsedMs, d.WalletHandle,
		boolToInt64(d.BackupEligible), boolToInt64(d.BackupState), d.DevicePubkey,
	)
	if err != nil {
		return err
	}
	if d.DevicePubkey != "" {
		if err := s.insertTrustedDevicePubkey(d.CredentialID, d.DevicePubkey, d.CreatedAtMs, d.LastUsedMs); err != nil {
			return err
		}
	}
	return nil
}

// LoadTrustedDevices returns all enrolled passkeys, newest first.
func (s *Store) LoadTrustedDevices() ([]TrustedDevice, error) {
	rows, err := s.db.Query(`
		SELECT credential_id, public_key, sign_count, aaguid, transports, friendly_name, created_at_ms, last_used_ms, wallet_handle, backup_eligible, backup_state, device_pubkey
		FROM trusted_devices
		ORDER BY created_at_ms DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrustedDevice
	for rows.Next() {
		var d TrustedDevice
		var signCount int64
		var transports string
		var be, bs int64
		if err := rows.Scan(&d.CredentialID, &d.PublicKey, &signCount, &d.AAGUID, &transports, &d.FriendlyName, &d.CreatedAtMs, &d.LastUsedMs, &d.WalletHandle, &be, &bs, &d.DevicePubkey); err != nil {
			return nil, err
		}
		d.SignCount = uint32(signCount)
		d.BackupEligible = be != 0
		d.BackupState = bs != 0
		if transports != "" {
			d.Transports = strings.Split(transports, ",")
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LookupTrustedDevice returns the device with the given credential_id,
// or sql.ErrNoRows if unknown.
func (s *Store) LookupTrustedDevice(credentialID []byte) (TrustedDevice, error) {
	var d TrustedDevice
	var signCount int64
	var transports string
	var be, bs int64
	err := s.db.QueryRow(`
		SELECT credential_id, public_key, sign_count, aaguid, transports, friendly_name, created_at_ms, last_used_ms, wallet_handle, backup_eligible, backup_state, device_pubkey
		FROM trusted_devices WHERE credential_id = ?`, credentialID).
		Scan(&d.CredentialID, &d.PublicKey, &signCount, &d.AAGUID, &transports, &d.FriendlyName, &d.CreatedAtMs, &d.LastUsedMs, &d.WalletHandle, &be, &bs, &d.DevicePubkey)
	if err != nil {
		return d, err
	}
	d.SignCount = uint32(signCount)
	d.BackupEligible = be != 0
	d.BackupState = bs != 0
	if transports != "" {
		d.Transports = strings.Split(transports, ",")
	}
	return d, nil
}

// UpdateTrustedDeviceSignCount records a successful login. sign_count
// must monotonically increase per WebAuthn spec; a regression is a
// signal the authenticator was cloned (caller must reject the login).
func (s *Store) UpdateTrustedDeviceSignCount(credentialID []byte, newCount uint32, lastUsedMs int64) error {
	res, err := s.db.Exec(`
		UPDATE trusted_devices
		SET sign_count = ?, last_used_ms = ?
		WHERE credential_id = ?`,
		int64(newCount), lastUsedMs, credentialID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteTrustedDevice removes a passkey by credential_id. Idempotent.
func (s *Store) DeleteTrustedDevice(credentialID []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM trusted_device_pubkeys WHERE credential_id = ?`, credentialID); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM trusted_devices WHERE credential_id = ?`, credentialID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SetTrustedDevicePubkey pins a browser-local device key to an existing
// credential. The legacy trusted_devices.device_pubkey column is kept for old
// readers and is only overwritten when allowOverwrite is true, but every
// canonical browser key is also recorded in trusted_device_pubkeys so one synced
// WebAuthn credential can remember several browsers/devices.
func (s *Store) SetTrustedDevicePubkey(credentialID []byte, devicePubkey string, allowOverwrite bool) error {
	if devicePubkey == "" {
		return nil
	}
	var dummy int
	if err := s.db.QueryRow(`SELECT 1 FROM trusted_devices WHERE credential_id = ?`, credentialID).Scan(&dummy); err != nil {
		if err == sql.ErrNoRows {
			return sql.ErrNoRows
		}
		return err
	}
	now := time.Now().UnixMilli()
	if err := s.insertTrustedDevicePubkey(credentialID, devicePubkey, now, now); err != nil {
		return err
	}
	q := `UPDATE trusted_devices SET device_pubkey = ? WHERE credential_id = ?`
	if !allowOverwrite {
		// Only fill when currently empty — never clobber an already-pinned key.
		q = `UPDATE trusted_devices SET device_pubkey = ? WHERE credential_id = ? AND device_pubkey = ''`
	}
	res, err := s.db.Exec(q, devicePubkey, credentialID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either the credential is unknown, or (no-overwrite path) it already has
		// a pinned key. Disambiguate with a presence check so callers that only
		// want "is this credential gone?" get the right signal.
		var dummy int
		if qerr := s.db.QueryRow(`SELECT 1 FROM trusted_devices WHERE credential_id = ?`, credentialID).Scan(&dummy); qerr == sql.ErrNoRows {
			return sql.ErrNoRows
		}
	}
	return nil
}

func (s *Store) insertTrustedDevicePubkey(credentialID []byte, devicePubkey string, createdAtMs, lastUsedMs int64) error {
	if devicePubkey == "" {
		return nil
	}
	if createdAtMs == 0 {
		createdAtMs = time.Now().UnixMilli()
	}
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO trusted_device_pubkeys
			(device_pubkey, credential_id, created_at_ms, last_used_ms)
		VALUES (?, ?, ?, ?)`,
		devicePubkey, credentialID, createdAtMs, lastUsedMs)
	return err
}

// TrustedDevicePubkeyRecords returns each remembered browser key with its owning
// credential. It includes the legacy trusted_devices.device_pubkey column so
// older DBs remain visible after migration.
func (s *Store) TrustedDevicePubkeyRecords() ([]TrustedDevicePubkey, error) {
	rows, err := s.db.Query(`
		SELECT device_pubkey, credential_id, MIN(created_at_ms), MAX(last_used_ms), MAX(legacy)
		FROM (
			SELECT device_pubkey, credential_id, created_at_ms, last_used_ms, 0 AS legacy
			FROM trusted_device_pubkeys
			WHERE device_pubkey <> ''
			UNION ALL
			SELECT device_pubkey, credential_id, created_at_ms, last_used_ms, 1 AS legacy
			FROM trusted_devices
			WHERE device_pubkey <> ''
		)
		GROUP BY device_pubkey, credential_id
		ORDER BY MAX(last_used_ms) DESC, MIN(created_at_ms) DESC, device_pubkey ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrustedDevicePubkey
	for rows.Next() {
		var r TrustedDevicePubkey
		var legacy int64
		if err := rows.Scan(&r.DevicePubkey, &r.CredentialID, &r.CreatedAtMs, &r.LastUsedMs, &legacy); err != nil {
			return nil, err
		}
		r.Legacy = legacy != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// TouchTrustedDevicePubkey records successful use of a remembered browser key.
func (s *Store) TouchTrustedDevicePubkey(devicePubkey string, lastUsedMs int64) error {
	if devicePubkey == "" {
		return nil
	}
	if lastUsedMs == 0 {
		lastUsedMs = time.Now().UnixMilli()
	}
	if _, err := s.db.Exec(`UPDATE trusted_device_pubkeys SET last_used_ms = ? WHERE device_pubkey = ?`, lastUsedMs, devicePubkey); err != nil {
		return err
	}
	_, err := s.db.Exec(`UPDATE trusted_devices SET last_used_ms = ? WHERE device_pubkey = ?`, lastUsedMs, devicePubkey)
	return err
}

// DeleteTrustedDevicePubkey removes one remembered browser key. It also clears
// the legacy single-key column if it still points at the same key.
func (s *Store) DeleteTrustedDevicePubkey(devicePubkey string) error {
	if devicePubkey == "" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM trusted_device_pubkeys WHERE device_pubkey = ?`, devicePubkey); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`UPDATE trusted_devices SET device_pubkey = '' WHERE device_pubkey = ?`, devicePubkey); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// TrustedDevicePubkeys returns the set of non-empty device_pubkeys across all
// enrolled credentials, de-duplicated and sorted for a stable wire order. This
// is the set the Pi publishes to the relay (C1) so a browser can prove
// possession of a trusted device key before the relay forwards its signaling
// offer (C2). An empty result means no enrolled credential carries a device key
// yet (pre-feature credentials), in which case the relay device-gate is closed.
func (s *Store) TrustedDevicePubkeys() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT device_pubkey FROM trusted_device_pubkeys
		UNION
		SELECT device_pubkey FROM trusted_devices WHERE device_pubkey <> ''
		ORDER BY device_pubkey ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pk string
		if err := rows.Scan(&pk); err != nil {
			return nil, err
		}
		out = append(out, pk)
	}
	return out, rows.Err()
}

// LookupTrustedDeviceByPubkey returns the credential whose device_pubkey matches
// (exactly, byte-for-byte) the supplied key, or sql.ErrNoRows if no pinned
// credential carries it. The empty string never matches a row (the WHERE guard
// excludes pre-feature rows whose device_pubkey is empty), so a caller that passes
// "" can never be mistaken for a trusted device.
func (s *Store) LookupTrustedDeviceByPubkey(devicePubkey string) (TrustedDevice, error) {
	var d TrustedDevice
	if devicePubkey == "" {
		return d, sql.ErrNoRows
	}
	var signCount int64
	var transports string
	var be, bs int64
	err := s.db.QueryRow(`
		SELECT credential_id, public_key, sign_count, aaguid, transports, friendly_name, created_at_ms, last_used_ms, wallet_handle, backup_eligible, backup_state, device_pubkey
		FROM trusted_devices
		WHERE credential_id = (
			SELECT credential_id FROM trusted_device_pubkeys WHERE device_pubkey = ?
			UNION
			SELECT credential_id FROM trusted_devices WHERE device_pubkey = ? AND device_pubkey <> ''
			LIMIT 1
		)`, devicePubkey, devicePubkey).
		Scan(&d.CredentialID, &d.PublicKey, &signCount, &d.AAGUID, &transports, &d.FriendlyName, &d.CreatedAtMs, &d.LastUsedMs, &d.WalletHandle, &be, &bs, &d.DevicePubkey)
	if err != nil {
		return d, err
	}
	d.SignCount = uint32(signCount)
	d.BackupEligible = be != 0
	d.BackupState = bs != 0
	if transports != "" {
		d.Transports = strings.Split(transports, ",")
	}
	return d, nil
}
