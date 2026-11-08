package api

// Agents on the box's own page (docs/agents.md, Approval on the box page).
//
// An owner phone manages agents over its session, each grant signed by its
// approval key. On the home network the box is itself the authority, so
// these routes ask for no signature. They ask for something else: widening
// access needs a browser signed in with the house password, on another
// device or on the box itself. What proves the owner is the password, not
// the address: a driver's Lua reaches the box from inside but does not know
// it, and the session passthrough is refused outright. With api.lan_auth
// off every device on the home network is an owner with no sign-in at all,
// and granting a remote agent from there would let any of them open the
// house to the outside; the page asks for a password first, and choosing
// one signs that browser in. Narrowing stays open to any LAN client, as
// DELETE /api/cloud/grant does.
//
// Every route is local: the session passthrough never carries them, so a
// phone must use its signed messages and an agent can never reach them.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/srcfl/ftw/go/internal/localzone"
	"net/http"
	"slices"
	"strconv"

	"github.com/srcfl/ftw/go/protocol"
)

// AgentAdmin is the box page's view of agent grants. by names the caller
// for the audit log: "box" for the box itself, or "lan:<address>".
type AgentAdmin interface {
	Agents() []protocol.AgentRow
	Requests() []AgentRequest
	ApprovalKeys() []protocol.ApprovalKeyInfo
	// Home is this box's home id at FTW Cloud, or "" before it links.
	Home() string
	// Wall is the wall clock in ms, and whether NTP has set it.
	Wall() (int64, bool)
	// OpenOffer checks a signed offer, base64url as the consent page gave
	// it, against the offer keys this build pins.
	OpenOffer(raw string) (protocol.GrantOffer, error)
	// ReplaceApprovalKey sets an owner phone's approval key, for a phone
	// that lost its own.
	ReplaceApprovalKey(by, deviceID string, pub []byte) error
	AddAgent(by string, m protocol.AgentAdd) (protocol.AgentRow, error)
	WidenAgent(by string, m protocol.AgentWiden) (protocol.AgentRow, error)
	RevokeAgent(by, id string) error
	Decide(ctx context.Context, by, reqID string, approve bool) (string, *protocol.CmdResult, error)
	Audit(beforeSeq int64, n int) ([]protocol.AuditRow, bool, error)
	// ResetClock makes the box trust its clock again after it once ran
	// ahead, and ends every agent's grant.
	ResetClock(by string) error
	// Facts are what the box page checks before it asks the owner for a
	// code, which works once.
	Facts() AgentFacts
}

// AgentRequest is a request as the box page lists it: the owner's view,
// with the box's own ids.
type AgentRequest struct {
	protocol.Request
	// Names maps each arg that names a charger or car the box still knows
	// to what the box calls it. The page is local and its caller an owner,
	// so it may name them; nothing an agent or FTW Cloud reads carries
	// this.
	Names map[string]string `json:"names,omitempty"`
}

// AgentFacts say whether this box can take a grant now, and where the owner's
// browser looks a code up.
type AgentFacts struct {
	Timezone string `json:"timezone"`
	// Offers says this build pins an FTW Cloud offer key. A build that pins
	// none opens no offer.
	Offers bool `json:"offers"`
	// Lookup is FTW Cloud's code lookup. The owner's browser asks it, and
	// posts the signed offer it gets here.
	Lookup string `json:"lookup"`
	// Clock is ClockOK, ClockUnsynced or ClockBehind.
	Clock string `json:"clock"`
	// NowMs is the box's wall clock, and MarkMs the highest wall time it
	// has seen.
	NowMs  int64 `json:"nowMs"`
	MarkMs int64 `json:"markMs"`
	// Widen says this request would pass the gate for widening access: a
	// browser signed in with the house password. The list sets it per
	// request, so the page stops before a code is spent on a grant the box
	// would refuse.
	Widen bool `json:"widen"`
	// LongestMs is the longest grant the box takes for each role, so the
	// page offers no length the box would refuse.
	LongestMs map[string]int64 `json:"longestMs"`
}

