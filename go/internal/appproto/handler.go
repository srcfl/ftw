package appproto

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/control"
)

// Config wires the message layer to the box. Every field except Logger,
// Caps and NewLeaseID is required.
type Config struct {
	Clock  UptimeClock
	Site   SiteReader
	Info   BoxInfoReader
	Modes  ModeController
	Plans  PlanReader
	Codec  FrameCodec
	Sender Sender

	// History serves stored series, or nil on a box that keeps none. Optional
	// on purpose: the capability is only advertised when a provider exists,
	// and an advertised-but-broken feature is worse than an absent one.
	History HistoryProvider

	// Prices serves stored electricity prices, or nil on a box with no price
	// service or no zone to fetch one for. Optional for the same reason.
	Prices PriceReader

	// API is the box's own HTTP API, served in process. Nil on a box that
	// does not offer it; the capability then stays unsaid and the app hides
	// every view that needs it, exactly like history.
	API APIGateway

	// Loadpoints is the box's EV charging, or nil on a box with none. The
	// two loadpoint ops then answer E_UNAVAILABLE — the session's word for
	// the 503 the HTTP routes give — because the op exists on every box of
	// this build and the subsystem is what is missing.
	Loadpoints Loadpoints

	// Caller is whose session this is. Required: a session always belongs to
	// one enrolled device, and a handler that does not know which one cannot
	// refuse anything.
	Caller apiauth.Caller

	// Grants is that enrolment as it stands right now, re-read on every
	// privileged request. Required, because revocation is not optional.
	Grants GrantReader

	// Caps is what this box advertises. Names must come from the generated
	// contract constants; a typo is refused at construction rather than
	// silently hiding a feature in the app.
	Caps []string

	// Source ids for the three frozen power fields. The dictionary points
	// each reading at one of these, which is how the app turns a number
	// into a freshness claim.
	SrcGrid    string
	SrcPV      string
	SrcBattery string

	// NewLeaseID makes dispatcher lease ids. Injectable so tests are not
	// forced to match random strings.
	NewLeaseID func() string

	Logger *slog.Logger
}

// Handler turns frames into box behaviour and back.
//
// One handler serves one session. It owns no hardware and reaches control only
// through the ports above.
type Handler struct {
	cfg   Config
	log   *slog.Logger
	modes []ModeInfo
	dict  map[string]FieldDef
	cmds  *cmdLog
	// ops is this handler's own copy of the operation table.
	ops map[string]opSpec

	// stepUpAtMs is the box uptime at this session's last genuine passkey
	// ceremony (a configure request that arrived carrying stepUp), or 0 for
	// none yet. It opens a short grace window — StepUpWindowMs — inside which
	// further configure calls are accepted without a fresh ceremony, so a
	// multi-step settings screen costs one Face ID rather than one per write.
	// Bound to this handler, and a handler serves exactly one enrolled device,
	// so no session inherits another's window. Monotonic, so a wall-clock jump
	// cannot widen it. Atomic: the passthrough queue is depth one, but reading
	// it lock-free keeps the gate off the handler mutex.
	stepUpAtMs atomic.Int64

	// ctx is the session's lifetime and cancel ends it. A passthrough
	// request derives from this, so Close stops a call already in flight
	// rather than only the next one.
	ctx    context.Context
	cancel context.CancelFunc
	// apiBusy is the passthrough queue, and its depth is one.
	apiBusy atomic.Bool

	// History has one worker per session while work exists. A new query replaces
	// the queued or running one, and cancellation reaches SQLite through the
	// HistoryProvider context.
	histMu         sync.Mutex
	histGeneration uint64
	histActive     uint64
	histRunning    bool
	histCancel     context.CancelFunc
	histPending    *historyRequest

	mu         sync.Mutex
	proto      int
	subscribed bool
	bucket     int
	// cadenceEvery is the number of one-second uplink pulses between lane 0
	// frames. cadenceLeft counts down to the next one. Only 1 and 5 are valid:
	// one hertz while visible, 0.2 hertz while hidden.
	cadenceEvery uint8
	cadenceLeft  uint8
	seq          uint64
	// lastSent is the value the client was last told for each field. Only
	// fields actually put on the wire are recorded, so anything dropped to
	// fit a bucket is simply still different next tick and goes then.
	lastSent    map[string]int64
	lastSources map[string]SourceWire
	lastBlocked []string
	// lastRev is the controlRev this client was last told. A command carries
	// the rev it expected, so a client left holding a stale one would have
	// every command refused with no way to find out the new number — the
	// snapshot is the only message that carries it.
	lastRev uint64
}

