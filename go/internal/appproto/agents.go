package appproto

// Agents on the app session (docs/agents.md).
//
// An agent's session is the app session: the same frames, the same Noise,
// the same handler. What differs is the door it went through — FTW Cloud's
// tunnel and nowhere else — and the checks every call meets. An agent's
// frames go through dispatchAgent, which knows a short list of messages and
// asks, on each one, whether the grant still exists, whether the clock says
// it has run out, and whether its scopes open the message. A command then
// meets the per-grant op allowlist, the rate and the caps in runCmd, in
// Core's usual order.
//
// Owner phones manage agents through the same session: they add, widen,
// revoke, list and confirm, each signed by the phone's approval key where
// docs/agents.md says so. The checks of a grant and of a signature live in
// appenroll, behind Access; this file decides who may ask.

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/redact"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// Access is the box's agent grants and the owners' approval keys.
type Access interface {
	AddAgent(deviceID string, m protocol.AgentAdd) (protocol.AgentRow, error)
	WidenAgent(deviceID string, m protocol.AgentWiden) (protocol.AgentRow, error)
	// RevokeAgent removes an agent row and ends its sessions.
	RevokeAgent(id string) error
	Agents() []protocol.AgentRow
	ApprovalKeys() []protocol.ApprovalKeyInfo
	SetApprovalKey(deviceID string, m protocol.ApprovalKeySet) (int64, error)
	CheckConfirm(deviceID string, m protocol.AgentConfirm, req protocol.Request) error
	// EndHolds releases every hold the agent set.
	EndHolds(agentID string)
	// Wall is the wall clock in ms, and whether NTP has set it.
	Wall() (int64, bool)
	// Notify pushes a catalogue kind to every owner. client names the
	// client given access, on agent.added, and is empty otherwise.
	Notify(kind, client string)
}

// AgentViews builds the typed reads' bodies: FTW Cloud's class bodies, with
// every name cut into an id under key.
type AgentViews interface {
	Live(key []byte) cloudwire.Live
	Loadpoints(key []byte) (cloudwire.LP, bool)
	// Cars is the car class under key: opaque ids, levels and goals, and
	// nothing that names a car. False on a box that keeps no cars.
	Cars(key []byte) (cloudwire.Cars, bool)
}

// AuditLog keeps the box's audit rows.
type AuditLog interface {
	Append(protocol.AuditRow)
	Page(beforeSeq uint64, n int) ([]protocol.AuditRow, bool, error)
}

// LogSource is the box's recent log lines, oldest first.
type LogSource interface {
	Recent(n int) []redact.Line
}

// AgentGrant is an agent row's grant as the box holds it this second.
type AgentGrant struct {
	Scopes      []string
	Ops         []string
	Caps        protocol.Caps
	ExpiresAtMs int64
	Principal   protocol.Principal
	// Expired is true once the wall clock passed ExpiresAtMs; NoClock while
	// the box cannot tell. Either way the agent may do nothing.
	Expired bool
	NoClock bool
}

// AgentScopes is what an agent holds now: the scopes its grant lists that
// its role allows and its approver holds this moment.
func AgentScopes(grant []string, role, approverRole string) []string {
	own := ScopesForRole(role)
	approver := ScopesForRole(approverRole)
	out := make([]string, 0, len(grant))
	for _, s := range grant {
		if own.Has(s) && approver.Has(s) {
			out = append(out, s)
		}
	}
	return out
}

// ScopesForEnrolment is what an enrolment holds now: a phone's role, a
// watch's role within its phone's, or an agent's grant within its approver's.
func ScopesForEnrolment(e Enrolment) apiauth.ScopeSet {
	if e.Agent != nil {
		return apiauth.NewScopeSet(AgentScopes(e.Agent.Scopes, e.Role, e.ParentRole)...)
	}
	return ScopesForGrant(e.Role, e.ParentRole)
}

// agentReads maps each read an agent may send to the scope it needs: the
// registry's reads table. sub is not one: its source table names devices.
var agentReads = protocol.RegistryReads

// agentOther is every other message an agent may send. Its own checks run
// in the handler.
var agentOther = map[string]bool{
	MsgHello:                 true,
	MsgCmd:                   true,
	protocol.MsgAgentRequest: true,
	protocol.MsgAgentList:    true,
	protocol.MsgAgentRevoke:  true,
}

func (h *Handler) isAgent() bool { return h.cfg.Caller.Kind == apiauth.KindAgent }

// selfID is this session's own row id: an agent's or a phone's.
func (h *Handler) selfID() string {
	_, id, _ := strings.Cut(h.cfg.Caller.Subject, ":")
	return id
}

func (h *Handler) refuse(id *uint32, code string, args map[string]any) error {
	return h.sendError(id, ErrorBody{Code: code, Retryable: ErrorRetryable[code], Args: args})
}

// refuseErr answers a refusal from Access, or an internal failure as one
// the caller may retry.
func (h *Handler) refuseErr(id *uint32, err error) error {
	var r *protocol.Refusal
	if errors.As(err, &r) {
		return h.refuse(id, r.Code, r.Args)
	}
	h.log.Warn("agent request failed", "err", err)
	return h.refuse(id, ErrUnavailable, map[string]any{"subsystem": "access"})
}

func codeOf(err error) string {
	var r *protocol.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ErrUnavailable
}

func (h *Handler) agentOK(id *uint32, body any) error {
	return h.sendBulk(protocol.MsgAgentOK, id, body)
}

// wall is the wall clock in ms and whether it can be trusted.
func (h *Handler) wall() (int64, bool) {
	if h.cfg.Access != nil {
		return h.cfg.Access.Wall()
	}
	return h.cfg.Clock.Now().UnixMilli(), true
}

