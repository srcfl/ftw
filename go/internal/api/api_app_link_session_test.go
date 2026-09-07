package api

// Sharing, from the app, over the session.
//
// The other sharing test is the LAN door. This one is the session door, and
// every test here is written against the box's REAL enrolment — appenroll's
// own Identity, minting its own codes and stamping its own roles — because
// the question this file exists to answer cannot be asked of a stub.
//
// The question is: what does the guest end up being? Not what the app asked
// for, not what the answer echoed back, but what the box wrote down when the
// code was spent. Every earlier version of this feature answered the first two
// correctly and the third wrongly, and the app's own suite stayed green
// throughout, because the simulator it tests against agreed with the app about
// a field the box does not read.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/internal/control"
)

// --------------------------------------------------------------------------
// The box's own enrolment, behind the API
// --------------------------------------------------------------------------

// realEnroller is AppEnroller backed by appenroll.Identity, which is what
// cmd/ftw wires in production. Only the TTL choice and the error translation
// are this file's; everything that decides a role is the box's.
type realEnroller struct{ id *appenroll.Identity }

func (e *realEnroller) MintPairingCode(role string) ([]byte, time.Time, error) {
	ttl := appenroll.PairingTTL
	if role != apiauth.RoleOwner {
		ttl = appenroll.InviteTTL
	}
	code, expires, err := e.id.MintPairingCode(role, ttl)
	return code, expires, enrollError(err)
}

func (e *realEnroller) MintSpokenCode(role string) (string, time.Time, error) {
	code, expires, err := e.id.MintSpokenCode(role)
	return code, expires, enrollError(err)
}

func (e *realEnroller) EnrollmentURL(code []byte, lanHint string) (string, error) {
	return e.id.EnrollmentURL(code, lanHint)
}

func (e *realEnroller) AuthorisedCount() int { return e.id.AuthorisedCount() }

func (e *realEnroller) Devices() []AppDevice {
	out := []AppDevice{}
	for _, d := range e.id.Devices() {
		out = append(out, AppDevice{
			ID: d.ID, AddedAtMs: d.AddedAtMs, LastSeenMs: d.LastSeenMs,
			Role: d.Role, LastOwner: d.LastOwner,
		})
	}
	return out
}

func (e *realEnroller) SetDeviceRole(id, role string) error {
	return enrollError(e.id.SetRole(id, role))
}

func (e *realEnroller) RevokeDevice(id string, atTheBox bool) error {
	_, err := e.id.Revoke(id, appenroll.Presence(atTheBox))
	return enrollError(err)
}

func enrollError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, appenroll.ErrUnknownDevice):
		return ErrUnknownAppDevice
	case errors.Is(err, appenroll.ErrLastOwnerProtected):
		return ErrLastAppOwnerProtected
	case errors.Is(err, appenroll.ErrUnknownRole):
		return ErrUnknownAppRole
	default:
		return err
	}
}

// newEnrolment is a box with one owner's phone already on it.
//
// The owner matters. appenroll makes the FIRST enrolment an owner whatever its
// code said, because a box with nobody to administer it can never be
// administered again — so a test that minted a viewer code onto an empty box
// would be measuring that rule and not this one.
func newEnrolment(t *testing.T) *realEnroller {
	t.Helper()
	id, err := appenroll.LoadOrCreate(filepath.Join(t.TempDir(), "app.key"))
	if err != nil {
		t.Fatalf("building the box's enrolment: %v", err)
	}

	code, _, err := id.MintPairingCode(apiauth.RoleOwner, appenroll.PairingTTL)
	if err != nil {
		t.Fatalf("minting the first owner's code: %v", err)
	}
	if _, err := id.Authorise(appKey(1), code); err != nil {
		t.Fatalf("enrolling the first owner: %v", err)
	}
	return &realEnroller{id: id}
}

func withEnrolment(e *realEnroller) func(*Deps) {
	return func(d *Deps) { d.AppEnroll = e }
}

// appKey is a phone's Noise static key. Distinct per number, and its content
// is never inspected — appenroll keys its list on the bytes.
func appKey(n byte) []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = n
	}
	return key
}

// --------------------------------------------------------------------------
// Talking to the session
// --------------------------------------------------------------------------