// DefaultCaps is what this layer alone can back.
//
// History and the DER capabilities are absent on purpose: a capability the box
// advertises but cannot answer is worse than one it never mentions, because
// the app hides an absent feature and hangs on a broken one.
func DefaultCaps() []string {
	return []string{
		CapStatusCore,
		CapCmdLease,
		CapCmdPrecondition,
		CapCmdReadback,
		CapPlanDispatch,
	}
}

// New builds a handler.
func New(cfg Config) (*Handler, error) {
	switch {
	case cfg.Clock == nil:
		return nil, errors.New("appproto: Clock is required")
	case cfg.Site == nil:
		return nil, errors.New("appproto: Site is required")
	case cfg.Info == nil:
		return nil, errors.New("appproto: Info is required")
	case cfg.Modes == nil:
		return nil, errors.New("appproto: Modes is required")
	case cfg.Plans == nil:
		return nil, errors.New("appproto: Plans is required")
	case cfg.Codec == nil:
		return nil, errors.New("appproto: Codec is required")
	case cfg.Sender == nil:
		return nil, errors.New("appproto: Sender is required")
	case cfg.Grants == nil:
		return nil, errors.New("appproto: Grants is required")
	}

	// A role nobody recognises carries nothing, and a handler that would
	// refuse everything is a bug worth failing at construction rather than
	// one session at a time.
	if _, known := apiauth.RoleScopes[cfg.Caller.Role]; !known {
		return nil, fmt.Errorf("appproto: %q is not a role in contract/registry.yaml", cfg.Caller.Role)
	}

	if cfg.Caps == nil {
		cfg.Caps = DefaultCaps()
	}
	known := make(map[string]bool, len(AllCapabilities))
	for _, c := range AllCapabilities {
		known[c] = true
	}
	for _, c := range cfg.Caps {
		if !known[c] {
			return nil, fmt.Errorf("appproto: %q is not a capability in contract/registry.yaml", c)
		}
	}

	if cfg.NewLeaseID == nil {
		cfg.NewLeaseID = uuid.NewString
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Handler{
		cfg:          cfg,
		log:          cfg.Logger.With("component", "appproto"),
		modes:        wireModes(control.ModeCatalog()),
		dict:         fieldDict(cfg.SrcGrid, cfg.SrcPV, cfg.SrcBattery),
		cmds:         newCmdLog(),
		ops:          defaultOps(),
		ctx:          ctx,
		cancel:       cancel,
		proto:        ProtoMax,
		bucket:       512,
		cadenceEvery: 1,
		cadenceLeft:  1,
		lastSent:     map[string]int64{},
	}, nil
}

// Close ends the handler's own work when the session goes.
//
// The one thing it must do is stop a passthrough request that is running now:
// a device locked out mid-call must not finish the call it is making. Safe to
// call more than once, and safe to call on a session that never used the
// passthrough.
func (h *Handler) Close() {
	h.cancel()
	h.histMu.Lock()
	if h.histCancel != nil {
		h.histCancel()
	}
	h.histPending = nil
	h.histMu.Unlock()
}

// ScopesForRole expands a registry role into the scopes it carries.
//
// The expansion lives here, once, because a role is a projection over scopes
// and a second projection somewhere else is how a codebase ends up with three
// authorisation namespaces. A role the registry does not define expands to
// nothing, so an unrecognised name can do nothing rather than everything.
func ScopesForRole(role string) apiauth.ScopeSet {
	return apiauth.NewScopeSet(apiauth.RoleScopes[role]...)
}

// liveCaller is whose session this is, as the box's records stand this second.
//
// Not the snapshot the handshake took. A socket outlives a revoke, and it
// outlives a role change too: an owner demoted to a viewer while their phone
// is in their hand must lose their writes at the next request, not at the next
// reconnect. Both doors — cmd and the passthrough — ask this, and neither
// trusts Config.Caller for anything that can change underneath it.
//
// The second result is false only when the enrolment is gone, which is a
// revoke and ends the session. A role that merely changed is not a revoke and
// must not be reported as one: the session carries on under the new role. The
// buttons the app drew from hello_ok are then wrong until it reconnects, and
// that is the right way round — the app drawing a button is presentation, and
// the box refusing it is the enforcement.
func (h *Handler) liveCaller() (apiauth.Caller, bool) {
	role, epoch, ok := h.cfg.Grants.Grant()
	if !ok {
		return apiauth.Caller{}, false
	}
	caller := h.cfg.Caller
	caller.Role = role
	caller.Epoch = epoch
	caller.Scopes = ScopesForRole(role)
	return caller, true
}

// Subscribed reports whether the client asked for the telemetry stream.
func (h *Handler) Subscribed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.subscribed
}

