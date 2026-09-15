package appproto

// The app's window onto the box's own HTTP API.
//
// Why this exists: the app could ask the box six things, and the box's own web
// UI could ask it 124. Every new view in the app meant a box release. This
// carries an ordinary HTTP request in process — no socket, no port, no TLS —
// and hands the answer back as a byte stream.
//
// It is a security improvement, not a relaxation. That same API is already
// served on the home LAN with no authentication at all; reaching it through a
// Noise session pinned to an enrolled device, gated by role and tier, is
// strictly stronger than what a household runs today.
//
// Two doors, and this is only one of them. Reads and configuration come
// through here. Anything that moves energy stays on cmd, because a command
// carries an expiry and preconditions and the box revalidates against fresh
// state before acting — and an HTTP request carries none of that.
//
// What a route costs is decided by what its handler DOES, declared beside the
// handler in api.routes and never inferred from the method. See apiauth.Tier
// for the four tiers and the two routes whose verbs lied about them.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

const (
	// APIChunkBytes is one chunk of an answer.
	//
	// Fixed, not sized by trial: the largest bulk bucket is 16384 with six
	// bytes of frame header plus the CBOR envelope, so 12 KiB always lands
	// inside it and the box never discovers an overrun on a live session.
	APIChunkBytes = 12288

	// APIMaxBytes is the most of an answer the box will send. A 4 MB answer
	// is some 340 chunks, which is fine over a relay and is why the app's
	// timeout for this matches its history timeout.
	APIMaxBytes = 8 << 20

	// apiHandlerTimeout bounds one in-process request.
	apiHandlerTimeout = 15 * time.Second

	// StepUpWindowMs is how long a genuine passkey ceremony vouches for the
	// configure calls that follow it on the same session.
	//
	// Five minutes, in box uptime like every other expiry here. A configure
	// tier action needs recent human presence, and one ceremony proves it;
	// what it must not do is prove it once per write. A settings screen that
	// subscribes, saves its rules and sends a test is three configure calls in
	// a row, and before this each paid its own Face ID — plus another for every
	// failed attempt. Five minutes is long enough to cover that flow with
	// retries and short enough that "recent" is still true: it is the sudo
	// grace shape, and it sits well under the 15-minute dispatcher lease, so a
	// window is never the longest-lived thing a session is trusted with. The
	// window runs from the last real ceremony and is never extended by a call
	// it merely waved through, so it can outlast a ceremony by at most this.
	StepUpWindowMs int64 = 5 * 60 * 1000
)

// APIGateway is the box's HTTP API as this package needs it.
//
// An interface, so the message layer never imports the API package: this
// package's job is what a message means, and it stays reviewable on its own.
type APIGateway interface {
	// ServeHTTP runs the request. The implementation is the same handler the
	// LAN listener serves, trust boundary included — going round it to the
	// bare mux would be a second, weaker door onto the same handlers.
	http.Handler

	// Route says what a route costs before it runs.
	Route(*http.Request) apiauth.RouteFacts
}

// GrantReader is this session's enrolment, as it stands right now.
//
// Read on every privileged request rather than at the handshake, because a
// socket outlives a revoke and it outlives a role change too. One string and
// one integer behind a mutex that is already there.
type GrantReader interface {
	// Grant is what the enrolment currently carries: the role it holds and
	// the epoch that answer was written under. ok is false when the device is
	// no longer enrolled at all — a revoke is an absence, not a stale number.
	Grant() (role string, epoch uint64, ok bool)
}

// --------------------------------------------------------------------------
// Taking the request
// --------------------------------------------------------------------------

func (h *Handler) onAPIReq(env Envelope) error {
	if env.ID == nil {
		// A response nobody can route is a response nobody asked for.
		return nil
	}
	if h.cfg.API == nil {
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnavailable,
			Retryable: ErrorRetryable[ErrUnavailable],
			Args:      map[string]any{"subsystem": "api"},
		})
	}

	var req APIReq
	if err := Unmarshal(env.B, &req); err != nil {
		h.log.Warn("undecodable api.req dropped", "err", err)
		return nil
	}

	if field := badAPIRequest(req); field != "" {
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnknownOp,
			Retryable: false,
			Args:      map[string]any{"t": MsgAPIReq, "field": field},
		})
	}

	// One at a time. Not for want of memory: sessions.route hands frames to
	// Handle on the reader goroutine, so a fifteen-second call that ran there
	// would stall inbound frames for every phone on this connection —
	// including their lane 0 stream.
	if !h.apiBusy.CompareAndSwap(false, true) {
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnavailable,
			Retryable: true,
			Args:      map[string]any{"reason": "busy"},
		})
	}

	id := *env.ID
	go func() {
		defer h.apiBusy.Store(false)
		if err := h.serveAPI(id, req); err != nil {
			h.log.Warn("api passthrough failed", "err", err)
		}
	}()
	return nil
}