// call sends one API request over the session and returns the status and body
// a handler answered with. A refusal that never reached a handler comes back
// as status 0 with the refusal code, because those are two different facts and
// a test that conflated them would pass on either.
func (r *appRig) call(t *testing.T, id uint32, req appproto.APIReq) (int, string, []byte) {
	t.Helper()
	r.send(t, appproto.MsgAPIReq, id, req)

	env := awaitID(t, r.frames, id)
	if env.T == appproto.MsgError {
		return 0, decode[appproto.ErrorBody](t, env).Code, nil
	}

	head := decode[appproto.APIHeadMsg](t, env)
	end := decode[appproto.APIEnd](t, awaitIDType(t, r.frames, id, appproto.MsgAPIEnd))
	var body []byte
	for _, e := range r.frames.snapshot() {
		if e.T == appproto.MsgAPIChunk && e.ID != nil && *e.ID == id {
			body = append(body, decode[appproto.APIChunk](t, e).Data...)
		}
	}
	if end.Truncated || end.Bytes != int64(len(body)) {
		t.Fatalf("incomplete answer to request %d: end=%+v body_bytes=%d", id, end, len(body))
	}
	return head.Status, "", body
}

func awaitIDType(t *testing.T, frames *appFrames, id uint32, msgType string) appproto.Envelope {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, env := range frames.snapshot() {
			if env.T == msgType && env.ID != nil && *env.ID == id {
				return env
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no %s answer to request %d", msgType, id)
	return appproto.Envelope{}
}

type appReplyGateway struct{}

func (appReplyGateway) Route(*http.Request) apiauth.RouteFacts {
	return apiauth.RouteFacts{Tier: apiauth.TierRead}
}

func (appReplyGateway) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"forbidden"}`))
}

type headPausingSender struct {
	frames  *appFrames
	reached chan struct{}
	release chan struct{}
}

func (s *headPausingSender) Send(raw []byte) error {
	frame, err := appuplink.Codec().DecodeFrame(raw)
	if err != nil {
		return err
	}
	if err := s.frames.Send(raw); err != nil {
		return err
	}
	if frame.Envelope.T == appproto.MsgAPIHead {
		close(s.reached)
		<-s.release
	}
	return nil
}

func TestAppRigCallWaitsForTheCompleteResponse(t *testing.T) {
	frames := &appFrames{}
	sender := &headPausingSender{frames: frames, reached: make(chan struct{}), release: make(chan struct{})}
	ctrl := control.NewState(0, 50, "meter")
	box := &appBox{ctrl: ctrl}
	handler, err := appproto.New(appproto.Config{
		Clock:  appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
		Site:   box,
		Info:   box,
		Modes:  box,
		Plans:  box,
		Codec:  appuplink.Codec(),
		Sender: sender,
		API:    appReplyGateway{},
		Caller: apiauth.Caller{
			Subject: apiauth.KindApp + ":aBcD1234",
			Kind:    apiauth.KindApp,
			Role:    apiauth.RoleOwner,
			Scopes:  appproto.ScopesForRole(apiauth.RoleOwner),
			Epoch:   1,
		},
		Grants:     stillEnrolled{role: apiauth.RoleOwner},
		SrcGrid:    "meter",
		SrcPV:      "meter",
		SrcBattery: "meter",
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(handler.Close)
	rig := &appRig{handler: handler, frames: frames}
	released := false
	defer func() {
		if !released {
			close(sender.release)
		}
	}()

	type result struct {
		status  int
		refusal string
		body    []byte
	}
	done := make(chan result, 1)
	go func() {
		status, refusal, body := rig.call(t, 1, appproto.APIReq{Method: appproto.APIGet, Path: "/api/test"})
		done <- result{status: status, refusal: refusal, body: body}
	}()
	select {
	case <-sender.reached:
	case <-time.After(5 * time.Second):
		t.Fatal("response did not reach api.head")
	}
	select {
	case <-done:
		t.Fatal("call returned after api.head while the response was still in flight")
	case <-time.After(100 * time.Millisecond):
	}
	close(sender.release)
	released = true
	select {
	case got := <-done:
		if got.status != http.StatusForbidden || got.refusal != "" || string(got.body) != `{"error":"forbidden"}` {
			t.Fatalf("call returned %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return after api.end")
	}
}

// pairingCodeIn digs the code out of a QR payload, the way a guest's phone
// does after the camera reads the square.
//
//	https://app.ftw.energy/p#v2.<box key>.<pairing code>.<lan hint>.<rendezvous>
func pairingCodeIn(t *testing.T, url string) []byte {
	t.Helper()
	_, fragment, ok := strings.Cut(url, "#")
	if !ok {
		t.Fatalf("no payload in %q", url)
	}
	parts := strings.Split(fragment, ".")
	if len(parts) != 5 || parts[0] != appenroll.PayloadVersion {
		t.Fatalf("payload has %d segments: %q", len(parts), fragment)
	}
	code, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decoding the pairing code: %v", err)
	}
	return code
}

func lanHintIn(t *testing.T, url string) string {
	t.Helper()
	_, fragment, _ := strings.Cut(url, "#")
	parts := strings.Split(fragment, ".")
	if len(parts) != 5 {
		t.Fatalf("payload has %d segments: %q", len(parts), fragment)
	}
	hint, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		t.Fatalf("decoding the lan hint: %v", err)
	}
	return string(hint)
}

// --------------------------------------------------------------------------
// What the guest actually becomes
// --------------------------------------------------------------------------

// The whole feature, end to end: an owner invites from the app, somebody
// scans the square, and the box writes down a VIEWER.
//
// The last three lines are the test. Everything above them is what the app
// already believed — and what it believed was true of the request it sent, of
// the answer it got back, and of nothing at all about the guest.
func TestAnInviteFromTheAppEnrolsAViewer(t *testing.T) {
	enrol := newEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	status, refusal, body := rig.call(t, 1, appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/app-link/pairing",
		Body:   []byte(`{"role":"viewer"}`),
		StepUp: true,
	})
	if status != http.StatusOK {
		t.Fatalf("inviting answered %d %q; the app's invite button is dead", status, refusal)
	}

	var answer appLinkPairing
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decoding the invitation: %v (%q)", err, body)
	}
	if answer.Role != apiauth.RoleViewer {
		t.Fatalf("the answer names %q; the screen prints that above the square", answer.Role)
	}
	if answer.URL == "" {
		t.Fatal("nothing for the guest to scan")
	}

	// The guest's phone, spending the code it just scanned. This is the only
	// evidence that counts: what the box wrote down.
	grant, err := enrol.id.Authorise(appKey(2), pairingCodeIn(t, answer.URL))
	if err != nil {
		t.Fatalf("the guest could not pair with the code the app handed out: %v", err)
	}
	if grant.Role != apiauth.RoleViewer {
		t.Fatalf("the guest is a %q; the owner pressed \"Invite someone to view\"", grant.Role)
	}
}

// The shape the app actually sent, which the box does not read.
//
// inviteViewer() put the role in the QUERY STRING. The box reads it from a
// JSON body, so the field arrived as absent — and absent used to mean owner.
// Every "invite a family member to look" was a house handed over, masked only
// because the door in front of it was shut. It is a 400 now, and the guest
// that never was is still nowhere on the list.
func TestARoleInTheQueryStringMintsNothing(t *testing.T) {
	enrol := newEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	status, refusal, _ := rig.call(t, 1, appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/app-link/pairing",
		Query:  map[string]string{"role": apiauth.RoleViewer},
		StepUp: true,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("a role the box never read answered %d %q, want 400", status, refusal)
	}

	// And nothing is live to be spent. A code minted here would admit whoever
	// scanned it as whatever the box guessed.
	if _, err := enrol.id.Authorise(appKey(2), make([]byte, appenroll.PairingCodeBytes)); err == nil {
		t.Fatal("a refused request left a live pairing code behind")
	}
	if n := enrol.AuthorisedCount(); n != 1 {
		t.Fatalf("%d phones are enrolled, want the one owner", n)
	}
}

// An owner is admitted at the box, in the house. A session proves enrolment
// and says nothing about where the phone is, so it cannot make another owner —
// by minting a code, by reading one down a phone line, or by promoting a guest
// who is already on the list.
func TestTheAppCannotMakeAnotherOwner(t *testing.T) {
	enrol := newEnrolment(t)

	// A guest to try to promote, admitted the way a guest is.
	code, _, err := enrol.id.MintPairingCode(apiauth.RoleViewer, appenroll.InviteTTL)
	if err != nil {
		t.Fatalf("minting the guest's code: %v", err)
	}
	guest, err := enrol.id.Authorise(appKey(2), code)
	if err != nil {
		t.Fatalf("enrolling the guest: %v", err)
	}

	for i, c := range []struct {
		name string
		req  appproto.APIReq
	}{
		{"a scanned owner code", appproto.APIReq{
			Method: appproto.APIPost, Path: "/api/app-link/pairing",
			Body: []byte(`{"role":"owner"}`), StepUp: true}},
		{"a spoken owner code", appproto.APIReq{
			Method: appproto.APIPost, Path: "/api/app-link/pairing",
			Body: []byte(`{"role":"owner","kind":"spoken"}`), StepUp: true}},
		{"promoting the guest", appproto.APIReq{
			Method: appproto.APIPatch, Path: "/api/app-link/devices/" + guest.DeviceID,
			Body: []byte(`{"role":"owner"}`), StepUp: true}},
	} {
		rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))
		status, refusal, body := rig.call(t, uint32(i+1), c.req)
		if status != http.StatusForbidden {
			t.Fatalf("%s answered %d %q %s, want 403", c.name, status, refusal, body)
		}
	}

	// The list is what it was: one owner and one guest, and the guest is still
	// a guest. A refusal that had already written something would be worse
	// than no refusal at all.
	owners := 0
	for _, d := range enrol.Devices() {
		if d.Role == apiauth.RoleOwner {
			owners++
		}
		if d.ID == guest.DeviceID && d.Role != apiauth.RoleViewer {
			t.Fatalf("the guest is now a %q", d.Role)
		}
	}
	if owners != 1 {
		t.Fatalf("%d owners, want the one who was there before", owners)
	}
}

// A code to read aloud is made at the box, whatever role it is for.
//
// Forty bits are safe to say down a phone line because of what surrounds
// them, and the load-bearing part is that every minting costs somebody a walk
// to the box: five wrong guesses burn a code, and asking for another needs a
// person in the room. Opening this door to the app would have taken that
// argument away for a viewer's code — and bought nothing, because a typed code
// carries the pairing code alone and cannot admit a phone that has never seen
// this box.
func TestTheAppCannotMintACodeToReadAloud(t *testing.T) {
	enrol := newEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	status, refusal, body := rig.call(t, 1, appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/app-link/pairing",
		Body:   []byte(`{"role":"viewer","kind":"spoken"}`),
		StepUp: true,
	})
	if status != http.StatusForbidden {
		t.Fatalf("a spoken code from the app answered %d %q %s, want 403", status, refusal, body)
	}

	// Nothing live to guess at. A code minted and then withheld is still five
	// guesses somebody did not have to walk to the box for.
	if _, err := enrol.id.Authorise(appKey(3), make([]byte, appenroll.SpokenCodeBytes)); err == nil {
		t.Fatal("a refused request left a live code behind")
	}
}

// A code minted from the app carries no LAN hint.
//
// The passthrough builds its request with Host "localhost" so the API's trust
// boundary sees a local client. Copied into the payload, that would hand the
// guest an address pointing at their own phone — a square that scans, parses,
// and then cannot reach anything.
func TestAnInviteFromTheAppCarriesNoLANHint(t *testing.T) {
	enrol := newEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	status, refusal, body := rig.call(t, 1, appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/app-link/pairing",
		Body:   []byte(`{"role":"viewer"}`),
		StepUp: true,
	})
	if status != http.StatusOK {
		t.Fatalf("inviting answered %d %q", status, refusal)
	}
	var answer appLinkPairing
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decoding the invitation: %v", err)
	}

	if hint := lanHintIn(t, answer.URL); hint != "" {
		t.Fatalf("the payload tells the guest to look for the box at %q", hint)
	}
}

// --------------------------------------------------------------------------
// Who may ask
// --------------------------------------------------------------------------

// The roster is not a guest's to see, and the door is not a guest's to open.
//
// Refused inside the handler on the scope, not by the passthrough's tier gate:
// reading the list is a read, so the tier lets it through and the members
// scope is what stops it. A viewer's grant carries neither members scope.
func TestAGuestCannotSeeOrChangeWhoHasAccess(t *testing.T) {
	enrol := newEnrolment(t)

	for i, c := range []struct {
		name string
		req  appproto.APIReq
	}{
		{"the roster", appproto.APIReq{
			Method: appproto.APIGet, Path: "/api/app-link/devices"}},
		{"an invitation", appproto.APIReq{
			Method: appproto.APIPost, Path: "/api/app-link/pairing",
			Body: []byte(`{"role":"viewer"}`), StepUp: true}},
		{"a revoke", appproto.APIReq{
			Method: appproto.APIDelete, Path: "/api/app-link/devices/aaaa1111",
			StepUp: true}},
	} {
		rig := newAppSession(t, apiauth.RoleViewer, withEnrolment(enrol))
		status, refusal, body := rig.call(t, uint32(i+1), c.req)
		if status == http.StatusOK {
			t.Fatalf("a guest read or changed %s: %s", c.name, body)
		}
		// Either door is a refusal: the passthrough's role gate on a write,
		// or the handler's scope check on the read. What must not happen is a
		// household's list of phones reaching a guest.
		if status != 0 && status != http.StatusForbidden {
			t.Fatalf("a guest asking for %s got %d %s", c.name, status, body)
		}
		if status == 0 && refusal != appproto.ErrScopeDenied {
			t.Fatalf("a guest asking for %s was refused with %q", c.name, refusal)
		}
		if strings.Contains(string(body), "local network") {
			t.Fatalf("a guest is told to go home: %s", body)
		}
	}

	if n := enrol.AuthorisedCount(); n != 1 {
		t.Fatalf("%d phones enrolled after a guest's attempts, want 1", n)
	}
}

// An owner may look, which is the other half of the test above. Without it,
// refusing everybody would pass.
func TestAnOwnerReadsTheRosterThroughTheApp(t *testing.T) {
	enrol := newEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	status, refusal, body := rig.call(t, 1, appproto.APIReq{
		Method: appproto.APIGet,
		Path:   "/api/app-link/devices",
	})
	if status != http.StatusOK {
		t.Fatalf("the roster answered %d %q; the sharing screen has nothing to draw",
			status, refusal)
	}

	var answer struct {
		Devices []AppDevice `json:"devices"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("decoding the roster: %v (%q)", err, body)
	}
	if len(answer.Devices) != 1 || answer.Devices[0].Role != apiauth.RoleOwner {
		t.Fatalf("roster = %+v, want the one owner naming its role", answer.Devices)
	}
	// The rows carry key prefixes and never keys, over the session as on the
	// LAN. It is the same handler, and this is what says so.
	if strings.Contains(string(body), "noiseSecret") {
		t.Fatal("the roster leaked key material to the app")
	}
}

