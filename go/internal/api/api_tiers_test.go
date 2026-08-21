package api

// What a route costs, decided by what its handler does.
//
// The tests here exist because the box used to decide it from the HTTP method,
// and a method does not know what a handler does. Two routes proved it: a GET
// that hands out a password, and a POST that drives every battery in the house
// through a step pattern.

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/internal/battery"
	"github.com/srcfl/ftw/go/internal/calendar"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/selftune"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// caldavPassword is the credential the reviewer walked away with. It is a
// literal on purpose: the assertion is that these bytes never cross the
// session, and matching on them is the only way to say that.
const caldavPassword = "S3CRET-CALDAV-PASSWORD"

// tieredRig is a session onto a box that has the two subsystems these tests
// are about. The other passthrough tests use a bare box; a bare box has no
// credential to leak and no battery to drive, which is why they missed both.
type tieredRig struct {
	*appRig
	srv      *Server
	selfTune *selftune.Coordinator
}

func newTieredSession(t *testing.T, role string) *tieredRig {
	t.Helper()

	ctrl := control.NewState(0, 50, "meter")
	tel := telemetry.NewStore()
	tel.DriverHealthMut("meter").RecordSuccess()

	cfg := &config.Config{
		Drivers: []config.Driver{{Name: "pixii-1", BatteryCapacityWh: 16000}},
		CalDAV: &config.CalDAV{
			Enabled:  true,
			Username: "ftw",
			Password: caldavPassword,
		},
	}
	coordinator := selftune.NewCoordinator()

	srv := New(&Deps{
		Ctrl: ctrl, CtrlMu: &sync.Mutex{},
		Tel: tel, LogRing: telemetry.NewLogRing(), Version: "test",
		CfgMu: &sync.RWMutex{}, Cfg: cfg,
		CalDAV:   calendar.New(*cfg.CalDAV, nil, nil, "lp1"),
		SelfTune: coordinator,
		Models:   map[string]*battery.Model{"pixii-1": battery.New("pixii-1")},
		ModelsMu: &sync.Mutex{},
		DtS:      5,
		WebDir:   t.TempDir(),
	})

	frames := &appFrames{}
	box := &appBox{ctrl: ctrl}
	handler, err := appproto.New(appproto.Config{
		Clock:  appproto.SystemClock{StartedAt: time.Now(), Source: "ntp"},
		Site:   box,
		Info:   box,
		Modes:  box,
		Plans:  box,
		Codec:  appuplink.Codec(),
		Sender: frames,
		API:    srv,
		Caller: apiauth.Caller{
			Subject: apiauth.KindApp + ":aBcD1234",
			Kind:    apiauth.KindApp,
			Role:    role,
			Scopes:  appproto.ScopesForRole(role),
			Epoch:   1,
		},
		Grants:     stillEnrolled{role: role},
		SrcGrid:    "meter",
		SrcPV:      "meter",
		SrcBattery: "meter",
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("building the app session: %v", err)
	}
	t.Cleanup(handler.Close)

	return &tieredRig{
		appRig:   &appRig{handler: handler, frames: frames, ctrl: ctrl, saved: &savedConfigs{}},
		srv:      srv,
		selfTune: coordinator,
	}
}

// carried is every byte the session sent back, head, chunks and all. What a
// leak test needs is the wire, not one message.
func (r *tieredRig) carried(t *testing.T) string {
	t.Helper()
	var out strings.Builder
	for _, env := range r.frames.snapshot() {
		if env.T == appproto.MsgAPIChunk {
			out.Write(decode[appproto.APIChunk](t, env).Data)
		}
	}
	return out.String()
}

// --------------------------------------------------------------------------
// A read that hands out a credential is not a read
// --------------------------------------------------------------------------

// The CalDAV credential is a write channel into dispatch: the calendar it
// unlocks is what tells this box when the house is away and when the car has
// to be full. A family member given read-only access to watch the house walked
// away able to drive it.
func TestAViewerCannotReadACredential(t *testing.T) {
	rig := newTieredSession(t, apiauth.RoleViewer)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIGet, Path: "/api/caldav/credentials",
	})
	refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))

	if refusal.Code != appproto.ErrLocalOnly {
		t.Fatalf("refusal = %+v, want E_LOCAL_ONLY", refusal)
	}
	if rig.frames.has(appproto.MsgAPIHead) {
		t.Fatal("the credential handler answered an app session")
	}
	if body := rig.carried(t); strings.Contains(body, caldavPassword) {
		t.Fatalf("the CalDAV password crossed the session: %q", body)
	}
}

