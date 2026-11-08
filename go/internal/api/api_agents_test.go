package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll/approvaltest"
	"github.com/srcfl/ftw/go/protocol"
)

type stubAgents struct {
	keys     map[string][]byte
	replaced []string
	added    []protocol.AgentAdd
	revoked  []string
	decided  []bool
	by       []string
	home     string
	unsynced bool
	resets   int
	// noKeys is a build that pins no offer key.
	noKeys   bool
	requests []AgentRequest
}

func (a *stubAgents) Agents() []protocol.AgentRow              { return nil }
func (a *stubAgents) Requests() []AgentRequest                 { return a.requests }
func (a *stubAgents) ApprovalKeys() []protocol.ApprovalKeyInfo { return nil }
func (a *stubAgents) Home() string                             { return a.home }
func (a *stubAgents) Wall() (int64, bool)                      { return 1_000_000, !a.unsynced }
func (a *stubAgents) AddAgent(by string, m protocol.AgentAdd) (protocol.AgentRow, error) {
	a.added = append(a.added, m)
	a.by = append(a.by, by)
	return protocol.AgentRow{ID: "agent001", Role: m.Role}, nil
}
func (a *stubAgents) WidenAgent(by string, m protocol.AgentWiden) (protocol.AgentRow, error) {
	a.by = append(a.by, by)
	return protocol.AgentRow{ID: m.ID}, nil
}
func (a *stubAgents) RevokeAgent(by, id string) error {
	a.revoked = append(a.revoked, id)
	a.by = append(a.by, by)
	return nil
}
func (a *stubAgents) Decide(_ context.Context, by, _ string, approve bool) (string, *protocol.CmdResult, error) {
	a.decided = append(a.decided, approve)
	a.by = append(a.by, by)
	return "approved", nil, nil
}
func (a *stubAgents) Audit(int64, int) ([]protocol.AuditRow, bool, error) { return nil, false, nil }
func (a *stubAgents) ResetClock(by string) error {
	a.resets++
	a.by = append(a.by, by)
	return nil
}
func (a *stubAgents) Facts() AgentFacts {
	return AgentFacts{Offers: !a.noKeys, Lookup: "https://cloud.ftw.energy/oauth/requests", Clock: ClockOK, NowMs: 1_000_000}
}
func (a *stubAgents) OpenOffer(raw string) (protocol.GrantOffer, error) {
	signed, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return protocol.GrantOffer{}, err
	}
	var so protocol.SignedOffer
	if err := protocol.UnmarshalSigned(signed, &so); err != nil {
		return protocol.GrantOffer{}, err
	}
	keys := a.keys
	if a.noKeys {
		keys = nil
	}
	return protocol.OpenOffer(keys, so, 1_000_000)
}
func (a *stubAgents) ReplaceApprovalKey(by, deviceID string, _ []byte) error {
	a.replaced = append(a.replaced, deviceID)
	a.by = append(a.by, by)
	return nil
}

// offerSigner is FTW Cloud's offer key, for the tests.
var offerSigner = approvaltest.New()

func newStubAgents(home string) *stubAgents {
	return &stubAgents{home: home, keys: map[string][]byte{"k1": offerSigner.Pub()}}
}

func offerBody(t *testing.T, home string) string {
	t.Helper()
	return signedOfferBody(t, home, offerSigner)
}

func signedOfferBody(t *testing.T, home string, signer *approvaltest.Signer) string {
	t.Helper()
	return offerOf(t, protocol.GrantOffer{
		V: protocol.GrantOfferVersion, Home: home, Key: make([]byte, 32), Role: apiauth.RoleAgent,
		Client: "Claude", Scopes: []string{protocol.ScopeLiveRead}, Ops: []string{}, TTLS: 3600, Code: "ABCD",
		ExpMs: 1_000_000 + 60_000,
	}, signer)
}

func offerOf(t *testing.T, offer protocol.GrantOffer, signer *approvaltest.Signer) string {
	t.Helper()
	o, err := protocol.Marshal(offer)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.OfferBytes(o)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := protocol.Marshal(protocol.SignedOffer{O: o, Kid: "k1", Sig: signer.Sign(msg)})
	if err != nil {
		t.Fatal(err)
	}
	return `{"offer":"` + base64.RawURLEncoding.EncodeToString(raw) + `"}`
}