// The agent clock as the box page reports it.
const (
	ClockOK = "ok"
	// ClockUnsynced is a clock NTP has not set.
	ClockUnsynced = "unsynced"
	// ClockBehind is a set clock below the highest time the box has seen.
	// Agents stay locked out until it passes that time again, or an owner
	// resets the mark.
	ClockBehind = "behind"
)

func (s *Server) agentRoutes() {
	s.agentInviteRoutes()
	s.handle("GET    /api/agents", Local, s.handleAgentsList)
	s.handle("GET    /api/agents/audit", Local, s.handleAgentsAudit)
	s.handle("POST   /api/agents/offer", Local, s.handleAgentsOffer)
	s.handle("POST   /api/agents", Local, s.handleAgentsAdd)
	s.handle("POST   /api/agents/{id}/widen", Local, s.handleAgentsWiden)
	s.handle("DELETE /api/agents/{id}", Local, s.handleAgentsRevoke)
	s.handle("POST   /api/agent-requests/{reqId}", Local, s.handleAgentsDecide)
	s.handle("POST   /api/agents/approval-key", Local, s.handleAgentsApprovalKey)
	s.handle("POST   /api/agents/clock-reset", Local, s.handleAgentsClockReset)
}

// handleAgentsClockReset is for a clock that once ran ahead and now keeps
// every agent locked out. It needs the house password, and every agent's
// grant ends.
func (s *Server) handleAgentsClockReset(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, true)
	if !ok {
		return
	}
	if err := s.deps.Agents.ResetClock(by); err != nil {
		agentError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// agentsGate admits a client on the home network, and with widen only one
// signed in with the house password, the box itself included. It returns
// the caller's name for the audit log.
func (s *Server) agentsGate(w http.ResponseWriter, r *http.Request, widen bool) (string, bool) {
	if s.deps == nil || s.deps.Agents == nil {
		writeAppLinkError(w, http.StatusServiceUnavailable, "agents are not available on this box")
		return "", false
	}
	auth, err := parseAuthority(r.Host)
	if hasForwardingHeader(r.Header) || appLinkOverSession(r) || err != nil ||
		!isLocalAuthority(auth) || !isLocalClient(r.RemoteAddr) {
		writeAppLinkError(w, http.StatusForbidden, "agents are managed in the app or on your home network")
		return "", false
	}
	by := "lan:" + remoteHost(r)
	if isLoopbackClient(r.RemoteAddr) {
		by = "box"
	}
	ok, checked := lanSecretFrom(r.Context())
	if checked && !ok {
		// A wrong house password is a refusal, even for a route that only
		// narrows.
		writeAppLinkError(w, http.StatusForbidden, "sign in with the house password")
		return "", false
	}
	if !widen || ok {
		return by, true
	}
	writeAppLinkError(w, http.StatusForbidden,
		"give access in the app, or on your home network signed in with the house password")
	return "", false
}

// agentError answers a refusal with its code, and anything else as a 500.
func agentError(w http.ResponseWriter, err error) {
	var r *protocol.Refusal
	if !errors.As(err, &r) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	status := http.StatusBadRequest
	switch r.Code {
	case protocol.ErrNotFound:
		status = http.StatusNotFound
	case protocol.ErrConflict:
		status = http.StatusConflict
	case protocol.ErrUnavailable:
		status = http.StatusServiceUnavailable
	case protocol.ErrScopeDenied:
		status = http.StatusForbidden
	}
	writeJSON(w, status, map[string]any{"error": r.Code, "args": r.Args})
}

// agentsPage is agent.list as the box page reads it, its requests with their
// chargers' and cars' names, the box's facts and the registry's view of
// agent ops, so the page names nothing by hand.
type agentsPage struct {
	Agents   []protocol.AgentRow        `json:"agents"`
	Requests []AgentRequest             `json:"requests"`
	Keys     []protocol.ApprovalKeyInfo `json:"keys"`
	Box      AgentFacts                 `json:"box"`
	Registry agentsRegistry             `json:"registry"`
}

type agentsRegistry struct {
	// AgentOps maps each op an agent may name to the scope that opens it.
	AgentOps map[string]string `json:"agentOps"`
	// WriteScopes change something, now or once an owner says yes.
	WriteScopes []string `json:"writeScopes"`
	CallScopes  []string `json:"callScopes"`
	// AgentArgs are each op's arguments, in the registry's order, with the
	// label an owner reads.
	AgentArgs map[string][]agentArgView `json:"agentArgs"`
}

type agentArgView struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Unit  string `json:"unit,omitempty"`
	// Cap names the grant's cap that bounds the argument too.
	Cap string `json:"cap,omitempty"`
}