// An owner is refused too, and that is the point of the tier rather than a
// role check. The credential is the same credential whoever asks for it, and
// the box's own page — which needs somebody at home — is where it is shown.
func TestAnOwnerCannotReadACredentialEither(t *testing.T) {
	rig := newTieredSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIGet, Path: "/api/caldav/credentials", StepUp: true,
	})
	refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))

	if refusal.Code != appproto.ErrLocalOnly {
		t.Fatalf("refusal = %+v, want E_LOCAL_ONLY", refusal)
	}
	if body := rig.carried(t); strings.Contains(body, caldavPassword) {
		t.Fatalf("the CalDAV password crossed the session: %q", body)
	}
}

// The LAN still serves it. The claim being made is "only from your box's own
// page, from home" — if the box's page could not show it either, the sentence
// the app says would be a lie and the calendar feature would be unusable.
func TestTheBoxsOwnPageStillShowsTheCredential(t *testing.T) {
	rig := newTieredSession(t, apiauth.RoleOwner)

	req := httptest.NewRequest(http.MethodGet, "/api/caldav/credentials", nil)
	rec := httptest.NewRecorder()
	rig.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("the LAN got %d for the credential", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), caldavPassword) {
		t.Fatalf("the LAN no longer sees the credential: %s", rec.Body.String())
	}
}

// Every route whose answer carries a reusable secret, or a whole file this box
// cannot vouch for, swept as a viewer. None of them reaches a handler.
func TestNoSecretBearingReadCrossesTheSession(t *testing.T) {
	secretBearing := []string{
		"/api/caldav/credentials",
		"/api/config",
		"/api/backups/x",
		"/api/support/dump",
		"/api/oauth/myuplink/start",
		"/api/oauth/myuplink/callback",
		// vehicle_id is the RFID idTag the card presented, which is what
		// authorizes a charge at the charger itself.
		"/api/ocpp/chargers",
	}

	rig := newTieredSession(t, apiauth.RoleViewer)
	for i, path := range secretBearing {
		id := uint32(i + 1)
		rig.send(t, appproto.MsgAPIReq, id, appproto.APIReq{
			Method: appproto.APIGet, Path: path,
		})
		env := awaitID(t, rig.frames, id)
		if env.T != appproto.MsgError {
			t.Fatalf("GET %s answered %s; a secret-bearing read reached a handler", path, env.T)
		}
		if code := decode[appproto.ErrorBody](t, env).Code; code != appproto.ErrLocalOnly {
			t.Fatalf("GET %s was refused with %q, want E_LOCAL_ONLY", path, code)
		}
	}
	if rig.frames.has(appproto.MsgAPIHead) {
		t.Fatal("a secret-bearing read reached a handler")
	}
}

// --------------------------------------------------------------------------
// A POST that drives batteries is not configuration
// --------------------------------------------------------------------------

// Self-tune pauses normal control and drives each battery through ±1000 W and
// ±3000 W for about two and a half minutes. That is the house's batteries
// moving, so it belongs on the door with the lease — and there is no command
// for it, which is the honest answer the app gets.
func TestSelfTuneNeverRunsThroughThePassthrough(t *testing.T) {
	for _, role := range []string{apiauth.RoleOwner, apiauth.RoleViewer} {
		t.Run(role, func(t *testing.T) {
			rig := newTieredSession(t, role)

			rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
				Method: appproto.APIPost,
				Path:   "/api/self_tune/start",
				Body:   []byte(`{"batteries":["pixii-1"]}`),
				// Step-up asserted, so the refusal cannot be the step-up gate
				// standing in for the tier.
				StepUp: true,
			})
			refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))

			if role == apiauth.RoleOwner && refusal.Code != appproto.ErrUseCmd {
				t.Fatalf("refusal = %+v, want E_USE_CMD", refusal)
			}
			if rig.frames.has(appproto.MsgAPIHead) {
				t.Fatal("self-tune reached its handler through the passthrough")
			}
			// The box, which is what the test is about.
			if rig.selfTune.Status().Active {
				t.Fatal("the batteries were put under a step pattern from a phone")
			}
		})
	}
}