// Giving an agent access from the box page needs the house password, from
// any address: with lan_auth off every device on the Wi-Fi is an owner with
// no sign-in, and a driver or the passthrough can reach the box from inside.
func TestWhoMayGiveAnAgentAccessOnTheBoxPage(t *testing.T) {
	body := offerBody(t, "home-1")
	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"the box itself, without the password", loopbackRequest(http.MethodPost, "/api/agents", body), http.StatusForbidden},
		{"a LAN client", localRequest(http.MethodPost, "/api/agents", body), http.StatusForbidden},
		{"a LAN client with the house password", func() *http.Request {
			r := localRequest(http.MethodPost, "/api/agents", body)
			return r.WithContext(withLANSecret(r.Context(), true))
		}(), http.StatusCreated},
		{"an owner's phone through the passthrough", sessionRequest(http.MethodPost, "/api/agents", body), http.StatusForbidden},
		{"through a proxy", func() *http.Request {
			r := loopbackRequest(http.MethodPost, "/api/agents", body)
			r.Header.Set("X-Forwarded-For", "203.0.113.9")
			return r
		}(), http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			agents := newStubAgents("home-1")
			s := New(&Deps{Agents: agents})
			w := httptest.NewRecorder()
			s.handleAgentsAdd(w, c.req)
			if w.Code != c.want {
				t.Fatalf("got %d, want %d: %s", w.Code, c.want, w.Body.String())
			}
			if added := len(agents.added) == 1; added != (c.want == http.StatusCreated) {
				t.Fatalf("added = %v with status %d", added, w.Code)
			}
		})
	}
}

func signedIn(r *http.Request) *http.Request { return r.WithContext(withLANSecret(r.Context(), true)) }

func TestTheBoxPageTakesASignedOfferAndItsLength(t *testing.T) {
	agents := newStubAgents("home-1")
	s := New(&Deps{Agents: agents})
	add := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleAgentsAdd(w, signedIn(localRequest(http.MethodPost, "/api/agents", body)))
		return w
	}
	if w := add(offerBody(t, "")); w.Code != http.StatusCreated || len(agents.added) != 1 {
		t.Fatalf("an offer that leaves the home to the owner: %d %s", w.Code, w.Body.String())
	}
	m := agents.added[0]
	if m.ExpiresAtMs != 1_000_000+3_600_000 || m.Principal.Client != "Claude" || agents.by[0] != "lan:192.168.1.5" {
		t.Errorf("grant %+v by %v", m, agents.by)
	}
	if w := add(offerBody(t, "home-1")); w.Code != http.StatusCreated {
		t.Errorf("an offer for this box: %d", w.Code)
	}
	if w := add(offerBody(t, "another-home")); w.Code != http.StatusBadRequest {
		t.Errorf("an offer for another box: %d", w.Code)
	}
	if w := add(signedOfferBody(t, "", approvaltest.New())); w.Code != http.StatusBadRequest {
		t.Errorf("an offer no pinned key signed: %d", w.Code)
	}
	more := func(extra string) string {
		b := offerBody(t, "home-1")
		return b[:len(b)-1] + "," + extra + "}"
	}
	for _, extra := range []string{`"scopes":["ftw.logs.read"]`, `"ops":["loadpoint.hold"]`, `"caps":{"holdS":60}`, `"ttlS":7200`} {
		if w := add(more(extra)); w.Code != http.StatusBadRequest {
			t.Errorf("more than the offer, %s: %d", extra, w.Code)
		}
	}
	if w := add(more(`"scopes":[],"ttlS":60`)); w.Code != http.StatusCreated {
		t.Errorf("less than the offer: %d %s", w.Code, w.Body.String())
	}
	agents.unsynced = true
	if w := add(offerBody(t, "home-1")); w.Code != http.StatusServiceUnavailable {
		t.Errorf("without a clock: %d", w.Code)
	}
}