// audit writes one row naming this session's caller. An agent's refusal is
// folded by the minute like its reads, so junk cannot crowd out real rows.
func (h *Handler) audit(by, what, target, result string) {
	if h.cfg.Audit == nil {
		return
	}
	if h.isAgent() && result != "ok" && !isState(result) {
		h.foldAudit(h.auditRow(by, what, "", result))
		return
	}
	h.cfg.Audit.Append(h.auditRow(by, what, target, result))
}

// isState is a command's or a request's outcome, which gets its own row.
func isState(result string) bool {
	switch result {
	case CmdApplied, CmdRejected, CmdExpired, CmdSuperseded, CmdUnconfirmed,
		protocol.RequestApproved, protocol.RequestDeclined:
		return true
	}
	return false
}

func (h *Handler) foldAudit(row protocol.AuditRow) {
	if done := h.cfg.Agents.foldRead(row, h.cfg.Clock.Now().UnixMilli()); done != nil {
		h.cfg.Audit.Append(*done)
	}
}

// auditRead counts an agent's read into its minute's row; the row is
// written when the minute ends.
func (h *Handler) auditRead(by, what string) {
	if h.cfg.Audit == nil {
		return
	}
	h.foldAudit(h.auditRow(by, what, "", "ok"))
}

func (h *Handler) auditRow(by, what, target, result string) protocol.AuditRow {
	at, ok := h.wall()
	if !ok {
		at = 0
	}
	row := protocol.AuditRow{AtMs: at, Actor: h.cfg.Caller.Subject, Role: h.cfg.Caller.Role, By: auditText(by),
		What: auditWord(what), Target: auditText(target), Result: auditWord(result)}
	if h.isAgent() {
		if grant, ok := h.cfg.Grants.Grant(); ok && grant.Agent != nil {
			row.Principal = grant.Agent.Principal.Client
			if grant.Agent.Principal.Ticket != "" {
				row.Principal = grant.Agent.Principal.Ticket
			}
		}
	}
	return row
}

func resultOf(err error) string {
	if err == nil {
		return "ok"
	}
	return codeOf(err)
}

// --------------------------------------------------------------------------
// An agent's session
// --------------------------------------------------------------------------

// agentGate is the live grant's answer for an agent: nil to go on, or the
// refusal and, when the session must end, why.
func (h *Handler) agentGate() (Enrolment, *ErrorBody, string) {
	grant, ok := h.cfg.Grants.Grant()
	switch {
	case !ok || grant.Agent == nil:
		return grant, &ErrorBody{Code: ErrGrantRevoked, Retryable: ErrorRetryable[ErrGrantRevoked]}, TerminateRevoked
	case grant.Agent.NoClock:
		return grant, &ErrorBody{Code: ErrUnavailable, Retryable: ErrorRetryable[ErrUnavailable],
			Args: map[string]any{"subsystem": "clock"}}, ""
	case grant.Agent.Expired:
		return grant, &ErrorBody{Code: protocol.ErrGrantExpired, Retryable: ErrorRetryable[protocol.ErrGrantExpired]},
			protocol.TerminateExpired
	}
	return grant, nil, ""
}

// dispatchAgent is dispatch for an agent's session.
func (h *Handler) dispatchAgent(ctx context.Context, env Envelope) error {
	if env.T == MsgCmd {
		// A command answers in cmd.result under its own id, so runCmd
		// makes every check, the grant's included.
		return h.onCmd(ctx, env, env.By)
	}
	grant, refusal, end := h.agentGate()
	if refusal != nil {
		h.audit(env.By, env.T, "", refusal.Code)
		if err := h.sendError(env.ID, *refusal); err != nil {
			return err
		}
		if end != "" {
			return h.Terminate(end)
		}
		return nil
	}
	if refusal := h.supportBy(env.By); refusal != nil {
		h.audit("", env.T, "", refusal.Code)
		return h.sendError(env.ID, *refusal)
	}
	need, read := agentReads[env.T]
	callScope, setup := protocol.RegistryAgentCalls[env.T]
	if setup {
		need = callScope
	}
	if !read && !setup && !agentOther[env.T] {
		h.audit(env.By, env.T, "", ErrScopeDenied)
		return h.refuse(env.ID, ErrScopeDenied, map[string]any{"t": env.T})
	}
	if read || setup {
		if !ScopesForEnrolment(grant).Has(need) {
			h.audit(env.By, env.T, "", ErrScopeDenied)
			return h.refuse(env.ID, ErrScopeDenied, map[string]any{"t": env.T, "needScope": need, "role": grant.Role})
		}
		if ok, wait := h.cfg.Agents.allowRead(h.selfID(), h.cfg.Clock.UptimeMs()); !ok {
			h.audit(env.By, env.T, "", protocol.ErrRateLimited)
			return h.sendError(env.ID, ErrorBody{Code: protocol.ErrRateLimited,
				Retryable: ErrorRetryable[protocol.ErrRateLimited], RetryAfterMs: &wait})
		}
		if read {
			h.auditRead(env.By, env.T)
		}
	}
	if setup {
		return h.onSetupCall(env)
	}

	switch env.T {
	case MsgHello:
		return h.onHello(env)
	case MsgSub:
		return h.onSub(env)
	case MsgPlanGet:
		return h.onPlanGet(env.ID)
	case MsgPriceGet:
		return h.onPriceGet(ctx, env)
	case MsgHistQuery:
		return h.onHistQuery(ctx, env)
	case protocol.MsgAgentRequest:
		return h.onAgentRequest(env, grant)
	case protocol.MsgAgentList:
		return h.onAgentList(env)
	case protocol.MsgAgentRevoke:
		return h.onAgentRevoke(env)
	default:
		return h.onRead(env)
	}
}

