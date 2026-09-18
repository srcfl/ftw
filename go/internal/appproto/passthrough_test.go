package appproto

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

type discardFrameSender struct{}

func (discardFrameSender) Send([]byte) error { return nil }

type failAtFrameSender struct {
	calls  int
	failAt int
	err    error
}

func (s *failAtFrameSender) Send([]byte) error {
	s.calls++
	if s.calls == s.failAt {
		return s.err
	}
	return nil
}

// stubAPI stands in for the box's HTTP layer where the test is about the
// carriage rather than about who may ask. The refusals that matter — a viewer
// writing, an actuating route, a whole-document write — are tested against the
// real handler in internal/api, because a fake that agrees with the gate
// proves nothing about the box.
type stubAPI struct {
	facts apiauth.RouteFacts
	serve func(http.ResponseWriter, *http.Request)

	mu   sync.Mutex
	seen *http.Request
}

func (s *stubAPI) Route(*http.Request) apiauth.RouteFacts { return s.facts }

func (s *stubAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seen = r
	s.mu.Unlock()
	s.serve(w, r)
}

func (s *stubAPI) request() *http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen
}

func text(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

// newAPIRig is a subscribed owner session over a stub gateway.
func newAPIRig(t *testing.T, api *stubAPI) (*Handler, *fakeBox, *recorder, *fakeGrants) {
	t.Helper()
	h, box, rec, _ := newRig(t)
	grants := newGrants()
	h.cfg.API = api
	h.cfg.Grants = grants
	t.Cleanup(h.Close)
	subscribe(t, h, rec)
	return h, box, rec, grants
}

// waitFor blocks until a frame of this type has been sent, because the
// passthrough answers on its own goroutine and Handle returns before it.
func waitFor(t *testing.T, rec *recorder, msgType string) sent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range rec.snapshot() {
			if f.env.T == msgType {
				return f
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no %s frame; got %s", msgType, rec.types())
	return sent{}
}

func apiChunks(rec *recorder) []byte {
	var out []byte
	for _, f := range rec.snapshot() {
		if f.env.T != MsgAPIChunk {
			continue
		}
		var c APIChunk
		if err := Unmarshal(f.env.B, &c); err == nil {
			out = append(out, c.Data...)
		}
	}
	return out
}

// --------------------------------------------------------------------------
// What may be asked
// --------------------------------------------------------------------------

// The path rules are the whole of what stops the session becoming a second
// origin onto the box's own web UI.
func TestAPathTheSessionDoesNotCarryIsRefused(t *testing.T) {
	cases := map[string]APIReq{
		"the static handler":  {Method: APIGet, Path: "/index.html"},
		"the root":            {Method: APIGet, Path: "/"},
		"a traversal":         {Method: APIGet, Path: "/api/../secrets"},
		"a doubled separator": {Method: APIGet, Path: "/api//status"},
		"a smuggled query":    {Method: APIGet, Path: "/api/status?force=1"},
		"a fragment":          {Method: APIGet, Path: "/api/status#x"},
		"a control character": {Method: APIGet, Path: "/api/sta\ntus"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			h, _, rec, _ := newAPIRig(t, &stubAPI{serve: text(`{}`)})
			deliver(t, h, MsgAPIReq, ptrU32(4), req)

			body := body[ErrorBody](t, rec.only(t, MsgError))
			if body.Code != ErrUnknownOp || body.Args["field"] != "path" {
				t.Fatalf("refusal was %+v, want E_UNKNOWN_OP on the path", body)
			}
			if rec.has(MsgAPIHead) {
				t.Fatal("a handler ran for a path the passthrough does not carry")
			}
		})
	}
}

func TestAMethodTheSessionDoesNotCarryIsRefused(t *testing.T) {
	h, _, rec, _ := newAPIRig(t, &stubAPI{serve: text(`{}`)})
	deliver(t, h, MsgAPIReq, ptrU32(4), APIReq{Method: "TRACE", Path: "/api/status"})

	body := body[ErrorBody](t, rec.only(t, MsgError))
	if body.Code != ErrUnknownOp || body.Args["field"] != "method" {
		t.Fatalf("refusal was %+v, want E_UNKNOWN_OP on the method", body)
	}
}

// A tier the gate has no branch for is refused, not served.
//
// The gate is one switch and every tier passes through it, so the only way
// past it is a value nobody wrote a branch for — a fifth tier added upstairs,
// or a route registered without one. Either way the handler must not run. The
// gate this replaced was a chain of cases with no read branch at all, and a
// route the router happened to call a read met nothing to fail: that is how a
// CalDAV password reached a viewer's phone.
func TestATierTheGateDoesNotKnowIsRefused(t *testing.T) {
	for _, tier := range []apiauth.Tier{"", "somebody's new idea"} {
		t.Run(string(tier), func(t *testing.T) {
			api := &stubAPI{
				facts: apiauth.RouteFacts{Tier: tier},
				serve: text(`{"password":"hunter2"}`),
			}
			h, _, rec, _ := newAPIRig(t, api)
			deliver(t, h, MsgAPIReq, ptrU32(4), APIReq{
				Method: APIGet, Path: "/api/whatever", StepUp: true,
			})

			refusal := body[ErrorBody](t, waitFor(t, rec, MsgError))
			if refusal.Code != ErrLocalOnly {
				t.Fatalf("refusal was %+v, want E_LOCAL_ONLY", refusal)
			}
			if rec.has(MsgAPIHead) || rec.has(MsgAPIChunk) {
				t.Fatal("a route with no tier the gate knows was served anyway")
			}
			if api.request() != nil {
				t.Fatal("a route with no tier the gate knows reached the handler")
			}
		})
	}
}

// A route the box does price as local is refused for that reason, with no role
// and no ceremony consulted. It is not a permission an owner is missing.
func TestALocalRouteIsRefusedForEveryCaller(t *testing.T) {
	api := &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierLocal},
		serve: text(`{"password":"hunter2"}`),
	}
	h, _, rec, _ := newAPIRig(t, api)
	deliver(t, h, MsgAPIReq, ptrU32(4), APIReq{
		Method: APIGet, Path: "/api/caldav/credentials", StepUp: true,
	})

	refusal := body[ErrorBody](t, waitFor(t, rec, MsgError))
	if refusal.Code != ErrLocalOnly {
		t.Fatalf("refusal was %+v, want E_LOCAL_ONLY", refusal)
	}
	if refusal.Args["path"] != "/api/caldav/credentials" {
		t.Fatalf("refusal args = %v, want the path it is about", refusal.Args)
	}
	if api.request() != nil {
		t.Fatal("a local route reached its handler through the session")
	}
	if strings.Contains(string(apiChunks(rec)), "hunter2") {
		t.Fatal("a local route's answer crossed the session")
	}
}