// The box page shows the owner what the box read in an offer before it
// grants, and each reason it will not take one, in words of the page's own.
// Reading an offer grants nothing, so any LAN client may ask.
func TestTheBoxPageReadsAnOfferBeforeGranting(t *testing.T) {
	agents := newStubAgents("home-1")
	s := New(&Deps{Agents: agents})
	check := func(body string) (int, map[string]any) {
		w := httptest.NewRecorder()
		s.handleAgentsOffer(w, localRequest(http.MethodPost, "/api/agents/offer", body))
		var got map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return w.Code, got
	}
	full := protocol.GrantOffer{
		V: protocol.GrantOfferVersion, Key: make([]byte, 32), Role: apiauth.RoleAgent,
		Client: "Claude", ClientID: "https://claude.ai/oauth/mcp-oauth-client-metadata", ClientHost: "claude.ai", Verified: true,
		Scopes: []string{protocol.ScopeDispatchHold, protocol.ScopeLiveRead}, Ops: []string{protocol.OpLoadpointHold},
		Caps: protocol.Caps{HoldS: 1800, MaxW: 11000, PerMin: 6}, TTLS: 2_592_000, Code: "ABCDEFGH", ExpMs: 1_000_000 + 60_000,
	}
	code, got := check(offerOf(t, full, offerSigner))
	if code != http.StatusOK || got["client"] != "Claude" || got["clientHost"] != "claude.ai" || got["verified"] != true ||
		got["code"] != "ABCDEFGH" || got["ttlS"] != float64(2_592_000) || len(got["ops"].([]any)) != 1 {
		t.Fatalf("a good offer: %d %v", code, got)
	}
	if caps := got["caps"].(map[string]any); caps["holdS"] != float64(1800) || caps["maxW"] != float64(11000) {
		t.Errorf("caps %v", caps)
	}
	if len(agents.added) != 0 {
		t.Fatal("reading an offer granted it")
	}

	expired := full
	expired.ExpMs = 1_000_000
	other := full
	other.Home = "another-home"
	for _, c := range []struct {
		name, body, reason string
	}{
		{"signed by a key the box does not pin", signedOfferBody(t, "", approvaltest.New()), OfferUnsigned},
		{"expired by the box's clock", offerOf(t, expired, offerSigner), OfferExpired},
		{"for another home", offerOf(t, other, offerSigner), OfferOtherHome},
		{"not an offer", `{"offer":"bm90IGFuIG9mZmVy"}`, OfferMalformed},
	} {
		code, got := check(c.body)
		if code != http.StatusBadRequest || got["reason"] != c.reason || got["boxMs"] != float64(1_000_000) {
			t.Errorf("%s: %d %v", c.name, code, got)
		}
	}

	agents.noKeys = true
	if code, got := check(offerOf(t, full, offerSigner)); code != http.StatusBadRequest || got["reason"] != OfferNoKeys {
		t.Errorf("a build that pins no offer key: %d %v", code, got)
	}
	agents.noKeys = false
	agents.unsynced = true
	if code, got := check(offerOf(t, full, offerSigner)); code != http.StatusServiceUnavailable || got["error"] != protocol.ErrUnavailable {
		t.Errorf("without a clock: %d %v", code, got)
	}

	// Granting answers with the same reasons.
	agents.unsynced = false
	w := httptest.NewRecorder()
	s.handleAgentsAdd(w, signedIn(localRequest(http.MethodPost, "/api/agents", offerOf(t, expired, offerSigner))))
	if !strings.Contains(w.Body.String(), `"reason":"expired"`) {
		t.Errorf("granting an expired offer: %d %s", w.Code, w.Body.String())
	}
}

