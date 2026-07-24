package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	_ "modernc.org/sqlite"
)

func testHomeLinkCredential(site string, id byte) HomeLinkCredentialRecord {
	return HomeLinkCredentialRecord{
		SiteID: site, CredentialID: []byte{id}, PublicKey: []byte{0xa1, id},
		Label: "phone", UserHandle: bytes.Repeat([]byte{id}, homeLinkUserHandleBytes),
		Status: HomeLinkCredentialActive, Revision: 1, CreatedAtMS: 1, UpdatedAtMS: 1,
	}
}

func TestHomeLinkCredentialsAreSiteLocalAndPersistVerifierState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	first := testHomeLinkCredential("001122334455667788", 1)
	second := testHomeLinkCredential("112233445566778899", 2)
	if err := store.RegisterHomeLinkCredential(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterHomeLinkCredential(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	active, err := store.ActiveHomeLinkCredentials(context.Background(), first.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || !bytes.Equal(active[0].CredentialID, first.CredentialID) {
		t.Fatalf("site-local credentials = %+v", active)
	}
	if _, err := store.HomeLinkCredential(
		context.Background(), first.SiteID, second.CredentialID,
	); !errors.Is(err, ErrHomeLinkCredentialNotFound) {
		t.Fatalf("cross-site lookup = %v", err)
	}
}

func TestHomeLinkCredentialSurvivesSnapshotRestore(t *testing.T) {
	store := freshStore(t)
	record := testHomeLinkCredential("001122334455667788", 1)
	if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(t.TempDir(), "snapshot.db")
	if err := store.SnapshotTo(snapshotPath); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Open(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { snapshot.Close() })
	restored, err := snapshot.HomeLinkCredential(
		context.Background(), record.SiteID, record.CredentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.PublicKey, record.PublicKey) ||
		!bytes.Equal(restored.UserHandle, record.UserHandle) ||
		restored.Status != HomeLinkCredentialActive {
		t.Fatalf("restored verifier = %+v", restored)
	}
}

func TestHomeLinkRegistrationRequiresOneSiteUserHandle(t *testing.T) {
	store := freshStore(t)
	first := testHomeLinkCredential("001122334455667788", 1)
	if err := store.RegisterHomeLinkCredential(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	sameHandle := testHomeLinkCredential(first.SiteID, 2)
	sameHandle.UserHandle = bytes.Clone(first.UserHandle)
	if err := store.RegisterHomeLinkCredential(context.Background(), sameHandle); err != nil {
		t.Fatalf("same site handle: %v", err)
	}
	differentHandle := testHomeLinkCredential(first.SiteID, 3)
	if err := store.RegisterHomeLinkCredential(context.Background(), differentHandle); err == nil {
		t.Fatal("accepted another user handle for the same site")
	}
}

func TestHomeLinkAssertionCounterAndBackupPolicy(t *testing.T) {
	tests := []struct {
		name       string
		stored     uint32
		next       uint32
		storedBE   bool
		nextBE     bool
		nextBS     bool
		wantStatus HomeLinkCredentialStatus
		wantErr    error
	}{
		{"zero stays zero", 0, 0, false, false, false, HomeLinkCredentialActive, nil},
		{"zero adopts positive", 0, 1, false, false, false, HomeLinkCredentialActive, nil},
		{"positive increases", 4, 5, true, true, true, HomeLinkCredentialActive, nil},
		{"positive to zero", 4, 0, false, false, false, HomeLinkCredentialUncertain, ErrHomeLinkCredentialPolicy},
		{"equal positive", 4, 4, false, false, false, HomeLinkCredentialUncertain, ErrHomeLinkCredentialPolicy},
		{"counter regression", 4, 3, false, false, false, HomeLinkCredentialUncertain, ErrHomeLinkCredentialPolicy},
		{"backup eligibility changes", 0, 0, false, true, false, HomeLinkCredentialUncertain, ErrHomeLinkCredentialPolicy},
		{"backup state without eligibility", 0, 0, false, false, true, HomeLinkCredentialUncertain, ErrHomeLinkCredentialPolicy},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := freshStore(t)
			record := testHomeLinkCredential("001122334455667788", byte(index+1))
			record.SignCount = test.stored
			record.BackupEligible = test.storedBE
			if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			updated, err := store.ApplyHomeLinkAssertion(context.Background(), HomeLinkAssertionUpdate{
				SiteID: record.SiteID, CredentialID: record.CredentialID,
				ExpectedRevision: 1, SignCount: test.next,
				BackupEligible: test.nextBE, BackupState: test.nextBS, UpdatedAtMS: 2,
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("update error = %v, want %v", err, test.wantErr)
			}
			if updated.Status != test.wantStatus {
				t.Fatalf("returned status = %q, want %q", updated.Status, test.wantStatus)
			}
			stored, err := store.HomeLinkCredential(
				context.Background(), record.SiteID, record.CredentialID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != test.wantStatus {
				t.Fatalf("stored status = %q, want %q", stored.Status, test.wantStatus)
			}
		})
	}
}

func TestHomeLinkAssertionRevisionSerializesConcurrentUpdates(t *testing.T) {
	store := freshStore(t)
	record := testHomeLinkCredential("001122334455667788", 1)
	if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, count := range []uint32{1, 2} {
		wg.Add(1)
		go func(count uint32) {
			defer wg.Done()
			<-start
			_, err := store.ApplyHomeLinkAssertion(context.Background(), HomeLinkAssertionUpdate{
				SiteID: record.SiteID, CredentialID: record.CredentialID,
				ExpectedRevision: 1, SignCount: count, UpdatedAtMS: int64(count + 1),
			})
			errs <- err
		}(count)
	}
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful updates = %d, want 1", successes)
	}
}

func TestHomeLinkRevokeFailureStaysUncertainAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record := testHomeLinkCredential("001122334455667788", 1)
	if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER homelink_fail_revoke
		BEFORE UPDATE OF status ON homelink_credentials
		WHEN NEW.status = 'revoked'
		BEGIN SELECT RAISE(ABORT, 'revoke commit failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeHomeLinkCredential(
		context.Background(), record.SiteID, record.CredentialID, 2,
	); err == nil {
		t.Fatal("revoke unexpectedly succeeded")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	stored, err := store.HomeLinkCredential(
		context.Background(), record.SiteID, record.CredentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != HomeLinkCredentialUncertain {
		t.Fatalf("status after failed revoke = %q", stored.Status)
	}
	if active, err := store.ActiveHomeLinkCredentials(context.Background(), record.SiteID); err != nil {
		t.Fatal(err)
	} else if len(active) != 0 {
		t.Fatalf("uncertain credential listed as active: %+v", active)
	}
}

func TestHomeLinkFirstRevokeFenceFailurePersistsIntentAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record := testHomeLinkCredential("001122334455667788", 1)
	if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER homelink_fail_first_revoke_fence
		BEFORE UPDATE OF status ON homelink_credentials
		WHEN OLD.status = 'active' AND NEW.status = 'uncertain'
		BEGIN SELECT RAISE(ABORT, 'first revoke fence failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeHomeLinkCredential(
		context.Background(), record.SiteID, record.CredentialID, 2,
	); err == nil {
		t.Fatal("revoke unexpectedly succeeded")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	stored, err := store.HomeLinkCredential(
		context.Background(), record.SiteID, record.CredentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != HomeLinkCredentialUncertain {
		t.Fatalf("status with pending revoke = %q", stored.Status)
	}
	if active, err := store.ActiveHomeLinkCredentials(context.Background(), record.SiteID); err != nil {
		t.Fatal(err)
	} else if len(active) != 0 {
		t.Fatalf("pending revoke listed active credentials: %+v", active)
	}
	if _, err := store.ApplyHomeLinkAssertion(context.Background(), HomeLinkAssertionUpdate{
		SiteID: record.SiteID, CredentialID: record.CredentialID,
		ExpectedRevision: record.Revision, SignCount: 2,
		BackupEligible: record.BackupEligible, BackupState: record.BackupState,
		UpdatedAtMS: 3,
	}); !errors.Is(err, ErrHomeLinkCredentialInactive) {
		t.Fatalf("assertion with pending revoke = %v", err)
	}
	if _, err := store.db.Exec(`DROP TRIGGER homelink_fail_first_revoke_fence`); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeHomeLinkCredential(
		context.Background(), record.SiteID, record.CredentialID, 4,
	); err != nil {
		t.Fatalf("retry revoke: %v", err)
	}
	stored, err = store.HomeLinkCredential(context.Background(), record.SiteID, record.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != HomeLinkCredentialRevoked {
		t.Fatalf("status after retry = %q", stored.Status)
	}
}

func TestHomeLinkMigrationOpensOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE config(key TEXT PRIMARY KEY NOT NULL, value TEXT NOT NULL);
		INSERT INTO config(key, value) VALUES ('legacy', 'kept')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	var value string
	if err := store.db.QueryRow(`SELECT value FROM config WHERE key = 'legacy'`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "kept" {
		t.Fatalf("legacy value = %q", value)
	}
	var table string
	if err := store.db.QueryRow(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND name = 'homelink_credentials'`).Scan(&table); err != nil {
		t.Fatal(err)
	}
}

func TestHomeLinkSchemaHasNoSecretOrPrivateColumns(t *testing.T) {
	store := freshStore(t)
	rows, err := store.db.Query(`PRAGMA table_info(homelink_credentials)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"secret", "private", "challenge", "assertion"} {
			if bytes.Contains([]byte(name), []byte(forbidden)) {
				t.Fatalf("persistent credential column %q contains forbidden data class", name)
			}
		}
	}
}