// Proto is the negotiated protocol version.
func (h *Handler) Proto() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.proto
}

// Handle takes one frame from the carrier.
//
// A frame that cannot be parsed is dropped, not fatal: the next one may be
// fine, and tearing down a working session over one bad frame is a worse
// failure than ignoring it.
func (h *Handler) Handle(ctx context.Context, frame []byte) error {
	f, err := h.cfg.Codec.DecodeFrame(frame)
	if err != nil {
		h.log.Warn("undecodable frame dropped", "err", err)
		return nil
	}
	return h.dispatch(ctx, f.Envelope)
}

func (h *Handler) dispatch(ctx context.Context, env Envelope) error {
	switch env.T {
	case MsgHello:
		return h.onHello(env)
	case MsgSub:
		return h.onSub(env)
	case MsgCmd:
		return h.onCmd(ctx, env)
	case MsgPlanGet:
		return h.onPlanGet(env.ID)
	case MsgPriceGet:
		return h.onPriceGet(ctx, env)
	case MsgHistQuery:
		return h.onHistQuery(ctx, env)
	case MsgAPIReq:
		return h.onAPIReq(env)
	default:
		// Unknown types are ignored, never fatal. That rule in both
		// directions is what lets a newer app talk to an older box and a
		// service worker pin a bundle without turning into a white screen.
		if env.ID != nil {
			return h.sendError(env.ID, ErrorBody{
				Code:      ErrUnknownOp,
				Retryable: ErrorRetryable[ErrUnknownOp],
				Args:      map[string]any{"t": env.T},
			})
		}
		h.log.Debug("ignoring unknown message type", "t", env.T)
		return nil
	}
}

// --------------------------------------------------------------------------
// Handshake
// --------------------------------------------------------------------------

func (h *Handler) onHello(env Envelope) error {
	var hello Hello
	if err := Unmarshal(env.B, &hello); err != nil {
		h.log.Warn("undecodable hello dropped", "err", err)
		return nil
	}

	// The box picks min(box max, client max) and never refuses. A hard
	// version wall is a white screen for everyone whose service worker
	// pinned an old bundle, which is a failure the user cannot act on.
	proto := ProtoMax
	if hello.Proto.Max < ProtoMax {
		proto = max(ProtoFloor, hello.Proto.Max)
	}

	boot := h.cfg.Info.Boot()

	mode := BoxModeFull
	switch {
	case boot != nil:
		mode = BoxModeBooting
	case proto == ProtoFloor:
		mode = BoxModeFloor
	case !h.canWrite():
		mode = BoxModeReadonly
	}

	caps := h.cfg.Caps
	hint := ""
	if proto == ProtoFloor {
		// Floor mode speaks the frozen subset and nothing else. Advertising
		// more would invite an app that cannot use it to try.
		caps = []string{CapStatusCore}
		hint = HintAppUpdate
	}

	source, syncedAtMs := h.cfg.Clock.Sync()
	id := h.cfg.Info.Identity()

	fastSub := hello.Sub != nil && boot == nil
	body := HelloOK{
		Proto: proto,
		Mode:  mode,
		Box:   BoxInfo{ID: id.ID, Build: id.Build, TZ: id.TZ},
		Clock: Clock{
			Source:     source,
			SyncedAtMs: syncedAtMs,
			UptimeMs:   h.cfg.Clock.UptimeMs(),
		},
		Caps:       caps,
		CapsHash:   capsHash(caps),
		Modes:      h.modes,
		Boot:       boot,
		Hint:       hint,
		Role:       h.cfg.Caller.Role,
		Scopes:     h.cfg.Caller.Scopes.Names(),
		Subscribed: fastSub,
	}

	h.mu.Lock()
	h.proto = proto
	h.mu.Unlock()

	// Bulk, not lane 0. The reply carries the capability list and the mode
	// catalogue, both of which vary in size with what the box supports, and
	// lane 0 exists so that its frames vary in size with nothing.
	if err := h.sendBulk(MsgHelloOK, nil, body); err != nil {
		return err
	}
	if fastSub {
		return h.applySub(*hello.Sub)
	}
	return nil
}