// supportBy refuses a support call that names nobody.
// A support call must name who made it, as staff:<id> or bot:<name>, and
// no other session may name anyone.
func (h *Handler) supportBy(by string) *ErrorBody {
	support := h.cfg.Caller.Role == apiauth.RoleSupport
	switch {
	case support && by == "":
		return &ErrorBody{Code: ErrUnknownOp, Retryable: false, Args: map[string]any{"missing": "by"}}
	case support && !protocol.ValidBy(by), !support && by != "":
		return &ErrorBody{Code: ErrUnknownOp, Retryable: false, Args: map[string]any{"field": "by"}}
	}
	return nil
}

// agentTick ends an agent's session once its grant is gone or has run out,
// without waiting for its next call.
func (h *Handler) agentTick() error {
	_, refusal, end := h.agentGate()
	if refusal == nil || end == "" {
		return nil
	}
	h.audit("", "session", "", refusal.Code)
	// Its holds end with it, whatever they had left.
	h.cfg.Access.EndHolds(h.selfID())
	_ = h.Terminate(end)
	return errAgentEnded
}

var errAgentEnded = errors.New("appproto: the agent's grant ended")

// cmdCaller is liveCaller for a command, with an agent's own checks: the
// grant, the clock, the allowlist, the rate and the caps. end names why the
// session must end, or is empty.
func (h *Handler) cmdCaller(cmd Cmd, by string) (apiauth.Caller, Cmd, *ErrorBody, string) {
	if !h.isAgent() {
		caller, enrolled := h.liveCaller()
		if !enrolled {
			return caller, cmd, &ErrorBody{Code: ErrGrantRevoked, Retryable: ErrorRetryable[ErrGrantRevoked]}, TerminateRevoked
		}
		return caller, cmd, nil, ""
	}
	grant, refusal, end := h.agentGate()
	if refusal != nil {
		return apiauth.Caller{}, cmd, refusal, end
	}
	if refusal := h.supportBy(by); refusal != nil {
		return apiauth.Caller{}, cmd, refusal, ""
	}
	caller, enrolled := h.liveCaller()
	if !enrolled {
		return caller, cmd, &ErrorBody{Code: ErrGrantRevoked, Retryable: ErrorRetryable[ErrGrantRevoked]}, TerminateRevoked
	}
	need, ok := protocol.RegistryAgentOps[cmd.Op]
	switch {
	case !ok:
		return caller, cmd, &ErrorBody{Code: protocol.ErrOpNotGranted, Args: map[string]any{"op": cmd.Op}}, ""
	case need == protocol.ScopeGoalsRequest:
		return caller, cmd, &ErrorBody{Code: protocol.ErrNeedsOwner,
			Args: map[string]any{"op": cmd.Op, "use": protocol.MsgAgentRequest}}, ""
	case !caller.Scopes.Has(need):
		return caller, cmd, &ErrorBody{Code: ErrScopeDenied,
			Args: map[string]any{"op": cmd.Op, "needScope": need, "role": caller.Role}}, ""
	case !slices.Contains(grant.Agent.Ops, cmd.Op):
		return caller, cmd, &ErrorBody{Code: protocol.ErrOpNotGranted, Args: map[string]any{"op": cmd.Op}}, ""
	}
	args, refusal := h.agentArgs(cmd.Op, cmd.Args, grant.Agent.Caps)
	if refusal != nil {
		return caller, cmd, refusal, ""
	}
	if ok, wait := h.cfg.Agents.allowCmd(h.selfID(), grant.Agent.Caps.PerMin, h.cfg.Clock.UptimeMs()); !ok {
		return caller, cmd, &ErrorBody{Code: protocol.ErrRateLimited, Retryable: true, RetryAfterMs: &wait}, ""
	}
	if len(cmd.Expect.Guards) > 0 && !caller.Scopes.Has(ScopeLiveRead) {
		// A guard's refusal says what a field reads; without live.read an
		// agent may not ask.
		return caller, cmd, &ErrorBody{Code: ErrScopeDenied,
			Args: map[string]any{"op": cmd.Op, "needScope": ScopeLiveRead, "field": "guards"}}, ""
	}
	cmd = withArgs(cmd, args)
	if refusal := h.holdRules(cmd); refusal != nil {
		return caller, cmd, refusal, ""
	}
	if id, _ := cmd.Args["id"].(string); cmd.Op == OpLoadpointHold && id != "" {
		if ok, wait := h.cfg.Agents.allowHold(h.selfID(), id, h.cfg.Clock.UptimeMs()); !ok {
			return caller, cmd, &ErrorBody{Code: protocol.ErrRateLimited, Retryable: true, RetryAfterMs: &wait,
				Args: map[string]any{"op": cmd.Op, "reason": "hold_gap"}}, ""
		}
	}
	return caller, cmd, nil, ""
}

func capExceeded(cap string, max uint64) *ErrorBody {
	return &ErrorBody{Code: protocol.ErrCapExceeded, Args: map[string]any{"cap": cap, "max": max}}
}

// noteCmd remembers what an audited command was, so its result can name it.
func (h *Handler) noteCmd(cmdID string, row cmdAudit) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmdAudits == nil {
		h.cmdAudits = map[string]cmdAudit{}
	}
	h.cmdAudits[cmdID] = row
}

type cmdAudit struct{ by, what, target string }

// auditCmdResult writes the audit row for a command's outcome.
func (h *Handler) auditCmdResult(res CmdResult) {
	h.mu.Lock()
	row, ok := h.cmdAudits[res.CmdID]
	delete(h.cmdAudits, res.CmdID)
	h.mu.Unlock()
	if !ok {
		return
	}
	result := res.State
	if res.Error != nil && res.State != CmdRejected {
		result = res.Error.Code
	}
	if res.Error != nil && res.State == CmdRejected && h.isAgent() {
		// A refused command folds by the minute, with its code.
		h.foldAudit(h.auditRow(row.by, row.what, "", res.Error.Code))
		return
	}
	if res.Error != nil {
		result = res.Error.Code
	}
	h.audit(row.by, row.what, row.target, result)
}