// badAPIRequest names the field that makes a request unserveable, or "".
//
// The path rules are narrow on purpose. Only /api/: the box's static handler
// stays unreachable, because serving the box's own HTML through the session
// would be a second origin under another name. No "?" or "#": the query is a
// parsed map, and a path that can smuggle a query is a tier decided over a
// string the router will read differently.
func badAPIRequest(req APIReq) string {
	switch req.Method {
	case APIGet, APIHead, APIPost, APIPut, APIPatch, APIDelete:
	default:
		return "method"
	}
	switch {
	case !strings.HasPrefix(req.Path, "/api/"),
		len(req.Path) > 1024,
		strings.Contains(req.Path, ".."),
		strings.Contains(req.Path, "//"),
		strings.ContainsAny(req.Path, "?#"),
		strings.IndexFunc(req.Path, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0:
		return "path"
	}
	for key := range req.Query {
		if key == "" || strings.ContainsAny(key, "\x00\n\r") {
			return "query"
		}
	}
	return ""
}

// --------------------------------------------------------------------------
// Serving it
// --------------------------------------------------------------------------

func (h *Handler) serveAPI(id uint32, req APIReq) error {
	ctx, cancel := context.WithTimeout(h.ctx, apiHandlerTimeout)
	defer cancel()

	// Revocation, checked here and on every request. The session dying and
	// the next handshake failing are the other two layers; this is the one
	// that closes the window where a socket outlives the revoke.
	caller, enrolled := h.liveCaller()
	if !enrolled {
		if err := h.sendError(&id, ErrorBody{
			Code:      ErrGrantRevoked,
			Retryable: false,
		}); err != nil {
			return err
		}
		return h.Terminate(TerminateRevoked)
	}

	request := buildAPIRequest(ctx, caller, req)

	facts := h.cfg.API.Route(request)
	if refusal := h.gateAPI(caller, facts, req); refusal != nil {
		h.log.Info("api passthrough refused",
			"subject", caller.Subject, "method", req.Method, "path", req.Path,
			"tier", facts.Tier, "code", refusal.Code)
		return h.sendError(&id, *refusal)
	}

	if facts.Tier != apiauth.TierRead {
		// The box's own contribution to step-up is the part only the box can
		// do: record it. Nothing here is a claim that a passkey was checked.
		h.log.Info("api passthrough write",
			"subject", caller.Subject, "role", caller.Role,
			"method", req.Method, "path", req.Path, "stepUp", req.StepUp)
	}

	max := int64(APIMaxBytes)
	if req.MaxBytes > 0 && req.MaxBytes < max {
		max = req.MaxBytes
	}
	writer := &apiWriter{
		h: h, id: id, ctx: ctx, max: max, header: http.Header{},
		buf: make([]byte, 0, APIChunkBytes),
	}
	h.serveHandler(writer, request)
	return writer.finish()
}

// serveHandler runs the box's handler and survives it panicking.
//
// net/http recovers a panicking handler and loses one connection. In process
// there is no connection to lose: an unrecovered panic here would end the box,
// and a phone that could end the box by opening the wrong screen would stop a
// house's dispatch. The request is reported as failed and the session lives.
func (h *Handler) serveHandler(w *apiWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			h.log.Error("api handler panicked", "path", r.URL.Path, "err", p)
			w.panicked = true
		}
	}()
	h.cfg.API.ServeHTTP(w, r)
}