// canWrite reports whether this grant carries any scope that changes
// something. A grant with none is a session the app should draw without its
// buttons — which is presentation, not the enforcement. The enforcement is at
// every door: onCmd checks the operation's scope and the passthrough checks
// the role.
func (h *Handler) canWrite() bool {
	for _, scope := range WriteScopes {
		if h.cfg.Caller.Scopes.Has(scope) {
			return true
		}
	}
	return false
}

// capsHash lets the app cache a capability set across sessions and notice when
// it changed without diffing the list. Sorted first, because the same set in a
// different order is the same set.
func capsHash(caps []string) string {
	sorted := append([]string(nil), caps...)
	sort.Strings(sorted)
	sum := sha256.New()
	for _, c := range sorted {
		sum.Write([]byte(c))
		sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))[:16]
}

// --------------------------------------------------------------------------
// Telemetry
// --------------------------------------------------------------------------

func (h *Handler) onSub(env Envelope) error {
	var sub Sub
	if err := Unmarshal(env.B, &sub); err != nil {
		h.log.Warn("undecodable sub dropped", "err", err)
		return nil
	}

	if boot := h.cfg.Info.Boot(); boot != nil {
		args := map[string]any{"phase": boot.Phase, "pct": boot.Pct}
		if boot.EtaMs != nil {
			args["etaMs"] = *boot.EtaMs
		}
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrBooting,
			Retryable: ErrorRetryable[ErrBooting],
			Args:      args,
		})
	}

	return h.applySub(sub)
}

func (h *Handler) applySub(sub Sub) error {
	bucket := sub.Bucket
	if bucket != 256 && bucket != 512 {
		// Lane 0 is one of two sizes for the life of the session. An
		// unrecognised request gets the larger one rather than an error:
		// the client still gets its stream, at a size the box can hold to.
		bucket = 512
	}

	cadence := uint8(1)
	if sub.Hz == 0.2 {
		cadence = 5
	}

	h.mu.Lock()
	if h.subscribed {
		// The control bucket stays fixed for this Noise session. A visibility
		// change updates only the cadence and preserves the delta sequence.
		h.cadenceEvery = cadence
		h.cadenceLeft = cadence
		h.mu.Unlock()
		return nil
	}
	h.bucket = bucket
	h.seq = 0
	h.cadenceEvery = cadence
	h.cadenceLeft = cadence
	h.mu.Unlock()

	// Keep subscribed false until the full snapshot is on the wire. Otherwise
	// the one-second ticker can race ahead and send a delta before the snapshot
	// that gives it meaning.
	if err := h.sendSnapshot(); err != nil {
		return err
	}
	h.mu.Lock()
	h.subscribed = true
	h.mu.Unlock()
	return nil
}

// sendSnapshot puts the full site state on the bulk lane and records what the
// client now knows.
func (h *Handler) sendSnapshot() error {
	uptimeMs := h.cfg.Clock.UptimeMs()
	snap := h.cfg.Site.Snapshot()
	fields := fieldValues(snap, h.modes)
	sources := wireSources(snap, uptimeMs)
	blocked := snap.DispatchBlockedBy
	if blocked == nil {
		blocked = []string{}
	}

	h.mu.Lock()
	h.lastSent = make(map[string]int64, len(fields))
	for k, v := range fields {
		h.lastSent[k] = v
	}
	h.lastSources = sources
	h.lastBlocked = blocked
	h.lastRev = snap.ControlRev
	h.mu.Unlock()

	return h.sendBulk(MsgSnap, nil, Snap{
		UptimeMs:          uptimeMs,
		ControlRev:        snap.ControlRev,
		Dict:              h.dict,
		Fields:            fields,
		Sources:           sources,
		DispatchBlockedBy: blocked,
	})
}

