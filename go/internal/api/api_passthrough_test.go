package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/appuplink"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The app's passthrough, against this box's real HTTP handler.
//
// Everything here runs the request the box would run, through the handler the
// LAN listener serves. A fake gateway that agreed with the gate would prove
// nothing: what is being asserted is that a viewer cannot change this box, and
// the only convincing evidence is the box not changing.

// --------------------------------------------------------------------------
// A session, wired to the real API
// --------------------------------------------------------------------------

type appRig struct {
	handler *appproto.Handler
	frames  *appFrames
	ctrl    *control.State
	saved   *savedConfigs
}

// savedConfigs records what POST /api/config managed to write, so a test can
// assert on the box rather than on the error message.
type savedConfigs struct {
	mu   sync.Mutex
	runs int
}

func (s *savedConfigs) save(string, *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs++
	return nil
}

func (s *savedConfigs) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs
}

type appFrames struct {
	mu   sync.Mutex
	sent []appproto.Envelope
}

func (f *appFrames) Send(raw []byte) error {
	frame, err := appuplink.Codec().DecodeFrame(raw)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, frame.Envelope)
	return nil
}

func (f *appFrames) snapshot() []appproto.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]appproto.Envelope(nil), f.sent...)
}

// await returns the first frame of a type, or fails. The passthrough answers
// on its own goroutine, so there is nothing to wait on but the answer.
func (f *appFrames) await(t *testing.T, msgType string) appproto.Envelope {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, env := range f.snapshot() {
			if env.T == msgType {
				return env
			}
		}
		time.Sleep(time.Millisecond)
	}
	var seen []string
	for _, env := range f.snapshot() {
		seen = append(seen, env.T)
	}
	t.Fatalf("no %s frame; got %v", msgType, seen)
	return appproto.Envelope{}
}

func (f *appFrames) has(msgType string) bool {
	for _, env := range f.snapshot() {
		if env.T == msgType {
			return true
		}
	}
	return false
}

func decode[T any](t *testing.T, env appproto.Envelope) T {
	t.Helper()
	var v T
	if err := appproto.Unmarshal(env.B, &v); err != nil {
		t.Fatalf("decode %s: %v", env.T, err)
	}
	return v
}

// stillEnrolled is the grant as the box holds it, unchanged for the life of
// these tests. Revocation and demotion have their own tests in appproto.
type stillEnrolled struct{ role string }

func (s stillEnrolled) Grant() (string, uint64, bool) { return s.role, 1, true }

// newAppSession builds a real API server and one app session onto it.
//
// The options are for the dependencies a particular test needs the box to
// actually have — enrolment is one, and the sharing tests hand in the real
// appenroll.Identity so that what a guest ends up being is measured on the
// box rather than on a stub that agreed with the request.
func newAppSession(t *testing.T, role string, opts ...func(*Deps)) *appRig {
	t.Helper()

	ctrl := control.NewState(0, 50, "meter")
	tel := telemetry.NewStore()
	tel.DriverHealthMut("meter").RecordSuccess()
	saved := &savedConfigs{}
	cfg := &config.Config{}

	deps := &Deps{
		Ctrl: ctrl, CtrlMu: &sync.Mutex{},
		Tel: tel, LogRing: telemetry.NewLogRing(), Version: "test",
		CfgMu: &sync.RWMutex{}, Cfg: cfg,
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		SaveConfig: saved.save,
	}
	for _, opt := range opts {
		opt(deps)
	}
	srv := New(deps)

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

	return &appRig{handler: handler, frames: frames, ctrl: ctrl, saved: saved}
}

// send hands one message to the session as bytes, the way the relay would.
func (r *appRig) send(t *testing.T, msgType string, id uint32, body any) {
	t.Helper()
	raw, err := appproto.Marshal(body)
	if err != nil {
		t.Fatalf("marshal %s: %v", msgType, err)
	}
	frame, err := appuplink.Codec().EncodeFrame(appproto.Frame{
		Lane:     appproto.LaneBulk,
		Bucket:   16384,
		Envelope: appproto.Envelope{T: msgType, ID: &id, B: raw},
	})
	if err != nil {
		t.Fatalf("encode %s: %v", msgType, err)
	}
	if err := r.handler.Handle(context.Background(), frame); err != nil {
		t.Fatalf("handle %s: %v", msgType, err)
	}
}