// onAgentRequest takes a durable change an agent asks for. It waits for an
// owner; nothing here acts.
func (h *Handler) onAgentRequest(env Envelope, grant Enrolment) error {
	var m protocol.AgentRequest
	if err := Unmarshal(env.B, &m); err != nil {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
	}
	refuse := func(code string, args map[string]any) error {
		h.audit(env.By, env.T, m.Op, code)
		return h.refuse(env.ID, code, args)
	}
	need, ok := protocol.RegistryAgentOps[m.Op]
	if _, known := h.ops[m.Op]; !known {
		return refuse(ErrUnknownOp, map[string]any{"op": m.Op})
	}
	switch {
	case !ok || need != protocol.ScopeGoalsRequest:
		return refuse(protocol.ErrOpNotGranted, map[string]any{"op": m.Op})
	case !ScopesForEnrolment(grant).Has(need):
		return refuse(ErrScopeDenied, map[string]any{"op": m.Op, "needScope": need})
	case !slices.Contains(grant.Agent.Ops, m.Op):
		return refuse(protocol.ErrOpNotGranted, map[string]any{"op": m.Op})
	case len(m.Why) > protocol.RequestWhyMax:
		return refuse(ErrUnknownOp, map[string]any{"field": "why"})
	}
	if raw, err := Marshal(m.Args); err != nil || len(raw) > protocol.RequestArgsMax {
		return refuse(ErrUnknownOp, map[string]any{"field": "args"})
	}
	// The args meet the registry's allowlist and the op's own checks now,
	// so the owner is never asked to approve something that cannot run.
	resolved, bad := h.agentArgs(m.Op, m.Args, grant.Agent.Caps)
	if bad == nil {
		bad = h.opArgs(m.Op, resolved)
	}
	if bad != nil {
		h.audit(env.By, env.T, m.Op, bad.Code)
		return h.sendError(env.ID, *bad)
	}
	if ok, wait := h.cfg.Agents.allowCmd(h.selfID(), grant.Agent.Caps.PerMin, h.cfg.Clock.UptimeMs()); !ok {
		h.audit(env.By, env.T, m.Op, protocol.ErrRateLimited)
		return h.sendError(env.ID, ErrorBody{Code: protocol.ErrRateLimited, Retryable: true, RetryAfterMs: &wait})
	}
	now, _ := h.wall()
	req, refusal := h.cfg.Agents.add(h.selfID(), m.Op, resolved, m.Args, m.Why, now)
	if refusal != nil {
		return refuse(refusal.Code, refusal.Args)
	}
	h.audit(env.By, env.T, m.Op+" "+req.ReqID, "ok")
	h.cfg.Access.Notify(protocol.PushAgentRequest, "")
	return h.agentOK(env.ID, map[string]any{"reqId": req.ReqID, "expiresAtMs": req.ExpiresAtMs})
}

// --------------------------------------------------------------------------
// Owners managing agents, and anyone listing or revoking
// --------------------------------------------------------------------------

// ownerPhone is the caller if it is an owner phone now, or the refusal.
func (h *Handler) ownerPhone(id *uint32) (apiauth.Caller, bool, error) {
	if h.isAgent() || h.cfg.Access == nil {
		return apiauth.Caller{}, false, h.refuse(id, ErrScopeDenied, map[string]any{"needRole": apiauth.RoleOwner})
	}
	caller, enrolled := h.liveCaller()
	if !enrolled {
		if err := h.refuse(id, ErrGrantRevoked, nil); err != nil {
			return caller, false, err
		}
		return caller, false, h.Terminate(TerminateRevoked)
	}
	if caller.Role != apiauth.RoleOwner {
		return caller, false, h.refuse(id, ErrScopeDenied, map[string]any{"needRole": apiauth.RoleOwner, "role": caller.Role})
	}
	return caller, true, nil
}

func (h *Handler) onApprovalKeySet(env Envelope) error {
	if _, ok, err := h.ownerPhone(env.ID); !ok {
		return err
	}
	var m protocol.ApprovalKeySet
	if err := protocol.UnmarshalSigned(env.B, &m); err != nil {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T, "field": "body"})
	}
	setAt, err := h.cfg.Access.SetApprovalKey(h.selfID(), m)
	h.audit("", env.T, h.selfID(), resultOf(err))
	if err != nil {
		return h.refuseErr(env.ID, err)
	}
	// A new key is a new way to let others in: every owner hears of it.
	h.cfg.Access.Notify(protocol.PushApprovalKeySet, "")
	return h.agentOK(env.ID, map[string]any{"deviceId": h.selfID(), "setAtMs": setAt})
}

func (h *Handler) onAgentAdd(env Envelope) error {
	if _, ok, err := h.ownerPhone(env.ID); !ok {
		return err
	}
	var m protocol.AgentAdd
	if err := protocol.UnmarshalSigned(env.B, &m); err != nil {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T, "field": "body"})
	}
	row, err := h.cfg.Access.AddAgent(h.selfID(), m)
	h.audit("", env.T, row.ID, resultOf(err))
	if err != nil {
		return h.refuseErr(env.ID, err)
	}
	return h.agentOK(env.ID, map[string]any{"agent": row})
}