// Tick consumes one global one-second pulse. The handler gates that pulse to
// this subscription's 1 Hz or 0.2 Hz cadence. On each due pulse it emits a
// frame even when nothing changed, so the relay cannot distinguish an idle
// house from an active one within the chosen cadence.
func (h *Handler) Tick() error {
	h.mu.Lock()
	if !h.subscribed {
		h.mu.Unlock()
		return nil
	}
	if h.cadenceLeft > 1 {
		h.cadenceLeft--
		h.mu.Unlock()
		return nil
	}
	h.cadenceLeft = h.cadenceEvery
	bucket := h.bucket
	h.mu.Unlock()

	if boot := h.cfg.Info.Boot(); boot != nil {
		return nil
	}

	uptimeMs := h.cfg.Clock.UptimeMs()
	snap := h.cfg.Site.Snapshot()

	// A controlRev the client has not been told is a client whose every
	// command will be refused for a conflict it cannot see. Only the snapshot
	// carries the number, so a change in it costs one bulk frame — which is
	// also how the client resyncs mid-session rather than on reconnect.
	h.mu.Lock()
	stale := snap.ControlRev != h.lastRev
	h.mu.Unlock()
	if stale {
		if err := h.sendSnapshot(); err != nil {
			return err
		}
	}

	fields := fieldValues(snap, h.modes)
	sources := wireSources(snap, uptimeMs)
	blocked := snap.DispatchBlockedBy
	if blocked == nil {
		blocked = []string{}
	}

	h.mu.Lock()
	changed := map[string]int64{}
	for k, v := range fields {
		if prev, ok := h.lastSent[k]; !ok || prev != v {
			changed[k] = v
		}
	}
	sourcesMoved := !sourcesEqual(sources, h.lastSources) || !stringsEqual(blocked, h.lastBlocked)
	h.seq++
	seq := h.seq
	h.mu.Unlock()

	if len(changed) == 0 && !sourcesMoved {
		return h.sendControl(MsgTick, nil, Tick{Seq: seq, UptimeMs: uptimeMs}, 0, bucket)
	}

	body := Delta{Seq: seq, UptimeMs: uptimeMs, Fields: changed}
	if sourcesMoved {
		// Source state MUST ride along in a delta, not wait for the next
		// snapshot. A device that goes quiet mid-session is precisely what
		// the freshness model exists to show, and a client that only hears
		// about it on reconnect would keep drawing a stale number as live.
		body.Sources = sources
		body.DispatchBlockedBy = blocked
	}

	sentFields, flags, err := h.fitDelta(&body, bucket)
	if err != nil {
		return err
	}

	if err := h.sendControl(MsgDelta, nil, body, flags, bucket); err != nil {
		return err
	}

	h.mu.Lock()
	for k := range sentFields {
		h.lastSent[k] = sentFields[k]
	}
	if body.Sources != nil {
		h.lastSources = sources
		h.lastBlocked = blocked
	}
	h.mu.Unlock()
	return nil
}

// fitDelta trims a delta until it fits its bucket, lowest-priority field
// first, and flags what it dropped.
//
// The bucket never grows and the frame never fragments, because both would
// make frame size a function of what happened in the house — which is the one
// thing lane 0's constancy exists to hide. Dropped fields are simply not
// recorded as sent, so they are still different on the next tick and go then.
// Field ids ascend by importance: 1-9 are the frozen core view.
func (h *Handler) fitDelta(body *Delta, bucket int) (map[string]int64, uint8, error) {
	const headerBytes = 6

	fits := func() (bool, error) {
		env, err := newEnvelope(MsgDelta, nil, body)
		if err != nil {
			return false, err
		}
		raw, err := EncodeEnvelope(env)
		if err != nil {
			return false, err
		}
		return len(raw)+headerBytes <= bucket, nil
	}

	ok, err := fits()
	if err != nil {
		return nil, 0, err
	}
	if ok {
		return body.Fields, 0, nil
	}

	var flags uint8 = FlagTrunc

	// Sources go first. They are the largest part of a delta by far, and a
	// fresh snapshot on the bulk lane carries them properly; the readings
	// are what the user is watching change.
	if body.Sources != nil {
		body.Sources = nil
		body.DispatchBlockedBy = nil
		ok, err = fits()
		if err != nil {
			return nil, 0, err
		}
		if ok {
			return body.Fields, flags, nil
		}
	}

	keys := make([]string, 0, len(body.Fields))
	for k := range body.Fields {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return fidOf(keys[i]) < fidOf(keys[j]) })

	for len(keys) > 0 {
		drop := keys[len(keys)-1]
		keys = keys[:len(keys)-1]
		delete(body.Fields, drop)
		ok, err = fits()
		if err != nil {
			return nil, 0, err
		}
		if ok {
			return body.Fields, flags, nil
		}
	}

	return nil, 0, fmt.Errorf("appproto: an empty delta does not fit a %d-byte bucket", bucket)
}

