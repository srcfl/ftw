package api

// Adding a watch, through the door it takes: an owner's phone, over its
// session, with a passkey step-up on the request itself (ADR 0009,
// decision 4).

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
)

// sessionPhoneKey is a Noise key whose device id is aBcD1234, the subject
// newAppSession's caller carries. The watch code binds to that phone, so the
// box must know it by the same id.
func sessionPhoneKey(t *testing.T) []byte {
	t.Helper()
	prefix, err := base64.RawURLEncoding.DecodeString("aBcD1234")
	if err != nil {
		t.Fatal(err)
	}
	return append(prefix, make([]byte, 32-len(prefix))...)
}

// newWatchEnrolment is a box whose owner is the session's own phone.
func newWatchEnrolment(t *testing.T) *realEnroller {
	t.Helper()
	id, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "app.key"))
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := id.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	phone, err := id.Authorise(sessionPhoneKey(t), code)
	if err != nil {
		t.Fatal(err)
	}
	if phone.DeviceID != "aBcD1234" {
		t.Fatalf("the session's phone enrolled as %q", phone.DeviceID)
	}
	return &realEnroller{id: id}
}

func watchPairing(stepUp bool) appproto.APIReq {
	return appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/app-link/pairing",
		Body:   []byte(`{"role":"watch"}`),
		StepUp: stepUp,
	}
}

// A watch is added by its key. A request without one comes from an app that
// would carry a code, which no watch can redeem once the relay stopped, so it
// is refused and adds nothing.
func TestAWatchWithoutAKeyIsRefused(t *testing.T) {
	enrol := newWatchEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	status, _, body := rig.call(t, 1, watchPairing(true))
	if status != http.StatusBadRequest {
		t.Fatalf("a watch without a key answered %d %s", status, body)
	}
	for _, d := range enrol.Devices() {
		if d.Role == apiauth.RoleWatch {
			t.Fatal("a watch joined")
		}
	}
}

// The grace window a settings screen opens does not stand in for a step-up:
// a watch is a new key that can charge the car from anywhere.
func TestAWatchNeedsAStepUpOnTheRequestItself(t *testing.T) {
	enrol := newWatchEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	// A genuine ceremony on another configure route opens the window. The
	// window counts box uptime and treats zero as never, and this session's
	// clock started a moment ago.
	time.Sleep(5 * time.Millisecond)
	if status, refusal, body := rig.call(t, 1, appproto.APIReq{
		Method: appproto.APIPost, Path: "/api/app-link/pairing",
		Body: []byte(`{"role":"viewer"}`), StepUp: true,
	}); status != http.StatusOK {
		t.Fatalf("opening the window answered %d %q %s", status, refusal, body)
	}

	status, refusal, body := rig.call(t, 2, watchByKey(appKey(9), false))
	if status != http.StatusForbidden {
		t.Fatalf("a watch without a step-up answered %d %q %s", status, refusal, body)
	}
	var answer map[string]string
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	if answer["code"] != appproto.ErrNeedsStepUp {
		t.Fatalf("the refusal names %q, want %q so the app can ask for the passkey", answer["code"], appproto.ErrNeedsStepUp)
	}
	for _, d := range enrol.Devices() {
		if d.Role == apiauth.RoleWatch {
			t.Fatal("a watch joined")
		}
	}
}

// A viewer's phone cannot add a watch, and the box's own page cannot either:
// the code binds to the phone that asks, and the LAN has no phone.
func TestOnlyAnOwnersPhoneAddsAWatch(t *testing.T) {
	enrol := newWatchEnrolment(t)

	viewer := newAppSession(t, apiauth.RoleViewer, withEnrolment(enrol))
	if status, refusal, _ := viewer.call(t, 1, watchByKey(appKey(9), true)); status == http.StatusOK {
		t.Fatalf("a viewer added a watch (%q)", refusal)
	}

	s := New(&Deps{AppEnroll: enrol})
	w := httptest.NewRecorder()
	s.handleAppLinkPairing(w, pairingRequestRole("192.168.1.1", "127.0.0.1:1234", nil, apiauth.RoleWatch))
	if w.Code != http.StatusForbidden {
		t.Fatalf("the box's page minted a watch code: %d %s", w.Code, w.Body.String())
	}
}

// Adding a watch leaves an invite somebody is about to scan alone.
func TestAddingAWatchLeavesAnInviteAlive(t *testing.T) {
	enrol := newWatchEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	_, _, inviteBody := rig.call(t, 1, appproto.APIReq{
		Method: appproto.APIPost, Path: "/api/app-link/pairing",
		Body: []byte(`{"role":"viewer"}`), StepUp: true,
	})
	var invite appLinkPairing
	if err := json.Unmarshal(inviteBody, &invite); err != nil {
		t.Fatal(err)
	}
	if status, refusal, body := rig.call(t, 2, watchByKey(appKey(9), true)); status != http.StatusOK {
		t.Fatalf("adding a watch answered %d %q %s", status, refusal, body)
	}
	if _, err := enrol.id.Authorise(appKey(3), pairingCodeIn(t, invite.URL)); err != nil {
		t.Fatalf("the invite died when the watch code was minted: %v", err)
	}
}