func registryForPage() agentsRegistry {
	calls := []string{}
	for _, scope := range protocol.RegistryAgentCalls {
		if slices.Contains(protocol.WriteScopes, scope) {
			calls = append(calls, scope)
		}
	}
	slices.Sort(calls)
	calls = slices.Compact(calls)
	args := map[string][]agentArgView{}
	for op, list := range protocol.RegistryAgentArgs {
		for _, a := range list {
			args[op] = append(args[op], agentArgView{Name: a.Name, Label: a.Label, Kind: a.Kind, Unit: a.Unit, Cap: a.Cap})
		}
	}
	return agentsRegistry{AgentOps: protocol.RegistryAgentOps, WriteScopes: protocol.WriteScopes, CallScopes: calls, AgentArgs: args}
}

func (s *Server) handleAgentsList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.agentsGate(w, r, false); !ok {
		return
	}
	a := s.deps.Agents
	facts := a.Facts()
	facts.Timezone = localzone.Name()
	facts.Widen, _ = lanSecretFrom(r.Context())
	writeJSON(w, http.StatusOK, agentsPage{
		Agents: nonNil(a.Agents()), Requests: nonNil(a.Requests()), Keys: nonNil(a.ApprovalKeys()),
		Box:      facts,
		Registry: registryForPage(),
	})
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func (s *Server) handleAgentsAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.agentsGate(w, r, false); !ok {
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	rows, more, err := s.deps.Agents.Audit(before, n)
	if err != nil {
		agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.AuditPage{Rows: nonNil(rows), More: more})
}

// agentAddBody is a grant offer, as FTW Cloud's consent page shows it, and
// what the owner chose to grant of it. A field left out grants what the
// offer asks, except ttlS, which grants at most 30 days.
type agentAddBody struct {
	Offer  string         `json:"offer"`
	Scopes []string       `json:"scopes,omitempty"`
	Ops    []string       `json:"ops,omitempty"`
	Caps   *protocol.Caps `json:"caps,omitempty"`
	TTLS   uint64         `json:"ttlS,omitempty"`
}

func (s *Server) handleAgentsAdd(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, true)
	if !ok {
		return
	}
	var body agentAddBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is not a grant"})
		return
	}
	offer, now, ok := s.openOffer(w, body.Offer)
	if !ok {
		return
	}
	if !withinOffer(offer, body) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the box may grant less than the offer, not more"})
		return
	}
	m := protocol.AgentAdd{
		Key: offer.Key, Role: offer.Role, Scopes: offer.Scopes, Ops: offer.Ops, Caps: offer.Caps,
		Principal: protocol.Principal{Client: offer.Client, ClientID: offer.ClientID, Ticket: offer.Ticket},
	}
	if body.Scopes != nil {
		m.Scopes = body.Scopes
	}
	if body.Ops != nil {
		m.Ops = body.Ops
	}
	if body.Caps != nil {
		m.Caps = *body.Caps
	}
	ttl := min(offer.TTLS, defaultTTLS)
	if body.TTLS != 0 {
		ttl = body.TTLS
	}
	end, err := grantEnd(now, ttl, m.Role)
	if err != nil {
		agentError(w, err)
		return
	}
	m.ExpiresAtMs = end
	row, err := s.deps.Agents.AddAgent(by, m)
	if err != nil {
		agentError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}