// The request the box builds is the whole of what the handler sees. Nothing
// the app sent describes the caller, and nothing about the caller is on the
// wire — the identity is put on the context here and nowhere else.
func TestTheRequestCarriesTheCallerAndNothingElseClaimsTo(t *testing.T) {
	api := &stubAPI{facts: apiauth.RouteFacts{Tier: apiauth.TierRead}, serve: text(`{}`)}
	h, _, rec, _ := newAPIRig(t, api)

	deliver(t, h, MsgAPIReq, ptrU32(7), APIReq{
		Method: APIGet,
		Path:   "/api/energy/history",
		Query:  map[string]string{"to": "2", "from": "1"},
	})
	waitFor(t, rec, MsgAPIEnd)

	got := api.request()
	if got == nil {
		t.Fatal("the handler never ran")
	}
	caller, ok := apiauth.FromRequest(got)
	if !ok {
		t.Fatal("the request reached the handler with no caller on it")
	}
	if caller.Kind != apiauth.KindApp || caller.Role != apiauth.RoleOwner {
		t.Fatalf("caller = %+v, want the session's enrolled owner", caller)
	}
	// Sorted, because url.Values.Encode sorts and both ends must agree on
	// the bytes the router sees.
	if got.URL.RawQuery != "from=1&to=2" {
		t.Fatalf("query = %q, want from=1&to=2", got.URL.RawQuery)
	}
	if got.Host != "localhost" || got.RemoteAddr != "127.0.0.1:0" {
		t.Fatalf("host %q from %q, want a local request", got.Host, got.RemoteAddr)
	}
	if got.Header.Get("Origin") != "" || got.Header.Get("Sec-Fetch-Site") != "" {
		t.Fatal("the synthetic request carries browser fetch metadata it never had")
	}
	if got.Header.Get("Authorization") != "" {
		t.Fatal("the synthetic request carries a bearer token")
	}
}

