package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/cloudlink"
	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

type stubCloud struct {
	granted []cloudlink.GrantRequest
	revoked []string
	stats   []cloudlink.StatsRequest
	stopped []string
	err     error
}

func (c *stubCloud) Status() cloudlink.Status   { return cloudlink.Status{State: "off"} }
func (c *stubCloud) Preview() cloudlink.Preview { return cloudlink.Preview{State: "off", Sample: true} }
func (c *stubCloud) Grant(req cloudlink.GrantRequest) error {
	c.granted = append(c.granted, req)
	return c.err
}
func (c *stubCloud) End(by string) error { return c.Revoke(by) }

func (c *stubCloud) Revoke(by string) error {
	c.revoked = append(c.revoked, by)
	return nil
}
func (c *stubCloud) Stats(req cloudlink.StatsRequest) error {
	c.stats = append(c.stats, req)
	return c.err
}
func (c *stubCloud) StopStats(by string) error {
	c.stopped = append(c.stopped, by)
	return nil
}

func viewerRequest(method, target, body string) *http.Request {
	r := sessionRequest(method, target, body)
	caller, _ := apiauth.FromRequest(r)
	caller.Role = apiauth.RoleViewer
	caller.Scopes = apiauth.NewScopeSet(apiauth.RoleScopes[apiauth.RoleViewer]...)
	return r.WithContext(apiauth.WithCaller(r.Context(), caller))
}

func loopbackRequest(method, target, body string) *http.Request {
	r := localRequest(method, target, body)
	r.Host = "localhost:8080"
	r.RemoteAddr = "127.0.0.1:5555"
	return r
}

const grantBody = `{"scopes":["ftw.live.read"],"policy":"2026-10"}`

// Turning FTW Cloud on follows the box's owner rules. With the optional house
// password off, a direct private LAN client may consent, as it may save settings.
func TestWhoMayTurnFTWCloudOn(t *testing.T) {
	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"owner over a session", sessionRequest(http.MethodPost, "/api/cloud/grant", grantBody), http.StatusOK},
		{"viewer over a session", viewerRequest(http.MethodPost, "/api/cloud/grant", grantBody), http.StatusForbidden},
		{"the box itself", loopbackRequest(http.MethodPost, "/api/cloud/grant", grantBody), http.StatusOK},
		{"a LAN client", localRequest(http.MethodPost, "/api/cloud/grant", grantBody), http.StatusOK},
		{"a LAN client with the house password", func() *http.Request {
			r := localRequest(http.MethodPost, "/api/cloud/grant", grantBody)
			return r.WithContext(withLANSecret(r.Context(), true))
		}(), http.StatusOK},
		{"through a proxy", func() *http.Request {
			r := loopbackRequest(http.MethodPost, "/api/cloud/grant", grantBody)
			r.Header.Set("X-Forwarded-For", "203.0.113.9")
			return r
		}(), http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cloud := &stubCloud{}
			s := New(&Deps{Cloud: cloud})
			w := httptest.NewRecorder()
			s.handleCloudGrant(w, c.req)
			if w.Code != c.want {
				t.Fatalf("got %d, want %d: %s", w.Code, c.want, w.Body.String())
			}
			if granted := len(cloud.granted) == 1; granted != (c.want == http.StatusOK) {
				t.Fatalf("granted = %v with status %d", granted, w.Code)
			}
		})
	}
}

func TestCloudConsentUsesTheConfiguredLANProtection(t *testing.T) {
	for _, choice := range []struct{ path, body string }{
		{"/api/cloud/grant", grantBody},
		{"/api/cloud/stats", statsBody},
	} {
		t.Run(choice.path, func(t *testing.T) {
			resetLANGuesses(t)
			s := newLANAuthServer(t)
			cloud := &stubCloud{}
			s.deps.Cloud = cloud
			attempt := func(req *http.Request, want, calls int) {
				t.Helper()
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, req)
				if w.Code != want {
					t.Fatalf("got %d, want %d: %s", w.Code, want, w.Body.String())
				}
				if got := len(cloud.granted) + len(cloud.stats); got != calls {
					t.Fatalf("Cloud writes = %d, want %d", got, calls)
				}
			}
			attempt(localRequest(http.MethodPost, choice.path, choice.body), http.StatusOK, 1)
			enableStoredLANAuth(t, s)
			attempt(localRequest(http.MethodPost, choice.path, choice.body), http.StatusUnauthorized, 1)

			login := postLANAuthJSON(s, http.MethodPost, "http://ftw.local:8080/api/auth/login", "192.168.1.10:43210",
				`{"password":"`+testHousePassword+`"}`)
			if login.Code != http.StatusOK {
				t.Fatalf("login = %d: %s", login.Code, login.Body.String())
			}
			req := localRequest(http.MethodPost, choice.path, choice.body)
			req.AddCookie(&http.Cookie{Name: lanSessionCookieName, Value: lanSessionFromRecorder(t, login)})
			attempt(req, http.StatusOK, 2)

			req = localRequest(http.MethodPost, choice.path, choice.body)
			req.Header.Set("Authorization", "Bearer "+testHousePassword)
			attempt(req, http.StatusOK, 3)
			req = localRequest(http.MethodPost, choice.path, choice.body)
			req.Header.Set("Authorization", "Bearer wrong-password")
			attempt(req, http.StatusUnauthorized, 3)
		})
	}
}