// gateAPI is the whole of what the passthrough refuses.
//
// Every request passes through the switch below and every tier has a branch,
// including read. That shape is the fix for how a credential got out: the gate
// used to be a chain of cases with no read branch at all, so a route the
// router happened to call a read met no check to fail. A switch with a closed
// default cannot be skipped by a tier nobody thought about.
func (h *Handler) gateAPI(caller apiauth.Caller, facts apiauth.RouteFacts, req APIReq) *ErrorBody {
	if facts.Static {
		// A path under /api/ that no handler claims falls through to the box's
		// own file server. The path rules above cannot see that — only the
		// router can — and the same refusal is the honest one: the session
		// does not carry this path.
		return &ErrorBody{
			Code:      ErrUnknownOp,
			Retryable: false,
			Args:      map[string]any{"t": MsgAPIReq, "field": "path"},
		}
	}

	switch facts.Tier {
	case apiauth.TierRead:
		// Nothing to check, and that is a claim about the tier rather than an
		// absence. A route is Read only if its answer changes nothing and
		// carries nothing back that could be replayed as authority — the box
		// decides that per route, beside the handler, in api.routes. Anything
		// that hands out a credential is Local and never arrives here.
		return nil

	case apiauth.TierConfigure:
		if facts.ReplacesAll {
			// A body that replaces a whole document drops every field the
			// sender did not know about. On the LAN the browser had just
			// loaded that document from this box; a phone on a relay, possibly
			// a year behind the box, has no such guarantee. The app's sentence
			// for this is that the setting lives on the box's own page.
			return &ErrorBody{
				Code:      ErrWholeDocument,
				Retryable: false,
				Args:      map[string]any{"path": req.Path},
			}
		}
		if caller.Role != apiauth.RoleOwner {
			// The role governs the passthrough; scopes govern named
			// operations. A path-to-scope map across 132 endpoints is
			// precisely the list that rots, so it is not built.
			//
			// The role here is the one on file this second, not the one the
			// handshake saw. An owner demoted while their phone is connected
			// is refused at their very next write.
			return &ErrorBody{
				Code:      ErrScopeDenied,
				Retryable: false,
				Args:      map[string]any{"needRole": apiauth.RoleOwner, "role": caller.Role},
			}
		}
		if facts.NoStepUp {
			// Owner is enough. Login already proved who is asking; a
			// second Face ID is the wrong cost for this write. Do not
			// open the window: a ready-time save must not let an
			// unmarked configure through.
			return nil
		}
		// Step-up, with a grace window. A configure action needs recent human
		// presence; one ceremony proves it, and the box now remembers that
		// proof for StepUpWindowMs instead of demanding a fresh one per write.
		// A genuine ceremony (stepUp on the wire) opens the window and every
		// configure call inside it is waved through — the sudo-timestamp shape,
		// which is what turns a settings screen's three prompts back into one.
		// The window is measured on the monotonic uptime clock the rest of the
		// box expires against, and it runs only from a real ceremony: a call it
		// merely waved through never extends it, so it outlives a ceremony by at
		// most the window and no chain of writes can hold it open unattended.
		now := h.cfg.Clock.UptimeMs()
		if req.StepUp {
			// What step-up buys: a phone left unlocked on a table cannot be
			// picked up and used to reconfigure the site, because the app will
			// not send stepUp without a ceremony.
			//
			// What it does not buy: anything against a modified client. The
			// box CANNOT verify that a passkey ceremony happened — it has no
			// relationship with the authenticator, and being a WebAuthn
			// relying party would need an origin, which the box is
			// deliberately never. A modified client on an enrolled device can
			// already send a cmd today. Do not let this comment, or any other,
			// grow into a claim that the box checked a signature. The window
			// below changes none of this: it remembers the app's word, it does
			// not upgrade it to proof.
			h.stepUpAtMs.Store(now)
			return nil
		}
		if at := h.stepUpAtMs.Load(); at > 0 && now-at < StepUpWindowMs {
			// A genuine ceremony on this session is still recent. Accept
			// without re-prompting — this is the whole point of the window.
			return nil
		}
		return &ErrorBody{
			Code:      ErrNeedsStepUp,
			Retryable: true,
			Args:      map[string]any{"tier": string(facts.Tier)},
		}

	case apiauth.TierActuate:
		// The second door, and the sharper call than "actuation costs an
		// extra step". A command carries an expiry and the box revalidates
		// against fresh state before acting; an HTTP request has no expiry,
		// and a request with no expiry must never move energy. So actuation
		// has exactly one door and it is the one with the lease.
		//
		// No role check and no step-up check above it: the tier is refused for
		// everybody, so a stronger caller would only reach the same answer by
		// a longer route.
		args := map[string]any{"path": req.Path}
		if facts.CmdOp != "" {
			args["op"] = facts.CmdOp
		}
		return &ErrorBody{Code: ErrUseCmd, Retryable: false, Args: args}

	case apiauth.TierLocal:
		// Its answer holds a credential or a whole file, or doing it needs
		// somebody standing at the box. Not a permission the owner is missing,
		// so neither the role nor the ceremony is consulted — asking again as
		// somebody else gets the same answer. The app's sentence is that this
		// one lives on the box's own page, from home.
		return &ErrorBody{
			Code:      ErrLocalOnly,
			Retryable: false,
			Args:      map[string]any{"path": req.Path},
		}

	default:
		// A tier this gate has no branch for. It can only be a route nobody
		// priced, so it is refused exactly as Local is and logged loudly
		// enough to be fixed. Wrong-safe: the cost is a view the app cannot
		// draw, not a control a stranger can reach.
		h.log.Error("api route has no tier the gate knows",
			"path", req.Path, "method", req.Method, "tier", facts.Tier)
		return &ErrorBody{
			Code:      ErrLocalOnly,
			Retryable: false,
			Args:      map[string]any{"path": req.Path},
		}
	}
}

