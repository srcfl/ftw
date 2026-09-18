// Package apiauth says who is asking the box's HTTP API for something.
//
// Until now nobody was: the API is served on the home LAN with no
// authentication at all, and no handler has ever had a way to name its caller.
// This package is the seam that changes that, and it is deliberately the
// smallest thing that can be one — a value, a context key and a scope set.
//
// It imports nothing of the box, so every source of callers can produce one
// without dragging the others in: the app uplink today, a LAN token or a Home
// Assistant add-on later. The whole future job of authenticating the LAN API
// is replacing one branch in api.Authenticate; every handler downstream is
// already written against apiauth.From.
//
// A Caller never travels on a wire. It is put on the request context inside
// the box process by whoever authenticated the request, and there is no
// serialised form of it for anything to forge.
package apiauth

import (
	"context"
	"net/http"
	"sort"
)

// Caller is one authenticated origin of a request.
type Caller struct {
	// Subject is opaque and stable, and it is what an audit line names:
	// "app:" + the enrolled device id for a phone.
	Subject string

	// Kind is where the authority came from. It is not a permission — two
	// callers of different kinds can hold identical scopes.
	Kind string

	// Role is a registry role key: "owner" or "viewer". It decides
	// presentation and the passthrough's configure gate; scopes decide
	// named operations.
	Role string

	// Scopes is the role expanded through the registry's role table.
	Scopes ScopeSet

	// StepUp says this request carried a fresh passkey ceremony on the
	// client. The box cannot verify that and must never claim to — see the
	// note above the check in appproto's passthrough for what it does and
	// does not buy.
	StepUp bool

	// Epoch is how many times this enrolment has changed. It is stamped at
	// admission and refreshed whenever the grant is re-read, so a log line
	// can say which version of a grant a request ran under.
	//
	// It is not what stops a revoked phone, and nothing compares it. What
	// stops one is the grant being re-read on every privileged request: a
	// row that is gone ends the session, and a row whose role changed is
	// obeyed as it now reads. Comparing the epoch as well would refuse a
	// demoted owner's reads, which they still have every right to.
	Epoch uint64
}

// Kinds of caller. More will exist; each one is a different authenticator,
// never a different level of trust.
const (
	// KindApp is a phone enrolled over the app link, authenticated by its
	// Noise static key.
	KindApp = "app"
	// KindLAN is an unauthenticated request off the box's own LAN listener.
	// Naming it is not a promotion: it writes down what the LAN already is.
	KindLAN = "lan"
)

// Tier is how much a route costs before it runs.
//
// It comes from what the handler DOES, declared beside the handler, and never
// from the request's method. The method was tried and it failed twice in one
// review: GET /api/caldav/credentials hands out a password that is a write
// channel into dispatch, and POST /api/self_tune/start drives every battery in
// the house through ±3000 W for minutes. A verb cannot know either of those,
// so it is not asked.
//
// The cost of that is one line per route and no free reads: a view written in
// the app next year needs the box to have named the path. That is the right
// direction to be wrong in — a route nobody has priced is refused rather than
// served, which is allow-list over deny-list one level up.
type Tier string

const (
	// TierRead answers a question, changes nothing, and carries nothing back
	// that could be replayed as authority. A shared viewer may ask for it.
	TierRead Tier = "read"
	// TierConfigure changes a setting. A late execution is the same
	// instruction, only later. Owner, and usually a step-up. A route
	// marked NoStepUp skips the ceremony: the session already proved
	// who is asking.
	TierConfigure Tier = "configure"
	// TierActuate moves energy, or takes control of what is moving it. It
	// never travels over the passthrough, because an HTTP request carries no
	// expiry and a request with no expiry must not move energy. A late
	// execution here is a DIFFERENT instruction.
	TierActuate Tier = "actuate"
	// TierLocal is served only on the box's own page, at home. Either its
	// answer carries a credential or a whole file the box cannot vouch for,
	// or doing it needs somebody standing at the box — a browser origin, a
	// person in the room, or code about to run on a live battery.
	//
	// It is not a permission an owner is missing. No role and no ceremony
	// changes the answer, which is why it is a tier and not a role check.
	TierLocal Tier = "local"
)

// Tiers is every tier the box knows, and the gate has a branch for each. A
// value outside this set is a route nobody priced, and both the router and the
// gate treat that as TierLocal — closed.
var Tiers = []Tier{TierRead, TierConfigure, TierActuate, TierLocal}

// Known reports whether a tier is one of the four. The zero Tier is not, so a
// route that never named one cannot be mistaken for a read.
func (t Tier) Known() bool {
	for _, known := range Tiers {
		if t == known {
			return true
		}
	}
	return false
}

// RouteFacts is what the box knows about a route before it runs it.
type RouteFacts struct {
	Tier Tier
	// CmdOp names the command that does this instead, when one exists. Empty
	// means the box has no command for it yet, and the honest answer to the
	// app is that this control is not available over the session.
	CmdOp string
	// Static marks the catch-all that serves the box's own web UI. Nothing
	// reachable through it belongs in an app session: the box's HTML served
	// under another name would be a second origin, which the architecture
	// rejected. The passthrough refuses it as a path it does not carry.
	Static bool

	// ReplacesAll marks a route whose body replaces a whole document rather
	// than editing part of one. POST /api/config is the example: it writes
	// the entire configuration, so a client that sent a body built from an
	// older idea of the document silently drops every field it never knew
	// about. That has already cost this project one regression, on the LAN,
	// where at least the browser had just loaded the whole document from the
	// same box. A phone on a relay has no such guarantee.
	ReplacesAll bool

	// NoStepUp marks a configure route that needs owner but not a fresh
	// passkey ceremony. The charging schedule is the case: changing when
	// the car should be ready is a household setting, and the session
	// already proved who is asking.
	NoStepUp bool
}

// ScopeSet is what a caller may ask for.
type ScopeSet struct {
	// every short-circuits Has. It exists for the LAN, which is served with
	// no authentication at all; writing the list out instead would be a copy
	// of the registry's scope table in a place the generator cannot reach.
	every bool
	names map[string]struct{}
}

// NewScopeSet builds a set from explicit names, which come from the generated
// role table and never from a literal at the call site.
func NewScopeSet(names ...string) ScopeSet {
	set := ScopeSet{names: make(map[string]struct{}, len(names))}
	for _, n := range names {
		set.names[n] = struct{}{}
	}
	return set
}

// EveryScope is unrestricted authority.
func EveryScope() ScopeSet { return ScopeSet{every: true} }

// Has reports whether the set carries a scope. The zero ScopeSet carries
// none, so a Caller nobody filled in can do nothing.
func (s ScopeSet) Has(scope string) bool {
	if s.every {
		return true
	}
	_, ok := s.names[scope]
	return ok
}

// Names lists the set, sorted. An unrestricted set has no list to give and
// returns nil — the app is only ever told its own role's expansion, which is
// always explicit.
func (s ScopeSet) Names() []string {
	if s.every || len(s.names) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.names))
	for n := range s.names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

type contextKey struct{}

// WithCaller puts a caller on a request context. Only an authenticator calls
// this, and only inside the box process.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}

// From reads the caller back. The second result is false when nothing
// authenticated the request, which a handler must treat as no authority
// rather than as full authority.
func From(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(contextKey{}).(Caller)
	return c, ok
}

// FromRequest is From, for handlers that hold a request rather than a context.
func FromRequest(r *http.Request) (Caller, bool) {
	if r == nil {
		return Caller{}, false
	}
	return From(r.Context())
}
