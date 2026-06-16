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

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// HTTP allowlist tests for the Lua host. Port-aware semantics + scheme
// gating are the SSRF mitigation that gates every Lua-side network
// call; if hostAllowed silently regresses, drivers can probe arbitrary
// hosts and ports on the LAN.

func runHTTPTestDriver(t *testing.T, allowed []string, targetURL string) (gotBody bool, errMsg string) {
	t.Helper()
	tel := telemetry.NewStore()
	env := NewHostEnv("httptest", tel).WithHTTP()
	if allowed != nil {
		env.WithHTTPAllowedHosts(allowed)
	}
	src := `
		function driver_init() end
		function driver_poll()
			local body, err = host.http_get("` + targetURL + `")
			if err then
				host.emit_metric("result_err", 1)
				host.log("info", "ERR:" .. tostring(err))
			else
				host.emit_metric("result_ok", 1)
				host.log("info", "BODY:" .. tostring(body))
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
	if v, _, ok := tel.LatestMetric("httptest", "result_ok"); ok && v == 1 {
		return true, ""
	}
	if v, _, ok := tel.LatestMetric("httptest", "result_err"); ok && v == 1 {
		return false, "errored"
	}
	return false, "neither metric set — driver did not run"
}

func TestLuaHTTPAllowlistEnforcement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	srvHost := strings.TrimPrefix(srv.URL, "http://")

	cases := []struct {
		name     string
		allowed  []string
		url      string
		wantBody bool
	}{
		{"empty allowlist permits all (scheme ok)", nil, srv.URL, true},
		{"host-only entry permits this port", []string{strings.Split(srvHost, ":")[0]}, srv.URL + "/path", true},
		{"host:port match exact", []string{srvHost}, srv.URL, true},
		{"host:port mismatch on port", []string{strings.Split(srvHost, ":")[0] + ":1"}, srv.URL, false},
		{"different host blocked", []string{"10.99.99.99"}, srv.URL, false},
		// file:// would otherwise hit /etc/passwd through stdlib client.
		{"file scheme rejected", []string{srvHost}, "file:///etc/passwd", false},
		{"ftp scheme rejected", []string{srvHost}, "ftp://" + srvHost + "/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, errMsg := runHTTPTestDriver(t, tc.allowed, tc.url)
			if got != tc.wantBody {
				t.Errorf("wantBody=%v, got=%v (err=%s)", tc.wantBody, got, errMsg)
			}
		})
	}
}

func TestLuaHTTPCapabilityNotGranted(t *testing.T) {
	tel := telemetry.NewStore()
	env := NewHostEnv("nohttp", tel) // NO WithHTTP()
	src := `
		function driver_init() end
		function driver_poll()
			local _, err = host.http_get("http://example.com/")
			if err then host.emit_metric("blocked", 1) end
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
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	_ = d.Init(context.Background(), nil)
	_, _ = d.Poll(context.Background())
	v, _, ok := tel.LatestMetric("nohttp", "blocked")
	if !ok || v != 1 {
		t.Error("driver without HTTP cap should have been blocked")
	}
}

func TestSplitHostPortLowerHandlesIPv6(t *testing.T) {
	cases := []struct {
		in      string
		host    string
		port    string
		hasPort bool
	}{
		{"192.168.1.50", "192.168.1.50", "", false},
		{"192.168.1.50:8080", "192.168.1.50", "8080", true},
		{"[::1]:8080", "::1", "8080", true},
		{"[::1]", "::1", "", false},
		{"fe80::1", "fe80::1", "", false},
		{"HOST:80", "host", "80", true},
		{"", "", "", false},
		{"foo:", "foo:", "", false},
	}
	for _, tc := range cases {
		h, p, hp := splitHostPortLower(tc.in)
		if h != tc.host || p != tc.port || hp != tc.hasPort {
			t.Errorf("splitHostPortLower(%q) = (%s,%s,%v), want (%s,%s,%v)",
				tc.in, h, p, hp, tc.host, tc.port, tc.hasPort)
		}
	}
}

func TestHTTPTestRigSelfCheck(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	got, _ := runHTTPTestDriver(t, nil, srv.URL)
	if !got {
		t.Fatal("rig sanity: empty allowlist + reachable server must succeed")
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("rig sanity: server saw %d hits, want 1", atomic.LoadInt32(&hits))
	}
}

// ---- http_patch tests ----

