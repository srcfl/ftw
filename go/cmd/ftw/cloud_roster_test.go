package main

import (
	"crypto/rand"
	"path/filepath"
	"slices"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appuplink"
)

func rosterKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func rosterPhone(t *testing.T, id *appenroll.Identity, role string) appenroll.Grant {
	t.Helper()
	code, _, err := id.MintPairingCode(role, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := id.Authorise(rosterKey(t), code)
	if err != nil {
		t.Fatal(err)
	}
	return grant
}

func rosterScopes(t *testing.T, roster appuplink.CloudRoster, deviceID string) []string {
	t.Helper()
	for _, d := range roster.Devices() {
		if d.ID == deviceID {
			return d.Scopes
		}
	}
	t.Fatalf("%s is not in the roster", deviceID)
	return nil
}

// FTW Cloud holds no role table, so the roster carries the scopes the box
// derives. A watch's are its role's, cut to what its phone holds now.
func TestTheRosterNarrowsAWatchToWhatItsPhoneHolds(t *testing.T) {
	id, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	rosterPhone(t, id, apiauth.RoleOwner)
	phone := rosterPhone(t, id, apiauth.RoleOwner)
	watch, err := id.AddWatch(phone.DeviceID, rosterKey(t))
	if err != nil {
		t.Fatal(err)
	}
	roster := appuplink.CloudRoster{Enroll: id}

	if got := rosterScopes(t, roster, watch.DeviceID); !slices.Equal(got, apiauth.RoleScopes[apiauth.RoleWatch]) {
		t.Fatalf("under an owner the roster gives the watch %v", got)
	}
	if got := rosterScopes(t, roster, phone.DeviceID); !slices.Equal(got, apiauth.RoleScopes[apiauth.RoleOwner]) {
		t.Fatalf("the owner phone's scopes changed to %v", got)
	}

	if err := id.SetRole(phone.DeviceID, apiauth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	if got := rosterScopes(t, roster, watch.DeviceID); !slices.Equal(got, apiauth.RoleScopes[apiauth.RoleViewer]) {
		t.Fatalf("under a viewer the roster gives the watch %v", got)
	}
}