func TestCloudConsentWithoutAPasswordStaysOnThePrivateNetwork(t *testing.T) {
	for _, choice := range []struct{ path, body string }{
		{"/api/cloud/grant", grantBody},
		{"/api/cloud/stats", statsBody},
	} {
		t.Run(choice.path, func(t *testing.T) {
			for _, tc := range []struct {
				name, host, remote, header, value string
				want                              int
			}{
				{"private IPv4", "ftw.local:8080", "192.168.1.10:1234", "", "", http.StatusOK},
				{"private IPv6", "[fd00::1]:8080", "[fd00::2]:1234", "", "", http.StatusOK},
				{"CGNAT", "ftw.local:8080", "100.64.0.10:1234", "", "", http.StatusForbidden},
				{"public client", "ftw.local:8080", "203.0.113.9:1234", "", "", http.StatusForbidden},
				{"public host", "box.example.com", "192.168.1.10:1234", "", "", http.StatusForbidden},
				{"forwarded LAN", "ftw.local:8080", "192.168.1.10:1234", "X-Forwarded-For", "192.168.1.20", http.StatusForbidden},
				{"loopback proxy", "localhost:8080", "127.0.0.1:1234", "Forwarded", "for=203.0.113.9", http.StatusForbidden},
				{"cross-site origin", "ftw.local:8080", "192.168.1.10:1234", "Origin", "https://other.example", http.StatusForbidden},
				{"cross-site fetch", "ftw.local:8080", "192.168.1.10:1234", "Sec-Fetch-Site", "cross-site", http.StatusForbidden},
				{"non-JSON body", "ftw.local:8080", "192.168.1.10:1234", "Content-Type", "text/plain", http.StatusUnsupportedMediaType},
			} {
				t.Run(tc.name, func(t *testing.T) {
					cloud := &stubCloud{}
					s := New(&Deps{Cloud: cloud})
					req := localRequest(http.MethodPost, choice.path, choice.body)
					req.Host, req.RemoteAddr = tc.host, tc.remote
					req.Header.Set("Content-Type", "application/json")
					if tc.header != "" {
						req.Header.Set(tc.header, tc.value)
					}
					w := httptest.NewRecorder()
					s.Handler().ServeHTTP(w, req)
					if w.Code != tc.want {
						t.Fatalf("got %d, want %d: %s", w.Code, tc.want, w.Body.String())
					}
					if called := len(cloud.granted)+len(cloud.stats) == 1; called != (tc.want == http.StatusOK) {
						t.Fatalf("Cloud write = %v with status %d", called, w.Code)
					}
				})
			}
		})
	}
}

// The audit line names the door, never anything the caller sent.
func TestTheGrantRecordsWhichDoorAsked(t *testing.T) {
	cloud := &stubCloud{}
	s := New(&Deps{Cloud: cloud})
	s.handleCloudGrant(httptest.NewRecorder(), sessionRequest(http.MethodPost, "/api/cloud/grant",
		`{"scopes":["ftw.live.read"],"policy":"p","By":"someone else"}`))
	s.handleCloudGrant(httptest.NewRecorder(), loopbackRequest(http.MethodPost, "/api/cloud/grant", grantBody))
	if len(cloud.granted) != 2 || cloud.granted[0].By != "app:aaaa1111" || cloud.granted[1].By != "box" {
		t.Fatalf("granted %+v", cloud.granted)
	}
}