func fidOf(key string) uint64 {
	v, err := strconv.ParseUint(key, 10, 32)
	if err != nil {
		// An unparseable key sorts last, so it is the first thing dropped.
		return ^uint64(0)
	}
	return v
}

// --------------------------------------------------------------------------
// Commands
// --------------------------------------------------------------------------

func (h *Handler) onCmd(ctx context.Context, env Envelope) error {
	var cmd Cmd
	if err := Unmarshal(env.B, &cmd); err != nil {
		h.log.Warn("undecodable cmd dropped", "err", err)
		return nil
	}

	if cmd.CmdID == "" {
		// Without an idempotency key there is nothing to answer to and no
		// way to make a retry safe, so the command is refused outright.
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnknownOp,
			Retryable: false,
			Args:      map[string]any{"t": MsgCmd, "missing": "cmdId"},
		})
	}

	// Idempotency first. A retry must return the original outcome, not act a
	// second time — that is the entire reason the key exists.
	if rec, ok := h.cmds.lookup(cmd.CmdID); ok {
		if err := h.sendCmdAck(rec.ack); err != nil {
			return err
		}
		if rec.result != nil {
			return h.sendCmdResult(*rec.result)
		}
		return nil
	}

	uptimeMs := h.cfg.Clock.UptimeMs()

	// A command queued while the phone was in a tunnel must never execute as
	// though the user had just pressed the button.
	if cmd.NotValidAfterMs <= uptimeMs {
		return h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdExpired,
			Error: &ErrorBody{
				Code:      ErrCmdExpired,
				Retryable: ErrorRetryable[ErrCmdExpired],
				Args:      map[string]any{"notValidAfterMs": cmd.NotValidAfterMs, "uptimeMs": uptimeMs},
			},
		})
	}

	spec, known := h.ops[cmd.Op]
	if !known {
		return h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnknownOp,
				Retryable: false,
				Args:      map[string]any{"op": cmd.Op},
			},
		})
	}

	// Who is asking, as the box's records stand right now — the same question
	// the passthrough asks, answered the same way. A phone revoked or demoted
	// mid-session must not get one last command through on the strength of
	// what its handshake said.
	caller, enrolled := h.liveCaller()
	if !enrolled {
		if err := h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrGrantRevoked,
				Retryable: ErrorRetryable[ErrGrantRevoked],
			},
		}); err != nil {
			return err
		}
		return h.Terminate(TerminateRevoked)
	}

	// The scope the operation declares, checked against the scope this
	// session holds. defaultOps has declared a scope for every op since the
	// day it was written and nothing ever read it, which meant a viewer's
	// command was refused by nothing at all. This is the line that makes a
	// role real on the command lane; the passthrough's role gate is the same
	// property on the other one.
	if !caller.Scopes.Has(spec.scope) {
		return h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrScopeDenied,
				Retryable: ErrorRetryable[ErrScopeDenied],
				Args: map[string]any{
					"op":        cmd.Op,
					"needScope": spec.scope,
					"role":      caller.Role,
				},
			},
		})
	}

	// Fresh state, read here and not before: this is the dispatch boundary,
	// and a guard checked against the state the user was looking at is not a
	// guard.
	snap := h.cfg.Site.Snapshot()

	if cmd.Expect.Rev != snap.ControlRev {
		return h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrConflict,
				Retryable: ErrorRetryable[ErrConflict],
				Args:      map[string]any{"expected": cmd.Expect.Rev, "actual": snap.ControlRev},
			},
		})
	}

	if spec.dispatchWrite && len(snap.DispatchBlockedBy) > 0 {
		// The box's own invariant: stale meter data stops dispatch. An
		// operation that moves energy does not get to go around it.
		return h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"dispatchBlockedBy": snap.DispatchBlockedBy},
			},
		})
	}

	if ok, failed := guardsHold(cmd.Expect.Guards, fieldValues(snap, h.modes)); !ok {
		return h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrPrecondition,
				Retryable: ErrorRetryable[ErrPrecondition],
				Args: map[string]any{
					"fid":    int64(failed.Fid),
					"op":     failed.Op,
					"value":  failed.Value,
					"guards": sortedGuardFids(cmd.Expect.Guards),
				},
			},
		})
	}

	switch cmd.Op {
	case OpSetMode:
		return h.setMode(ctx, cmd, uptimeMs)
	case OpLoadpointHold:
		return h.loadpointHold(cmd, uptimeMs)
	case OpLoadpointBoost:
		return h.loadpointBoost(cmd, uptimeMs)
	case OpLoadpointSoCSet:
		return h.loadpointSoCSet(cmd, uptimeMs)
	case OpLoadpointSurplusOnlySet:
		return h.loadpointSurplusOnlySet(cmd, uptimeMs)
	default:
		return fmt.Errorf("appproto: op %q is in the table but has no handler", cmd.Op)
	}
}