// Without step-up it is refused too. Both gates hold; neither is standing in
// for the other.
func TestSelfTuneIsRefusedWithoutStepUpAsWell(t *testing.T) {
	rig := newTieredSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/self_tune/start",
		Body:   []byte(`{"batteries":["pixii-1"]}`),
	})
	rig.frames.await(t, appproto.MsgError)

	if rig.selfTune.Status().Active {
		t.Fatal("the batteries were put under a step pattern from a phone")
	}
}

// The box's own page still starts one. Same reason as the credential: a tier
// that made the feature unreachable everywhere would be a different change.
func TestTheBoxsOwnPageStillStartsSelfTune(t *testing.T) {
	rig := newTieredSession(t, apiauth.RoleOwner)

	req := httptest.NewRequest(http.MethodPost, "/api/self_tune/start",
		strings.NewReader(`{"batteries":["pixii-1"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rig.srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("the LAN got %d starting self-tune: %s", rec.Code, rec.Body.String())
	}
	if !rig.selfTune.Status().Active {
		t.Fatal("the LAN's self-tune did not start")
	}
	rig.selfTune.Cancel()
}

// --------------------------------------------------------------------------
// A route with no tier is refused, not served
// --------------------------------------------------------------------------

// The wrong-safe default, and the whole argument for it: a route somebody adds
// without saying what it costs must fail loudly rather than be handed to
// whoever asks. This is allow-list over deny-list, one level up.
func TestARouteRegisteredWithoutATierIsRefused(t *testing.T) {
	srv := New(&Deps{Version: "test", WebDir: t.TempDir()})

	// Registered past handle(), which is the only way to end up with no tier
	// now that handle() demands one. It stands in for the next person's
	// mistake.
	var ran bool
	srv.mux.HandleFunc("GET /api/untiered", func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		writeJSON(w, 200, map[string]string{"secret": "served anyway"})
	})

	facts := srv.Route(newSyntheticRequest(http.MethodGet, "/api/untiered"))
	if facts.Tier != apiauth.TierLocal {
		t.Fatalf("an untiered route reports tier %q, want the closed one", facts.Tier)
	}
	if ran {
		t.Fatal("asking what a route costs ran it")
	}
}

// handle() refuses a tier it does not know, at startup rather than on the
// first request. A box that will not start is a box nobody is quietly
// over-trusting.
func TestHandleRefusesARouteWithoutAKnownTier(t *testing.T) {
	for _, tier := range []apiauth.Tier{"", "kind-of-read"} {
		t.Run(string(tier), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("registering a route at tier %q was allowed", tier)
				}
			}()
			srv := New(&Deps{Version: "test", WebDir: t.TempDir()})
			srv.handle("GET /api/nonsense", tier, func(http.ResponseWriter, *http.Request) {})
		})
	}
}

// Every route the table registers names a tier the box knows. Read out of the
// server rather than out of the source, so a route registered anywhere is in
// this test the day it is written.
func TestEveryRouteCarriesAKnownTier(t *testing.T) {
	srv := New(&Deps{Version: "test", WebDir: t.TempDir()})

	if len(srv.marks) < 100 {
		t.Fatalf("only %d routes carry a mark; the table has stopped registering them", len(srv.marks))
	}
	for pattern, mark := range srv.marks {
		if pattern == staticPattern {
			continue
		}
		if !mark.tier.Known() {
			t.Fatalf("%s carries tier %q, which is not one the gate has a branch for",
				pattern, mark.tier)
		}
	}
}
