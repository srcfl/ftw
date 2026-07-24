package state

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
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

func TestHomeLinkEmergencyBlockUsesPinnedDatabaseParent(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	moved := filepath.Join(root, "moved")
	if err := os.Mkdir(live, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(live, "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record := testHomeLinkCredential("001122334455667788", 1)
	if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(live, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(live, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureHomeLinkCredentialEmergencyBlock(
		context.Background(), record.SiteID, record.CredentialID, 2,
	); err != nil {
		t.Fatalf("write through pinned database parent: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(live); err != nil || len(entries) != 0 {
		t.Fatalf("replacement parent was changed: entries=%v err=%v", entries, err)
	}
	if err := os.RemoveAll(live); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, live); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	active, err := restarted.ActiveHomeLinkCredentials(
		context.Background(), record.SiteID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("credential reopened after database parent replacement: %+v", active)
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

func TestHomeLinkPolicyViolationFenceSurvivesFailedStatusWriteAndRestart(t *testing.T) {
	tests := []struct {
		name     string
		stored   uint32
		next     uint32
		storedBE bool
		nextBE   bool
		nextBS   bool
	}{
		{"positive to zero", 4, 0, false, false, false},
		{"equal positive", 4, 4, false, false, false},
		{"counter regression", 4, 3, false, false, false},
		{"backup eligibility changes", 0, 0, false, true, false},
		{"backup state without eligibility", 0, 0, false, false, true},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			record := testHomeLinkCredential("001122334455667788", byte(index+1))
			record.SignCount = test.stored
			record.BackupEligible = test.storedBE
			if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`CREATE TRIGGER homelink_fail_policy_fence
				BEFORE UPDATE OF status ON homelink_credentials
				WHEN OLD.status = 'active' AND NEW.status = 'uncertain'
				BEGIN SELECT RAISE(ABORT, 'policy fence failed'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ApplyHomeLinkAssertion(context.Background(), HomeLinkAssertionUpdate{
				SiteID: record.SiteID, CredentialID: record.CredentialID,
				ExpectedRevision: record.Revision, SignCount: test.next,
				BackupEligible: test.nextBE, BackupState: test.nextBS, UpdatedAtMS: 2,
			}); err == nil {
				t.Fatal("policy violation unexpectedly succeeded")
			}
			var blocks int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM homelink_credential_policy_blocks
				WHERE site_id = ? AND credential_id = ?`,
				record.SiteID, record.CredentialID).Scan(&blocks); err != nil {
				t.Fatal(err)
			}
			if blocks != 1 {
				t.Fatalf("durable policy blocks = %d, want 1", blocks)
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
				t.Fatalf("status after failed policy fence = %q", stored.Status)
			}
			active, err := store.ActiveHomeLinkCredentials(context.Background(), record.SiteID)
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 0 {
				t.Fatalf("policy-blocked credential listed active: %+v", active)
			}
			if _, err := store.ApplyHomeLinkAssertion(context.Background(), HomeLinkAssertionUpdate{
				SiteID: record.SiteID, CredentialID: record.CredentialID,
				ExpectedRevision: record.Revision, SignCount: test.stored + 10,
				BackupEligible: record.BackupEligible, BackupState: record.BackupState,
				UpdatedAtMS: 3,
			}); !errors.Is(err, ErrHomeLinkCredentialInactive) {
				t.Fatalf("later assertion after restart = %v", err)
			}
			if _, err := store.db.Exec(`DROP TRIGGER homelink_fail_policy_fence`); err != nil {
				t.Fatal(err)
			}
			if err := store.RevokeHomeLinkCredential(
				context.Background(), record.SiteID, record.CredentialID, 4,
			); err != nil {
				t.Fatalf("revoke policy-blocked credential: %v", err)
			}
			stored, err = store.HomeLinkCredential(
				context.Background(), record.SiteID, record.CredentialID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != HomeLinkCredentialRevoked {
				t.Fatalf("status after revoke = %q", stored.Status)
			}
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM homelink_credential_policy_blocks
				WHERE site_id = ? AND credential_id = ?`,
				record.SiteID, record.CredentialID).Scan(&blocks); err != nil {
				t.Fatal(err)
			}
			if blocks != 1 {
				t.Fatalf("policy block was removed after revoke: %d", blocks)
			}
		})
	}
}

func TestHomeLinkPolicyViolationFallsBackWhenPolicyIntentWriteFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record := testHomeLinkCredential("001122334455667788", 1)
	record.SignCount = 4
	if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER homelink_fail_policy_intent
		BEFORE INSERT ON homelink_credential_policy_blocks
		BEGIN SELECT RAISE(ABORT, 'policy intent failed'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyHomeLinkAssertion(context.Background(), HomeLinkAssertionUpdate{
		SiteID: record.SiteID, CredentialID: record.CredentialID,
		ExpectedRevision: record.Revision, SignCount: record.SignCount,
		BackupEligible: record.BackupEligible, BackupState: record.BackupState,
		UpdatedAtMS: 2,
	}); err == nil {
		t.Fatal("policy violation unexpectedly succeeded")
	}
	var policyBlocks, revokeBlocks int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM homelink_credential_policy_blocks
		WHERE site_id = ? AND credential_id = ?`,
		record.SiteID, record.CredentialID).Scan(&policyBlocks); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM homelink_credential_revocations
		WHERE site_id = ? AND credential_id = ?`,
		record.SiteID, record.CredentialID).Scan(&revokeBlocks); err != nil {
		t.Fatal(err)
	}
	if policyBlocks != 0 || revokeBlocks != 1 {
		t.Fatalf(
			"fallback blocks policy=%d revoke=%d, want policy=0 revoke=1",
			policyBlocks, revokeBlocks,
		)
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
		t.Fatalf("status after failed policy intent = %q", stored.Status)
	}
	active, err := store.ActiveHomeLinkCredentials(context.Background(), record.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("credential reopened after policy intent failure: %+v", active)
	}
	if _, err := store.ApplyHomeLinkAssertion(context.Background(), HomeLinkAssertionUpdate{
		SiteID: record.SiteID, CredentialID: record.CredentialID,
		ExpectedRevision: record.Revision, SignCount: record.SignCount + 1,
		BackupEligible: record.BackupEligible, BackupState: record.BackupState,
		UpdatedAtMS: 3,
	}); !errors.Is(err, ErrHomeLinkCredentialInactive) {
		t.Fatalf("later assertion after policy intent failure = %v", err)
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

func TestHomeLinkPolicyFenceSerializesWithValidAssertion(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		store := freshStore(t)
		record := testHomeLinkCredential("001122334455667788", byte(iteration+1))
		record.SignCount = 4
		if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, count := range []uint32{4, 5} {
			wg.Add(1)
			go func(count uint32) {
				defer wg.Done()
				<-start
				_, err := store.ApplyHomeLinkAssertion(context.Background(), HomeLinkAssertionUpdate{
					SiteID: record.SiteID, CredentialID: record.CredentialID,
					ExpectedRevision: record.Revision, SignCount: count,
					BackupEligible: record.BackupEligible, BackupState: record.BackupState,
					UpdatedAtMS: int64(count + 10),
				})
				errs <- err
			}(count)
		}
		close(start)
		wg.Wait()
		close(errs)
		var policyDetected bool
		for err := range errs {
			if errors.Is(err, ErrHomeLinkCredentialPolicy) {
				policyDetected = true
			}
		}
		stored, err := store.HomeLinkCredential(
			context.Background(), record.SiteID, record.CredentialID,
		)
		if err != nil {
			t.Fatal(err)
		}
		active, err := store.ActiveHomeLinkCredentials(context.Background(), record.SiteID)
		if err != nil {
			t.Fatal(err)
		}
		if policyDetected {
			if stored.Status != HomeLinkCredentialUncertain || len(active) != 0 {
				t.Fatalf(
					"iteration %d: detected policy violation ended status=%q active=%d",
					iteration, stored.Status, len(active),
				)
			}
			continue
		}
		if stored.Status != HomeLinkCredentialActive || stored.SignCount != 5 || len(active) != 1 {
			t.Fatalf(
				"iteration %d: valid winner ended status=%q count=%d active=%d",
				iteration, stored.Status, stored.SignCount, len(active),
			)
		}
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

func TestHomeLinkEmergencyRevokeBlockPersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record := testHomeLinkCredential("001122334455667788", 21)
	if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureHomeLinkCredentialEmergencyBlock(
		context.Background(), record.SiteID, record.CredentialID, 2,
	); err != nil {
		t.Fatal(err)
	}
	stored, err := store.HomeLinkCredential(
		context.Background(), record.SiteID, record.CredentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != HomeLinkCredentialUncertain {
		t.Fatalf("emergency-blocked status = %q", stored.Status)
	}
	if active, err := store.ActiveHomeLinkCredentials(
		context.Background(), record.SiteID,
	); err != nil {
		t.Fatal(err)
	} else if len(active) != 0 {
		t.Fatalf("emergency-blocked credential listed active: %+v", active)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stored, err = store.HomeLinkCredential(
		context.Background(), record.SiteID, record.CredentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != HomeLinkCredentialUncertain {
		t.Fatalf("emergency block after restart = %q", stored.Status)
	}
	if err := store.RegisterHomeLinkCredential(
		context.Background(), record,
	); !errors.Is(err, ErrHomeLinkCredentialInactive) {
		t.Fatalf("emergency-blocked credential re-registration = %v", err)
	}
}

func TestHomeLinkEmergencyRevokeBlockTamperFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record := testHomeLinkCredential("001122334455667788", 22)
	if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureHomeLinkCredentialEmergencyBlock(
		context.Background(), record.SiteID, record.CredentialID, 2,
	); err != nil {
		t.Fatal(err)
	}
	payload := homeLinkEmergencyBlockPayload(record.SiteID, record.CredentialID)
	marker := filepath.Join(
		path+homeLinkEmergencyBlockSuffix,
		homeLinkEmergencyBlockName(payload),
	)
	if err := os.WriteFile(marker, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActiveHomeLinkCredentials(
		context.Background(), record.SiteID,
	); err == nil {
		t.Fatal("tampered emergency block reopened active credential")
	}
	if _, err := store.ApplyHomeLinkAssertion(
		context.Background(),
		HomeLinkAssertionUpdate{
			SiteID: record.SiteID, CredentialID: record.CredentialID,
			ExpectedRevision: 1, SignCount: 1, UpdatedAtMS: 3,
		},
	); err == nil {
		t.Fatal("tampered emergency block allowed assertion update")
	}
}

func TestHomeLinkEmergencyRevokeBlockRejectsExtraHardLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record := testHomeLinkCredential("001122334455667788", 23)
	if err := store.RegisterHomeLinkCredential(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureHomeLinkCredentialEmergencyBlock(
		context.Background(), record.SiteID, record.CredentialID, 2,
	); err != nil {
		t.Fatal(err)
	}
	payload := homeLinkEmergencyBlockPayload(record.SiteID, record.CredentialID)
	directory := path + homeLinkEmergencyBlockSuffix
	marker := filepath.Join(directory, homeLinkEmergencyBlockName(payload))
	if err := os.Link(marker, filepath.Join(directory, "unexpected-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActiveHomeLinkCredentials(
		context.Background(), record.SiteID,
	); err == nil {
		t.Fatal("hard-linked emergency block reopened active credential")
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