// appBox is the least box a session needs. The control state is real, because
// it is what the assertions are about.
type appBox struct{ ctrl *control.State }

func (b *appBox) Snapshot() appproto.Snapshot {
	return appproto.Snapshot{Mode: b.ctrl.Mode, ControlRev: 1, DispatchBlockedBy: []string{}}
}
func (b *appBox) Identity() appproto.Identity {
	return appproto.Identity{ID: "box", Build: "test", TZ: "Europe/Stockholm"}
}
func (b *appBox) Boot() *appproto.BootProgress { return nil }
func (b *appBox) Latest() *mpc.Plan            { return nil }
func (b *appBox) Rev() uint64                  { return 0 }
func (b *appBox) CeilingW() *int64             { return nil }
func (b *appBox) SetMode(_ context.Context, m control.Mode) error {
	return b.ctrl.ApplyMode(m)
}
func (b *appBox) ObservedMode() (control.Mode, bool) { return b.ctrl.Mode, true }

// --------------------------------------------------------------------------
// A viewer cannot write. This is the one a reviewer will try hardest to break.
// --------------------------------------------------------------------------

func TestAViewerCannotWriteThroughEitherDoor(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleViewer)
	before := rig.ctrl.Mode

	// The command lane, with everything else about the command correct.
	rig.send(t, appproto.MsgCmd, 1, appproto.Cmd{
		CmdID:           "01920000-0000-7000-8000-000000000001",
		Op:              appproto.OpSetMode,
		Args:            map[string]any{"mode": string(control.ModeSelfConsumption)},
		NotValidAfterMs: 3_600_000, // box uptime, not wall clock
		Expect:          appproto.Expect{Rev: 1},
	})
	result := decode[appproto.CmdResult](t, rig.frames.await(t, appproto.MsgCmdResult))
	if result.State != appproto.CmdRejected {
		t.Fatalf("cmd state = %q, want rejected", result.State)
	}
	if result.Error == nil || result.Error.Code != appproto.ErrScopeDenied {
		t.Fatalf("cmd refusal = %+v, want E_SCOPE_DENIED", result.Error)
	}

	// The HTTP door, at a route that is only configuration.
	rig.send(t, appproto.MsgAPIReq, 2, appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/battery_covers_ev",
		Body:   []byte(`{"enabled":true}`),
		StepUp: true,
	})
	refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))
	if refusal.Code != appproto.ErrScopeDenied {
		t.Fatalf("api refusal = %+v, want E_SCOPE_DENIED", refusal)
	}
	if refusal.Args["needRole"] != apiauth.RoleOwner {
		t.Fatalf("refusal args = %v, want the role it needs", refusal.Args)
	}

	// The box, which is what the test is actually about.
	if rig.ctrl.Mode != before {
		t.Fatalf("a viewer changed the mode to %q", rig.ctrl.Mode)
	}
	if rig.ctrl.BatteryCoversEV {
		t.Fatal("a viewer changed a dispatch setting")
	}
	if rig.frames.has(appproto.MsgAPIHead) {
		t.Fatal("a viewer's write reached a handler")
	}
}

// The same route, the same body, an owner. Without this the test above would
// pass on a passthrough that refuses everything.
func TestAnOwnerConfiguresAndTheBoxChanges(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/battery_covers_ev",
		Body:   []byte(`{"enabled":true}`),
		StepUp: true,
	})
	head := decode[appproto.APIHeadMsg](t, rig.frames.await(t, appproto.MsgAPIHead))
	if head.Status != 200 {
		t.Fatalf("status = %d, want 200", head.Status)
	}
	rig.frames.await(t, appproto.MsgAPIEnd)

	if !rig.ctrl.BatteryCoversEV {
		t.Fatal("the owner's write reached a 200 but the box did not change")
	}
	if rig.frames.has(appproto.MsgError) {
		t.Fatal("an accepted request also produced an error")
	}
}

