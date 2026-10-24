package appenroll

// A watch, at the layer that decides what it is (ADR 0009, decisions 4, 5
// and 9).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

// addWatch adds one watch by its key, as its phone does over a session, and
// returns the watch's key and the grant the box stamped on it.
func addWatch(t *testing.T, id *Identity, phone Grant) ([]byte, Grant) {
	t.Helper()
	key := appKey(t)
	grant, err := id.AddWatch(phone.DeviceID, key)
	if err != nil {
		t.Fatalf("adding a watch: %v", err)
	}
	return key, grant
}

// checkWatch is the exchange's door, decided and committed at once.
func checkWatch(id *Identity, key []byte) (Grant, error) {
	_, commit, err := id.CheckWatch(key)
	if err != nil {
		return Grant{}, err
	}
	return commit()
}

func TestAnOwnerPhoneAddsAWatchThatNamesItAsParent(t *testing.T) {
	id, _ := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)

	key, watch := addWatch(t, id, phone)
	if watch.Role != apiauth.RoleWatch {
		t.Fatalf("the watch is a %q", watch.Role)
	}
	if watch.Parent != phone.DeviceID || watch.ParentRole != apiauth.RoleOwner {
		t.Fatalf("the watch's parent is %q (%q), want %q (owner)", watch.Parent, watch.ParentRole, phone.DeviceID)
	}

	// Its handshake needs no code: the box knows its key.
	again, err := checkWatch(id, key)
	if err != nil {
		t.Fatalf("the watch's second handshake was refused: %v", err)
	}
	if again.DeviceID != watch.DeviceID || again.Parent != phone.DeviceID {
		t.Fatalf("the second handshake found %+v", again)
	}
}

// A viewer cannot add a watch, and neither can a phone the box does not know.
// Each door admits its own kind and answers the other as a stranger.
func TestAWatchHasNoStreamAndAPhoneNoWatchDoor(t *testing.T) {
	id, _ := newIdentity(t)
	phoneKey := appKey(t)
	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	phone, err := id.Authorise(phoneKey, code)
	if err != nil {
		t.Fatal(err)
	}
	watchKey, _ := addWatch(t, id, phone)

	if _, err := id.Authorise(watchKey, nil); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("a watch opened a stream: %v", err)
	}
	if _, err := checkWatch(id, phoneKey); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("a phone came through the watch's door: %v", err)
	}
}

// Removing a phone removes its watches, and their keys come back so the
// caller can end their sessions. Removing a watch touches nothing else.
func TestRemovalRunsFromPhoneToWatchAndNeverBack(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)
	phone := pair(t, id, apiauth.RoleOwner)
	firstKey, first := addWatch(t, id, phone)
	secondKey, second := addWatch(t, id, phone)

	removed, err := id.Revoke(first.DeviceID, OverASession)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || string(removed[0].Key) != string(firstKey) {
		t.Fatalf("removing a watch removed %+v", removed)
	}
	if _, ok := id.GrantFor(phone.DeviceID); !ok {
		t.Fatal("removing a watch removed its phone")
	}
	if _, ok := id.GrantFor(second.DeviceID); !ok {
		t.Fatal("removing one watch removed another")
	}
	if _, err := checkWatch(id, firstKey); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("a removed watch's next message 1 was answered: %v", err)
	}

	removed, err = id.Revoke(phone.DeviceID, OverASession)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0].ID != phone.DeviceID || removed[1].ID != second.DeviceID {
		t.Fatalf("removing the phone removed %+v, want the phone then its watch", removed)
	}
	if _, ok := id.GrantFor(second.DeviceID); ok {
		t.Fatal("a watch outlived its phone")
	}
	if _, err := checkWatch(id, secondKey); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("a watch whose phone was removed was answered: %v", err)
	}
	for _, d := range id.Roster() {
		if d.ID == second.DeviceID {
			t.Fatal("a removed watch is still in the roster")
		}
	}
}

