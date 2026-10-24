package appproto

import (
	"context"
	"encoding/hex"
	"github.com/srcfl/ftw/go/protocol"
	"slices"
	"sort"

	"github.com/srcfl/ftw/go/internal/units"
)

// A watch's exchange session (ADR 0009, decision 2).
//
// A watch speaks HTTPS and nothing lower, so each of its requests is one Noise
// message that FTW Cloud carries and each answer one HTTP body. The session
// takes hello, cmd and now. It never streams: there is no socket to stream on.
// Every answer is two lane 0 frames of one size whatever the op or the
// outcome, so the service cannot tell a read from a command or a refusal from
// a success by length. appuplink pads an answer to two frames with Pad.

// ExchangeOps are the commands a watch's exchange takes: the charger's hold,
// which Charge now, Pause and Back to the plan send. A watch reads and
// charges and does nothing else (ADR 0009, decision B); its role's
// ftw.dispatch.write reaches further, and the exchange does not.
var ExchangeOps = []string{OpLoadpointHold}

func (h *Handler) dispatchExchange(ctx context.Context, env Envelope) error {
	switch env.T {
	case MsgHello:
		return h.onExchangeHello(env)
	case MsgCmd:
		var cmd Cmd
		if err := Unmarshal(env.B, &cmd); err == nil && !slices.Contains(ExchangeOps, cmd.Op) {
			return h.sendError(env.ID, ErrorBody{Code: protocol.ErrOpNotGranted, Args: map[string]any{"op": cmd.Op}})
		}
		return h.onCmd(ctx, env, "")
	case MsgNow:
		return h.onNow()
	default:
		// sub, api.req, hist.query, plan.get, price.get and anything newer.
		// Answered rather than ignored, because the watch waits for an
		// answer either way and a named refusal costs the same frame.
		return h.sendError(env.ID, ErrorBody{
			Code:      ErrUnknownOp,
			Retryable: ErrorRetryable[ErrUnknownOp],
			Args:      map[string]any{"t": env.T},
		})
	}
}

// onExchangeHello answers a watch's hello with what it needs: the protocol,
// the box's mode, the clock, its role and scopes, the controlRev a command
// expects, and FTW Cloud's session information while FTW Cloud is on.
func (h *Handler) onExchangeHello(env Envelope) error {
	var hello Hello
	if err := Unmarshal(env.B, &hello); err != nil {
		h.log.Warn("undecodable hello dropped", "err", err)
		return nil
	}

	proto := ProtoMax
	if hello.Proto.Max < ProtoMax {
		proto = max(ProtoFloor, hello.Proto.Max)
	}
	mode := BoxModeFull
	switch {
	case h.cfg.Info.Boot() != nil:
		mode = BoxModeBooting
	case proto == ProtoFloor:
		mode = BoxModeFloor
	case !h.canWrite():
		mode = BoxModeReadonly
	}

	var cloud *CloudSession
	if h.cfg.Cloud != nil && proto != ProtoFloor {
		cloud = h.cfg.Cloud()
	}
	source, syncedAtMs := h.cfg.Clock.Sync()

	h.mu.Lock()
	h.proto = proto
	h.mu.Unlock()

	body := ExchangeHelloOK{
		Proto: proto,
		Mode:  mode,
		Clock: Clock{
			Source:     source,
			SyncedAtMs: syncedAtMs,
			UptimeMs:   h.cfg.Clock.UptimeMs(),
		},
		Role:   h.cfg.Caller.Role,
		Scopes: h.cfg.Caller.Scopes.Names(),
		Rev:    h.cfg.Site.Snapshot().ControlRev,
		Cloud:  cloud,
	}
	if body.Cloud != nil {
		size, err := encodedSize(MsgHelloOK, body)
		if err != nil {
			return err
		}
		if bucket := bulkBucketFor(size); bucket == 0 || bucket > ExchangeHelloMaxBucket {
			// A house with too many sources to name in 1 KiB says less. The
			// watch then keeps the names its phone gave it.
			h.log.Warn("exchange hello_ok left FTW Cloud out to fit the exchange's answer", "bytes", size)
			body.Cloud = nil
		}
	}
	return h.sendBulk(MsgHelloOK, nil, body)
}

// ExchangeHelloMaxBucket is the largest bucket an exchange hello_ok may take:
// the smallest one. FTW Cloud carries an exchange message of at most 2 KiB
// (cloudwire.XDataMax), so the next bucket, 4 KiB, never fits.
const ExchangeHelloMaxBucket = 1024

func encodedSize(t string, body any) (int, error) {
	env, err := newEnvelope(t, nil, body)
	if err != nil {
		return 0, err
	}
	raw, err := EncodeEnvelope(env)
	if err != nil {
		return 0, err
	}
	return len(raw), nil
}

