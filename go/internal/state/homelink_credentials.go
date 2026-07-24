package state

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

type HomeLinkCredentialStatus string

const (
	HomeLinkCredentialActive    HomeLinkCredentialStatus = "active"
	HomeLinkCredentialRevoked   HomeLinkCredentialStatus = "revoked"
	HomeLinkCredentialUncertain HomeLinkCredentialStatus = "uncertain"
)

var (
	ErrHomeLinkCredentialNotFound = errors.New("Home Link credential was not found")
	ErrHomeLinkCredentialInactive = errors.New("Home Link credential is not active")
	ErrHomeLinkCredentialConflict = errors.New("Home Link credential state changed")
	ErrHomeLinkCredentialPolicy   = errors.New("Home Link credential policy denied the assertion")
)

const (
	maxHomeLinkSiteIDBytes         = 256
	maxHomeLinkCredentialIDBytes   = 1024
	maxHomeLinkPublicKeyBytes      = 4096
	maxHomeLinkLabelBytes          = 80
	homeLinkUserHandleBytes        = 32
	homeLinkEmergencyBlockSuffix   = ".homelink-blocks"
	homeLinkEmergencyBlockHeader   = "ftw-homelink-emergency-block-v1\n"
	maxHomeLinkEmergencyBlockBytes = 2048
)

// HomeLinkCredentialRecord contains only local WebAuthn verifier state.
type HomeLinkCredentialRecord struct {
	SiteID         string
	CredentialID   []byte
	PublicKey      []byte
	SignCount      uint32
	Label          string
	UserHandle     []byte
	BackupEligible bool
	BackupState    bool
	Status         HomeLinkCredentialStatus
	Revision       int64
	CreatedAtMS    int64
	UpdatedAtMS    int64
}

type HomeLinkAssertionUpdate struct {
	SiteID           string
	CredentialID     []byte
	ExpectedRevision int64
	SignCount        uint32
	BackupEligible   bool
	BackupState      bool
	UpdatedAtMS      int64
}

func (s *Store) RegisterHomeLinkCredential(ctx context.Context, record HomeLinkCredentialRecord) error {
	if err := validateHomeLinkCredential(record, true); err != nil {
		return err
	}
	blocked, err := s.homeLinkCredentialEmergencyBlocked(
		ctx, record.SiteID, record.CredentialID,
	)
	if err != nil {
		return err
	}
	if blocked {
		return ErrHomeLinkCredentialInactive
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin Home Link credential registration: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT user_handle FROM homelink_credentials WHERE site_id = ?`,
		record.SiteID)
	if err != nil {
		return fmt.Errorf("read Home Link site user handle: %w", err)
	}
	var handles int
	for rows.Next() {
		var existingHandle []byte
		if err := rows.Scan(&existingHandle); err != nil {
			rows.Close()
			return fmt.Errorf("read Home Link site user handle: %w", err)
		}
		handles++
		if handles > 1 || !bytes.Equal(existingHandle, record.UserHandle) {
			rows.Close()
			return errors.New("Home Link user handle does not match this site")
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("read Home Link site user handle: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read Home Link site user handle: %w", err)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO homelink_credentials(
		site_id, credential_id, public_key, sign_count, label, user_handle,
		backup_eligible, backup_state, status, revision, created_at_ms, updated_at_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', 1, ?, ?)`,
		record.SiteID, cloneBytes(record.CredentialID), cloneBytes(record.PublicKey),
		int64(record.SignCount), record.Label, cloneBytes(record.UserHandle),
		boolInt(record.BackupEligible), boolInt(record.BackupState),
		record.CreatedAtMS, record.UpdatedAtMS)
	if err != nil {
		return fmt.Errorf("insert Home Link credential: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit Home Link credential registration: %w", err)
	}
	return nil
}