func (h *Handler) onAgentWiden(env Envelope) error {
	if _, ok, err := h.ownerPhone(env.ID); !ok {
		return err
	}
	var m protocol.AgentWiden
	if err := protocol.UnmarshalSigned(env.B, &m); err != nil {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T, "field": "body"})
	}
	row, err := h.cfg.Access.WidenAgent(h.selfID(), m)
	h.audit("", env.T, m.ID, resultOf(err))
	var r *protocol.Refusal
	if errors.As(err, &r) && r.Code == ErrConflict {
		// A stale epoch: the owner reads the row again before signing again,
		// so the same request would only fail again.
		return h.sendError(env.ID, ErrorBody{Code: ErrConflict, Retryable: false, Args: r.Args})
	}
	if err != nil {
		return h.refuseErr(env.ID, err)
	}
	// A narrower grant ends any hold the old one set; every owner hears of
	// a change to someone's access.
	if !slices.Contains(row.Ops, OpLoadpointHold) || !slices.Contains(row.Scopes, protocol.ScopeDispatchHold) {
		h.cfg.Access.EndHolds(row.ID)
	}
	h.cfg.Access.Notify(protocol.PushAgentAdded, row.Principal.Client)
	return h.agentOK(env.ID, map[string]any{"agent": row})
}

// onAgentRevoke removes a grant: any, for an owner; its own, for an agent.
func (h *Handler) onAgentRevoke(env Envelope) error {
	var m protocol.AgentRevoke
	if err := Unmarshal(env.B, &m); err != nil {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
	}
	if h.isAgent() {
		if m.ID != h.selfID() {
			h.audit(env.By, env.T, m.ID, ErrScopeDenied)
			return h.refuse(env.ID, ErrScopeDenied, map[string]any{"agentId": m.ID})
		}
	} else if _, ok, err := h.ownerPhone(env.ID); !ok {
		return err
	}
	if !h.agentExists(m.ID) {
		h.audit(env.By, env.T, m.ID, protocol.ErrNotFound)
		return h.refuse(env.ID, protocol.ErrNotFound, map[string]any{"agentId": m.ID})
	}
	err := h.cfg.Access.RevokeAgent(m.ID)
	h.audit(env.By, env.T, m.ID, resultOf(err))
	if err != nil {
		return h.refuseErr(env.ID, err)
	}
	h.cfg.Agents.Forget(m.ID)
	return h.agentOK(env.ID, map[string]any{"id": m.ID})
}

func (h *Handler) agentExists(id string) bool {
	for _, row := range h.cfg.Access.Agents() {
		if row.ID == id {
			return true
		}
	}
	return false
}

// liveAgents is whether an agent still has a row, for pruning requests.
func (h *Handler) liveAgents() func(string) bool {
	rows := h.cfg.Access.Agents()
	return func(id string) bool {
		return slices.ContainsFunc(rows, func(r protocol.AgentRow) bool { return r.ID == id })
	}
}

// onAgentList answers an owner with every row and an agent with its own.
func (h *Handler) onAgentList(env Envelope) error {
	if h.cfg.Access == nil {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
	}
	defer h.cfg.Agents.tell("", protocol.AgentDecided{})
	now, _ := h.wall()
	out := protocol.AgentList{Agents: []protocol.AgentRow{}, Keys: []protocol.ApprovalKeyInfo{}}
	if h.isAgent() {
		for _, row := range h.cfg.Access.Agents() {
			if row.ID == h.selfID() {
				out.Agents = append(out.Agents, row)
			}
		}
		out.Requests = h.cfg.Agents.list(h.selfID(), now, h.liveAgents())
		return h.agentOK(env.ID, out)
	}
	caller, enrolled := h.liveCaller()
	if !enrolled || !caller.Scopes.Has(ScopeMembersRead) {
		return h.refuse(env.ID, ErrScopeDenied, map[string]any{"needScope": ScopeMembersRead})
	}
	out.Agents = append(out.Agents, h.cfg.Access.Agents()...)
	out.Requests = h.cfg.Agents.list("", now, h.liveAgents())
	out.Keys = append(out.Keys, h.cfg.Access.ApprovalKeys()...)
	out.LongestMs = protocol.LongestMs()
	return h.agentOK(env.ID, out)
}

// onAgentConfirm answers a request. Approving runs the op here, on the
// owner's own session and under the owner's scopes, as a command.
func (h *Handler) onAgentConfirm(ctx context.Context, env Envelope) error {
	if _, ok, err := h.ownerPhone(env.ID); !ok {
		return err
	}
	var m protocol.AgentConfirm
	if err := protocol.UnmarshalSigned(env.B, &m); err != nil {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T, "field": "body"})
	}
	now, synced := h.wall()
	if m.Approve && !synced {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "clock"})
	}
	live := h.liveAgents()
	req, ok := h.cfg.Agents.pending(m.ReqID, now, live)
	if !ok {
		h.audit("", env.T, m.ReqID, protocol.ErrNotFound)
		return h.refuse(env.ID, protocol.ErrNotFound, map[string]any{"reqId": m.ReqID})
	}
	defer h.cfg.Agents.tell("", protocol.AgentDecided{})
	if !m.Approve {
		h.cfg.Agents.decide(m.ReqID, protocol.RequestDeclined, now)
		h.audit("", env.T, m.ReqID, protocol.RequestDeclined)
		h.cfg.Agents.tell(req.Agent, protocol.AgentDecided{ReqID: m.ReqID, State: protocol.RequestDeclined})
		return h.agentOK(env.ID, map[string]any{"reqId": m.ReqID, "state": protocol.RequestDeclined})
	}
	if !h.stillGranted(req) {
		h.cfg.Agents.decide(m.ReqID, protocol.RequestExpired, now)
		h.audit("", env.T, m.ReqID, protocol.ErrNotFound)
		return h.refuse(env.ID, protocol.ErrNotFound, map[string]any{"reqId": m.ReqID})
	}
	if err := h.cfg.Access.CheckConfirm(h.selfID(), m, req); err != nil {
		h.audit("", env.T, m.ReqID, codeOf(err))
		return h.refuseErr(env.ID, err)
	}
	if !h.cfg.Agents.decide(m.ReqID, protocol.RequestApproved, now) {
		return h.refuse(env.ID, protocol.ErrNotFound, map[string]any{"reqId": m.ReqID})
	}
	h.audit("", env.T, m.ReqID, protocol.RequestApproved)
	cmdID := "req:" + m.ReqID
	if err := h.agentOK(env.ID, map[string]any{"reqId": m.ReqID, "state": protocol.RequestApproved, "cmdId": cmdID}); err != nil {
		return err
	}
	return h.runRequest(ctx, req)
}