func TestABodyIsSentAsJSONAndNothingElse(t *testing.T) {
	api := &stubAPI{facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure}, serve: text(`{}`)}
	h, _, rec, _ := newAPIRig(t, api)

	deliver(t, h, MsgAPIReq, ptrU32(8), APIReq{
		Method: APIPost,
		Path:   "/api/battery_covers_ev",
		Body:   []byte(`{"enabled":true}`),
		StepUp: true,
	})
	waitFor(t, rec, MsgAPIEnd)

	got := api.request()
	if ct := got.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	read, _ := io.ReadAll(got.Body)
	if string(read) != `{"enabled":true}` {
		t.Fatalf("body reached the handler as %q", read)
	}
}

// --------------------------------------------------------------------------
// Carrying the answer
// --------------------------------------------------------------------------

func TestAnAnswerArrivesAsWholeChunksInOrder(t *testing.T) {
	want := strings.Repeat("x", APIChunkBytes*2+17)
	h, _, rec, _ := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierRead},
		serve: text(want),
	})

	deliver(t, h, MsgAPIReq, ptrU32(9), APIReq{Method: APIGet, Path: "/api/logs"})
	end := body[APIEnd](t, waitFor(t, rec, MsgAPIEnd))

	head := body[APIHeadMsg](t, rec.only(t, MsgAPIHead))
	if head.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", head.Status)
	}
	if got := string(apiChunks(rec)); got != want {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(want))
	}
	if end.Truncated || end.Bytes != int64(len(want)) {
		t.Fatalf("end = %+v, want the whole answer", end)
	}

	var seqs []uint32
	var sizes []int
	for _, f := range rec.snapshot() {
		if f.env.T != MsgAPIChunk {
			continue
		}
		c := body[APIChunk](t, f)
		seqs = append(seqs, c.Seq)
		sizes = append(sizes, len(c.Data))
		if f.lane != LaneBulk {
			t.Fatal("a chunk went out on lane 0, whose size must never vary")
		}
	}
	if len(seqs) != 3 || seqs[0] != 0 || seqs[1] != 1 || seqs[2] != 2 {
		t.Fatalf("chunk sequence %v, want 0,1,2", seqs)
	}
	if sizes[0] != APIChunkBytes || sizes[1] != APIChunkBytes || sizes[2] != 17 {
		t.Fatalf("chunk sizes %v, want two full chunks and a remainder", sizes)
	}
}

func TestAnAnswerPastTheCeilingStopsAndSaysSo(t *testing.T) {
	h, _, rec, _ := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierRead},
		serve: text(strings.Repeat("y", 40_000)),
	})

	deliver(t, h, MsgAPIReq, ptrU32(10), APIReq{
		Method: APIGet, Path: "/api/energy/history.csv", MaxBytes: 20_000,
	})
	end := body[APIEnd](t, waitFor(t, rec, MsgAPIEnd))

	if !end.Truncated {
		t.Fatal("an answer that ran past the ceiling was reported whole")
	}
	if end.Bytes != 20_000 {
		t.Fatalf("sent %d bytes, want the ceiling of 20000", end.Bytes)
	}
	if got := len(apiChunks(rec)); got != 20_000 {
		t.Fatalf("chunks carried %d bytes, want 20000", got)
	}
}

func TestAPIWriterKeepsOnlyOneChunkBuffered(t *testing.T) {
	ctx := context.Background()
	h := &Handler{cfg: Config{Codec: testCodec{}, Sender: discardFrameSender{}}, ctx: ctx, bucket: 512}
	w := &apiWriter{
		h: h, id: 1, ctx: ctx, max: APIMaxBytes, header: make(http.Header),
		buf: make([]byte, 0, APIChunkBytes),
	}
	w.header.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	payload := make([]byte, 4<<20)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if cap(w.buf) > 2*APIChunkBytes {
		t.Fatalf("writer retained a %d-byte buffer, want at most one chunk", cap(w.buf))
	}
}