// Step-up costs one round trip on the first write of a session and nothing
// after. What it buys is a phone left unlocked on a table; the comment above
// the check says what it does not buy.
func TestAConfigureWithoutStepUpAsksForOne(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/battery_covers_ev",
		Body:   []byte(`{"enabled":true}`),
	})
	refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))

	if refusal.Code != appproto.ErrNeedsStepUp {
		t.Fatalf("refusal = %+v, want E_NEEDS_STEP_UP", refusal)
	}
	if refusal.Args["tier"] != string(apiauth.TierConfigure) {
		t.Fatalf("refusal args = %v, want the tier that needs it", refusal.Args)
	}
	if rig.ctrl.BatteryCoversEV {
		t.Fatal("a request that was told to step up changed the box anyway")
	}
}

// --------------------------------------------------------------------------
// The second door: actuation stays on cmd
// --------------------------------------------------------------------------

// An HTTP request carries no expiry, so it must never move energy — however
// well authenticated the phone sending it is.
func TestActuationThroughThePassthroughIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		req    appproto.APIReq
		wantOp string
	}{
		{
			name: "the mode, which has a command",
			req: appproto.APIReq{
				Method: appproto.APIPost, Path: "/api/mode",
				Body: []byte(`{"mode":"self_consumption"}`), StepUp: true,
			},
			wantOp: appproto.OpSetMode,
		},
		{
			name: "forcing a charge to start",
			req: appproto.APIReq{
				Method: appproto.APIPost, Path: "/api/loadpoints/1/force_start",
				Body: []byte(`{}`), StepUp: true,
			},
		},
		{
			name: "holding the battery",
			req: appproto.APIReq{
				Method: appproto.APIPost, Path: "/api/battery/manual_hold",
				Body: []byte(`{"power_w":3000}`), StepUp: true,
			},
		},
		{
			name: "the import ceiling that defends a fuse",
			req: appproto.APIReq{
				Method: appproto.APIPost, Path: "/api/peak_import_ceiling",
				Body: []byte(`{"peak_import_ceiling_w":1000}`), StepUp: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newAppSession(t, apiauth.RoleOwner)
			before := rig.ctrl.Mode

			rig.send(t, appproto.MsgAPIReq, 1, tc.req)
			refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))

			if refusal.Code != appproto.ErrUseCmd {
				t.Fatalf("refusal = %+v, want E_USE_CMD", refusal)
			}
			if tc.wantOp != "" && refusal.Args["op"] != tc.wantOp {
				t.Fatalf("refusal args = %v, want op %q", refusal.Args, tc.wantOp)
			}
			if rig.frames.has(appproto.MsgAPIHead) {
				t.Fatal("an actuating route ran through the passthrough")
			}
			if rig.ctrl.Mode != before || rig.ctrl.PeakImportCeilingW != 0 {
				t.Fatal("an actuating route reached the box through the passthrough")
			}
		})
	}
}

// The same command, on the lane that carries an expiry, still works.
func TestTheCommandLaneStillMovesTheMode(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgCmd, 1, appproto.Cmd{
		CmdID:           "01920000-0000-7000-8000-000000000002",
		Op:              appproto.OpSetMode,
		Args:            map[string]any{"mode": string(control.ModeSelfConsumption)},
		NotValidAfterMs: 3_600_000, // box uptime, not wall clock
		Expect:          appproto.Expect{Rev: 1},
	})
	result := decode[appproto.CmdResult](t, rig.frames.await(t, appproto.MsgCmdResult))

	if result.State != appproto.CmdApplied {
		t.Fatalf("cmd state = %q (%+v), want applied", result.State, result.Error)
	}
	if rig.ctrl.Mode != control.ModeSelfConsumption {
		t.Fatalf("mode = %q, want self_consumption", rig.ctrl.Mode)
	}
}

// --------------------------------------------------------------------------
// A write must not wipe settings its caller never knew about
// --------------------------------------------------------------------------

