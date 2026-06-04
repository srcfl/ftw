package state

// owner_sessions persists authenticated owner-access sessions so a process
// restart doesn't sign everyone out. The in-memory map in the api package is
// the hot path; this is the durable backing it loads on boot and writes on
// each new session.

// OwnerSession is one persisted owner-access session (the ftw_owner cookie).
type OwnerSession struct {
	Token        string
	CredentialID []byte
	ExpiresAtMs  int64
}

// SaveOwnerSession upserts a session token → (credential, expiry).
func (s *Store) SaveOwnerSession(token string, credentialID []byte, expiresAtMs int64) error {
	if credentialID == nil {
		credentialID = []byte{}
	}
	_, err := s.db.Exec(`
		INSERT INTO owner_sessions (token, credential_id, expires_at_ms)
		VALUES (?, ?, ?)
		ON CONFLICT(token) DO UPDATE SET
			credential_id = excluded.credential_id,
			expires_at_ms = excluded.expires_at_ms`,
		token, credentialID, expiresAtMs)
	return err
}

// LoadOwnerSessions returns every persisted session (expired included; the
// caller filters on load and prunes via PruneOwnerSessions).
func (s *Store) LoadOwnerSessions() ([]OwnerSession, error) {
	rows, err := s.db.Query(`SELECT token, credential_id, expires_at_ms FROM owner_sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OwnerSession
	for rows.Next() {
		var o OwnerSession
		if err := rows.Scan(&o.Token, &o.CredentialID, &o.ExpiresAtMs); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteOwnerSession removes a single session (sign-out). Idempotent.
func (s *Store) DeleteOwnerSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM owner_sessions WHERE token = ?`, token)
	return err
}

// PruneOwnerSessions deletes sessions that expired at or before nowMs.
func (s *Store) PruneOwnerSessions(nowMs int64) error {
	_, err := s.db.Exec(`DELETE FROM owner_sessions WHERE expires_at_ms <= ?`, nowMs)
	return err
}