// Turning it off narrows access, so the LAN may, without the password.
func TestAnyDoorMayTurnFTWCloudOff(t *testing.T) {
	for name, req := range map[string]*http.Request{
		"owner over a session": sessionRequest(http.MethodDelete, "/api/cloud/grant", ""),
		"a LAN client":         localRequest(http.MethodDelete, "/api/cloud/grant", ""),
	} {
		cloud := &stubCloud{}
		s := New(&Deps{Cloud: cloud})
		w := httptest.NewRecorder()
		s.handleCloudRevoke(w, req)
		if w.Code != http.StatusOK || len(cloud.revoked) != 1 {
			t.Fatalf("%s: %d, revoked %v", name, w.Code, cloud.revoked)
		}
	}
}

// Only an owner reads what leaves the house; a viewer sees no grant screen.
func TestOnlyAnOwnerReadsTheCloudStatusAndPreview(t *testing.T) {
	s := New(&Deps{Cloud: &stubCloud{}})
	for _, target := range []string{"/api/cloud", "/api/cloud/preview"} {
		handler := s.handleCloudStatus
		if target == "/api/cloud/preview" {
			handler = s.handleCloudPreview
		}
		w := httptest.NewRecorder()
		handler(w, sessionRequest(http.MethodGet, target, ""))
		if w.Code != http.StatusOK {
			t.Fatalf("owner %s: %d", target, w.Code)
		}
		w = httptest.NewRecorder()
		handler(w, viewerRequest(http.MethodGet, target, ""))
		if w.Code != http.StatusForbidden {
			t.Fatalf("viewer %s: %d", target, w.Code)
		}
	}
}

func TestCloudRoutesSayWhenThereIsNoCloud(t *testing.T) {
	s := New(&Deps{})
	w := httptest.NewRecorder()
	s.handleCloudStatus(w, sessionRequest(http.MethodGet, "/api/cloud", ""))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", w.Code)
	}
}

func TestCloudGrantRefusalsKeepTheirMeaning(t *testing.T) {
	for err, want := range map[error]int{
		cloudlink.ErrGrantRequest: http.StatusBadRequest,
		cloudlink.ErrPurgePending: http.StatusConflict,
		cloudlink.ErrLinkEnding:   http.StatusConflict,
		cloudlink.ErrNoService:    http.StatusServiceUnavailable,
	} {
		s := New(&Deps{Cloud: &stubCloud{err: err}})
		w := httptest.NewRecorder()
		s.handleCloudGrant(w, sessionRequest(http.MethodPost, "/api/cloud/grant", grantBody))
		if w.Code != want {
			t.Errorf("%v: got %d, want %d", err, w.Code, want)
		}
	}
}

// Through the router, so the tiers are the ones the passthrough reads.
func TestCloudRoutesAreRegisteredWithTheirTiers(t *testing.T) {
	s := New(&Deps{Cloud: &stubCloud{}})
	for _, c := range []struct {
		method, path string
		tier         apiauth.Tier
		noStepUp     bool
	}{
		{http.MethodGet, "/api/cloud", apiauth.TierRead, false},
		{http.MethodGet, "/api/cloud/preview", apiauth.TierRead, false},
		{http.MethodPost, "/api/cloud/grant", apiauth.TierConfigure, true},
		{http.MethodDelete, "/api/cloud/grant", apiauth.TierConfigure, true},
		{http.MethodPost, "/api/cloud/stats", apiauth.TierConfigure, true},
		{http.MethodDelete, "/api/cloud/stats", apiauth.TierConfigure, true},
	} {
		facts := s.Route(httptest.NewRequest(c.method, c.path, nil))
		if facts.Tier != c.tier || facts.NoStepUp != c.noStepUp {
			t.Errorf("%s %s: %+v", c.method, c.path, facts)
		}
	}
}

const statsBody = `{"policy":"stats-2026-10"}`

