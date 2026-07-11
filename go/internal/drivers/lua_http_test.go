package drivers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	env := NewHostEnv("httptest", telemetry.NewStore()).WithHTTP()
	if allowed != nil {
		env.WithHTTPAllowedHosts(allowed)
	}
	return runHTTPDriverWithEnv(t, env, targetURL)
}

// runHTTPDriverWithEnv runs a minimal http_get driver against targetURL
// with a caller-supplied HostEnv, so tests can exercise TLS pinning and
// other per-driver HTTP settings, not just the allowlist.
func runHTTPDriverWithEnv(t *testing.T, env *HostEnv, targetURL string) (gotBody bool, errMsg string) {
	t.Helper()
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
	if v, _, ok := env.Telemetry.LatestMetric(env.DriverName, "result_ok"); ok && v == 1 {
		return true, ""
	}
	if v, _, ok := env.Telemetry.LatestMetric(env.DriverName, "result_err"); ok && v == 1 {
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

func TestLuaHTTPAllowlistEnforcesRedirectTargets(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`redirected`))
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	allowedHostPort := strings.TrimPrefix(redirector.URL, "http://")
	got, errMsg := runHTTPTestDriver(t, []string{allowedHostPort}, redirector.URL)
	if got {
		t.Fatalf("redirect target outside allowed_hosts was fetched (err=%s)", errMsg)
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

// TLS pinning lets a driver reach a self-signed HTTPS endpoint (e.g. a
// NIBE heat pump's local REST API) by accepting exactly one leaf cert.
// httptest.NewTLSServer mints a self-signed cert NOT in the system root
// store, so it stands in perfectly for the pump: rejected without a pin,
// accepted with the right pin, rejected with a wrong one.
func TestLuaHTTPTLSPinning(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	sum := sha256.Sum256(srv.Certificate().Raw)
	goodPin := hex.EncodeToString(sum[:])

	t.Run("no pin rejects self-signed cert", func(t *testing.T) {
		env := NewHostEnv("nopin", telemetry.NewStore()).WithHTTP()
		if got, _ := runHTTPDriverWithEnv(t, env, srv.URL); got {
			t.Fatal("self-signed cert must be rejected when no pin is configured")
		}
	})

	t.Run("correct pin accepts (openssl colon/upper form)", func(t *testing.T) {
		// Feed the openssl-style "AB:CD:..." upper-case form to prove the
		// host normalises it before comparing.
		env := NewHostEnv("goodpin", telemetry.NewStore()).WithHTTP().
			WithHTTPTLSPin(colonizeHex(strings.ToUpper(goodPin)))
		if got, errMsg := runHTTPDriverWithEnv(t, env, srv.URL); !got {
			t.Fatalf("pinned fetch must succeed, err=%s", errMsg)
		}
	})

	t.Run("wrong pin rejects", func(t *testing.T) {
		env := NewHostEnv("badpin", telemetry.NewStore()).WithHTTP().
			WithHTTPTLSPin(strings.Repeat("ab", 32)) // 64 valid hex chars, wrong cert
		if got, _ := runHTTPDriverWithEnv(t, env, srv.URL); got {
			t.Fatal("a wrong pin must reject the connection")
		}
	})

	t.Run("malformed pin fails closed", func(t *testing.T) {
		// A configured-but-invalid pin is rejected as configuration error;
		// it must never silently fall back to the system trust store.
		env := NewHostEnv("junkpin", telemetry.NewStore()).WithHTTP().WithHTTPTLSPin("not-a-fingerprint")
		if got, _ := runHTTPDriverWithEnv(t, env, srv.URL); got {
			t.Fatal("malformed pin must reject the request")
		}
	})

	t.Run("configured pin forbids clear-text downgrade", func(t *testing.T) {
		var hits atomic.Int32
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			_, _ = w.Write([]byte(`ok`))
		}))
		defer plain.Close()
		env := NewHostEnv("cleartext", telemetry.NewStore()).WithHTTP().
			WithHTTPTLSPin(goodPin)
		if got, _ := runHTTPDriverWithEnv(t, env, plain.URL); got {
			t.Fatal("a pinned driver must not use clear-text HTTP")
		}
		if hits.Load() != 0 {
			t.Fatalf("clear-text server received %d requests, want 0", hits.Load())
		}
	})
}

// colonizeHex turns "abcd..." into "ab:cd:..." to mimic the openssl
// `-fingerprint` output format the operator copy-pastes.
func colonizeHex(h string) string {
	var parts []string
	for i := 0; i+2 <= len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

func TestNormalizeHexFingerprint(t *testing.T) {
	const canonical = "0011223344556677889900aabbccddeeff0123456789abcdef0011223344abcd"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already canonical", canonical, canonical},
		{"openssl colon+upper", colonizeHex(strings.ToUpper(canonical)), canonical},
		{"spaces tolerated", canonical[:8] + " " + canonical[8:24] + " " + canonical[24:], canonical},
		{"too short → empty", "deadbeef", ""},
		{"non-hex char → empty", "zz" + canonical[2:], ""},
		{"empty → empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeHexFingerprint(tc.in); got != tc.want {
				t.Errorf("normalizeHexFingerprint(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