// acceptCmd takes the intent: mints the lease, remembers the command id and
// tells the app. From here on a retry replays whatever outcome follows
// rather than acting a second time.
func (h *Handler) acceptCmd(cmd Cmd, uptimeMs int64) (CmdAck, error) {
	ack := CmdAck{
		CmdID:       cmd.CmdID,
		LeaseID:     h.cfg.NewLeaseID(),
		ExpiresAtMs: uptimeMs + LeaseMs,
	}
	h.cmds.remember(cmd.CmdID, ack, uptimeMs)
	return ack, h.sendCmdAck(ack)
}

// settleAndReport records the outcome, reports it, and pushes a fresh plan
// after anything applied. A mode change, a hold and a boost all alter what
// the box means to do next, and the app must not show the old intent beside
// the new state with neither looking wrong.
func (h *Handler) settleAndReport(cmdID string, res CmdResult) error {
	h.cmds.settle(cmdID, res)
	if err := h.sendCmdResult(res); err != nil {
		return err
	}
	if res.State == CmdApplied {
		return h.sendPlan(nil)
	}
	return nil
}

func (h *Handler) setMode(ctx context.Context, cmd Cmd, uptimeMs int64) error {
	raw, _ := cmd.Args["mode"].(string)
	wanted := control.Mode(raw)
	if !control.IsValidMode(wanted) {
		// control.IsValidMode is the box's own validator, shared with the
		// HTTP API and Home Assistant. A second list here would be a second
		// place for the three to disagree.
		return h.sendCmdResult(CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnknownOp,
				Retryable: false,
				Args:      map[string]any{"op": cmd.Op, "arg": "mode", "value": raw},
			},
		})
	}

	if _, err := h.acceptCmd(cmd, uptimeMs); err != nil {
		return err
	}

	if err := h.cfg.Modes.SetMode(ctx, wanted); err != nil {
		h.log.Warn("mode change refused by control", "mode", wanted, "err", err)
		return h.settleAndReport(cmd.CmdID, CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"op": cmd.Op},
			},
		})
	}

	// Read back. The echo of the requested value is never confirmation —
	// that is the difference between knowing a command was heard and knowing
	// it happened.
	observedMode, ok := h.cfg.Modes.ObservedMode()
	readAtMs := h.cfg.Clock.UptimeMs()

	var res CmdResult
	switch {
	case !ok:
		// Accepted, nothing read back. Not a failure and not a success, and
		// the app has to be able to say exactly that.
		res = CmdResult{CmdID: cmd.CmdID, State: CmdUnconfirmed}
	case observedMode != wanted:
		// Something else moved the mode between the write and the read.
		res = CmdResult{
			CmdID: cmd.CmdID,
			State: CmdSuperseded,
			Observed: &Observed{
				Value:    float64(modeIndex(h.modes, string(observedMode))),
				Src:      ObservedSrcCore,
				UptimeMs: readAtMs,
			},
		}
	default:
		res = CmdResult{
			CmdID: cmd.CmdID,
			State: CmdApplied,
			Observed: &Observed{
				Value:    float64(modeIndex(h.modes, string(observedMode))),
				Src:      ObservedSrcCore,
				UptimeMs: readAtMs,
			},
		}
	}

	return h.settleAndReport(cmd.CmdID, res)
}

// --------------------------------------------------------------------------
// Plan
// --------------------------------------------------------------------------