// An owner may lock a phone out from anywhere. This is the case the feature is
// for: the phone that was lost is not the phone in your hand, and telling
// somebody to walk to the box is telling them to do nothing for an hour.
func TestAnOwnerRevokesAPhoneThroughTheApp(t *testing.T) {
	enrol := newEnrolment(t)
	rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))

	code, _, err := enrol.id.MintPairingCode(apiauth.RoleViewer, appenroll.InviteTTL)
	if err != nil {
		t.Fatalf("minting the guest's code: %v", err)
	}
	guest, err := enrol.id.Authorise(appKey(2), code)
	if err != nil {
		t.Fatalf("enrolling the guest: %v", err)
	}

	status, refusal, body := rig.call(t, 1, appproto.APIReq{
		Method: appproto.APIDelete,
		Path:   "/api/app-link/devices/" + guest.DeviceID,
		StepUp: true,
	})
	if status != http.StatusOK {
		t.Fatalf("revoking answered %d %q %s", status, refusal, body)
	}
	if _, still := enrol.id.GrantFor(guest.DeviceID); still {
		t.Fatal("the box still trusts a phone the owner removed")
	}
}

// The last owner cannot remove themselves, from the app any more than from the
// box's own page — and the refusal has to be one a person can act on.
//
// Refused in the enrolment layer, which is what makes this true of both doors
// at once rather than of whichever one somebody remembered.
func TestTheLastOwnerCannotRemoveThemselvesThroughTheApp(t *testing.T) {
	enrol := newEnrolment(t)

	rows := enrol.Devices()
	if len(rows) != 1 {
		t.Fatalf("this test needs exactly one owner, got %+v", rows)
	}
	me := rows[0].ID

	for i, c := range []struct {
		name string
		req  appproto.APIReq
	}{
		{"removing myself", appproto.APIReq{
			Method: appproto.APIDelete, Path: "/api/app-link/devices/" + me,
			StepUp: true}},
		{"demoting myself", appproto.APIReq{
			Method: appproto.APIPatch, Path: "/api/app-link/devices/" + me,
			Body: []byte(`{"role":"viewer"}`), StepUp: true}},
	} {
		rig := newAppSession(t, apiauth.RoleOwner, withEnrolment(enrol))
		status, refusal, body := rig.call(t, uint32(i+1), c.req)
		if status != http.StatusConflict {
			t.Fatalf("%s answered %d %q, want 409", c.name, status, refusal)
		}
		if !strings.Contains(string(body), "Pair another owner first") {
			t.Fatalf("%s was refused without saying what to do: %s", c.name, body)
		}
	}

	// Still an owner, and still enrolled. A box that talked itself out of its
	// last owner can never be administered again, from anywhere, by anybody.
	rows = enrol.Devices()
	if len(rows) != 1 || rows[0].Role != apiauth.RoleOwner {
		t.Fatalf("the last owner is now %+v", rows)
	}
}