// The list names each watch's phone, and a watch keeps its role.
func TestTheListShowsAWatchUnderItsPhone(t *testing.T) {
	enrol := newWatchEnrolment(t)
	watch, err := enrol.id.AddWatch("aBcD1234", appKey(9))
	if err != nil {
		t.Fatal(err)
	}
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	status, _, body := rig.call(t, 1, appproto.APIReq{Method: appproto.APIGet, Path: "/api/app-link/devices"})
	if status != http.StatusOK {
		t.Fatalf("the list answered %d", status)
	}
	var list struct {
		Devices []AppDevice `json:"devices"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Devices) != 2 || list.Devices[1].ID != watch.DeviceID || list.Devices[1].Parent != "aBcD1234" {
		t.Fatalf("the list is %+v", list.Devices)
	}

	status, _, body = rig.call(t, 2, appproto.APIReq{
		Method: appproto.APIPatch, Path: "/api/app-link/devices/" + watch.DeviceID,
		Body: []byte(`{"role":"viewer"}`), StepUp: true,
	})
	if status != http.StatusConflict {
		t.Fatalf("changing a watch's role answered %d %s", status, body)
	}
}

func watchByKey(key []byte, stepUp bool) appproto.APIReq {
	return appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/app-link/pairing",
		Body:   []byte(`{"role":"watch","key":"` + base64.RawURLEncoding.EncodeToString(key) + `"}`),
		StepUp: stepUp,
	}
}

// Through FTW Cloud a watch has no code to carry: the phone names its key
// over its own session, and the box lists the watch at once under that phone.
func TestAnOwnerPhoneAddsAWatchByItsKey(t *testing.T) {
	enrol := newWatchEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	status, refusal, body := rig.call(t, 1, watchByKey(appKey(9), true))
	if status != http.StatusOK {
		t.Fatalf("adding a watch by its key answered %d %q %s", status, refusal, body)
	}
	var answer appLinkPairing
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Role != apiauth.RoleWatch || answer.URL != "" || answer.Code != "" || answer.DeviceID == "" {
		t.Fatalf("the answer is %+v", answer)
	}
	// The watch's first handshake needs no code.
	grant, _, err := enrol.id.CheckWatch(appKey(9))
	if err != nil {
		t.Fatalf("the watch could not open with its key alone: %v", err)
	}
	if grant.DeviceID != answer.DeviceID || grant.Role != apiauth.RoleWatch || grant.Parent != "aBcD1234" {
		t.Fatalf("the box wrote down %+v for %q", grant, answer.DeviceID)
	}

	// Asking again is the same watch, not a second one.
	if status, refusal, body := rig.call(t, 2, watchByKey(appKey(9), true)); status != http.StatusOK {
		t.Fatalf("adding the same watch again answered %d %q %s", status, refusal, body)
	}
	watches := 0
	for _, d := range enrol.Devices() {
		if d.Role == apiauth.RoleWatch {
			watches++
		}
	}
	if watches != 1 {
		t.Fatalf("the box lists %d watches", watches)
	}
}

// A key goes through the same three rules as a code: a session, an owner, a
// step-up on the request.
func TestAWatchKeyMeetsTheSameRules(t *testing.T) {
	enrol := newWatchEnrolment(t)

	// Without a step-up the session refuses before the route, or the route
	// does when a settings screen opened the grace window; either way the
	// app hears that it must ask for the passkey.
	owner := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))
	status, code, body := owner.call(t, 1, watchByKey(appKey(9), false))
	if status == http.StatusForbidden {
		var answer map[string]string
		_ = json.Unmarshal(body, &answer)
		code = answer["code"]
	}
	if code != appproto.ErrNeedsStepUp {
		t.Fatalf("a watch key without a step-up answered %d %q %s", status, code, body)
	}

	viewer := newAppSession(t, apiauth.RoleViewer, withEnrolment(enrol))
	if status, refusal, _ := viewer.call(t, 1, watchByKey(appKey(9), true)); status == http.StatusOK {
		t.Fatalf("a viewer added a watch by its key (%q)", refusal)
	}

	s := New(&Deps{AppEnroll: enrol})
	w := httptest.NewRecorder()
	req := pairingRequestRole("192.168.1.1", "127.0.0.1:1234", nil, apiauth.RoleWatch)
	s.handleAppLinkPairing(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("the box's page added a watch: %d %s", w.Code, w.Body.String())
	}

	for _, d := range enrol.Devices() {
		if d.Role == apiauth.RoleWatch {
			t.Fatal("a watch joined")
		}
	}
}

// A key that cannot be a watch's is refused: the wrong length, or the
// phone's own key.
func TestAKeyThatIsNoWatchIsRefused(t *testing.T) {
	enrol := newWatchEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))
	// Each request on the session has its own id.
	for i, c := range []struct {
		name string
		key  []byte
	}{{"short", appKey(9)[:16]}, {"the phone's own", sessionPhoneKey(t)}} {
		if status, _, body := rig.call(t, uint32(i+1), watchByKey(c.key, true)); status != http.StatusBadRequest {
			t.Fatalf("%s key answered %d %s", c.name, status, body)
		}
	}
}