// runRequest runs an approved request as a command on this session, under
// its caller's scopes, and tells the agent how it went.
func (h *Handler) runRequest(ctx context.Context, req protocol.Request) error {
	cmdID := "req:" + req.ReqID
	h.noteCmd(cmdID, cmdAudit{what: "agent.request.run", target: req.Op + " " + req.ReqID})
	snap := h.cfg.Site.Snapshot()
	err := h.runCmd(ctx, Cmd{
		CmdID: cmdID, Op: req.Op, Args: req.Args,
		NotValidAfterMs: h.cfg.Clock.UptimeMs() + confirmRunMs,
		Expect:          Expect{Rev: snap.ControlRev},
	}, "")
	d := protocol.AgentDecided{ReqID: req.ReqID, State: protocol.RequestApproved, CmdID: cmdID}
	if rec, ok := h.cfg.Commands.lookup(h.cmdRoot(), cmdID); ok {
		d.Result = rec.result
	}
	h.cfg.Agents.tell(req.Agent, d)
	return err
}

// confirmRunMs is how long a confirmed request's command stays valid: it
// runs at once, so this only bounds a stall.
const confirmRunMs = 30_000

// stillGranted says whether the agent behind a request may still ask for it.
func (h *Handler) stillGranted(req protocol.Request) bool {
	for _, row := range h.cfg.Access.Agents() {
		if row.ID == req.Agent {
			return slices.Contains(row.Ops, req.Op) && slices.Contains(row.Scopes, protocol.ScopeGoalsRequest)
		}
	}
	return false
}

func (h *Handler) onAuditGet(env Envelope) error {
	if h.isAgent() || h.cfg.Audit == nil {
		return h.refuse(env.ID, ErrScopeDenied, map[string]any{"t": env.T})
	}
	caller, enrolled := h.liveCaller()
	if !enrolled || !caller.Scopes.Has(ScopeMembersRead) {
		return h.refuse(env.ID, ErrScopeDenied, map[string]any{"needScope": ScopeMembersRead})
	}
	var m protocol.AuditGet
	if len(env.B) > 0 {
		if err := Unmarshal(env.B, &m); err != nil {
			return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
		}
	}
	n := int(m.N)
	if n <= 0 || n > protocol.AuditPageMax {
		n = protocol.AuditPageMax
	}
	rows, more, err := h.cfg.Audit.Page(m.BeforeSeq, n)
	if err != nil {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "audit"})
	}
	page := protocol.AuditPage{Rows: rows, More: more}
	if page.Rows == nil {
		page.Rows = []protocol.AuditRow{}
	}
	// One bulk frame holds the page; a long one gives up its oldest rows
	// and says more remain.
	for !h.fits(protocol.MsgAgentOK, env.ID, page) && len(page.Rows) > 1 {
		page.Rows = page.Rows[:len(page.Rows)*3/4]
		page.More = true
	}
	return h.agentOK(env.ID, page)
}

// fits says whether a body goes in one bulk frame.
func (h *Handler) fits(t string, id *uint32, body any) bool {
	env, err := newEnvelope(t, id, body)
	if err != nil {
		return false
	}
	raw, err := EncodeEnvelope(env)
	return err == nil && bulkBucketFor(len(raw)) != 0
}

// --------------------------------------------------------------------------
// Reads built for agents
// --------------------------------------------------------------------------

// onRead answers the reads docs/agents.md adds. A phone meets the same scope
// check an agent does; dispatchAgent has already made it for an agent.
func (h *Handler) onRead(env Envelope) error {
	if !h.isAgent() {
		caller, enrolled := h.liveCaller()
		if need := agentReads[env.T]; !enrolled || !caller.Scopes.Has(need) {
			return h.refuse(env.ID, ErrScopeDenied, map[string]any{"t": env.T, "needScope": need})
		}
	}
	switch env.T {
	case protocol.MsgAPIRead:
		return h.onAPIRead(env)
	case protocol.MsgHealthGet:
		return h.onHealthGet(env)
	case protocol.MsgDriversGet:
		return h.onDriversGet(env)
	case protocol.MsgLiveGet:
		return h.onLiveGet(env)
	case protocol.MsgLoadpointsGet:
		return h.onLoadpointsGet(env)
	case protocol.MsgLogsGet:
		return h.onLogsGet(env)
	case protocol.MsgConfigGet:
		return h.onConfigGet(env)
	case protocol.MsgCarsGet:
		return h.onCarsGet(env)
	case protocol.MsgEventsGet:
		return h.onEventsGet(env)
	default:
		// diag.run is in the contract and not built yet.
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
	}
}

// srcID is a source's id as agents see it.
func (h *Handler) srcID(name string) string { return cloudwire.SourceID(h.cfg.AgentKey, name) }

func (h *Handler) onLiveGet(env Envelope) error {
	if h.cfg.Views == nil {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "live"})
	}
	return h.sendBulk(protocol.MsgLive, env.ID, h.cfg.Views.Live(h.cfg.AgentKey))
}