// POST /api/config replaces the whole configuration, so a body built from an
// older idea of it drops every field the sender had not heard of. That has
// already cost this project one silent regression on the LAN, where the
// browser had at least just loaded the document from this box.
func TestAWholeConfigWriteIsRefused(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIPost,
		Path:   "/api/config",
		Body:   []byte(`{"site":{"name":"home"}}`),
		StepUp: true,
	})
	refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))

	if refusal.Code != appproto.ErrWholeDocument {
		t.Fatalf("refusal = %+v, want E_WHOLE_DOCUMENT", refusal)
	}
	if rig.saved.count() != 0 {
		t.Fatalf("the configuration was written %d times", rig.saved.count())
	}
	if rig.frames.has(appproto.MsgAPIHead) {
		t.Fatal("a whole-document write reached the handler")
	}
}

// Reading it is fine, and is what the app needs to draw a settings screen.
func TestReadingTheConfigIsAllowedForAnyRole(t *testing.T) {
	for _, role := range []string{apiauth.RoleOwner, apiauth.RoleViewer} {
		t.Run(role, func(t *testing.T) {
			rig := newAppSession(t, role)

			rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
				Method: appproto.APIGet, Path: "/api/modes",
			})
			head := decode[appproto.APIHeadMsg](t, rig.frames.await(t, appproto.MsgAPIHead))
			if head.Status != 200 {
				t.Fatalf("status = %d, want 200", head.Status)
			}
			if head.Headers["Content-Type"] != "application/json" {
				t.Fatalf("headers = %v, want the content type", head.Headers)
			}

			var out []byte
			for _, env := range rig.frames.snapshot() {
				if env.T == appproto.MsgAPIChunk {
					out = append(out, decode[appproto.APIChunk](t, env).Data...)
				}
			}
			rig.frames.await(t, appproto.MsgAPIEnd)

			var answer struct {
				Modes []struct {
					Key string `json:"key"`
				} `json:"modes"`
			}
			if err := json.Unmarshal(out, &answer); err != nil {
				t.Fatalf("the answer was not the handler's JSON: %v (%q)", err, out)
			}
			if len(answer.Modes) == 0 {
				t.Fatal("the mode catalogue came back empty")
			}
		})
	}
}

// --------------------------------------------------------------------------
// What the box's own answers look like from the app's side
// --------------------------------------------------------------------------

// A 404 from a handler is an answer, not a refusal. The two are told apart by
// which message arrived, never by reading a body — and they are different
// sentences to a user: this box has no such route, versus this box looked and
// there is no such driver.
func TestAHandlersOwnNotFoundComesBackAsAStatus(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIGet, Path: "/api/drivers/no-such-driver/draft",
	})
	head := decode[appproto.APIHeadMsg](t, rig.frames.await(t, appproto.MsgAPIHead))

	if head.Status != 404 {
		t.Fatalf("status = %d, want the handler's own 404", head.Status)
	}
	if rig.frames.has(appproto.MsgError) {
		t.Fatal("a 404 from a handler was also reported as a protocol error")
	}
}

// A route this box does not have never reaches a handler at all: it falls
// through to the static file server, which the passthrough does not carry.
func TestARouteThisBoxDoesNotHaveIsRefused(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIGet, Path: "/api/no-such-thing",
	})
	refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))

	if refusal.Code != appproto.ErrUnknownOp || refusal.Args["field"] != "path" {
		t.Fatalf("refusal = %+v, want E_UNKNOWN_OP on the path", refusal)
	}
	if rig.frames.has(appproto.MsgAPIHead) {
		t.Fatal("a route this box does not have still produced a status")
	}
}

// The support dump is a multi-megabyte ZIP of logs, telemetry and config, and
// a PWA inside a Noise session has nothing useful to do with one.
//
// It used to be refused for its media type, which meant the handler ran and
// started building the archive before anything said no. Naming the route Local
// refuses it before that, and for the reason a person would give: this one
// lives on your box's own page, from home. The media-type guard is still
// there — TestAnAnswerTheSessionCannotCarryIsRefusedAtTheHead in appproto is
// what exercises it — but nothing in the route table now depends on it.
func TestTheSupportDumpIsRefusedBeforeItIsBuilt(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIGet, Path: "/api/support/dump", StepUp: true,
	})
	refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))

	if refusal.Code != appproto.ErrLocalOnly {
		t.Fatalf("refusal = %+v, want E_LOCAL_ONLY", refusal)
	}
	if rig.frames.has(appproto.MsgAPIHead) || rig.frames.has(appproto.MsgAPIChunk) {
		t.Fatal("a refused answer still put bytes on the wire")
	}
}

