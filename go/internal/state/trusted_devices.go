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
	CredentialID []byte
	PublicKey    []byte
	SignCount    uint32
	AAGUID       []byte
	Transports   []string
	FriendlyName string
	CreatedAtMs    int64
	LastUsedMs     int64
	WalletHandle   string // opaque wallet (owner) handle this credential belongs to
	BackupEligible bool   // WebAuthn BE flag — must round-trip or login rejects synced passkeys
	BackupState    bool   // WebAuthn BS flag
	// DevicePubkey is the per-credential P-256 device key (uncompressed X||Y,
	// 128 lowercase hex chars) minted in the browser at LAN enrollment (C4).
	// Empty when the credential predates the device-key feature. It backs the
	// silent device-PoP login (C3) and is published to the relay (C1) so a
	// browser can prove it before the relay forwards a signaling offer (C2).
	DevicePubkey string
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
	return err
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
	_, err := s.db.Exec(`DELETE FROM trusted_devices WHERE credential_id = ?`, credentialID)
	return err
}

// SetTrustedDevicePubkey pins (or upgrades) the device_pubkey on an existing
// credential's row. Used at enroll/finish to bind the freshly-minted device key
// to the new credential, and on login/finish to upgrade a credential enrolled
// before the device-key feature existed. A non-empty existing value is only
// overwritten when allowOverwrite is true, so a malicious re-presentation can
// never silently repoint an already-pinned device key (defence in depth; the
// caller already gates which keys it accepts). Returns sql.ErrNoRows if the
// credential is unknown.
func (s *Store) SetTrustedDevicePubkey(credentialID []byte, devicePubkey string, allowOverwrite bool) error {
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

// TrustedDevicePubkeys returns the set of non-empty device_pubkeys across all
// enrolled credentials, de-duplicated and sorted for a stable wire order. This
// is the set the Pi publishes to the relay (C1) so a browser can prove
// possession of a trusted device key before the relay forwards its signaling
// offer (C2). An empty result means no enrolled credential carries a device key
// yet (pre-feature credentials), in which case the relay device-gate is closed.
func (s *Store) TrustedDevicePubkeys() ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT device_pubkey FROM trusted_devices
		WHERE device_pubkey <> ''
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
// excludes pre-feature rows whose device_pubkey is ''), so a caller that passes
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
		FROM trusted_devices WHERE device_pubkey = ? AND device_pubkey <> ''`, devicePubkey).
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
