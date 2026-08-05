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

	"github.com/google/uuid"
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

	mu         sync.Mutex
	proto      int
	subscribed bool
	bucket     int
	seq        uint64
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

	return &Handler{
		cfg:      cfg,
		log:      cfg.Logger.With("component", "appproto"),
		modes:    wireModes(control.ModeCatalog()),
		dict:     fieldDict(cfg.SrcGrid, cfg.SrcPV, cfg.SrcBattery),
		cmds:     newCmdLog(),
		ops:      defaultOps(),
		proto:    ProtoMax,
		bucket:   512,
		lastSent: map[string]int64{},
	}, nil
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
	case MsgHistQuery:
		// Not built yet. Answering the request it belongs to is the whole
		// point of a request id: the app narrows one chart instead of
		// raising a session-wide banner.
		if env.ID != nil {
			return h.sendError(env.ID, ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"subsystem": "history"},
			})
		}
		return nil
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

	body := HelloOK{
		Proto: proto,
		Mode:  mode,
		Box:   BoxInfo{ID: id.ID, Build: id.Build, TZ: id.TZ},
		Clock: Clock{
			Source:     source,
			SyncedAtMs: syncedAtMs,
			UptimeMs:   h.cfg.Clock.UptimeMs(),
		},
		Caps:     caps,
		CapsHash: capsHash(caps),
		Modes:    h.modes,
		Boot:     boot,
		Hint:     hint,
	}

	h.mu.Lock()
	h.proto = proto
	h.mu.Unlock()

	// Bulk, not lane 0. The reply carries the capability list and the mode
	// catalogue, both of which vary in size with what the box supports, and
	// lane 0 exists so that its frames vary in size with nothing.
	return h.sendBulk(MsgHelloOK, nil, body)
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

	bucket := sub.Bucket
	if bucket != 256 && bucket != 512 {
		// Lane 0 is one of two sizes for the life of the session. An
		// unrecognised request gets the larger one rather than an error:
		// the client still gets its stream, at a size the box can hold to.
		bucket = 512
	}

	h.mu.Lock()
	h.subscribed = true
	h.bucket = bucket
	h.seq = 0
	h.mu.Unlock()

	return h.sendSnapshot()
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

// Tick emits one telemetry frame. The caller drives it on the cadence the
// subscription asked for and never skips a beat: a tick goes out even when
// nothing changed, because silence would tell the relay operator that nothing
// happened in the house that second.
func (h *Handler) Tick() error {
	h.mu.Lock()
	if !h.subscribed {
		h.mu.Unlock()
		return nil
	}
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
	default:
		return fmt.Errorf("appproto: op %q is in the table but has no handler", cmd.Op)
	}
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

	ack := CmdAck{
		CmdID:       cmd.CmdID,
		LeaseID:     h.cfg.NewLeaseID(),
		ExpiresAtMs: uptimeMs + LeaseMs,
	}
	h.cmds.remember(cmd.CmdID, ack, uptimeMs)
	if err := h.sendCmdAck(ack); err != nil {
		return err
	}

	if err := h.cfg.Modes.SetMode(ctx, wanted); err != nil {
		h.log.Warn("mode change refused by control", "mode", wanted, "err", err)
		res := CmdResult{
			CmdID: cmd.CmdID,
			State: CmdRejected,
			Error: &ErrorBody{
				Code:      ErrUnavailable,
				Retryable: ErrorRetryable[ErrUnavailable],
				Args:      map[string]any{"op": cmd.Op},
			},
		}
		h.cmds.settle(cmd.CmdID, res)
		return h.sendCmdResult(res)
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

	h.cmds.settle(cmd.CmdID, res)
	if err := h.sendCmdResult(res); err != nil {
		return err
	}

	// A mode change alters what the planner does, so the plan is remade and
	// pushed unasked. Otherwise the app would show the old intent beside the
	// new mode and neither would look wrong.
	if res.State == CmdApplied {
		return h.sendPlan(nil)
	}
	return nil
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
	return h.sendBulk(MsgPlan, id, body)
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