// A watch is a watch until it is removed; its reach follows its phone.
func TestTheWatchRoleCannotBeGivenOrTakenByARoleChange(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)
	phone := pair(t, id, apiauth.RoleOwner)
	_, watch := addWatch(t, id, phone)

	if err := id.SetRole(watch.DeviceID, apiauth.RoleOwner); !errors.Is(err, ErrWatchRole) {
		t.Fatalf("a watch was made an owner: %v", err)
	}
	if err := id.SetRole(phone.DeviceID, apiauth.RoleWatch); !errors.Is(err, ErrWatchRole) {
		t.Fatalf("a phone was made a watch: %v", err)
	}
	if _, _, err := id.MintPairingCode(apiauth.RoleWatch, PairingTTL); !errors.Is(err, ErrWatchRole) {
		t.Fatalf("a phone's code was minted for a watch: %v", err)
	}
	if _, _, err := id.MintSpokenCode(apiauth.RoleWatch); !errors.Is(err, ErrWatchRole) {
		t.Fatalf("a box code was minted for a watch: %v", err)
	}
}

// Demoting the phone narrows its watch: the grant names the parent's new role
// and moves its epoch, so the next roster carries the narrower scopes.
func TestDemotingAPhoneMovesItsWatchsGrant(t *testing.T) {
	id, _ := newIdentity(t)
	pair(t, id, apiauth.RoleOwner)
	phone := pair(t, id, apiauth.RoleOwner)
	_, watch := addWatch(t, id, phone)

	if err := id.SetRole(phone.DeviceID, apiauth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	after, ok := id.GrantFor(watch.DeviceID)
	if !ok {
		t.Fatal("demoting the phone removed its watch")
	}
	if after.ParentRole != apiauth.RoleViewer {
		t.Fatalf("the watch's parent role is %q after the demotion", after.ParentRole)
	}
	if after.Epoch <= watch.Epoch {
		t.Fatalf("the watch's epoch stayed at %d", after.Epoch)
	}
	for _, d := range id.Roster() {
		if d.ID == watch.DeviceID && (d.ParentRole != apiauth.RoleViewer || d.Epoch != after.Epoch) {
			t.Fatalf("the roster has %+v for the watch", d)
		}
	}
}

// The list puts each watch right after its phone.
func TestTheDeviceListPutsAWatchUnderItsPhone(t *testing.T) {
	id, _ := newIdentity(t)
	now := time.UnixMilli(1_760_000_000_000)
	id.now = func() time.Time { return now }

	first := pair(t, id, apiauth.RoleOwner)
	now = now.Add(time.Minute)
	second := pair(t, id, apiauth.RoleOwner)
	now = now.Add(time.Minute)
	_, watch := addWatch(t, id, first)

	rows := id.Devices()
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	got := []string{rows[0].ID, rows[1].ID, rows[2].ID}
	want := []string{second.DeviceID, first.DeviceID, watch.DeviceID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rows in order %v, want %v", got, want)
	}
	if rows[2].Parent != first.DeviceID || rows[2].Role != apiauth.RoleWatch || rows[2].LastOwner {
		t.Fatalf("the watch's row is %+v", rows[2])
	}
	if rows[0].Parent != "" || rows[1].Parent != "" {
		t.Fatal("a phone's row names a parent")
	}
}

// A watch is a device in the roster, with its role and its phone's role.
func TestAWatchIsInTheRoster(t *testing.T) {
	id, _ := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	key, watch := addWatch(t, id, phone)

	for _, d := range id.Roster() {
		if d.ID != watch.DeviceID {
			continue
		}
		if d.Role != apiauth.RoleWatch || d.ParentRole != apiauth.RoleOwner || string(d.Key) != string(key) {
			t.Fatalf("the watch's roster entry is %+v", d)
		}
		return
	}
	t.Fatal("the watch is not in the roster")
}

func TestAWatchAndItsParentSurviveARestart(t *testing.T) {
	id, keyPath := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	key, watch := addWatch(t, id, phone)

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := checkWatch(again, key)
	if err != nil {
		t.Fatalf("the watch was forgotten across a restart: %v", err)
	}
	if grant.DeviceID != watch.DeviceID || grant.Parent != phone.DeviceID {
		t.Fatalf("after a restart the watch is %+v", grant)
	}
}

// A watch whose phone is missing from the file goes at load rather than
// lingering as a device nobody can narrow.
func TestAWatchWithoutItsPhoneIsDroppedAtLoad(t *testing.T) {
	id, keyPath := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	_, watch := addWatch(t, id, phone)

	// Write the file as a crash between the two halves of a removal would
	// have left it: the phone gone, its watch still there.
	path := filepath.Join(filepath.Dir(keyPath), FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	id.mu.Lock()
	phoneKey, _ := id.findLocked(phone.DeviceID)
	delete(id.authorised, phoneKey)
	id.mu.Unlock()
	if err := id.save(); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) == string(raw) {
		t.Fatal("the test did not change the file")
	}

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := again.GrantFor(watch.DeviceID); ok {
		t.Fatal("an orphaned watch loaded")
	}
	if n := again.AuthorisedCount(); n != 0 {
		t.Fatalf("%d enrolments after loading an orphan", n)
	}
}

// A watch added by its key is the same watch a code adds: the owner phone is
// its parent, it opens with no code, a viewer cannot add one, and it goes
// with its phone.
func TestAWatchAddedByItsKey(t *testing.T) {
	id, _ := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	viewer := pair(t, id, apiauth.RoleViewer)
	key := appKey(t)

	if _, err := id.AddWatch(viewer.DeviceID, key); !errors.Is(err, ErrWatchNeedsOwner) {
		t.Fatalf("a viewer added a watch: %v", err)
	}
	if _, err := id.AddWatch("nobody00", key); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("an unknown phone added a watch: %v", err)
	}
	if _, err := id.AddWatch(phone.DeviceID, key[:31]); !errors.Is(err, ErrWatchKey) {
		t.Fatalf("a short key was taken: %v", err)
	}

	watch, err := id.AddWatch(phone.DeviceID, key)
	if err != nil {
		t.Fatal(err)
	}
	if watch.Role != apiauth.RoleWatch || watch.Parent != phone.DeviceID || watch.ParentRole != apiauth.RoleOwner {
		t.Fatalf("the box wrote down %+v", watch)
	}
	opened, err := checkWatch(id, key)
	if err != nil || opened.DeviceID != watch.DeviceID {
		t.Fatalf("the watch could not open with its key alone: %+v %v", opened, err)
	}
	if again, err := id.AddWatch(phone.DeviceID, key); err != nil || again.DeviceID != watch.DeviceID {
		t.Fatalf("adding it again gave %+v %v", again, err)
	}

	// Under another phone, or as a phone's key, it is no watch to add.
	other := pair(t, id, apiauth.RoleOwner)
	if _, err := id.AddWatch(other.DeviceID, key); !errors.Is(err, ErrWatchKey) {
		t.Fatalf("another phone took the watch: %v", err)
	}

	if _, err := id.Revoke(phone.DeviceID, OverASession); err != nil {
		t.Fatal(err)
	}
	if _, err := checkWatch(id, key); err == nil {
		t.Fatal("the watch outlived its phone")
	}
}