// An answer the session cannot carry whole stops at the ceiling and says so.
// The app treats a truncated answer as a failure rather than drawing part of
// one as though it were the whole.
func TestAnAnswerPastTheCeilingIsTruncatedNotSilentlyShort(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIGet, Path: "/api/modes", MaxBytes: 64,
	})
	end := decode[appproto.APIEnd](t, rig.frames.await(t, appproto.MsgAPIEnd))

	if !end.Truncated {
		t.Fatal("an answer cut off at the ceiling was reported whole")
	}
	if end.Bytes != 64 {
		t.Fatalf("carried %d bytes, want the ceiling of 64", end.Bytes)
	}
}

// --------------------------------------------------------------------------
// The tier a route reports
// --------------------------------------------------------------------------

// The tier is a fact about the handler, and the verb is not consulted.
//
// Each pair below is one path where the two answers differ. If the method ever
// creeps back into Route, every one of them changes at once.
func TestRouteTierIgnoresTheMethod(t *testing.T) {
	srv := New(&Deps{})

	cases := []struct {
		method, path string
		want         apiauth.Tier
		why          string
	}{
		{"GET", "/api/status", apiauth.TierRead, "an ordinary read is still a read"},
		{"HEAD", "/api/status", apiauth.TierRead, "and so is a HEAD of one"},
		{"GET", "/api/energy/history", apiauth.TierRead, ""},

		// A GET that is not a read. The reviewer's case: this hands out a
		// password that is a write channel into dispatch.
		{"GET", "/api/caldav/credentials", apiauth.TierLocal, "a credential is not a read"},
		{"GET", "/api/config", apiauth.TierLocal, "masking fails open when the driver catalogue cannot be read"},
		{"GET", "/api/logs", apiauth.TierLocal, "the box promises nothing about what a driver logged"},
		{"GET", "/api/backups/1", apiauth.TierLocal, "the archive holds config and state.db"},
		{"GET", "/api/scan", apiauth.TierConfigure, "it sweeps the home network"},

		// A POST that is not configuration.
		{"POST", "/api/self_tune/start", apiauth.TierActuate, "it drives every battery through a step pattern"},
		{"POST", "/api/notifications/test", apiauth.TierConfigure, "a late test message is the same message"},
		{"POST", "/api/mode", apiauth.TierActuate, ""},
		{"GET", "/api/planner/prefs", apiauth.TierRead, "household planner prefs are status"},
		{"POST", "/api/planner/prefs", apiauth.TierActuate, "prefs change dispatch"},
		{"DELETE", "/api/battery/manual_hold", apiauth.TierActuate, ""},

		// Sibling routes priced apart on purpose: a charging schedule is a
		// standing instruction about future days, so a late save is the same
		// instruction, only later — while the target route moves energy now.
		{"PUT", "/api/loadpoints/1/schedule", apiauth.TierConfigure, "a schedule saved late is the same instruction, only later"},
		{"DELETE", "/api/loadpoints/1/schedule", apiauth.TierConfigure, "removing the standing instruction is configuration too"},
		{"POST", "/api/loadpoints/1/target", apiauth.TierActuate, "its sibling moves energy now"},

		// One path, two prices: the app's toggles must be showable without
		// the write's step-up ceremony.
		{"GET", "/api/notifications/rules", apiauth.TierRead, "showing the toggles is a read"},
		{"PUT", "/api/notifications/rules", apiauth.TierConfigure, "flipping one is a write into config"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := newSyntheticRequest(tc.method, tc.path)
			if got := srv.Route(req).Tier; got != tc.want {
				t.Fatalf("tier = %q, want %q (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// Every mark must name a route the router actually has. A mark whose pattern
// no longer matches is an actuating route the passthrough would wave through,
// and it would fail silently.
func TestEveryMarkedRouteIsReachable(t *testing.T) {
	srv := New(&Deps{})
	if len(srv.marks) == 0 {
		t.Fatal("no route carries a mark")
	}
	for pattern, mark := range srv.marks {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			// The static catch-all is registered for every method.
			method, path = "GET", pattern
		}
		// Wildcards stand in for a concrete id, the way a real call would.
		concrete := wildcard.ReplaceAllString(path, "1")
		facts := srv.Route(newSyntheticRequest(method, concrete))
		if facts.Tier != mark.tier && mark.tier != "" {
			t.Fatalf("%s resolves to tier %q, but is marked %q", pattern, facts.Tier, mark.tier)
		}
		if facts.ReplacesAll != mark.replacesAll {
			t.Fatalf("%s resolves to replacesAll=%v, but is marked %v",
				pattern, facts.ReplacesAll, mark.replacesAll)
		}
		if facts.Static != mark.static {
			t.Fatalf("%s resolves to static=%v, but is marked %v",
				pattern, facts.Static, mark.static)
		}
	}
}

var wildcard = regexp.MustCompile(`\{[^}]*\}`)

func newSyntheticRequest(method, path string) *http.Request {
	return &http.Request{
		Method:     method,
		URL:        &url.URL{Path: path},
		Host:       "localhost",
		RemoteAddr: "127.0.0.1:0",
		Header:     http.Header{},
	}
}

// --------------------------------------------------------------------------
// Who the LAN is
// --------------------------------------------------------------------------

// The LAN branch writes down what the LAN already is. Every handler from here
// on reads its caller from the context, so authenticating the LAN later is a
// change to this one branch and nothing else.
func TestALANRequestArrivesAsALocalOwner(t *testing.T) {
	var got apiauth.Caller
	var found bool
	handler := Authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, found = apiauth.FromRequest(r)
	}), MutationPolicy{})

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !found {
		t.Fatal("a LAN request reached a handler with no caller")
	}
	if got.Kind != apiauth.KindLAN || got.Role != apiauth.RoleOwner {
		t.Fatalf("caller = %+v, want a local owner", got)
	}
	if !got.Scopes.Has("ftw.dispatch.write") {
		t.Fatal("the local caller lost a scope the LAN already has")
	}
}

// A caller the session already authenticated must not be replaced by the
// local one, or the passthrough would hand every phone full authority.
func TestAnAuthenticatedCallerIsNotOverwritten(t *testing.T) {
	var got apiauth.Caller
	handler := Authenticate(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = apiauth.FromRequest(r)
	}), MutationPolicy{})

	viewer := apiauth.Caller{
		Subject: "app:aBcD1234",
		Kind:    apiauth.KindApp,
		Role:    apiauth.RoleViewer,
		Scopes:  apiauth.NewScopeSet("ftw.live.read"),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req = req.WithContext(apiauth.WithCaller(req.Context(), viewer))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got.Role != apiauth.RoleViewer || got.Kind != apiauth.KindApp {
		t.Fatalf("caller = %+v, want the session's own viewer", got)
	}
	if got.Scopes.Has("ftw.dispatch.write") {
		t.Fatal("a viewer was handed the LAN's authority")
	}
}

// A path under /api/ that no handler claims falls through to the box's own
// file server. Serving the box's HTML through a phone's session would be a
// second origin under another name, so the router's own answer — that this is
// the static catch-all — is what refuses it.
func TestThePassthroughNeverReachesTheStaticServer(t *testing.T) {
	rig := newAppSession(t, apiauth.RoleOwner)

	rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{
		Method: appproto.APIGet, Path: "/api/index.html",
	})
	refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))

	if refusal.Code != appproto.ErrUnknownOp || refusal.Args["field"] != "path" {
		t.Fatalf("refusal = %+v, want E_UNKNOWN_OP on the path", refusal)
	}
	if rig.frames.has(appproto.MsgAPIHead) {
		t.Fatal("the static file server answered an app session")
	}
}