// Why the box will not take an offer, for the page to say in its own words.
const (
	OfferNoKeys    = "no_offer_keys"
	OfferUnsigned  = "unsigned"
	OfferExpired   = "expired"
	OfferMalformed = "malformed"
	OfferOtherHome = "other_home"
)

// openOffer reads an offer as the box would grant it. When it will not, it
// answers why and returns false. A clock the box cannot trust comes first:
// without one every offer would read as expired.
func (s *Server) openOffer(w http.ResponseWriter, raw string) (protocol.GrantOffer, int64, bool) {
	a := s.deps.Agents
	now, synced := a.Wall()
	if !synced {
		agentError(w, protocol.Refuse(protocol.ErrUnavailable, map[string]any{"subsystem": "clock"}))
		return protocol.GrantOffer{}, 0, false
	}
	refuse := func(reason, text string) (protocol.GrantOffer, int64, bool) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": text, "reason": reason, "boxMs": now})
		return protocol.GrantOffer{}, 0, false
	}
	offer, err := a.OpenOffer(raw)
	switch {
	case errors.Is(err, protocol.ErrOfferSignature) && !a.Facts().Offers:
		return refuse(OfferNoKeys, "this build of FTW pins no FTW Cloud offer key, so it takes no offer")
	case errors.Is(err, protocol.ErrOfferSignature):
		return refuse(OfferUnsigned, "no offer key this box knows signed the offer")
	case errors.Is(err, protocol.ErrOfferExpired):
		return refuse(OfferExpired, "the offer has expired by this box's clock")
	case err != nil:
		return refuse(OfferMalformed, "the offer is not readable")
	}
	if home := a.Home(); offer.Home != "" && offer.Home != home {
		return refuse(OfferOtherHome, "the offer is for another home")
	}
	return offer, now, true
}

// offerView is a signed offer as the box read it, for the owner to check
// before granting.
type offerView struct {
	Role       string        `json:"role"`
	Client     string        `json:"client"`
	ClientID   string        `json:"clientId"`
	ClientHost string        `json:"clientHost"`
	Verified   bool          `json:"verified"`
	Ticket     string        `json:"ticket"`
	Scopes     []string      `json:"scopes"`
	Ops        []string      `json:"ops"`
	Caps       protocol.Caps `json:"caps"`
	TTLS       uint64        `json:"ttlS"`
	Code       string        `json:"code"`
	ExpMs      int64         `json:"expMs"`
}