func (s *Store) HomeLinkCredential(
	ctx context.Context,
	siteID string,
	credentialID []byte,
) (HomeLinkCredentialRecord, error) {
	if err := validateHomeLinkLookup(siteID, credentialID); err != nil {
		return HomeLinkCredentialRecord{}, err
	}
	record, err := scanHomeLinkCredential(s.db.QueryRowContext(ctx, homeLinkCredentialSelect+
		` WHERE site_id = ? AND credential_id = ?`, siteID, credentialID))
	if err != nil {
		return HomeLinkCredentialRecord{}, err
	}
	pending, err := homeLinkCredentialBlocked(ctx, s.db, siteID, credentialID)
	if err != nil {
		return HomeLinkCredentialRecord{}, err
	}
	if pending && record.Status == HomeLinkCredentialActive {
		record.Status = HomeLinkCredentialUncertain
	}
	emergencyBlocked, err := s.homeLinkCredentialEmergencyBlocked(ctx, siteID, credentialID)
	if err != nil {
		return HomeLinkCredentialRecord{}, err
	}
	if emergencyBlocked && record.Status == HomeLinkCredentialActive {
		record.Status = HomeLinkCredentialUncertain
	}
	return record, nil
}

func (s *Store) ActiveHomeLinkCredentials(
	ctx context.Context,
	siteID string,
) ([]HomeLinkCredentialRecord, error) {
	if len(siteID) == 0 || len(siteID) > maxHomeLinkSiteIDBytes {
		return nil, errors.New("Home Link site id is invalid")
	}
	rows, err := s.db.QueryContext(ctx, homeLinkCredentialSelect+
		` WHERE site_id = ? AND status = 'active'
		AND NOT EXISTS (
			SELECT 1 FROM homelink_credential_revocations revocations
			WHERE revocations.site_id = homelink_credentials.site_id
			AND revocations.credential_id = homelink_credentials.credential_id
		)
		AND NOT EXISTS (
			SELECT 1 FROM homelink_credential_policy_blocks policy_blocks
			WHERE policy_blocks.site_id = homelink_credentials.site_id
			AND policy_blocks.credential_id = homelink_credentials.credential_id
		)
		ORDER BY credential_id`, siteID)
	if err != nil {
		return nil, fmt.Errorf("list active Home Link credentials: %w", err)
	}
	defer rows.Close()
	var records []HomeLinkCredentialRecord
	for rows.Next() {
		record, err := scanHomeLinkCredential(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list active Home Link credentials: %w", err)
	}
	filtered := records[:0]
	for _, record := range records {
		blocked, err := s.homeLinkCredentialEmergencyBlocked(
			ctx, siteID, record.CredentialID,
		)
		if err != nil {
			return nil, err
		}
		if !blocked {
			filtered = append(filtered, record)
		}
	}
	return filtered, nil
}

// HomeLinkSiteUserHandle returns the one opaque handle already owned by this
// local site. It never performs credential lookup.
func (s *Store) HomeLinkSiteUserHandle(ctx context.Context, siteID string) ([]byte, error) {
	if len(siteID) == 0 || len(siteID) > maxHomeLinkSiteIDBytes {
		return nil, errors.New("Home Link site id is invalid")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT user_handle FROM homelink_credentials WHERE site_id = ?`, siteID)
	if err != nil {
		return nil, fmt.Errorf("read Home Link site user handle: %w", err)
	}
	defer rows.Close()
	var handle []byte
	for rows.Next() {
		var candidate []byte
		if err := rows.Scan(&candidate); err != nil {
			return nil, fmt.Errorf("read Home Link site user handle: %w", err)
		}
		if handle != nil && !bytes.Equal(handle, candidate) {
			return nil, errors.New("stored Home Link user handles are inconsistent")
		}
		handle = cloneBytes(candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Home Link site user handle: %w", err)
	}
	return handle, nil
}

func (s *Store) ApplyHomeLinkAssertion(
	ctx context.Context,
	update HomeLinkAssertionUpdate,
) (HomeLinkCredentialRecord, error) {
	if err := validateHomeLinkLookup(update.SiteID, update.CredentialID); err != nil {
		return HomeLinkCredentialRecord{}, err
	}
	emergencyBlocked, err := s.homeLinkCredentialEmergencyBlocked(
		ctx, update.SiteID, update.CredentialID,
	)
	if err != nil {
		return HomeLinkCredentialRecord{}, err
	}
	if emergencyBlocked {
		return HomeLinkCredentialRecord{}, ErrHomeLinkCredentialInactive
	}
	if update.ExpectedRevision <= 0 || update.UpdatedAtMS <= 0 {
		return HomeLinkCredentialRecord{}, errors.New("Home Link assertion revision or time is invalid")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return HomeLinkCredentialRecord{}, fmt.Errorf("begin Home Link assertion update: %w", err)
	}
	defer tx.Rollback()
	record, err := scanHomeLinkCredential(tx.QueryRowContext(ctx, homeLinkCredentialSelect+
		` WHERE site_id = ? AND credential_id = ?`, update.SiteID, update.CredentialID))
	if err != nil {
		return HomeLinkCredentialRecord{}, err
	}
	if record.Status != HomeLinkCredentialActive {
		return HomeLinkCredentialRecord{}, ErrHomeLinkCredentialInactive
	}
	pending, err := homeLinkCredentialBlocked(ctx, tx, update.SiteID, update.CredentialID)
	if err != nil {
		return HomeLinkCredentialRecord{}, err
	}
	if pending {
		return HomeLinkCredentialRecord{}, ErrHomeLinkCredentialInactive
	}
	if record.Revision != update.ExpectedRevision {
		return HomeLinkCredentialRecord{}, ErrHomeLinkCredentialConflict
	}

	policyOK := update.BackupEligible == record.BackupEligible &&
		(!update.BackupState || update.BackupEligible) &&
		validHomeLinkCounterTransition(record.SignCount, update.SignCount)
	if !policyOK {
		rollbackErr := tx.Rollback()
		if err := s.EnsureHomeLinkCredentialPolicyBlock(
			ctx, update.SiteID, update.CredentialID, update.UpdatedAtMS,
		); err != nil {
			return HomeLinkCredentialRecord{}, errors.Join(
				ErrHomeLinkCredentialPolicy,
				fmt.Errorf("persist Home Link policy block: %w", err),
			)
		}
		if rollbackErr != nil {
			return HomeLinkCredentialRecord{}, errors.Join(
				ErrHomeLinkCredentialPolicy,
				fmt.Errorf("rollback denied Home Link assertion: %w", rollbackErr),
			)
		}
		fence, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return HomeLinkCredentialRecord{}, errors.Join(
				ErrHomeLinkCredentialPolicy,
				fmt.Errorf("begin Home Link policy fence: %w", err),
			)
		}
		defer fence.Rollback()
		result, err := fence.ExecContext(ctx, `UPDATE homelink_credentials SET
			sign_count = ?, backup_state = ?, status = 'uncertain',
			revision = revision + 1, updated_at_ms = ?
			WHERE site_id = ? AND credential_id = ? AND status = 'active' AND revision = ?`,
			int64(update.SignCount), boolInt(update.BackupState), update.UpdatedAtMS,
			update.SiteID, update.CredentialID, update.ExpectedRevision)
		if err != nil {
			return HomeLinkCredentialRecord{}, errors.Join(
				ErrHomeLinkCredentialPolicy,
				fmt.Errorf("commit Home Link policy fence: %w", err),
			)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return HomeLinkCredentialRecord{}, errors.Join(
				ErrHomeLinkCredentialPolicy,
				fmt.Errorf("check Home Link policy fence: %w", err),
			)
		}
		if changed != 1 {
			return HomeLinkCredentialRecord{}, ErrHomeLinkCredentialPolicy
		}
		if err := fence.Commit(); err != nil {
			return HomeLinkCredentialRecord{}, errors.Join(
				ErrHomeLinkCredentialPolicy,
				fmt.Errorf("commit Home Link policy fence: %w", err),
			)
		}
		record.SignCount = update.SignCount
		record.BackupState = update.BackupState
		record.Status = HomeLinkCredentialUncertain
		record.Revision++
		record.UpdatedAtMS = update.UpdatedAtMS
		return record, ErrHomeLinkCredentialPolicy
	}
	result, err := tx.ExecContext(ctx, `UPDATE homelink_credentials SET
		sign_count = ?, backup_state = ?, status = ?, revision = revision + 1,
		updated_at_ms = ?
		WHERE site_id = ? AND credential_id = ? AND status = 'active' AND revision = ?
		AND NOT EXISTS (
			SELECT 1 FROM homelink_credential_revocations
			WHERE site_id = ? AND credential_id = ?
		)
		AND NOT EXISTS (
			SELECT 1 FROM homelink_credential_policy_blocks
			WHERE site_id = ? AND credential_id = ?
		)`,
		int64(update.SignCount), boolInt(update.BackupState), string(HomeLinkCredentialActive), update.UpdatedAtMS,
		update.SiteID, update.CredentialID, update.ExpectedRevision,
		update.SiteID, update.CredentialID, update.SiteID, update.CredentialID)
	if err != nil {
		return HomeLinkCredentialRecord{}, fmt.Errorf("update Home Link assertion state: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return HomeLinkCredentialRecord{}, fmt.Errorf("check Home Link assertion update: %w", err)
	}
	if changed != 1 {
		return HomeLinkCredentialRecord{}, ErrHomeLinkCredentialConflict
	}
	if err := tx.Commit(); err != nil {
		return HomeLinkCredentialRecord{}, fmt.Errorf("commit Home Link assertion state: %w", err)
	}
	record.SignCount = update.SignCount
	record.BackupState = update.BackupState
	record.Status = HomeLinkCredentialActive
	record.Revision++
	record.UpdatedAtMS = update.UpdatedAtMS
	return record, nil
}

// EnsureHomeLinkCredentialPolicyBlock commits a permanent fail-closed marker
// when a verified assertion violates policy or its verifier-state write is
// ambiguous. If the dedicated marker cannot be written, the permanent revoke
// marker is a conservative fallback. A successful return always includes a
// read-back proving that the credential is no longer active.
func (s *Store) EnsureHomeLinkCredentialPolicyBlock(
	ctx context.Context,
	siteID string,
	credentialID []byte,
	nowMS int64,
) error {
	if err := validateHomeLinkLookup(siteID, credentialID); err != nil {
		return err
	}
	if nowMS <= 0 {
		return errors.New("Home Link policy block time is invalid")
	}
	_, policyErr := s.db.ExecContext(ctx, `INSERT INTO homelink_credential_policy_blocks(
		site_id, credential_id, started_at_ms
	) VALUES (?, ?, ?)
	ON CONFLICT(site_id, credential_id) DO NOTHING`,
		siteID, cloneBytes(credentialID), nowMS)
	if policyErr != nil {
		if _, revokeErr := s.db.ExecContext(ctx, `INSERT INTO homelink_credential_revocations(
			site_id, credential_id, started_at_ms
		) VALUES (?, ?, ?)
		ON CONFLICT(site_id, credential_id) DO NOTHING`,
			siteID, cloneBytes(credentialID), nowMS); revokeErr != nil {
			return errors.Join(
				fmt.Errorf("commit Home Link policy block: %w", policyErr),
				fmt.Errorf("commit conservative Home Link revoke block: %w", revokeErr),
			)
		}
	}
	record, err := s.HomeLinkCredential(ctx, siteID, credentialID)
	if err != nil {
		return fmt.Errorf("read back Home Link policy block: %w", err)
	}
	if record.Status == HomeLinkCredentialActive {
		return errors.New("Home Link policy block was not durable")
	}
	return nil
}

// RevokeHomeLinkCredential first commits a permanent intent row. All active
// reads and assertion updates consult that row, so any later error remains
// fail-closed across restart.
func (s *Store) RevokeHomeLinkCredential(ctx context.Context, siteID string, credentialID []byte, nowMS int64) error {
	if err := validateHomeLinkLookup(siteID, credentialID); err != nil {
		return err
	}
	if nowMS <= 0 {
		return errors.New("Home Link revoke time is invalid")
	}
	if err := s.EnsureHomeLinkCredentialEmergencyBlock(
		ctx, siteID, credentialID, nowMS,
	); err != nil {
		return err
	}
	record, err := scanHomeLinkCredential(s.db.QueryRowContext(
		ctx,
		homeLinkCredentialSelect+` WHERE site_id = ? AND credential_id = ?`,
		siteID,
		credentialID,
	))
	if err != nil {
		return err
	}
	if record.Status == HomeLinkCredentialRevoked {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO homelink_credential_revocations(
		site_id, credential_id, started_at_ms
	) VALUES (?, ?, ?)
	ON CONFLICT(site_id, credential_id) DO NOTHING`,
		siteID, cloneBytes(credentialID), nowMS); err != nil {
		return fmt.Errorf("commit Home Link revoke intent: %w", err)
	}
	if record.Status == HomeLinkCredentialActive {
		result, err := s.db.ExecContext(ctx, `UPDATE homelink_credentials SET
			status = 'uncertain', revision = revision + 1, updated_at_ms = ?
			WHERE site_id = ? AND credential_id = ? AND status = 'active' AND revision = ?`,
			nowMS, siteID, credentialID, record.Revision)
		if err != nil {
			return fmt.Errorf("commit Home Link revoke fence: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check Home Link revoke fence: %w", err)
		}
		if changed != 1 {
			return ErrHomeLinkCredentialConflict
		}
	}
	result, err := s.db.ExecContext(ctx, `UPDATE homelink_credentials SET
		status = 'revoked', revision = revision + 1, updated_at_ms = ?
		WHERE site_id = ? AND credential_id = ? AND status IN ('active', 'uncertain')`,
		nowMS, siteID, credentialID)
	if err != nil {
		return fmt.Errorf("commit Home Link revoke: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check Home Link revoke: %w", err)
	}
	if changed != 1 {
		return ErrHomeLinkCredentialConflict
	}
	return nil
}

// EnsureHomeLinkCredentialEmergencyBlock writes an append-only marker beside
// state.db before SQLite revocation starts. The marker is a second durable
// fail-closed path for a valid revoke request when SQLite is unavailable. It is
// never removed or reused for another credential.
func (s *Store) EnsureHomeLinkCredentialEmergencyBlock(
	ctx context.Context,
	siteID string,
	credentialID []byte,
	nowMS int64,
) error {
	if err := validateHomeLinkLookup(siteID, credentialID); err != nil {
		return err
	}
	if nowMS <= 0 {
		return errors.New("Home Link emergency block time is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	payload := homeLinkEmergencyBlockPayload(siteID, credentialID)
	name := homeLinkEmergencyBlockName(payload)

	s.homeLinkFenceMu.Lock()
	defer s.homeLinkFenceMu.Unlock()
	parent, directory, err := s.homeLinkEmergencyBlockRoot(true)
	if err != nil {
		return err
	}
	defer parent.Close()
	defer directory.Close()
	if blocked, err := readHomeLinkEmergencyBlock(
		ctx, directory, name, payload,
	); err != nil || blocked {
		return err
	}
	file, err := directory.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			_, verifyErr := readHomeLinkEmergencyBlock(ctx, directory, name, payload)
			return verifyErr
		}
		return fmt.Errorf("create Home Link emergency block: %w", err)
	}
	writeErr := writeFull(file, payload)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write Home Link emergency block: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close Home Link emergency block: %w", closeErr)
	}
	dirFile, err := directory.Open(".")
	if err != nil {
		return fmt.Errorf("open Home Link emergency block directory: %w", err)
	}
	syncErr := dirFile.Sync()
	closeErr = dirFile.Close()
	if syncErr != nil {
		return fmt.Errorf("sync Home Link emergency block directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close Home Link emergency block directory: %w", closeErr)
	}
	blocked, err := readHomeLinkEmergencyBlock(ctx, directory, name, payload)
	if err != nil {
		return err
	}
	if !blocked {
		return errors.New("Home Link emergency block was not durable")
	}
	return nil
}

