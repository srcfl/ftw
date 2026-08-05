package drivers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// host.http_patch is the first mutating HTTP verb exposed to Lua, added for
// the NIBE Solar PV feed (issue #537). It is gated twice: the plain http
// capability AND capabilities.http.allow_write. These tests pin both gates
// and the wire format, because a silent regression here would let any HTTP
// driver mutate device state with only a read grant.

// runPatchDriver runs a one-shot driver that PATCHes targetURL with a fixed
// JSON body and reports the outcome through metrics, mirroring the style of
// runHTTPDriverWithEnv. The specific denial gate is surfaced as a metric so
// tests can assert WHICH gate fired, not just that something failed.
func runPatchDriver(t *testing.T, env *HostEnv, targetURL string) (ok bool, deniedBy string) {
	t.Helper()
	src := `
		function driver_init() end
		function driver_poll()
			local body, err = host.http_patch("` + targetURL + `",
				'[{"variableId":5202,"integerValue":3000}]',
				{ Authorization = "Basic dTpw" })
			if err then
				host.emit_metric("patch_err", 1)
				local e = tostring(err)
				if string.find(e, "capability not granted", 1, true) then
					host.emit_metric("denied_http_cap", 1)
				elseif string.find(e, "write not granted", 1, true) then
					host.emit_metric("denied_write_cap", 1)
				elseif string.find(e, "redirect", 1, true) then
					host.emit_metric("denied_redirect", 1)
				end
				host.log("info", "ERR:" .. e)
			else
				host.emit_metric("patch_ok", 1)
			end
			return 60000
		end
		function driver_command() end
		function driver_default_mode() end
		function driver_cleanup() end
	`
	path := filepath.Join(t.TempDir(), "drv.lua")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	d, err := NewLuaDriver(path, env)
	if err != nil {
		t.Fatalf("load driver: %v", err)
	}
	defer d.Cleanup()
	if err := d.Init(context.Background(), nil); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := d.Poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if v, _, found := env.Telemetry.LatestMetric(env.DriverName, "patch_ok"); found && v == 1 {
		return true, ""
	}
	for _, gate := range []string{"denied_http_cap", "denied_write_cap", "denied_redirect"} {
		if v, _, found := env.Telemetry.LatestMetric(env.DriverName, gate); found && v == 1 {
			return false, gate
		}
	}
	return false, "other"
}

func TestLuaHTTPPatchSendsMethodBodyAndHeaders(t *testing.T) {
	var gotMethod, gotBody, gotCT, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[{"status":"modified"}]`))
	}))
	defer srv.Close()

	env := NewHostEnv("patcher", telemetry.NewStore()).WithHTTP().WithHTTPAllowWrite()
	ok, errText := runPatchDriver(t, env, srv.URL+"/api/v1/devices/1/points")
	if !ok {
		t.Fatalf("patch failed: %s", errText)
	}
	if gotMethod != "PATCH" {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotBody != `[{"variableId":5202,"integerValue":3000}]` {
		t.Errorf("body = %q", gotBody)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotAuth != "Basic dTpw" {
		t.Errorf("authorization header not forwarded, got %q", gotAuth)
	}
}

func TestLuaHTTPPatchRequiresAllowWrite(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	// http granted, allow_write NOT granted: the request must never leave.
	env := NewHostEnv("readonly", telemetry.NewStore()).WithHTTP()
	ok, deniedBy := runPatchDriver(t, env, srv.URL)
	if ok {
		t.Fatal("http_patch succeeded without capabilities.http.allow_write")
	}
	if deniedBy != "denied_write_cap" {
		t.Errorf("denied by %q, want the allow_write gate", deniedBy)
	}
	if hits.Load() != 0 {
		t.Fatalf("server was reached %d times despite missing allow_write", hits.Load())
	}
}

func TestLuaHTTPPatchWithoutHTTPCapability(t *testing.T) {
	// allow_write alone must not open the door either — both grants are
	// needed, and the request must never leave the host (the hit counter is
	// what makes this test able to catch a deleted gate; a dead port alone
	// would pass on connection-refused).
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	env := NewHostEnv("nohttp", telemetry.NewStore()).WithHTTPAllowWrite()
	ok, deniedBy := runPatchDriver(t, env, srv.URL)
	if ok {
		t.Fatal("http_patch succeeded without the http capability")
	}
	if deniedBy != "denied_http_cap" {
		t.Errorf("denied by %q, want the http capability gate", deniedBy)
	}
	if hits.Load() != 0 {
		t.Fatalf("server was reached %d times despite missing http capability", hits.Load())
	}
}

func TestLuaHTTPPatchRefusesRedirects(t *testing.T) {
	// Go's HTTP client re-issues most redirected writes as body-less GETs,
	// which would make http_patch report success for a write that never
	// reached the device. A redirected PATCH must surface as an error.
	var followUps atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followUps.Add(1)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	env := NewHostEnv("redirected", telemetry.NewStore()).WithHTTP().WithHTTPAllowWrite()
	ok, deniedBy := runPatchDriver(t, env, redirector.URL)
	if ok {
		t.Fatal("a redirected PATCH must not report success")
	}
	if deniedBy != "denied_redirect" {
		t.Errorf("denied by %q, want the redirect refusal", deniedBy)
	}
	if followUps.Load() != 0 {
		t.Fatalf("redirect target was reached %d times — the write was converted to a GET", followUps.Load())
	}
}

func TestLuaHTTPPatchRespectsAllowlist(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	srvHost := strings.TrimPrefix(srv.URL, "http://")

	env := NewHostEnv("fenced", telemetry.NewStore()).WithHTTP().WithHTTPAllowWrite().
		WithHTTPAllowedHosts([]string{"10.99.99.99"})
	ok, _ := runPatchDriver(t, env, srv.URL)
	if ok {
		t.Fatal("http_patch escaped the allowed_hosts fence")
	}
	if hits.Load() != 0 {
		t.Fatalf("server was reached despite the allowlist (host %s)", srvHost)
	}
}
