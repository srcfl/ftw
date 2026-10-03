package drivers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// loginServer mimics a web sign-in: POST /login sets a session cookie and
// redirects, GET /data answers only with that cookie.
func loginServer(t *testing.T) (*httptest.Server, string, *atomic.Int32) {
	t.Helper()
	var posts atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			posts.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "synthetic", Path: "/", Secure: true})
			w.Header().Set("Location", "/data")
			w.WriteHeader(http.StatusFound)
		case "/data":
			if c, err := r.Cookie("session"); err != nil || c.Value != "synthetic" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("no session"))
				return
			}
			_, _ = w.Write([]byte("reading"))
		case "/away":
			w.Header().Set("Location", "https://not-allowed.invalid/steal")
			w.WriteHeader(http.StatusFound)
		case "/device":
			posts.Add(1)
			_, _ = w.Write([]byte("written"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(srv.Certificate().Raw)
	return srv, hex.EncodeToString(sum[:]), &posts
}

func runRequestDriver(t *testing.T, env *HostEnv, policy *RuntimePolicy, script string, config map[string]any) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request.lua")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	var d *LuaDriver
	var err error
	if policy != nil {
		d, err = NewLuaDriverWithPolicy(path, env, policy)
	} else {
		d, err = NewLuaDriver(path, env)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer d.Cleanup()
	return d.Init(context.Background(), config)
}

const signInScript = `function driver_init(config)
  local r, err = host.http_request{url = config.base .. "/data"}
  assert(r, err)
  assert(r.status == 401, "status " .. tostring(r.status))
  assert(r.body == "no session", "a 4xx body comes back, not an error")

  r, err = host.http_request{method = "POST", url = config.base .. "/login",
    headers = {["Content-Type"] = "application/x-www-form-urlencoded"}, body = "email=x"}
  assert(r, err)
  assert(r.status == 302, "redirect is returned, not followed: " .. tostring(r.status))
  assert(r.location == config.base .. "/data", "location resolved: " .. tostring(r.location))
  assert(r.headers["set-cookie"] == nil, "cookies stay in the host jar")
  assert(r.headers["date"] ~= nil, "server time is readable")

  r, err = host.http_request{url = r.location}
  assert(r, err)
  assert(r.status == 200 and r.body == "reading", "jar sends the session: " .. tostring(r.status))

  local plain = host.http_get(config.base .. "/data")
  assert(plain == nil, "http_get does not use the jar")

  host.http_cookies_clear()
  r = host.http_request{url = config.base .. "/data"}
  assert(r.status == 401, "cleared jar")

  r, err = host.http_request{url = config.base .. "/away"}
  assert(r and r.status == 302, "redirect to another host is returned, not followed")
  r, err = host.http_request{url = r.location}
  assert(r == nil and err:find("not in allowed_hosts"), "next hop is checked: " .. tostring(err))
end`

func TestHTTPRequestSignInWithHostCookieJar(t *testing.T) {
	srv, pin, _ := loginServer(t)
	env := NewHostEnv("request", telemetry.NewStore()).WithHTTP().WithHTTPTLSPin(pin).
		WithHTTPAllowedHosts([]string{strings.TrimPrefix(srv.URL, "https://")})
	if err := runRequestDriver(t, env, nil, signInScript, map[string]any{"base": srv.URL}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPRequestRefusals(t *testing.T) {
	srv, pin, _ := loginServer(t)
	host := strings.TrimPrefix(srv.URL, "https://")
	script := `function driver_init(config)
  local r, err = host.http_request{method = config.method, url = config.url}
  assert(r == nil, "request should be refused")
  assert(err and err:find(config.want, 1, true), "reason: " .. tostring(err))
end`
	for _, tc := range []struct {
		name, method, url, want string
		allowed                 []string
	}{
		{"empty allowlist", "GET", srv.URL + "/data", "non-empty allowed_hosts", nil},
		{"plain http", "GET", "http://" + host + "/data", "https URL", []string{host}},
		{"other host", "GET", "https://not-allowed.invalid/data", "not in allowed_hosts", []string{host}},
		{"method", "PUT", srv.URL + "/data", "not supported", []string{host}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := NewHostEnv("request", telemetry.NewStore()).WithHTTP().WithHTTPTLSPin(pin)
			if tc.allowed != nil {
				env.WithHTTPAllowedHosts(tc.allowed)
			}
			if err := runRequestDriver(t, env, nil, script, map[string]any{"method": tc.method, "url": tc.url, "want": tc.want}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A read-only driver may POST through http_request only to the sign-in paths
// its signed metadata declares, exactly as with http_post.
func TestHTTPRequestReadOnlyPostOnlyToDeclaredPaths(t *testing.T) {
	srv, pin, posts := loginServer(t)
	host := strings.TrimPrefix(srv.URL, "https://")
	policy := readOnlyAuthPostPolicy("")
	policy.AuthPostPaths = []string{"/identifier", "/login"}
	script := `function driver_init(config)
  local r, err = host.http_request{method = "POST", url = config.base .. "/login", body = "x"}
  assert(r and r.status == 302, "declared path: " .. tostring(err))
  r, err = host.http_request{method = "POST", url = config.base .. "/device", body = "x"}
  assert(r == nil and err:find("cannot write"), "undeclared path: " .. tostring(err))
end`
	env := NewHostEnv("request", telemetry.NewStore()).WithHTTP().WithHTTPTLSPin(pin).
		WithHTTPAllowedHosts([]string{host})
	if err := runRequestDriver(t, env, policy, script, map[string]any{"base": srv.URL}); err != nil {
		t.Fatal(err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("POSTs reaching the server = %d, want 1", got)
	}
}

func TestHTTPRequestAssertionFailureFailsInit(t *testing.T) {
	srv, pin, _ := loginServer(t)
	env := NewHostEnv("request", telemetry.NewStore()).WithHTTP().WithHTTPTLSPin(pin).
		WithHTTPAllowedHosts([]string{strings.TrimPrefix(srv.URL, "https://")})
	script := `function driver_init(config)
  local r = host.http_request{url = config.base .. "/data"}
  assert(r.status == 200, "expected to fail")
end`
	if err := runRequestDriver(t, env, nil, script, map[string]any{"base": srv.URL}); err == nil {
		t.Fatal("a failing assertion must surface, or the other tests prove nothing")
	}
}

func TestAllowedHostJarDropsOtherHosts(t *testing.T) {
	allowed := func(raw string) (bool, string) {
		return strings.Contains(raw, "//good.example"), ""
	}
	inner, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar := &allowedHostJar{inner: inner, allowed: allowed}
	cookie := []*http.Cookie{{Name: "s", Value: "v", Path: "/"}}
	good := mustURL(t, "https://good.example/x")
	evil := mustURL(t, "https://evil.example/x")
	clear := mustURL(t, "http://good.example/x")
	jar.SetCookies(evil, cookie)
	jar.SetCookies(clear, cookie)
	if len(jar.inner.Cookies(evil)) != 0 || len(jar.inner.Cookies(good)) != 0 {
		t.Fatal("cookie stored for a host outside allowed_hosts or over http")
	}
	jar.SetCookies(good, cookie)
	if len(jar.Cookies(good)) != 1 {
		t.Fatal("allowed https host lost its cookie")
	}
	if len(jar.Cookies(clear)) != 0 {
		t.Fatal("cookie sent over plain http")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