// Statistics are a separate consent behind the grant's own door.
func TestWhoMayTurnStatisticsOn(t *testing.T) {
	cases := []struct {
		name string
		req  *http.Request
		want int
	}{
		{"owner over a session", sessionRequest(http.MethodPost, "/api/cloud/stats", statsBody), http.StatusOK},
		{"viewer over a session", viewerRequest(http.MethodPost, "/api/cloud/stats", statsBody), http.StatusForbidden},
		{"the box itself", loopbackRequest(http.MethodPost, "/api/cloud/stats", statsBody), http.StatusOK},
		{"a LAN client", localRequest(http.MethodPost, "/api/cloud/stats", statsBody), http.StatusOK},
		{"a LAN client with the house password", func() *http.Request {
			r := localRequest(http.MethodPost, "/api/cloud/stats", statsBody)
			return r.WithContext(withLANSecret(r.Context(), true))
		}(), http.StatusOK},
		{"through a proxy", func() *http.Request {
			r := loopbackRequest(http.MethodPost, "/api/cloud/stats", statsBody)
			r.Header.Set("X-Forwarded-For", "203.0.113.9")
			return r
		}(), http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cloud := &stubCloud{}
			s := New(&Deps{Cloud: cloud})
			w := httptest.NewRecorder()
			s.handleCloudStats(w, c.req)
			if w.Code != c.want {
				t.Fatalf("got %d, want %d: %s", w.Code, c.want, w.Body.String())
			}
			if asked := len(cloud.stats) == 1; asked != (c.want == http.StatusOK) {
				t.Fatalf("stats asked = %v with status %d", asked, w.Code)
			}
		})
	}
}

func TestStatisticsRecordWhichDoorAskedAndThePolicy(t *testing.T) {
	cloud := &stubCloud{}
	s := New(&Deps{Cloud: cloud})
	s.handleCloudStats(httptest.NewRecorder(), sessionRequest(http.MethodPost, "/api/cloud/stats",
		`{"policy":"stats-2026-10","By":"someone else"}`))
	if len(cloud.stats) != 1 || cloud.stats[0].By != "app:aaaa1111" || cloud.stats[0].Policy != "stats-2026-10" {
		t.Fatalf("stats %+v", cloud.stats)
	}
	w := httptest.NewRecorder()
	s.handleCloudStats(w, sessionRequest(http.MethodPost, "/api/cloud/stats", "not json"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a body that is not JSON: %d", w.Code)
	}
}

func TestAnyDoorMayTurnStatisticsOff(t *testing.T) {
	for name, req := range map[string]*http.Request{
		"owner over a session": sessionRequest(http.MethodDelete, "/api/cloud/stats", ""),
		"a LAN client":         localRequest(http.MethodDelete, "/api/cloud/stats", ""),
	} {
		cloud := &stubCloud{}
		s := New(&Deps{Cloud: cloud})
		w := httptest.NewRecorder()
		s.handleCloudStopStats(w, req)
		if w.Code != http.StatusOK || len(cloud.stopped) != 1 {
			t.Fatalf("%s: %d, stopped %v", name, w.Code, cloud.stopped)
		}
	}
}

// A viewer's session sees no FTW Cloud screen and changes nothing in it, even
// where turning something off would narrow access.
func TestAViewerReachesNoCloudRoute(t *testing.T) {
	cloud := &stubCloud{}
	s := New(&Deps{Cloud: cloud})
	for _, c := range []struct {
		method, target, body string
		handler              http.HandlerFunc
	}{
		{http.MethodGet, "/api/cloud", "", s.handleCloudStatus},
		{http.MethodGet, "/api/cloud/preview", "", s.handleCloudPreview},
		{http.MethodPost, "/api/cloud/grant", grantBody, s.handleCloudGrant},
		{http.MethodDelete, "/api/cloud/grant", "", s.handleCloudRevoke},
		{http.MethodDelete, "/api/cloud", "", s.handleCloudEnd},
		{http.MethodPost, "/api/cloud/stats", statsBody, s.handleCloudStats},
		{http.MethodDelete, "/api/cloud/stats", "", s.handleCloudStopStats},
	} {
		w := httptest.NewRecorder()
		c.handler(w, viewerRequest(c.method, c.target, c.body))
		if w.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s: %d", c.method, c.target, w.Code)
		}
	}
	if len(cloud.granted)+len(cloud.revoked)+len(cloud.stats)+len(cloud.stopped) != 0 {
		t.Fatalf("a viewer changed FTW Cloud: %+v", cloud)
	}
}

func TestStatisticsRefusalsKeepTheirMeaning(t *testing.T) {
	for err, want := range map[error]int{
		cloudlink.ErrGrantRequest: http.StatusBadRequest,
		cloudlink.ErrNotGranted:   http.StatusConflict,
		cloudlink.ErrPurgePending: http.StatusConflict,
		cloudlink.ErrNoService:    http.StatusServiceUnavailable,
	} {
		s := New(&Deps{Cloud: &stubCloud{err: err}})
		w := httptest.NewRecorder()
		s.handleCloudStats(w, sessionRequest(http.MethodPost, "/api/cloud/stats", statsBody))
		if w.Code != want {
			t.Errorf("%v: got %d, want %d", err, w.Code, want)
		}
	}
}