func (h *Handler) onPlanGet(id *uint32) error {
	if boot := h.cfg.Info.Boot(); boot != nil {
		return h.sendError(id, ErrorBody{
			Code:      ErrBooting,
			Retryable: ErrorRetryable[ErrBooting],
			Args:      map[string]any{"phase": boot.Phase, "pct": boot.Pct},
		})
	}
	return h.sendPlan(id)
}

func (h *Handler) sendPlan(id *uint32) error {
	body := planFrom(
		h.cfg.Plans.Latest(),
		h.cfg.Plans.Rev(),
		h.cfg.Plans.CeilingW(),
		h.cfg.Clock.UptimeMs(),
		h.cfg.Clock.Now(),
	)

	// The optimizer plans further than the wire carries: 193 fifteen-minute
	// slots encode to ~17 kB and the largest bulk bucket is 16 kB. Send the
	// nearest slots that fit and drop the far tail — the app shows twelve
	// hours, and slots two days out are a guess about a guess. What must
	// never happen is the alternative: an encode error here used to kill the
	// whole session, which the app experienced as "the box hangs up whenever
	// I open the plan".
	for {
		env, err := newEnvelope(MsgPlan, id, body)
		if err != nil {
			return err
		}
		raw, err := EncodeEnvelope(env)
		if err != nil {
			return err
		}
		if bulkBucketFor(len(raw)) != 0 {
			return h.send(Frame{Lane: LaneBulk, Bucket: bulkBucketFor(len(raw)), Envelope: env})
		}
		if len(body.Slots) <= 1 {
			return fmt.Errorf("appproto: a single plan slot does not fit any bulk bucket")
		}
		body.Slots = body.Slots[:len(body.Slots)*3/4]
	}
}

// --------------------------------------------------------------------------
// Teardown
// --------------------------------------------------------------------------

// Terminate ends the session from the box's side and stops the stream.
func (h *Handler) Terminate(reason string) error {
	h.mu.Lock()
	h.subscribed = false
	bucket := h.bucket
	h.mu.Unlock()
	return h.sendControl(MsgSessionTerminate, nil, SessionTerminate{Reason: reason}, 0, bucket)
}

// --------------------------------------------------------------------------
// Sending
// --------------------------------------------------------------------------

func (h *Handler) sendCmdAck(ack CmdAck) error {
	h.mu.Lock()
	bucket := h.bucket
	h.mu.Unlock()
	return h.sendControl(MsgCmdAck, nil, ack, 0, bucket)
}

func (h *Handler) sendCmdResult(res CmdResult) error {
	h.mu.Lock()
	bucket := h.bucket
	h.mu.Unlock()
	return h.sendControl(MsgCmdResult, nil, res, 0, bucket)
}

// sendError puts a stable code on the wire.
//
// An error carrying a request id belongs to that request alone. Without the
// id, a window the box could not serve would raise a session-wide banner and
// make the whole app look broken over one chart the user could simply narrow.
func (h *Handler) sendError(id *uint32, body ErrorBody) error {
	h.mu.Lock()
	bucket := h.bucket
	h.mu.Unlock()
	return h.sendControl(MsgError, id, body, 0, bucket)
}

func (h *Handler) sendControl(t string, id *uint32, body any, flags uint8, bucket int) error {
	env, err := newEnvelope(t, id, body)
	if err != nil {
		return err
	}
	return h.send(Frame{Lane: LaneControl, Flags: flags, Bucket: bucket, Envelope: env})
}

// sendBulk sizes the bucket from the encoded body rather than a guess, so a
// long mode catalogue or a wide source table cannot silently overrun.
func (h *Handler) sendBulk(t string, id *uint32, body any) error {
	env, err := newEnvelope(t, id, body)
	if err != nil {
		return err
	}
	raw, err := EncodeEnvelope(env)
	if err != nil {
		return err
	}
	bucket := bulkBucketFor(len(raw))
	if bucket == 0 {
		return fmt.Errorf("appproto: %s is %d bytes, larger than the biggest bulk bucket", t, len(raw))
	}
	return h.send(Frame{Lane: LaneBulk, Bucket: bucket, Envelope: env})
}

func (h *Handler) send(f Frame) error {
	bytes, err := h.cfg.Codec.EncodeFrame(f)
	if err != nil {
		return fmt.Errorf("appproto: encode %s: %w", f.Envelope.T, err)
	}
	return h.cfg.Sender.Send(bytes)
}