// runPATCHTestDriver spins up a Lua driver that calls host.http_patch and
// records whether the request succeeded. The test server captures the
// method and body so callers can assert on them.
func runPATCHTestDriver(t *testing.T, allowed []string, targetURL, patchBody string) (gotBody bool, errMsg string) {
	t.Helper()
	tel := telemetry.NewStore()
	env := NewHostEnv("patchtest", tel).WithHTTP()
	if allowed != nil {
		env.WithHTTPAllowedHosts(allowed)
	}
	src := `
		function driver_init() end
		function driver_poll()
			local body, err = host.http_patch("` + targetURL + `", [[` + patchBody + `]])
			if err then
				host.emit_metric("result_err", 1)
				host.log("info", "ERR:" .. tostring(err))
			else
				host.emit_metric("result_ok", 1)
				host.log("info", "BODY:" .. tostring(body))
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
	if v, _, ok := tel.LatestMetric("patchtest", "result_ok"); ok && v == 1 {
		return true, ""
	}
	if v, _, ok := tel.LatestMetric("patchtest", "result_err"); ok && v == 1 {
		return false, "errored"
	}
	return false, "neither metric set — driver did not run"
}

// TestLuaHTTPPatchMethod verifies that host.http_patch sends a PATCH request
// with the correct method and body, and that the allowlist is enforced the
// same way as for http_get / http_post.
func TestLuaHTTPPatchMethod(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	srvHost := strings.TrimPrefix(srv.URL, "http://")

	got, errMsg := runPATCHTestDriver(t, nil, srv.URL, `{"value":"42"}`)
	if !got {
		t.Fatalf("expected success, got error: %s", errMsg)
	}
	if gotMethod != "PATCH" {
		t.Errorf("server saw method %q, want PATCH", gotMethod)
	}
	if gotBody != `{"value":"42"}` {
		t.Errorf("server saw body %q, want {\"value\":\"42\"}", gotBody)
	}
	_ = srvHost
}

// TestLuaHTTPPatchContentType verifies that the Content-Type header is set
// to application/json by default, matching http_post behaviour.
func TestLuaHTTPPatchContentType(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	got, errMsg := runPATCHTestDriver(t, nil, srv.URL, `{}`)
	if !got {
		t.Fatalf("expected success: %s", errMsg)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
}

// TestLuaHTTPPatchAllowlistEnforcement verifies that the same host allowlist
// that gates http_get also blocks http_patch to non-allowlisted targets.
func TestLuaHTTPPatchAllowlistEnforcement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	srvHost := strings.TrimPrefix(srv.URL, "http://")

	cases := []struct {
		name     string
		allowed  []string
		wantBody bool
	}{
		{"empty allowlist permits all", nil, true},
		{"exact host:port match", []string{srvHost}, true},
		{"wrong port blocked", []string{strings.Split(srvHost, ":")[0] + ":1"}, false},
		{"different host blocked", []string{"10.99.99.99"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, errMsg := runPATCHTestDriver(t, tc.allowed, srv.URL, `{}`)
			if got != tc.wantBody {
				t.Errorf("wantBody=%v got=%v (err=%s)", tc.wantBody, got, errMsg)
			}
		})
	}
}

// TestLuaHTTPPatchCapabilityNotGranted verifies that a driver without the
// HTTP capability receives an error rather than a successful PATCH.
func TestLuaHTTPPatchCapabilityNotGranted(t *testing.T) {
	tel := telemetry.NewStore()
	env := NewHostEnv("nohttppatch", tel) // NO WithHTTP()
	src := `
		function driver_init() end
		function driver_poll()
			local _, err = host.http_patch("http://example.com/", "{}")
			if err then host.emit_metric("blocked", 1) end
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
		t.Fatalf("load: %v", err)
	}
	defer d.Cleanup()
	_ = d.Init(context.Background(), nil)
	_, _ = d.Poll(context.Background())
	v, _, ok := tel.LatestMetric("nohttppatch", "blocked")
	if !ok || v != 1 {
		t.Error("driver without HTTP cap should have been blocked by http_patch")
	}
}

// TestLuaHTTPPatch4xxReturnsError verifies that a 4xx response is surfaced
// as an error return (nil, errstring) rather than a successful body — same
// behaviour as http_post and http_get.
func TestLuaHTTPPatch4xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"bad value"}`))
	}))
	defer srv.Close()

	got, _ := runPATCHTestDriver(t, nil, srv.URL, `{}`)
	if got {
		t.Error("4xx response should be returned as error, not success")
	}
}