// The consent screen's whole round trip on a real link: on, statistics on,
// GET shows both purposes, statistics off, FTW Cloud off.
func TestTheConsentRoundTripShowsThePurposes(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	link, err := cloudlink.New(cloudlink.Options{
		Site: cloudSiteStub{}, Roster: cloudRosterStub{}, KV: &cloudKV{m: map[string]string{}},
		KeyPath: filepath.Join(t.TempDir(), "cloud.key"), ServiceKeys: []*ecdsa.PublicKey{&key.PublicKey},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(&Deps{Cloud: link})
	call := func(h http.HandlerFunc, method, target, body string, want int) cloudlink.Status {
		t.Helper()
		w := httptest.NewRecorder()
		h(w, sessionRequest(method, target, body))
		if w.Code != want {
			t.Fatalf("%s %s: %d, want %d: %s", method, target, w.Code, want, w.Body.String())
		}
		var st cloudlink.Status
		_ = json.Unmarshal(w.Body.Bytes(), &st)
		return st
	}

	current := `{"policy":"` + cloudwire.StatsPolicy + `"}`
	call(s.handleCloudStats, http.MethodPost, "/api/cloud/stats", statsBody, http.StatusConflict)
	call(s.handleCloudGrant, http.MethodPost, "/api/cloud/grant", grantBody, http.StatusOK)
	// Older terms did not include statistics: the current ones come first.
	if st := call(s.handleCloudStatus, http.MethodGet, "/api/cloud", "", http.StatusOK); !st.StatisticsNeedTerms || st.Statistics {
		t.Fatalf("status under older terms: %+v", st)
	}
	call(s.handleCloudStats, http.MethodPost, "/api/cloud/stats", current, http.StatusConflict)
	call(s.handleCloudGrant, http.MethodPost, "/api/cloud/grant", `{"scopes":["ftw.live.read"],"policy":"`+cloudwire.CloudTermsPolicy+`"}`, http.StatusOK)
	if st := call(s.handleCloudStatus, http.MethodGet, "/api/cloud", "", http.StatusOK); st.State != "on" ||
		strings.Join(st.Purposes, ",") != "serve,stats" || !st.Statistics || !st.StatisticsRequired {
		t.Fatalf("status under the current terms: %+v", st)
	}
	call(s.handleCloudStopStats, http.MethodDelete, "/api/cloud/stats", "", http.StatusConflict)
	if st := call(s.handleCloudRevoke, http.MethodDelete, "/api/cloud/grant", "", http.StatusOK); st.State != "revoking" {
		t.Fatalf("status after FTW Cloud off: %+v", st)
	}
	// While the copy is deleted, the box's own answer can still change.
	call(s.handleCloudStats, http.MethodPost, "/api/cloud/stats", statsBody, http.StatusConflict)
	if st := call(s.handleCloudStats, http.MethodPost, "/api/cloud/stats", current, http.StatusOK); !st.Statistics {
		t.Fatalf("status after the local yes: %+v", st)
	}
}

type cloudSiteStub struct{}

func (cloudSiteStub) Live() cloudlink.Live             { return cloudlink.Live{} }
func (cloudSiteStub) Loadpoints() (cloudwire.LP, bool) { return cloudwire.LP{}, false }
func (cloudSiteStub) Prices() (cloudwire.Prices, bool) { return cloudwire.Prices{}, false }

type cloudRosterStub struct{}

func (cloudRosterStub) Devices() []cloudlink.RosterDevice { return nil }

func (cloudRosterStub) Pending() ([]byte, time.Time, bool) { return nil, time.Time{}, false }
func (cloudRosterStub) Noise() []byte                      { return make([]byte, 32) }

type cloudKV struct {
	mu sync.Mutex
	m  map[string]string
}

func (k *cloudKV) LoadConfig(key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok
}

func (k *cloudKV) SaveConfig(key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.m[key] = value
	return nil
}

func TestWholeCloudOffUsesTheExistingNarrowingAccessRule(t *testing.T) {
	for _, req := range []*http.Request{sessionRequest(http.MethodDelete, "/api/cloud", ""), localRequest(http.MethodDelete, "/api/cloud", "")} {
		c := &stubCloud{}
		s := New(&Deps{Cloud: c})
		w := httptest.NewRecorder()
		s.handleCloudEnd(w, req)
		if w.Code != http.StatusOK || len(c.revoked) != 1 {
			t.Fatal("whole Cloud off failed", w.Code)
		}
	}
}
