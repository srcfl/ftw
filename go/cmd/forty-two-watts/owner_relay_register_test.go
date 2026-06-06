package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// fakeLocalAPI stands in for the real api.Server reachable on 127.0.0.1:<apiPort>.
// It records the inbound X-FTW-Tunnel marker + method/path so the test can assert
// exactly what the enroll-forward host stamped, and emits a Set-Cookie so the
// stripper can be exercised.
type fakeLocalAPI struct {
	gotMarker string
	gotMethod string
	gotPath   string
	hits      int
}

func (f *fakeLocalAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits++
	f.gotMarker = r.Header.Get("X-FTW-Tunnel")
	f.gotMethod = r.Method
	f.gotPath = r.URL.Path
	// The real api handler suppresses ftw_owner on the tunneled path; emit one
	// anyway so the test proves the host-side stripper is also a backstop.
	http.SetCookie(w, &http.Cookie{Name: "ftw_owner", Value: "leak"})
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// apiPortFromServer extracts the loopback port the httptest server bound, so the
// reverse proxy (which targets 127.0.0.1:<port>) reaches it.
func apiPortFromServer(t *testing.T, ts *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return p
}

// Under multi-tenant the enroll-forward host MUST route the two enroll POSTs to
// the local API with X-FTW-Tunnel stamped (so isTunneled is true Pi-side), and
// MUST strip any Set-Cookie the API emits on the way back.
func TestEnrollForwardHost_MultiTenant_StampsMarker(t *testing.T) {
	const marker = "deadbeefmarker"
	for _, which := range []string{"start", "finish"} {
		t.Run(which, func(t *testing.T) {
			fake := &fakeLocalAPI{}
			ts := httptest.NewServer(fake)
			defer ts.Close()
			h := buildStaticAssetHandler(apiPortFromServer(t, ts), true, marker)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/owner-access/enroll/"+which+"?pin=123456&ceremony_token=tok", strings.NewReader("{}"))
			// A browser/relay-forwarded request would carry no real marker; the host
			// must stamp the trusted one itself.
			req.Header.Set("X-FTW-Tunnel", "attacker-guess")
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("enroll/%s: got %d, want 200 (body=%q)", which, rec.Code, rec.Body.String())
			}
			if fake.hits != 1 {
				t.Fatalf("enroll/%s: local API hit %d times, want 1", which, fake.hits)
			}
			if fake.gotMarker != marker {
				t.Fatalf("enroll/%s: local API saw marker %q, want %q (host must stamp the trusted marker)", which, fake.gotMarker, marker)
			}
			if fake.gotMethod != http.MethodPost {
				t.Fatalf("enroll/%s: local API saw method %q, want POST", which, fake.gotMethod)
			}
			if fake.gotPath != "/api/owner-access/enroll/"+which {
				t.Fatalf("enroll/%s: local API saw path %q", which, fake.gotPath)
			}
			if got := rec.Header().Get("Set-Cookie"); got != "" {
				t.Fatalf("enroll/%s: Set-Cookie leaked through the host: %q", which, got)
			}
		})
	}
}

// A POST to a non-enroll /api path must still be refused even under multi-tenant
// (do NOT broaden the static host beyond the two enroll routes).
func TestEnrollForwardHost_MultiTenant_NonEnrollPostRefused(t *testing.T) {
	fake := &fakeLocalAPI{}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	h := buildStaticAssetHandler(apiPortFromServer(t, ts), true, "marker")

	for _, p := range []string{
		"/api/owner-access/login/finish",
		"/api/devices",
		"/api/owner-access/enroll-pin",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, p, strings.NewReader("{}"))
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("POST %s reached the API (code 200) — only the two enroll routes may forward", p)
		}
		if fake.hits != 0 {
			t.Fatalf("POST %s leaked to the local API", p)
		}
	}
}

// Single-tenant (multiTenant=false) must be byte-identical to today: every POST
// is 405, including the enroll routes — no enroll-forward host exists.
func TestEnrollForwardHost_SingleTenant_EnrollPostStill405(t *testing.T) {
	fake := &fakeLocalAPI{}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	h := buildStaticAssetHandler(apiPortFromServer(t, ts), false, "marker")

	for _, which := range []string{"start", "finish"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/owner-access/enroll/"+which, strings.NewReader("{}"))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("single-tenant POST enroll/%s: got %d, want 405", which, rec.Code)
		}
		if fake.hits != 0 {
			t.Fatalf("single-tenant POST enroll/%s leaked to the local API", which)
		}
	}
}

// GET static assets are still served (and any owner cookie stripped) regardless
// of multi-tenant mode — the static surface must not regress.
func TestEnrollForwardHost_StaticGetStillServed(t *testing.T) {
	for _, mt := range []bool{false, true} {
		fake := &fakeLocalAPI{}
		ts := httptest.NewServer(fake)
		defer ts.Close()
		h := buildStaticAssetHandler(apiPortFromServer(t, ts), mt, "marker")

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/login.html", nil)
		req.Header.Set("Cookie", "ftw_owner=leak")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("multiTenant=%v GET /login.html: got %d, want 200", mt, rec.Code)
		}
		if fake.gotMarker != "" {
			t.Fatalf("multiTenant=%v GET static must NOT be stamped with the tunnel marker, saw %q", mt, fake.gotMarker)
		}
		if got := rec.Header().Get("Set-Cookie"); got != "" {
			t.Fatalf("multiTenant=%v Set-Cookie leaked on static GET: %q", mt, got)
		}
	}
}