// buildAPIRequest makes the in-process request.
//
// Host localhost, RemoteAddr loopback, no Origin, no Sec-Fetch-Site and no
// forwarding header: the API's own trust boundary then sees a local client and
// does not reach for the LAN bearer token. Correct, because this session's
// Noise authentication is stronger than that token and it travels in the
// request context, where nothing on the wire can reach it.
func buildAPIRequest(ctx context.Context, caller apiauth.Caller, req APIReq) *http.Request {
	values := url.Values{}
	for k, v := range req.Query {
		values.Set(k, v)
	}
	target := &url.URL{Path: req.Path, RawQuery: values.Encode()}

	var body io.ReadCloser = http.NoBody
	var length int64
	if len(req.Body) > 0 {
		body = io.NopCloser(bytes.NewReader(req.Body))
		length = int64(len(req.Body))
	}

	request := &http.Request{
		Method:        req.Method,
		URL:           target,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          body,
		ContentLength: length,
		Host:          "localhost",
		RemoteAddr:    "127.0.0.1:0",
		RequestURI:    target.RequestURI(),
	}
	if length > 0 {
		request.Header.Set("Content-Type", "application/json")
	}

	caller.StepUp = req.StepUp
	return request.WithContext(apiauth.WithCaller(ctx, caller))
}

// --------------------------------------------------------------------------
// Writing the answer back
// --------------------------------------------------------------------------

// apiWriter turns an http.ResponseWriter into api.head, api.chunk and api.end.
type apiWriter struct {
	h   *Handler
	id  uint32
	ctx context.Context
	max int64

	header    http.Header
	buf       []byte
	seq       uint32
	written   int64
	wroteHead bool
	refused   bool
	truncated bool
	panicked  bool
	sendErr   error
}

func (w *apiWriter) Header() http.Header { return w.header }

func (w *apiWriter) WriteHeader(status int) {
	if w.wroteHead || w.refused {
		return
	}
	if w.ctx.Err() != nil {
		// Out of time, or the session is gone. Either way the status must not
		// go out: once one has, the app is committed to it, and finish is
		// what says so instead.
		return
	}

	// Refused by class, never by a path list. /api/support/dump streams a
	// multi-megabyte ZIP, and a PWA inside a Noise session has nothing useful
	// to do with one; carrying it would be promising more than this delivers.
	// The app's sentence: that one is only available on your box's own page,
	// from home.
	contentType := w.header.Get("Content-Type")
	if contentType != "" && !carriedMedia(contentType) {
		w.refused = true
		w.sendErr = w.h.sendError(&w.id, ErrorBody{
			Code:      ErrUnsupportedMedia,
			Retryable: false,
			Args:      map[string]any{"contentType": contentType},
		})
		return
	}

	w.wroteHead = true
	w.sendErr = w.h.sendBulk(MsgAPIHead, &w.id, APIHeadMsg{
		Status:  status,
		Headers: carriedHeaders(w.header),
		Len:     declaredLength(w.header),
	})
}