func (h *Handler) onLoadpointsGet(env Envelope) error {
	if h.cfg.Views == nil {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "loadpoints"})
	}
	lp, ok := h.cfg.Views.Loadpoints(h.cfg.AgentKey)
	if !ok {
		lp = cloudwire.LP{}
	}
	return h.sendBulk(protocol.MsgLoadpoints, env.ID, lp)
}

// onCarsGet is each car's level and goal by an id cut with the key: the same
// projection as FTW Cloud's car class. Names, makes, VINs, identifiers and
// sources stay home.
func (h *Handler) onCarsGet(env Envelope) error {
	if h.cfg.Views == nil {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "cars"})
	}
	cars, ok := h.cfg.Views.Cars(h.cfg.AgentKey)
	if !ok {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "cars"})
	}
	return h.sendBulk(protocol.MsgCars, env.ID, cars)
}

func (h *Handler) onDriversGet(env Envelope) error {
	snap := h.cfg.Site.Snapshot()
	up := h.cfg.Clock.UptimeMs()
	out := protocol.Drivers{Drivers: []protocol.DriverState{}}
	for _, s := range snap.Sources {
		age := int64(-1)
		if s.LastOkUptimeMs >= 0 {
			age = up - s.LastOkUptimeMs
		}
		out.Drivers = append(out.Drivers, protocol.DriverState{
			Src: h.srcID(s.ID), Kind: s.Kind, State: string(sourceState(s, up)), AgeMs: age,
		})
	}
	return h.sendBulk(protocol.MsgDrivers, env.ID, out)
}

// Health is the health summary: no readings and no names.
type Health struct {
	Version           string         `cbor:"version"`
	UptimeS           int64          `cbor:"uptimeS"`
	Clock             string         `cbor:"clock"`
	Drivers           map[string]int `cbor:"drivers"`
	Meter             *HealthMeter   `cbor:"meter,omitempty"`
	DispatchBlockedBy []string       `cbor:"dispatchBlockedBy"`
}

// HealthMeter is the site meter's freshness.
type HealthMeter struct {
	SrcState string `cbor:"srcState"`
	AgeMs    int64  `cbor:"ageMs"`
}

func (h *Handler) onHealthGet(env Envelope) error {
	snap := h.cfg.Site.Snapshot()
	up := h.cfg.Clock.UptimeMs()
	clock, _ := h.cfg.Clock.Sync()
	if _, synced := h.wall(); !synced {
		clock = "none"
	}
	out := Health{
		Version: h.cfg.Info.Identity().Build, UptimeS: up / 1000, Clock: clock,
		Drivers:           map[string]int{"ok": 0, "degraded": 0, "faulted": 0, "offline": 0},
		DispatchBlockedBy: []string{},
	}
	for _, s := range snap.Sources {
		state := sourceState(s, up)
		switch state {
		case SourceLive:
			out.Drivers["ok"]++
		case SourceLagging:
			out.Drivers["degraded"]++
		case SourceStale:
			out.Drivers["faulted"]++
		default:
			out.Drivers["offline"]++
		}
		if s.ID == h.cfg.SrcGrid {
			m := &HealthMeter{SrcState: string(state), AgeMs: -1}
			if s.LastOkUptimeMs >= 0 {
				m.AgeMs = up - s.LastOkUptimeMs
			}
			out.Meter = m
		}
	}
	for _, name := range snap.DispatchBlockedBy {
		// Names stay home: a blocked source goes by its agent id.
		out.DispatchBlockedBy = append(out.DispatchBlockedBy, h.srcID(name))
	}
	return h.sendBulk(protocol.MsgHealth, env.ID, out)
}

func (h *Handler) onLogsGet(env Envelope) error {
	if h.cfg.Logs == nil || h.cfg.Redactor == nil {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "logs"})
	}
	var m protocol.LogsGet
	if len(env.B) > 0 {
		if err := Unmarshal(env.B, &m); err != nil {
			return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
		}
	}
	n := int(m.N)
	if n <= 0 || n > protocol.LogsLinesMax {
		n = protocol.LogsLinesMax
	}
	floor := levelRank(m.Level)
	if m.Level == "" {
		floor = levelRank("WARN")
	}
	if m.BeforeMs < 0 || m.AfterMs < 0 || (m.BeforeMs > 0 && m.AfterMs >= m.BeforeMs) {
		return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T, "field": "beforeMs"})
	}
	red := h.cfg.Redactor()
	// The whole ring, every level: the filter and the cursors pick from it.
	var picked []redact.Line
	for _, l := range h.cfg.Logs.Recent(logsScan) {
		if levelRank(l.Level) < floor || (m.BeforeMs > 0 && l.TsMs >= m.BeforeMs) || (m.AfterMs > 0 && l.TsMs <= m.AfterMs) {
			continue
		}
		picked = append(picked, l)
	}
	// afterMs pages forward from the cursor: the oldest lines after it.
	// Otherwise the page is the newest lines before beforeMs, or now.
	forward := m.AfterMs > 0 && m.BeforeMs == 0
	out := protocol.Logs{Lines: []protocol.LogLine{}}
	if len(picked) > n {
		out.More = true
		if forward {
			picked = picked[:n]
		} else {
			picked = picked[len(picked)-n:]
		}
	}
	for _, l := range picked {
		r := red.Log(l)
		out.Lines = append(out.Lines, protocol.LogLine{Ts: r.TsMs, Level: r.Level, Src: r.Src, Msg: r.Msg})
	}
	// One bulk frame holds the answer. The lines furthest from the cursor
	// give way, and the next page reaches them.
	for !h.fits(protocol.MsgLogs, env.ID, out) && len(out.Lines) > 0 {
		drop := max(1, len(out.Lines)/4)
		out.Dropped += uint64(drop)
		out.More = true
		if forward {
			out.Lines = out.Lines[:len(out.Lines)-drop]
		} else {
			out.Lines = out.Lines[drop:]
		}
	}
	return h.sendBulk(protocol.MsgLogs, env.ID, out)
}