func TestAPIWriterReportsBytesAcceptedBeforeAFlushError(t *testing.T) {
	wantErr := errors.New("carrier write failed")
	sender := &failAtFrameSender{failAt: 2, err: wantErr} // api.head succeeds; first chunk fails
	ctx := context.Background()
	h := &Handler{cfg: Config{Codec: testCodec{}, Sender: sender}, ctx: ctx, bucket: 512}
	w := &apiWriter{
		h: h, id: 1, ctx: ctx, max: APIMaxBytes, header: make(http.Header),
		buf: make([]byte, 0, APIChunkBytes),
	}
	w.header.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	n, err := w.Write(make([]byte, APIChunkBytes+17))
	if !errors.Is(err, wantErr) {
		t.Fatalf("Write error = %v, want %v", err, wantErr)
	}
	if n != APIChunkBytes {
		t.Fatalf("Write accepted %d bytes, want the %d bytes consumed before the flush failed", n, APIChunkBytes)
	}
	if w.written != APIChunkBytes {
		t.Fatalf("writer counted %d bytes, want %d actually accepted", w.written, APIChunkBytes)
	}
}

func BenchmarkAPIWriterLargeResponse(b *testing.B) {
	for _, size := range []int{1 << 20, 4 << 20, 8 << 20} {
		b.Run(fmt.Sprintf("%dMiB", size>>20), func(b *testing.B) {
			payload := make([]byte, size)
			ctx := context.Background()
			h := &Handler{
				cfg: Config{Codec: testCodec{}, Sender: discardFrameSender{}},
				ctx: ctx, bucket: 512,
			}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w := &apiWriter{
					h: h, id: 1, ctx: ctx, max: int64(size), header: make(http.Header),
					buf: make([]byte, 0, APIChunkBytes),
				}
				w.header.Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if _, err := w.Write(payload); err != nil {
					b.Fatal(err)
				}
				if err := w.finish(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Refused by class, before a byte streams. A PWA inside a Noise session has
// nothing useful to do with a ZIP.
func TestAnAnswerTheSessionCannotCarryIsRefusedAtTheHead(t *testing.T) {
	h, _, rec, _ := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierRead},
		serve: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/zip")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(make([]byte, 4096))
		},
	})

	deliver(t, h, MsgAPIReq, ptrU32(11), APIReq{Method: APIGet, Path: "/api/support/dump"})
	err := body[ErrorBody](t, waitFor(t, rec, MsgError))

	if err.Code != ErrUnsupportedMedia || err.Args["contentType"] != "application/zip" {
		t.Fatalf("refusal was %+v, want E_UNSUPPORTED_MEDIA naming the type", err)
	}
	if rec.has(MsgAPIHead) || rec.has(MsgAPIChunk) {
		t.Fatal("a refused answer still put bytes on the wire")
	}
}

// --------------------------------------------------------------------------
// One at a time, and never on the reader goroutine
// --------------------------------------------------------------------------

// blockingAPI holds a request until the test lets it go, or until its context
// ends. It then writes its answer regardless, which is what a handler that
// pays no attention to cancellation does — and the case the writer has to
// swallow rather than put on the wire.
func blockingAPI(started chan<- struct{}, release <-chan struct{}) (*stubAPI, <-chan struct{}) {
	returned := make(chan struct{})
	return &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierRead},
		serve: func(w http.ResponseWriter, r *http.Request) {
			defer close(returned)
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"late":true}`)
		},
	}, returned
}

// A call that ran on the reader goroutine would stall inbound frames for
// every phone on the connection, including their lane 0 stream.
func TestARequestDoesNotStallTheReader(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	api, _ := blockingAPI(started, release)
	h, _, rec, _ := newAPIRig(t, api)

	done := make(chan struct{})
	go func() {
		deliver(t, h, MsgAPIReq, ptrU32(12), APIReq{Method: APIGet, Path: "/api/status"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Handle blocked until the request finished")
	}

	<-started
	// The stream keeps running underneath it.
	if err := h.Tick(); err != nil {
		t.Fatalf("tick during a passthrough call: %v", err)
	}
	if !rec.has(MsgTick) && !rec.has(MsgDelta) {
		t.Fatalf("no lane 0 frame while a call was in flight; got %s", rec.types())
	}
}

func TestASecondRequestWhileOneIsInFlightIsRefused(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	api, _ := blockingAPI(started, release)
	h, _, rec, _ := newAPIRig(t, api)

	deliver(t, h, MsgAPIReq, ptrU32(13), APIReq{Method: APIGet, Path: "/api/status"})
	<-started
	deliver(t, h, MsgAPIReq, ptrU32(14), APIReq{Method: APIGet, Path: "/api/status"})

	err := body[ErrorBody](t, waitFor(t, rec, MsgError))
	if err.Code != ErrUnavailable || err.Args["reason"] != "busy" {
		t.Fatalf("second request met %+v, want E_UNAVAILABLE busy", err)
	}
	if !err.Retryable {
		t.Fatal("busy was reported as permanent")
	}
}

// --------------------------------------------------------------------------
// Revocation
// --------------------------------------------------------------------------

func TestARevokedGrantIsRefusedAndTheSessionEnds(t *testing.T) {
	h, _, rec, grants := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierRead},
		serve: text(`{"secret":true}`),
	})

	grants.revoke()
	deliver(t, h, MsgAPIReq, ptrU32(15), APIReq{Method: APIGet, Path: "/api/status"})

	err := body[ErrorBody](t, waitFor(t, rec, MsgError))
	if err.Code != ErrGrantRevoked {
		t.Fatalf("refusal was %+v, want E_GRANT_REVOKED", err)
	}
	term := body[SessionTerminate](t, waitFor(t, rec, MsgSessionTerminate))
	if term.Reason != TerminateRevoked {
		t.Fatalf("termination reason %q, want %q", term.Reason, TerminateRevoked)
	}
	if rec.has(MsgAPIHead) {
		t.Fatal("a revoked device reached a handler")
	}
}

// A demotion bumps the epoch without deleting the row, and the session the
// phone is holding must stop trusting the role it was admitted with — at the
// very next request, not at its next reconnect.
//
// It must also stop at exactly that. A demotion is not a revoke: the phone is
// still enrolled and a viewer may still read. Ending the session here would
// tell its holder their access was withdrawn, which is not what happened.
func TestADemotedOwnerLosesWritesAndKeepsReads(t *testing.T) {
	h, _, rec, grants := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure},
		serve: text(`{}`),
	})

	grants.setRole(apiauth.RoleViewer)

	// The write, refused for want of the role the grant no longer carries.
	deliver(t, h, MsgAPIReq, ptrU32(16), APIReq{
		Method: APIPost, Path: "/api/battery_covers_ev", StepUp: true,
	})
	err := body[ErrorBody](t, waitFor(t, rec, MsgError))
	if err.Code != ErrScopeDenied {
		t.Fatalf("refusal was %+v, want E_SCOPE_DENIED", err)
	}
	if err.Args["role"] != apiauth.RoleViewer {
		t.Fatalf("refusal args %v, want the role on file now", err.Args)
	}
	if rec.has(MsgAPIHead) {
		t.Fatal("a demoted owner's write reached a handler")
	}
	if rec.has(MsgSessionTerminate) {
		t.Fatal("a demotion ended the session, which tells its holder they were revoked")
	}

	// The read, which a viewer is entitled to and which must still work.
	h.cfg.API = &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierRead},
		serve: text(`{"ok":true}`),
	}
	deliver(t, h, MsgAPIReq, ptrU32(17), APIReq{Method: APIGet, Path: "/api/status"})
	head := body[APIHeadMsg](t, waitFor(t, rec, MsgAPIHead))
	if head.Status != http.StatusOK {
		t.Fatalf("a demoted owner's read answered %d, want 200", head.Status)
	}
}

// The other direction, and the one a comment is most likely to be wrong about:
// a guest promoted to owner may write without reconnecting first.
func TestAPromotedViewerCanWriteAtOnce(t *testing.T) {
	h, _, rec, grants := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure},
		serve: text(`{}`),
	})
	h.cfg.Caller = viewerCaller()
	grants.setRole(apiauth.RoleViewer)

	deliver(t, h, MsgAPIReq, ptrU32(18), APIReq{
		Method: APIPost, Path: "/api/battery_covers_ev", StepUp: true,
	})
	if err := body[ErrorBody](t, waitFor(t, rec, MsgError)); err.Code != ErrScopeDenied {
		t.Fatalf("a viewer's write was refused with %+v, want E_SCOPE_DENIED", err)
	}

	grants.setRole(apiauth.RoleOwner)
	deliver(t, h, MsgAPIReq, ptrU32(19), APIReq{
		Method: APIPost, Path: "/api/battery_covers_ev", StepUp: true,
	})
	head := body[APIHeadMsg](t, waitFor(t, rec, MsgAPIHead))
	if head.Status != http.StatusOK {
		t.Fatalf("a promoted viewer's write answered %d, want 200", head.Status)
	}
}

// The one revocation case a per-request check cannot catch on its own: the
// call was already running when the owner pressed revoke.
func TestARevokeStopsACallAlreadyInFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	api, returned := blockingAPI(started, release)
	h, _, rec, _ := newAPIRig(t, api)

	deliver(t, h, MsgAPIReq, ptrU32(17), APIReq{Method: APIGet, Path: "/api/logs"})
	<-started

	// What sessions.dropByAppKey does to a session whose key was revoked.
	// Nothing releases the handler except the end of the session.
	h.Close()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler kept running after the session was torn down")
	}

	// It wrote its answer on the way out, as a handler that ignores its
	// context does. None of it may reach the app.
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rec.has(MsgAPIHead) || rec.has(MsgAPIChunk) || rec.has(MsgAPIEnd) {
			t.Fatalf("a revoked session finished its call: %s", rec.types())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --------------------------------------------------------------------------
// Construction
// --------------------------------------------------------------------------

func TestNewRefusesASessionWithNoIdentity(t *testing.T) {
	_, box, rec, clock := newRig(t)
	base := Config{
		Clock: clock, Site: box, Info: box, Modes: box, Plans: box,
		Codec: testCodec{}, Sender: rec,
	}

	withRole := base
	withRole.Grants = newGrants()
	withRole.Caller = apiauth.Caller{Role: "administrator"}
	if _, err := New(withRole); err == nil {
		t.Fatal("a role that is not in the registry was accepted")
	}

	withoutGrants := base
	withoutGrants.Caller = ownerCaller()
	if _, err := New(withoutGrants); err == nil {
		t.Fatal("a session with no way to see a revoke was accepted")
	}
}

func TestAViewerSessionIsAnnouncedAsReadOnly(t *testing.T) {
	h, _, rec, _ := newRig(t)
	h.cfg.Caller = viewerCaller()

	deliver(t, h, MsgHello, nil, Hello{
		Proto: ProtoRange{Min: ProtoMin, Max: ProtoMax},
		App:   AppInfo{Build: "test", UA: "pwa"},
	})

	ok := body[HelloOK](t, rec.only(t, MsgHelloOK))
	if ok.Mode != BoxModeReadonly {
		t.Fatalf("box mode = %q, want %q", ok.Mode, BoxModeReadonly)
	}
	if ok.Role != apiauth.RoleViewer {
		t.Fatalf("role = %q, want %q", ok.Role, apiauth.RoleViewer)
	}
	if len(ok.Scopes) != 1 || ok.Scopes[0] != ScopeLiveRead {
		t.Fatalf("scopes = %v, want only the live read", ok.Scopes)
	}
}

// A handler that panics must not take the box with it. net/http would lose a
// connection; in process there is no connection to lose, and a phone able to
// end the box by opening the wrong screen would stop a house's dispatch.
func TestAPanickingHandlerDoesNotEndTheBox(t *testing.T) {
	h, _, rec, _ := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierRead},
		serve: func(http.ResponseWriter, *http.Request) {
			panic("a driver map was nil")
		},
	})

	deliver(t, h, MsgAPIReq, ptrU32(18), APIReq{Method: APIGet, Path: "/api/drivers"})
	err := body[ErrorBody](t, waitFor(t, rec, MsgError))

	if err.Code != ErrUnavailable || err.Args["reason"] != "handler" {
		t.Fatalf("refusal was %+v, want E_UNAVAILABLE from the handler", err)
	}

	// And the session still works.
	if err := h.Tick(); err != nil {
		t.Fatalf("tick after a panicking handler: %v", err)
	}
}

// --------------------------------------------------------------------------
// Step-up, and the grace window that stops a settings screen re-prompting
// --------------------------------------------------------------------------

// stepUpClock reaches the rig's clock so a test can move box uptime across the
// window boundary.
func stepUpClock(t *testing.T, h *Handler) *fakeClock {
	t.Helper()
	c, ok := h.cfg.Clock.(*fakeClock)
	if !ok {
		t.Fatalf("clock is %T, want *fakeClock", h.cfg.Clock)
	}
	return c
}

// waitIdle blocks until the passthrough queue is free, so the next request is
// served rather than refused as busy. The frame lands just before the serving
// goroutine clears the flag, so a test that only waited on the frame could race
// the very next deliver.
func waitIdle(t *testing.T, h *Handler) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !h.apiBusy.Load() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("passthrough stayed busy")
}

// The security property the window must not weaken: a configure with no
// ceremony behind it, and no recent one either, is still refused. This is the
// first write of any session — the app was never meant to skip it.
func TestAConfigureWithNoCeremonyIsRefused(t *testing.T) {
	h, _, rec, _ := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure},
		serve: text(`{}`),
	})

	deliver(t, h, MsgAPIReq, ptrU32(20), APIReq{
		Method: APIPost, Path: "/api/battery_covers_ev",
	})
	if err := body[ErrorBody](t, waitFor(t, rec, MsgError)); err.Code != ErrNeedsStepUp {
		t.Fatalf("refusal was %+v, want E_NEEDS_STEP_UP", err)
	}
	if rec.has(MsgAPIHead) {
		t.Fatal("a configure with no ceremony reached the handler")
	}
}

// Owner is enough for a route marked NoStepUp. The charging schedule is
// the case: login already proved who is asking.
func TestANoStepUpConfigureRunsWithoutACeremony(t *testing.T) {
	h, _, rec, _ := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure, NoStepUp: true},
		serve: text(`{"ok":true}`),
	})

	deliver(t, h, MsgAPIReq, ptrU32(27), APIReq{
		Method: APIPut, Path: "/api/loadpoints/1/schedule",
	})
	if head := body[APIHeadMsg](t, waitFor(t, rec, MsgAPIHead)); head.Status != http.StatusOK {
		t.Fatalf("answered %d, want 200", head.Status)
	}
	if rec.has(MsgError) {
		t.Fatal("a NoStepUp configure was refused a ceremony")
	}
}

func TestANoStepUpConfigureStillNeedsTheOwner(t *testing.T) {
	h, _, rec, grants := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure, NoStepUp: true},
		serve: text(`{"ok":true}`),
	})
	grants.setRole(apiauth.RoleViewer)

	deliver(t, h, MsgAPIReq, ptrU32(28), APIReq{
		Method: APIPut, Path: "/api/loadpoints/1/schedule",
	})
	if err := body[ErrorBody](t, waitFor(t, rec, MsgError)); err.Code != ErrScopeDenied {
		t.Fatalf("refusal was %+v, want E_SCOPE_DENIED", err)
	}
	if rec.has(MsgAPIHead) {
		t.Fatal("a viewer's NoStepUp write reached the handler")
	}
}

// A ready-time save must not open the window that lets an unmarked
// configure through. Otherwise an unlocked phone could change access
// after touching the schedule.
func TestANoStepUpWriteDoesNotOpenTheWindow(t *testing.T) {
	api := &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure, NoStepUp: true},
		serve: text(`{}`),
	}
	h, _, rec, _ := newAPIRig(t, api)

	deliver(t, h, MsgAPIReq, ptrU32(29), APIReq{
		Method: APIPut, Path: "/api/loadpoints/1/schedule",
	})
	waitFor(t, rec, MsgAPIEnd)
	waitIdle(t, h)

	api.facts = apiauth.RouteFacts{Tier: apiauth.TierConfigure}
	rec.reset()
	deliver(t, h, MsgAPIReq, ptrU32(30), APIReq{
		Method: APIPost, Path: "/api/app-link/pairing",
	})
	if err := body[ErrorBody](t, waitFor(t, rec, MsgError)); err.Code != ErrNeedsStepUp {
		t.Fatalf("a later unmarked configure was %+v, want E_NEEDS_STEP_UP", err)
	}
	if rec.has(MsgAPIHead) {
		t.Fatal("a schedule save opened the step-up window for a privileged write")
	}
}

// The whole point: one ceremony, then the writes that follow it on the same
// session cost nothing. A settings screen that subscribes, saves and tests is
// three configure calls; this is what turns three Face IDs into one.
func TestASecondConfigureWithinTheWindowSkipsTheCeremony(t *testing.T) {
	h, _, rec, _ := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure},
		serve: text(`{}`),
	})
	clock := stepUpClock(t, h)

	// The one ceremony: refused first, the app ran the passkey and retried
	// with stepUp. This opens the window.
	deliver(t, h, MsgAPIReq, ptrU32(21), APIReq{
		Method: APIPost, Path: "/api/battery_covers_ev", StepUp: true,
	})
	if head := body[APIHeadMsg](t, waitFor(t, rec, MsgAPIHead)); head.Status != http.StatusOK {
		t.Fatalf("the ceremony write answered %d, want 200", head.Status)
	}
	waitFor(t, rec, MsgAPIEnd)
	waitIdle(t, h)
	rec.reset()

	// A later write in the same screen, four minutes on, carrying no ceremony.
	clock.uptimeMs += 4 * 60 * 1000
	deliver(t, h, MsgAPIReq, ptrU32(22), APIReq{
		Method: APIPost, Path: "/api/battery_covers_ev",
	})
	if head := body[APIHeadMsg](t, waitFor(t, rec, MsgAPIHead)); head.Status != http.StatusOK {
		t.Fatalf("a write inside the window answered %d, want 200", head.Status)
	}
	if rec.has(MsgError) {
		t.Fatal("a write inside the window was re-prompted for a ceremony")
	}
}

// The window is recent human presence, not a session-long pass. Once it has
// elapsed, the next write pays a fresh ceremony. The boundary is exclusive, so
// a jump of exactly the window is already out.
func TestAConfigureAfterTheWindowNeedsAFreshCeremony(t *testing.T) {
	h, _, rec, _ := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure},
		serve: text(`{}`),
	})
	clock := stepUpClock(t, h)

	deliver(t, h, MsgAPIReq, ptrU32(23), APIReq{
		Method: APIPost, Path: "/api/battery_covers_ev", StepUp: true,
	})
	waitFor(t, rec, MsgAPIEnd)
	waitIdle(t, h)
	rec.reset()

	clock.uptimeMs += StepUpWindowMs
	deliver(t, h, MsgAPIReq, ptrU32(24), APIReq{
		Method: APIPost, Path: "/api/battery_covers_ev",
	})
	if err := body[ErrorBody](t, waitFor(t, rec, MsgError)); err.Code != ErrNeedsStepUp {
		t.Fatalf("a write past the window was %+v, want E_NEEDS_STEP_UP", err)
	}
	if rec.has(MsgAPIHead) {
		t.Fatal("a write past the window reached the handler without a ceremony")
	}
}

// The window is bound to the enrolment behind the session, not shared across
// them: one device's ceremony must never let another skip its own. A handler
// serves exactly one enrolled device, and its window lives and dies with it.
func TestOneSessionsCeremonyDoesNotOpenAnothers(t *testing.T) {
	// Device A proves presence.
	hA, _, recA, _ := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure},
		serve: text(`{}`),
	})
	deliver(t, hA, MsgAPIReq, ptrU32(25), APIReq{
		Method: APIPost, Path: "/api/battery_covers_ev", StepUp: true,
	})
	waitFor(t, recA, MsgAPIEnd)

	// Device B is a different enrolment and has run no ceremony of its own.
	hB, _, recB, _ := newAPIRig(t, &stubAPI{
		facts: apiauth.RouteFacts{Tier: apiauth.TierConfigure},
		serve: text(`{}`),
	})
	hB.cfg.Caller = apiauth.Caller{
		Subject: apiauth.KindApp + ":device-B",
		Kind:    apiauth.KindApp,
		Role:    apiauth.RoleOwner,
		Scopes:  ScopesForRole(apiauth.RoleOwner),
		Epoch:   1,
	}
	deliver(t, hB, MsgAPIReq, ptrU32(26), APIReq{
		Method: APIPost, Path: "/api/battery_covers_ev",
	})
	if err := body[ErrorBody](t, waitFor(t, recB, MsgError)); err.Code != ErrNeedsStepUp {
		t.Fatalf("device B inherited a window; got %+v, want E_NEEDS_STEP_UP", err)
	}
	if recB.has(MsgAPIHead) {
		t.Fatal("device B's write landed on another device's ceremony")
	}
}