func (w *apiWriter) Write(p []byte) (int, error) {
	if !w.wroteHead && !w.refused {
		w.WriteHeader(http.StatusOK)
	}
	if w.refused {
		return 0, errAPIStopped
	}
	if w.sendErr != nil {
		return 0, w.sendErr
	}
	if w.ctx.Err() != nil {
		// Either the fifteen seconds are up or the session is gone. Either
		// way this answer is not going anywhere; say so to the handler rather
		// than let it keep building one.
		return 0, errAPIStopped
	}

	room := w.max - w.written
	if room <= 0 {
		w.truncated = true
		return 0, errAPIStopped
	}
	take := p
	if int64(len(take)) > room {
		take = take[:room]
		w.truncated = true
	}
	accepted := 0
	for len(take) > 0 {
		space := APIChunkBytes - len(w.buf)
		n := min(space, len(take))
		w.buf = append(w.buf, take[:n]...)
		take = take[n:]
		accepted += n
		w.written += int64(n)
		if len(w.buf) == APIChunkBytes {
			if err := w.flush(APIChunkBytes); err != nil {
				w.sendErr = err
				return accepted, err
			}
		}
	}
	if w.truncated {
		return accepted, errAPIStopped
	}
	return accepted, nil
}

func (w *apiWriter) flush(n int) error {
	chunk := APIChunk{Seq: w.seq, Data: w.buf[:n]}
	w.seq++
	if err := w.h.sendBulk(MsgAPIChunk, &w.id, chunk); err != nil {
		return err
	}
	copy(w.buf, w.buf[n:])
	w.buf = w.buf[:len(w.buf)-n]
	return nil
}

// finish closes the answer.
func (w *apiWriter) finish() error {
	if w.h.ctx.Err() != nil {
		// The session was torn down while the handler ran — revoked, or the
		// socket died. Nothing goes out: the transport is closed, so anything
		// written would go nowhere anyway. This is what makes a revoke stop a
		// call already in flight rather than merely the next one.
		return nil
	}
	if w.refused {
		return w.sendErr
	}
	if w.sendErr != nil {
		return w.sendErr
	}

	if !w.wroteHead {
		if w.ctx.Err() != nil || w.panicked {
			// Nothing was reported yet, so the honest answer is that the box
			// could not serve it. After a status has gone out the app is
			// committed to that status, which is why the same timeout ends
			// differently below.
			reason := "timeout"
			if w.panicked {
				reason = "handler"
			}
			return w.h.sendError(&w.id, ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"subsystem": "api", "reason": reason},
			})
		}
		// A handler that returned without writing anything. net/http would
		// have called this 200 with an empty body, and so does this.
		w.WriteHeader(http.StatusOK)
		if w.sendErr != nil {
			return w.sendErr
		}
	}

	if len(w.buf) > 0 {
		if err := w.flush(len(w.buf)); err != nil {
			return err
		}
	}
	// A panic after the status went out leaves an answer that stops in the
	// middle. The app is committed to the status by then, so the only honest
	// thing left to say is that this is not the whole of it.
	truncated := w.truncated || w.ctx.Err() != nil || w.panicked
	return w.h.sendBulk(MsgAPIEnd, &w.id, APIEnd{Bytes: w.written, Truncated: truncated})
}

// errAPIStopped tells a handler its answer is no longer wanted. A handler that
// ignores write errors simply finishes into a discarded buffer, which costs
// the box some work and the session nothing.
var errAPIStopped = errors.New("appproto: the passthrough stopped reading")

// carriedMedia is the class test. application/json, anything +json, and
// text/*. Everything else meets a refusal before a byte streams.
func carriedMedia(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	switch {
	case mediaType == "application/json",
		strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json"),
		strings.HasPrefix(mediaType, "text/"):
		return true
	}
	return false
}

// apiCarriedHeaders is everything the app is told about an answer. Short on
// purpose: a header the app has no use for is a fact about the box's LAN with
// nowhere useful to go.
var apiCarriedHeaders = []string{"Content-Type", "ETag", "Last-Modified"}

func carriedHeaders(header http.Header) map[string]string {
	out := map[string]string{}
	for _, name := range apiCarriedHeaders {
		if v := header.Get(name); v != "" {
			out[name] = v
		}
	}
	return out
}

// declaredLength is the handler's own Content-Length, when it set one. Most do
// not in process, and a guessed figure would be a progress bar that lies.
func declaredLength(header http.Header) *int64 {
	raw := header.Get("Content-Length")
	if raw == "" {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	return &n
}