// logsScan is how far back logs.get looks: the box's whole log ring.
const logsScan = 2000

func levelRank(level string) int {
	switch strings.ToUpper(level) {
	case "DEBUG":
		return 0
	case "INFO":
		return 1
	case "WARN", "WARNING":
		return 2
	case "ERROR":
		return 3
	}
	return 1
}

// ConfigGet asks for one top-level section of the configuration, or for the
// list of sections when Section is empty.
type ConfigGet struct {
	Section string `cbor:"section,omitempty"`
}

func (h *Handler) onConfigGet(env Envelope) error {
	if h.cfg.ConfigDoc == nil || h.cfg.Redactor == nil {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "config"})
	}
	var m ConfigGet
	if len(env.B) > 0 {
		if err := Unmarshal(env.B, &m); err != nil {
			return h.refuse(env.ID, ErrUnknownOp, map[string]any{"t": env.T})
		}
	}
	doc, err := h.cfg.ConfigDoc()
	if err != nil {
		return h.refuse(env.ID, ErrUnavailable, map[string]any{"subsystem": "config"})
	}
	top, _ := h.cfg.Redactor().Config(doc).(map[string]any)
	if m.Section == "" {
		sections := make([]string, 0, len(top))
		for k := range top {
			sections = append(sections, k)
		}
		slices.Sort(sections)
		return h.sendBulk(protocol.MsgConfig, env.ID, map[string]any{"sections": sections})
	}
	section, ok := top[m.Section]
	if !ok {
		return h.refuse(env.ID, protocol.ErrNotFound, map[string]any{"section": m.Section})
	}
	body := map[string]any{"section": m.Section, "doc": section}
	if !h.fits(protocol.MsgConfig, env.ID, body) {
		return h.refuse(env.ID, ErrRangeTooLarge, map[string]any{"section": m.Section})
	}
	return h.sendBulk(protocol.MsgConfig, env.ID, body)
}

// Decide answers a pending request from the box's own page, where the owner
// signed in on the home network and the box is itself the authority, so no
// signature is asked. h is a handler made for the box page, with a LAN
// owner's caller. Approving runs the op on it, as a command under the
// owner's scopes, and returns the command's outcome.
func (h *Handler) Decide(ctx context.Context, reqID string, approve bool) (string, *CmdResult, error) {
	if h.cfg.Access == nil {
		return "", nil, protocol.Refuse(ErrUnavailable, map[string]any{"subsystem": "access"})
	}
	now, synced := h.wall()
	if approve && !synced {
		// A request's life is wall time. Without a clock to trust, the box
		// cannot tell one that has ended from one that has not.
		return "", nil, protocol.Refuse(ErrUnavailable, map[string]any{"subsystem": "clock"})
	}
	req, ok := h.cfg.Agents.pending(reqID, now, h.liveAgents())
	if !ok {
		h.audit("", protocol.MsgAgentConfirm, reqID, protocol.ErrNotFound)
		return "", nil, protocol.Refuse(protocol.ErrNotFound, map[string]any{"reqId": reqID})
	}
	defer h.cfg.Agents.tell("", protocol.AgentDecided{})
	if !approve {
		h.cfg.Agents.decide(reqID, protocol.RequestDeclined, now)
		h.audit("", protocol.MsgAgentConfirm, reqID, protocol.RequestDeclined)
		h.cfg.Agents.tell(req.Agent, protocol.AgentDecided{ReqID: reqID, State: protocol.RequestDeclined})
		return protocol.RequestDeclined, nil, nil
	}
	if !h.stillGranted(req) {
		h.cfg.Agents.decide(reqID, protocol.RequestExpired, now)
		return "", nil, protocol.Refuse(protocol.ErrNotFound, map[string]any{"reqId": reqID})
	}
	if !h.cfg.Agents.decide(reqID, protocol.RequestApproved, now) {
		return "", nil, protocol.Refuse(protocol.ErrNotFound, map[string]any{"reqId": reqID})
	}
	h.audit("", protocol.MsgAgentConfirm, reqID, protocol.RequestApproved)
	if err := h.runRequest(ctx, req); err != nil {
		return protocol.RequestApproved, nil, err
	}
	var res *CmdResult
	if rec, ok := h.cfg.Commands.lookup(h.cmdRoot(), "req:"+reqID); ok {
		res = rec.result
	}
	return protocol.RequestApproved, res, nil
}

// Requests lists every request the box holds, for the box page.
func (h *Handler) Requests() []protocol.Request {
	now, _ := h.wall()
	return h.cfg.Agents.list("", now, h.liveAgents())
}

// sendDecided tells this agent's session how one of its requests ended.
func (h *Handler) sendDecided(d protocol.AgentDecided) {
	if d.Result != nil {
		// The owner's outcome, retold to the agent: names become agent ids.
		res := h.forAgentResult(*d.Result)
		d.Result = &res
	}
	if err := h.sendBulk(protocol.MsgAgentDecided, nil, d); err != nil {
		h.log.Debug("could not tell an agent how its request ended", "err", err)
	}
}

var (
	reAuditWord = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]{0,47}$`)
	reAuditText = regexp.MustCompile(`^[A-Za-z0-9_.:@ -]{0,96}$`)
)

// auditWord keeps a message type, op or code the sender chose only when it
// is short and plain; anything else is "other". The log is the owner's, not
// a place an agent can write into.
func auditWord(s string) string {
	if reAuditWord.MatchString(s) {
		return s
	}
	return "other"
}

// auditText is auditWord for a target or a staff id: a little longer, and
// cut rather than replaced.
func auditText(s string) string {
	if reAuditText.MatchString(s) {
		return s
	}
	return "[omitted]"
}
