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
	CreatedAtMs  int64
	LastUsedMs   int64
	WalletHandle string // opaque wallet (owner) handle this credential belongs to
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
			(credential_id, public_key, sign_count, aaguid, transports, friendly_name, created_at_ms, last_used_ms, wallet_handle)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.CredentialID, d.PublicKey, int64(d.SignCount), d.AAGUID,
		strings.Join(d.Transports, ","), d.FriendlyName, d.CreatedAtMs, d.LastUsedMs, d.WalletHandle,
	)
	return err
}

// LoadTrustedDevices returns all enrolled passkeys, newest first.
func (s *Store) LoadTrustedDevices() ([]TrustedDevice, error) {
	rows, err := s.db.Query(`
		SELECT credential_id, public_key, sign_count, aaguid, transports, friendly_name, created_at_ms, last_used_ms, wallet_handle
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
		if err := rows.Scan(&d.CredentialID, &d.PublicKey, &signCount, &d.AAGUID, &transports, &d.FriendlyName, &d.CreatedAtMs, &d.LastUsedMs, &d.WalletHandle); err != nil {
			return nil, err
		}
		d.SignCount = uint32(signCount)
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
	err := s.db.QueryRow(`
		SELECT credential_id, public_key, sign_count, aaguid, transports, friendly_name, created_at_ms, last_used_ms, wallet_handle
		FROM trusted_devices WHERE credential_id = ?`, credentialID).
		Scan(&d.CredentialID, &d.PublicKey, &signCount, &d.AAGUID, &transports, &d.FriendlyName, &d.CreatedAtMs, &d.LastUsedMs, &d.WalletHandle)
	if err != nil {
		return d, err
	}
	d.SignCount = uint32(signCount)
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