func (s *Store) homeLinkCredentialEmergencyBlocked(
	ctx context.Context,
	siteID string,
	credentialID []byte,
) (bool, error) {
	if err := validateHomeLinkLookup(siteID, credentialID); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	payload := homeLinkEmergencyBlockPayload(siteID, credentialID)
	name := homeLinkEmergencyBlockName(payload)

	s.homeLinkFenceMu.Lock()
	defer s.homeLinkFenceMu.Unlock()
	parent, directory, err := s.homeLinkEmergencyBlockRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer parent.Close()
	defer directory.Close()
	return readHomeLinkEmergencyBlock(ctx, directory, name, payload)
}

func (s *Store) homeLinkEmergencyBlockRoot(
	create bool,
) (*os.Root, *os.Root, error) {
	absolute, err := filepath.Abs(s.mainDBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve Home Link emergency block root: %w", err)
	}
	parent, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return nil, nil, fmt.Errorf("open Home Link emergency block root: %w", err)
	}
	directoryName := filepath.Base(absolute) + homeLinkEmergencyBlockSuffix
	info, err := parent.Lstat(directoryName)
	created := false
	if errors.Is(err, os.ErrNotExist) && create {
		if err := parent.Mkdir(directoryName, 0o700); err != nil &&
			!errors.Is(err, os.ErrExist) {
			parent.Close()
			return nil, nil, fmt.Errorf("create Home Link emergency block directory: %w", err)
		}
		created = true
		info, err = parent.Lstat(directoryName)
	}
	if err != nil {
		parent.Close()
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		parent.Close()
		return nil, nil, errors.New("Home Link emergency block directory is unsafe")
	}
	if created {
		parentFile, err := parent.Open(".")
		if err != nil {
			parent.Close()
			return nil, nil, fmt.Errorf("open Home Link emergency block parent: %w", err)
		}
		syncErr := parentFile.Sync()
		closeErr := parentFile.Close()
		if syncErr != nil {
			parent.Close()
			return nil, nil, fmt.Errorf("sync Home Link emergency block parent: %w", syncErr)
		}
		if closeErr != nil {
			parent.Close()
			return nil, nil, fmt.Errorf("close Home Link emergency block parent: %w", closeErr)
		}
	}
	directory, err := parent.OpenRoot(directoryName)
	if err != nil {
		parent.Close()
		return nil, nil, fmt.Errorf("open Home Link emergency block directory: %w", err)
	}
	opened, err := directory.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		directory.Close()
		parent.Close()
		return nil, nil, errors.New("Home Link emergency block directory changed")
	}
	after, err := parent.Lstat(directoryName)
	if err != nil || !os.SameFile(opened, after) {
		directory.Close()
		parent.Close()
		return nil, nil, errors.New("Home Link emergency block directory changed")
	}
	return parent, directory, nil
}