// The list carries what the page checks before it asks for a code, which
// works once, and the registry's view of agent ops, so the page names
// nothing by hand.
func TestTheAgentListCarriesTheBoxFacts(t *testing.T) {
	s := New(&Deps{Agents: newStubAgents("")})
	w := httptest.NewRecorder()
	s.handleAgentsList(w, localRequest(http.MethodGet, "/api/agents", ""))
	var got struct {
		Agents   []protocol.AgentRow `json:"agents"`
		Box      AgentFacts          `json:"box"`
		Registry struct {
			AgentOps    map[string]string         `json:"agentOps"`
			WriteScopes []string                  `json:"writeScopes"`
			AgentArgs   map[string][]agentArgView `json:"agentArgs"`
		} `json:"registry"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v %s", w.Code, err, w.Body.String())
	}
	if got.Agents == nil || !got.Box.Offers || got.Box.Clock != ClockOK || got.Box.Lookup == "" {
		t.Errorf("box %+v agents %v", got.Box, got.Agents)
	}
	if got.Registry.AgentOps[protocol.OpLoadpointHold] != protocol.ScopeDispatchHold ||
		!slices.Contains(got.Registry.WriteScopes, protocol.ScopeGoalsRequest) {
		t.Errorf("registry %+v", got.Registry)
	}
	args := got.Registry.AgentArgs[protocol.OpLoadpointHold]
	if len(args) < 2 || args[0].Name != "id" || args[1].Name != "power_w" || args[1].Label == "" ||
		args[1].Unit != "W" || args[1].Cap != "maxW" {
		t.Errorf("loadpoint.hold's arguments, in the registry's order: %+v", args)
	}
}

// The list tells the page whether this request can widen access before the
// owner spends a code on a grant the box would refuse. The house password
// counts wherever the browser runs, the box itself included; without it,
// even the box cannot.
func TestTheListSaysWhetherThisRequestCanWiden(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	s := New(&Deps{Agents: newStubAgents("")})
	widenWith := func(url, remote, auth string) (bool, int) {
		var after *http.Request
		serveLANAuth(policy, lanAuthRequest(http.MethodGet, url, remote, auth),
			func(r *http.Request) { after = r })
		w := httptest.NewRecorder()
		s.handleAgentsList(w, after)
		var got struct {
			Box AgentFacts `json:"box"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		add := httptest.NewRecorder()
		post := after.Clone(after.Context())
		post.Method = http.MethodPost
		post.Body = io.NopCloser(strings.NewReader(offerBody(t, "")))
		s.handleAgentsAdd(add, post)
		return got.Box.Widen, add.Code
	}
	widen := func(url, remote string) (bool, int) { return widenWith(url, remote, "Bearer "+testHousePassword) }
	if ok, code := widen("http://127.0.0.1:8080/api/agents", "127.0.0.1:1234"); !ok || code != http.StatusCreated {
		t.Errorf("on the box itself, with the house password: widen %v, grant %d", ok, code)
	}
	if ok, code := widenWith("http://127.0.0.1:8080/api/agents", "127.0.0.1:1234", ""); ok || code != http.StatusForbidden {
		t.Errorf("on the box itself, without it: widen %v, grant %d", ok, code)
	}
	if ok, code := widenWith("http://127.0.0.1:8080/api/agents", "127.0.0.1:1234", "Bearer not-the-password"); ok || code != http.StatusForbidden {
		t.Errorf("on the box itself, with a wrong one: widen %v, grant %d", ok, code)
	}
	if ok, code := widen("http://192.168.1.1:8080/api/agents", "192.168.1.5:1234"); !ok || code != http.StatusCreated {
		t.Errorf("another device, signed in: widen %v, grant %d", ok, code)
	}
}