// The exchange's check writes nothing: a message 1 that FTW Cloud replays
// stamps no visit until the session proves itself and commits.
func TestCheckingAWatchWritesNothingUntilItCommits(t *testing.T) {
	id, _ := newIdentity(t)
	phone := pair(t, id, apiauth.RoleOwner)
	key, watch := addWatch(t, id, phone)
	seen := func() int64 {
		for _, d := range id.Devices() {
			if d.ID == watch.DeviceID {
				return d.LastSeenMs
			}
		}
		t.Fatal("the watch is not listed")
		return 0
	}

	for i := 0; i < 3; i++ {
		if _, _, err := id.CheckWatch(key); err != nil {
			t.Fatal(err)
		}
	}
	if seen() != 0 {
		t.Fatal("a check stamped the watch's visit")
	}
	_, commit, err := id.CheckWatch(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commit(); err != nil || seen() == 0 {
		t.Fatalf("a commit did not stamp the visit: %v", err)
	}
	if !id.HasWatch(watch.DeviceID) || id.HasWatch(phone.DeviceID) || id.HasWatch("nobody00") {
		t.Fatal("HasWatch names the wrong devices")
	}

	// A watch removed between the check and the commit is not let in.
	_, commit, err = id.CheckWatch(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := id.Revoke(watch.DeviceID, OverASession); err != nil {
		t.Fatal(err)
	}
	if _, err := commit(); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("a removed watch committed: %v", err)
	}
}