func homeLinkEmergencyBlockPayload(siteID string, credentialID []byte) []byte {
	return []byte(homeLinkEmergencyBlockHeader + siteID + "\n" +
		base64.RawURLEncoding.EncodeToString(credentialID) + "\n")
}

func homeLinkEmergencyBlockName(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func readHomeLinkEmergencyBlock(
	ctx context.Context,
	root *os.Root,
	name string,
	want []byte,
) (bool, error) {
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Home Link emergency block: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Mode().Perm()&0o077 != 0 ||
		before.Size() <= 0 || before.Size() > maxHomeLinkEmergencyBlockBytes {
		return false, errors.New("Home Link emergency block is unsafe")
	}
	file, err := root.Open(name)
	if err != nil {
		return false, fmt.Errorf("open Home Link emergency block: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		file.Close()
		return false, errors.New("Home Link emergency block changed before read")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxHomeLinkEmergencyBlockBytes+1))
	afterFD, statErr := file.Stat()
	closeErr := file.Close()
	afterPath, pathErr := root.Lstat(name)
	if readErr != nil {
		return false, fmt.Errorf("read Home Link emergency block: %w", readErr)
	}
	if statErr != nil || pathErr != nil || !os.SameFile(opened, afterFD) ||
		!os.SameFile(afterFD, afterPath) || int64(len(data)) != afterFD.Size() {
		return false, errors.New("Home Link emergency block changed during read")
	}
	if closeErr != nil {
		return false, fmt.Errorf("close Home Link emergency block: %w", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !bytes.Equal(data, want) {
		return false, errors.New("Home Link emergency block content is invalid")
	}
	return true, nil
}

func writeFull(file *os.File, data []byte) error {
	for len(data) > 0 {
		written, err := file.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

type homeLinkQueryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func homeLinkCredentialBlocked(
	ctx context.Context,
	db homeLinkQueryRower,
	siteID string,
	credentialID []byte,
) (bool, error) {
	var blocked int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM homelink_credential_revocations
		WHERE site_id = ? AND credential_id = ?
	) OR EXISTS(
		SELECT 1 FROM homelink_credential_policy_blocks
		WHERE site_id = ? AND credential_id = ?
	)`, siteID, credentialID, siteID, credentialID).Scan(&blocked); err != nil {
		return false, fmt.Errorf("read Home Link credential block: %w", err)
	}
	return blocked == 1, nil
}

const homeLinkCredentialSelect = `SELECT
	site_id, credential_id, public_key, sign_count, label, user_handle,
	backup_eligible, backup_state, status, revision, created_at_ms, updated_at_ms
	FROM homelink_credentials`

type rowScanner interface {
	Scan(...any) error
}

func scanHomeLinkCredential(row rowScanner) (HomeLinkCredentialRecord, error) {
	var record HomeLinkCredentialRecord
	var count int64
	var backupEligible, backupState int
	err := row.Scan(
		&record.SiteID, &record.CredentialID, &record.PublicKey, &count, &record.Label,
		&record.UserHandle, &backupEligible, &backupState, &record.Status, &record.Revision,
		&record.CreatedAtMS, &record.UpdatedAtMS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return HomeLinkCredentialRecord{}, ErrHomeLinkCredentialNotFound
	}
	if err != nil {
		return HomeLinkCredentialRecord{}, fmt.Errorf("read Home Link credential: %w", err)
	}
	if count < 0 || count > math.MaxUint32 {
		return HomeLinkCredentialRecord{}, errors.New("stored Home Link counter is invalid")
	}
	record.SignCount = uint32(count)
	record.BackupEligible = backupEligible == 1
	record.BackupState = backupState == 1
	record.CredentialID = cloneBytes(record.CredentialID)
	record.PublicKey = cloneBytes(record.PublicKey)
	record.UserHandle = cloneBytes(record.UserHandle)
	if err := validateHomeLinkCredential(record, false); err != nil {
		return HomeLinkCredentialRecord{}, fmt.Errorf("stored Home Link credential is invalid: %w", err)
	}
	return record, nil
}

func validateHomeLinkCredential(record HomeLinkCredentialRecord, registration bool) error {
	if err := validateHomeLinkLookup(record.SiteID, record.CredentialID); err != nil {
		return err
	}
	if len(record.PublicKey) == 0 || len(record.PublicKey) > maxHomeLinkPublicKeyBytes {
		return errors.New("Home Link public key is invalid")
	}
	if strings.TrimSpace(record.Label) == "" || len(record.Label) > maxHomeLinkLabelBytes {
		return errors.New("Home Link credential label is invalid")
	}
	if len(record.UserHandle) != homeLinkUserHandleBytes {
		return errors.New("Home Link user handle is invalid")
	}
	if record.BackupState && !record.BackupEligible &&
		(registration || record.Status == HomeLinkCredentialActive) {
		return errors.New("Home Link backup state is inconsistent")
	}
	if registration {
		if record.Status != HomeLinkCredentialActive || record.Revision != 1 ||
			record.CreatedAtMS <= 0 || record.UpdatedAtMS <= 0 {
			return errors.New("new Home Link credential state is invalid")
		}
	} else if record.Status != HomeLinkCredentialActive &&
		record.Status != HomeLinkCredentialRevoked &&
		record.Status != HomeLinkCredentialUncertain {
		return errors.New("Home Link credential status is invalid")
	}
	return nil
}

func validateHomeLinkLookup(siteID string, credentialID []byte) error {
	if len(siteID) == 0 || len(siteID) > maxHomeLinkSiteIDBytes {
		return errors.New("Home Link site id is invalid")
	}
	if len(credentialID) == 0 || len(credentialID) > maxHomeLinkCredentialIDBytes {
		return errors.New("Home Link credential id is invalid")
	}
	return nil
}

func validHomeLinkCounterTransition(stored, next uint32) bool {
	if stored == 0 && next == 0 {
		return true
	}
	return next > stored
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func cloneBytes(value []byte) []byte {
	return bytes.Clone(value)
}