// A request on the box page keeps its args and adds the names of the
// chargers and cars they name; one with nothing to name carries no names.
func TestTheAgentListNamesARequestsChargerAndCar(t *testing.T) {
	agents := newStubAgents("")
	agents.requests = []AgentRequest{
		{Request: protocol.Request{ReqID: "r1", Op: protocol.OpCarGoalSet, State: protocol.RequestPending,
			Args: map[string]any{"id": "0123456789abcdef0123456789abcdef", "charger_id": "garage", "target": 0.8}},
			Names: map[string]string{"id": "Volvo", "charger_id": "garage"}},
		{Request: protocol.Request{ReqID: "r2", Op: protocol.OpSetMode, State: protocol.RequestPending,
			Args: map[string]any{"mode": "idle"}}},
	}
	s := New(&Deps{Agents: agents})
	w := httptest.NewRecorder()
	s.handleAgentsList(w, localRequest(http.MethodGet, "/api/agents", ""))
	var got struct {
		Requests []map[string]any `json:"requests"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Requests) != 2 {
		t.Fatalf("%d %v %s", w.Code, err, w.Body.String())
	}
	first := got.Requests[0]
	args, _ := first["args"].(map[string]any)
	names, _ := first["names"].(map[string]any)
	if first["reqId"] != "r1" || args["id"] != "0123456789abcdef0123456789abcdef" || names["id"] != "Volvo" ||
		names["charger_id"] != "garage" {
		t.Errorf("a car's goal on the box page: %v", first)
	}
	if _, ok := got.Requests[1]["names"]; ok {
		t.Errorf("a request with nothing to name: %v", got.Requests[1])
	}
}

func TestALostApprovalKeyIsReplacedOnlyWithThePassword(t *testing.T) {
	agents := newStubAgents("")
	s := New(&Deps{Agents: agents})
	body := `{"deviceId":"ownerPh1","pub":"` + base64.RawURLEncoding.EncodeToString(approvaltest.New().Pub()) + `"}`
	w := httptest.NewRecorder()
	s.handleAgentsApprovalKey(w, localRequest(http.MethodPost, "/api/agents/approval-key", body))
	if w.Code != http.StatusForbidden || len(agents.replaced) != 0 {
		t.Fatalf("without the password: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleAgentsApprovalKey(w, signedIn(localRequest(http.MethodPost, "/api/agents/approval-key", body)))
	if w.Code != http.StatusNoContent || len(agents.replaced) != 1 {
		t.Fatalf("with the password: %d %s", w.Code, w.Body.String())
	}
}

// Resetting the clock's mark ends every grant and needs the house password.
func TestTheClockResetNeedsThePassword(t *testing.T) {
	agents := newStubAgents("")
	s := New(&Deps{Agents: agents})
	w := httptest.NewRecorder()
	s.handleAgentsClockReset(w, localRequest(http.MethodPost, "/api/agents/clock-reset", ""))
	if w.Code != http.StatusForbidden || agents.resets != 0 {
		t.Fatalf("without the password: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleAgentsClockReset(w, signedIn(localRequest(http.MethodPost, "/api/agents/clock-reset", "")))
	if w.Code != http.StatusNoContent || agents.resets != 1 {
		t.Fatalf("with the password: %d %s", w.Code, w.Body.String())
	}
}

// Removing access and declining a request narrow it, so any LAN client may.
// Approving a request widens what an agent did, so it needs the password.
func TestNarrowingIsOpenOnTheLANAndApprovingIsNot(t *testing.T) {
	agents := newStubAgents("")
	s := New(&Deps{Agents: agents})

	w := httptest.NewRecorder()
	r := localRequest(http.MethodDelete, "/api/agents/agent001", "")
	r.SetPathValue("id", "agent001")
	s.handleAgentsRevoke(w, r)
	if w.Code != http.StatusNoContent || len(agents.revoked) != 1 || agents.by[0] != "lan:192.168.1.5" {
		t.Fatalf("revoke: %d %v %v", w.Code, agents.revoked, agents.by)
	}

	w = httptest.NewRecorder()
	r = localRequest(http.MethodPost, "/api/agent-requests/r1", `{"approve":false}`)
	r.SetPathValue("reqId", "r1")
	s.handleAgentsDecide(w, r)
	if w.Code != http.StatusOK || len(agents.decided) != 1 {
		t.Fatalf("decline: %d", w.Code)
	}

	// A wrong house password is refused even where none is needed.
	w = httptest.NewRecorder()
	r = localRequest(http.MethodDelete, "/api/agents/agent002", "")
	r = r.WithContext(withLANSecret(r.Context(), false))
	r.SetPathValue("id", "agent002")
	s.handleAgentsRevoke(w, r)
	if w.Code != http.StatusForbidden || len(agents.revoked) != 1 {
		t.Fatalf("revoke with a wrong password: %d %v", w.Code, agents.revoked)
	}

	w = httptest.NewRecorder()
	r = localRequest(http.MethodPost, "/api/agent-requests/r1", `{"approve":true}`)
	r.SetPathValue("reqId", "r1")
	s.handleAgentsDecide(w, r)
	if w.Code != http.StatusForbidden || len(agents.decided) != 1 {
		t.Fatalf("approve without the password: %d", w.Code)
	}

	w = httptest.NewRecorder()
	r = localRequest(http.MethodPost, "/api/agent-requests/r1", `{"approve":true}`)
	r = r.WithContext(withLANSecret(r.Context(), true))
	r.SetPathValue("reqId", "r1")
	s.handleAgentsDecide(w, r)
	if w.Code != http.StatusOK || len(agents.decided) != 2 || !agents.decided[1] {
		t.Fatalf("approve with the password: %d", w.Code)
	}
}

// Every agent route is local: the passthrough never carries one.
func TestAgentRoutesAreLocal(t *testing.T) {
	s := New(&Deps{Agents: newStubAgents("")})
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/agents"},
		{http.MethodGet, "/api/agents/audit"},
		{http.MethodPost, "/api/agents/offer"},
		{http.MethodPost, "/api/agents"},
		{http.MethodPost, "/api/agents/abc/widen"},
		{http.MethodDelete, "/api/agents/abc"},
		{http.MethodPost, "/api/agent-requests/r1"},
		{http.MethodPost, "/api/agents/approval-key"},
	} {
		if facts := s.Route(httptest.NewRequest(c.method, c.path, nil)); facts.Tier != apiauth.TierLocal {
			t.Errorf("%s %s is %q", c.method, c.path, facts.Tier)
		}
	}
}

// With the house password on, a LAN client that has not signed in reads
// neither the agents nor their audit log, and changes nothing.
func TestTheAgentRoutesNeedSignInWithLANAuthOn(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/agents"},
		{http.MethodGet, "/api/agents/audit"},
		{http.MethodPost, "/api/agents/offer"},
		{http.MethodDelete, "/api/agents/agent001"},
		{http.MethodPost, "/api/agent-requests/r1"},
	} {
		if rr := serveLANAuth(policy, lanAuthRequest(c.method, "http://ftw.local:8080"+c.path, "192.168.1.5:1234", ""), nil); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without sign-in: %d", c.method, c.path, rr.Code)
		}
	}
	if rr := serveLANAuth(policy, lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/agents", "192.168.1.5:1234", "Bearer "+testHousePassword), nil); rr.Code != http.StatusNoContent {
		t.Errorf("signed in: %d", rr.Code)
	}
}

// yearOfferBody is an offer that asks for a year, as FTW Cloud's does.
func yearOfferBody(t *testing.T, extra string) string {
	t.Helper()
	o, err := protocol.Marshal(protocol.GrantOffer{
		V: protocol.GrantOfferVersion, Key: make([]byte, 32), Role: apiauth.RoleAgent, Client: "Claude",
		Scopes: []string{protocol.ScopeLiveRead}, Ops: []string{}, TTLS: 365 * 24 * 3600, Code: "ABCD",
		ExpMs: 1_000_000 + 60_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := protocol.OfferBytes(o)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := protocol.Marshal(protocol.SignedOffer{O: o, Kid: "k1", Sig: offerSigner.Sign(msg)})
	if err != nil {
		t.Fatal(err)
	}
	return `{"offer":"` + base64.RawURLEncoding.EncodeToString(raw) + `"` + extra + `}`
}

// A grant with no length named gets 30 days of an offer's year; one past a
// year, or so long it would wrap, is refused as the box would refuse it,
// with the latest end it takes.
func TestTheBoxPageBoundsALength(t *testing.T) {
	const day = int64(24 * 3600 * 1000)
	agents := newStubAgents("")
	s := New(&Deps{Agents: agents})
	post := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := signedIn(localRequest(http.MethodPost, path, body))
		if strings.HasSuffix(path, "/widen") {
			r.SetPathValue("id", "agent001")
			s.handleAgentsWiden(w, r)
		} else {
			s.handleAgentsAdd(w, r)
		}
		return w
	}
	capped := func(w *httptest.ResponseRecorder) bool {
		var got struct {
			Error string         `json:"error"`
			Args  map[string]any `json:"args"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		return got.Error == protocol.ErrCapExceeded && got.Args["cap"] == "expiresAtMs" &&
			got.Args["max"] == float64(1_000_000+365*day)
	}

	if w := post("/api/agents", yearOfferBody(t, "")); w.Code != http.StatusCreated ||
		agents.added[0].ExpiresAtMs != 1_000_000+30*day {
		t.Fatalf("no length named: %d, ends %d", w.Code, agents.added[len(agents.added)-1].ExpiresAtMs)
	}
	if w := post("/api/agents", yearOfferBody(t, `,"ttlS":31536000`)); w.Code != http.StatusCreated ||
		agents.added[1].ExpiresAtMs != 1_000_000+365*day {
		t.Fatalf("a year: %d", w.Code)
	}
	for _, ttl := range []string{"31536001", "18446744073709552"} {
		if w := post("/api/agents/agent001/widen", `{"epoch":1,"scopes":["ftw.live.read"],"ops":[],"caps":{},"ttlS":`+ttl+`}`); !capped(w) {
			t.Errorf("renewed for %s s: %d %s", ttl, w.Code, w.Body.String())
		}
	}
	if w := post("/api/agents/agent001/widen", `{"epoch":1,"scopes":["ftw.live.read"],"ops":[],"caps":{},"ttlS":31536000}`); w.Code != http.StatusOK {
		t.Errorf("renewed for a year: %d %s", w.Code, w.Body.String())
	}
	if len(agents.added) != 2 {
		t.Errorf("a refused length reached the box: %d grants", len(agents.added))
	}
}

// The box page reads the longest grant from box.longestMs, and no stray
// top-level copy of it.
func TestTheBoxPageListCarriesNoStrayLongest(t *testing.T) {
	s := New(&Deps{Agents: newStubAgents("")})
	w := httptest.NewRecorder()
	s.handleAgentsList(w, localRequest(http.MethodGet, "/api/agents", ""))
	var got map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if _, stray := got["longestMs"]; stray {
		t.Errorf("GET /api/agents carries a top-level longestMs: %s", w.Body.String())
	}
}