// handleAgentsOffer opens a signed offer for the box page, so the owner sees
// what the box read in it, or why it will not take it, before granting. It
// grants nothing, so it asks only what reading the agents asks.
func (s *Server) handleAgentsOffer(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.agentsGate(w, r, false); !ok {
		return
	}
	var body struct {
		Offer string `json:"offer"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body needs offer"})
		return
	}
	o, _, ok := s.openOffer(w, body.Offer)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, offerView{
		Role: o.Role, Client: o.Client, ClientID: o.ClientID, ClientHost: o.ClientHost, Verified: o.Verified,
		Ticket: o.Ticket, Scopes: nonNil(o.Scopes), Ops: nonNil(o.Ops), Caps: o.Caps, TTLS: o.TTLS,
		Code: o.Code, ExpMs: o.ExpMs,
	})
}

// defaultTTLS is the length a grant gets when the request names none and the
// offer asks for more: 30 days, where the app starts.
const defaultTTLS = 30 * 24 * 60 * 60

// grantEnd is the end of a grant of ttlS seconds from now. A length past the
// role's longest grant is refused here, as the box would refuse it, before
// it can overflow.
func grantEnd(now int64, ttlS uint64, role string) (int64, error) {
	most, ok := protocol.LongestMs()[role]
	if !ok {
		most = protocol.AgentMaxMs
	}
	if ttlS > uint64(most/1000) {
		return 0, protocol.Refuse(protocol.ErrCapExceeded, map[string]any{"cap": "expiresAtMs", "max": now + most})
	}
	return now + int64(ttlS)*1000, nil
}

// withinOffer reports whether what the owner chose on the page gives no more
// than the offer the agent's client signed for.
func withinOffer(offer protocol.GrantOffer, body agentAddBody) bool {
	for _, scope := range body.Scopes {
		if !slices.Contains(offer.Scopes, scope) {
			return false
		}
	}
	for _, op := range body.Ops {
		if !slices.Contains(offer.Ops, op) {
			return false
		}
	}
	if c := body.Caps; c != nil {
		perMin := func(n uint64) uint64 {
			if n == 0 {
				return protocol.PerMinDefault
			}
			return n
		}
		if c.HoldS > offer.Caps.HoldS || c.MaxW > offer.Caps.MaxW || perMin(c.PerMin) > perMin(offer.Caps.PerMin) {
			return false
		}
	}
	return body.TTLS <= offer.TTLS
}

// agentWidenBody is a grant's new rights, whole.
type agentWidenBody struct {
	Epoch  uint64        `json:"epoch"`
	Scopes []string      `json:"scopes"`
	Ops    []string      `json:"ops"`
	Caps   protocol.Caps `json:"caps"`
	TTLS   uint64        `json:"ttlS"`
}

func (s *Server) handleAgentsWiden(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, true)
	if !ok {
		return
	}
	var body agentWidenBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body is not a grant"})
		return
	}
	now, synced := s.deps.Agents.Wall()
	if !synced {
		agentError(w, protocol.Refuse(protocol.ErrUnavailable, map[string]any{"subsystem": "clock"}))
		return
	}
	id := r.PathValue("id")
	role := protocol.RoleAgent
	for _, row := range s.deps.Agents.Agents() {
		if row.ID == id {
			role = row.Role
		}
	}
	end, err := grantEnd(now, body.TTLS, role)
	if err != nil {
		agentError(w, err)
		return
	}
	row, err := s.deps.Agents.WidenAgent(by, protocol.AgentWiden{
		ID: id, Epoch: body.Epoch, Scopes: body.Scopes, Ops: body.Ops, Caps: body.Caps, ExpiresAtMs: end,
	})
	if err != nil {
		agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleAgentsRevoke(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, false)
	if !ok {
		return
	}
	if err := s.deps.Agents.RevokeAgent(by, r.PathValue("id")); err != nil {
		agentError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentsDecide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Approve bool `json:"approve"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body needs approve"})
		return
	}
	// Approving needs what widening needs; declining is open, as removing
	// access is.
	by, ok := s.agentsGate(w, r, body.Approve)
	if !ok {
		return
	}
	state, res, err := s.deps.Agents.Decide(r.Context(), by, r.PathValue("reqId"), body.Approve)
	if err != nil {
		agentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": state, "result": res})
}

// handleAgentsApprovalKey sets an owner phone's approval key from the box
// page, for a phone whose key a new Face ID enrolment voided.
func (s *Server) handleAgentsApprovalKey(w http.ResponseWriter, r *http.Request) {
	by, ok := s.agentsGate(w, r, true)
	if !ok {
		return
	}
	var body struct {
		DeviceID string `json:"deviceId"`
		Pub      string `json:"pub"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body needs deviceId and pub"})
		return
	}
	pub, err := base64.RawURLEncoding.DecodeString(body.Pub)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "pub is not base64url"})
		return
	}
	if err := s.deps.Agents.ReplaceApprovalKey(by, body.DeviceID, pub); err != nil {
		agentError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