// onNow answers a watch's read with the Now screen in one lane 0 frame.
func (h *Handler) onNow() error {
	caller, enrolled := h.liveCaller()
	if !enrolled {
		if err := h.sendError(nil, ErrorBody{
			Code:      ErrGrantRevoked,
			Retryable: ErrorRetryable[ErrGrantRevoked],
		}); err != nil {
			return err
		}
		return h.Terminate(TerminateRevoked)
	}
	if !caller.Scopes.Has(ScopeLiveRead) {
		return h.sendError(nil, ErrorBody{
			Code:      ErrScopeDenied,
			Retryable: ErrorRetryable[ErrScopeDenied],
			Args:      map[string]any{"t": MsgNow, "needScope": ScopeLiveRead, "role": caller.Role},
		})
	}
	if boot := h.cfg.Info.Boot(); boot != nil {
		return h.sendError(nil, ErrorBody{
			Code:      ErrBooting,
			Retryable: ErrorRetryable[ErrBooting],
			Args:      map[string]any{"phase": boot.Phase, "pct": boot.Pct},
		})
	}

	uptimeMs := h.cfg.Clock.UptimeMs()
	snap := h.cfg.Site.Snapshot()
	blocked := snap.DispatchBlockedBy
	if blocked == nil {
		blocked = []string{}
	}
	body := Now{
		UptimeMs:          uptimeMs,
		ControlRev:        snap.ControlRev,
		Fields:            fieldValues(snap, h.modes),
		Sources:           h.nowSources(snap, uptimeMs),
		DispatchBlockedBy: blocked,
		Loadpoints:        h.nowLoadpoints(),
	}

	h.mu.Lock()
	bucket := h.bucket
	h.mu.Unlock()
	flags, err := fitNow(&body, bucket)
	if err != nil {
		return err
	}
	return h.sendControl(MsgNow, nil, body, flags, bucket)
}

// nowSources ages the three sources the power fields draw on, keyed by the
// frozen field that names each one.
func (h *Handler) nowSources(snap Snapshot, uptimeMs int64) map[string]NowSource {
	out := map[string]NowSource{}
	for _, ref := range []struct {
		fid Fid
		id  string
	}{
		{FidSrcGrid, h.cfg.SrcGrid},
		{FidSrcPv, h.cfg.SrcPV},
		{FidSrcBattery, h.cfg.SrcBattery},
	} {
		for _, s := range snap.Sources {
			if ref.id == "" || s.ID != ref.id {
				continue
			}
			age := int64(-1)
			if s.LastOkUptimeMs >= 0 {
				age = max(0, uptimeMs-s.LastOkUptimeMs)
			}
			out[fidKey(ref.fid)] = NowSource{State: sourceState(s, uptimeMs), AgeMs: age}
			break
		}
	}
	return out
}

func (h *Handler) nowLoadpoints() []NowLoadpoint {
	out := []NowLoadpoint{}
	if h.cfg.Loadpoints == nil {
		return out
	}
	for _, r := range h.cfg.Loadpoints.Readings() {
		lp := NowLoadpoint{ID: r.ID, Plugged: r.Plugged, PowerW: roundW(r.PowerW),
			CarID: rawCarID(r.Car.CarID), CarIdentifiedBy: r.Car.CarIdentifiedBy, CarCorrected: r.Car.CarCorrected}
		for _, id := range r.Car.Choose {
			if raw := rawCarID(id); raw != nil {
				lp.Choose = append(lp.Choose, raw)
			}
		}
		if r.Car.CarIdentifiedBy == protocol.CarIdentifiedByAsking {
			// A watch taps only while the box asks; the session costs
			// lane 0 bytes on every other charger.
			lp.Session = r.Car.Session
		}
		if r.SoCKnown {
			soc := units.PermilleFromFraction(r.SoC)
			lp.SocPermille = &soc
			lp.SocSource, lp.SocAgeS, lp.SocEstimate = r.SoCSource, r.SoCAgeS, r.SoCEstimate
		}
		out = append(out, lp)
	}
	return out
}

// fitNow trims a now until it fits its bucket, and flags what it dropped.
//
// The car comes before the sources' ages, because charging is what a watch is
// for. Chargers go from the last, then the blocked list, then the source ages,
// then the first charger, then fields from the highest id.
func fitNow(body *Now, bucket int) (uint8, error) {
	const headerBytes = 6
	fits := func() (bool, error) {
		env, err := newEnvelope(MsgNow, nil, body)
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
	if err != nil || ok {
		return 0, err
	}
	for len(body.Loadpoints) > 1 {
		body.Loadpoints = body.Loadpoints[:len(body.Loadpoints)-1]
		if ok, err = fits(); err != nil || ok {
			return FlagTrunc, err
		}
	}
	body.DispatchBlockedBy = []string{}
	if ok, err = fits(); err != nil || ok {
		return FlagTrunc, err
	}
	body.Sources = map[string]NowSource{}
	if ok, err = fits(); err != nil || ok {
		return FlagTrunc, err
	}
	body.Loadpoints = []NowLoadpoint{}
	if ok, err = fits(); err != nil || ok {
		return FlagTrunc, err
	}
	keys := make([]string, 0, len(body.Fields))
	for k := range body.Fields {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return fidOf(keys[i]) < fidOf(keys[j]) })
	for len(keys) > 0 {
		delete(body.Fields, keys[len(keys)-1])
		keys = keys[:len(keys)-1]
		if ok, err = fits(); err != nil || ok {
			return FlagTrunc, err
		}
	}
	return FlagTrunc, nil
}

// PadFrame is one tick on lane 0, encoded and not sent. The exchange fills
// every answer to two frames with it, so a refusal, a read and a command
// answer with the same length.
func (h *Handler) PadFrame() ([]byte, error) {
	h.mu.Lock()
	bucket := h.bucket
	h.mu.Unlock()
	env, err := newEnvelope(MsgTick, nil, Tick{UptimeMs: h.cfg.Clock.UptimeMs()})
	if err != nil {
		return nil, err
	}
	return h.cfg.Codec.EncodeFrame(Frame{Lane: LaneControl, Bucket: bucket, Envelope: env})
}

// rawCarID is a car id as the watch's now carries it: its 16 bytes. Nil for
// none, or for an id that is not 32 hex digits.
func rawCarID(id string) []byte {
	raw, err := hex.DecodeString(id)
	if err != nil || len(raw) != 16 {
		return nil
	}
	return raw
}
