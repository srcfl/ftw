package homelink

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestRemoteRuntimeStatusRedactsLocalErrorToFixedCode(t *testing.T) {
	status := &RemoteRuntimeStatus{}
	status.SetConnected(false, errors.New("connect Home Link uplink: relay rejected gateway identity"))

	payload, err := json.Marshal(status.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	got := string(payload)
	if !strings.Contains(got, `"last_error":"connection-failed"`) {
		t.Fatalf("runtime payload = %s, want fixed public error code", got)
	}
	if strings.Contains(got, "relay rejected gateway identity") {
		t.Fatalf("runtime payload leaked local error: %s", got)
	}
}

func TestDisabledLocalAdminCanRevokeExistingCredential(t *testing.T) {
	const siteID = "001122334455667788"
	defer forgetSiteCredentialCoordinatorForTest(siteID)
	store := newAuthorityTestStore(t)
	fixture := newWebAuthnFixture(t, []byte{1, 2, 3, 4})
	if err := store.RegisterHomeLinkCredential(
		context.Background(),
		state.HomeLinkCredentialRecord{
			SiteID: siteID, CredentialID: fixture.credentialID,
			PublicKey: fixture.publicKey, Label: "phone",
			UserHandle: bytes.Repeat([]byte{3}, webAuthnUserHandleBytes),
			Status:     state.HomeLinkCredentialActive, Revision: 1,
			CreatedAtMS: 1, UpdatedAtMS: 1,
		},
	); err != nil {
		t.Fatal(err)
	}
	authority, err := NewPersistentCredentialAuthority(
		PersistentCredentialAuthorityOptions{Store: store, SiteID: siteID},
	)
	if err != nil {
		t.Fatal(err)
	}
	admin := &LocalAdmin{enabled: false, authority: authority}
	credentialID := base64.RawURLEncoding.EncodeToString(fixture.credentialID)
	if err := admin.RevokeCredential(context.Background(), credentialID); err != nil {
		t.Fatalf("revoke while disabled: %v", err)
	}
	record, err := store.HomeLinkCredential(
		context.Background(), siteID, fixture.credentialID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != state.HomeLinkCredentialRevoked {
		t.Fatalf("status = %q, want revoked", record.Status)
	}
}
