package drivers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// A read-only driver that reads a vendor cloud cannot read anything until it
// has exchanged a token, and it exchanges one from init or poll. Those are the
// phases allowWrite refuses, so before this the choice was to publish such a
// driver control-capable -- which is what myuplink did, and why its catalog
// entry claimed a control path its driver_command has always refused.

func readOnlyAuthPostPolicy(path string) *RuntimePolicy {
	return &RuntimePolicy{
		PackageID:      "com.sourceful.driver.myuplink",
		Version:        "1.2.0",
		ArtifactSHA256: strings.Repeat("a", 64),
		RuntimeABI:     "gopher-lua-source-v1", HostAPIProfile: "sourceful.host/ftw-core/v1",
		ReadOnly:     true,
		Permissions:  map[string]bool{"http.get": true, "http.post": true},
		AuthPostPath: path,
	}
}

func TestManagedReadOnlyOAuthHTTPBoundary(t *testing.T) {
	var requests atomic.Int32
	var deviceRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != "POST" || r.URL.Path != "/oauth/token" {
			deviceRequests.Add(1)
		}
		if r.URL.Path == "/oauth/token" {
			switch r.URL.Query().Get("redirect") {
			case "307":
				w.Header().Set("Location", "/v2/devices/1/points")
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			case "308":
				w.Header().Set("Location", "/v2/devices/1/points")
				w.WriteHeader(http.StatusPermanentRedirect)
				return
			}
		}
		_, _ = w.Write([]byte(`{"access_token":"synthetic"}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "oauth.lua")
	if err := os.WriteFile(path, []byte(`function driver_init(config)
local body, err = host.http_post(config.url, "synthetic")
host.emit_metric("post_ok", body and 1 or 0)
if not body and not err then error("missing denial reason") end
end`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, url             string
		noPermission, allowed bool
	}{
		{"auth", server.URL + "/oauth/token", false, true},
		{"auth_query", server.URL + "/oauth/token?x=1", false, true},
		{"device_write", server.URL + "/v2/devices/1/points", false, false},
		{"path_traversal", server.URL + "/oauth/token/../device", false, false},
		{"path_suffix", server.URL + "/oauth/token/extra", false, false},
		{"other_host", "http://not-allowed.invalid/oauth/token", false, false},
		{"invalid_url", ":bad", false, false},
		{"missing_post_permission", server.URL + "/oauth/token", true, false},
		{"redirect_307", server.URL + "/oauth/token?redirect=307", false, false},
		{"redirect_308", server.URL + "/oauth/token?redirect=308", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := readOnlyAuthPostPolicy("/oauth/token")
			if tc.noPermission {
				delete(policy.Permissions, "http.post")
			}
			tel := telemetry.NewStore()
			env := NewHostEnv("oauth", tel).WithHTTP().WithHTTPAllowedHosts([]string{strings.TrimPrefix(server.URL, "http://")})
			d, err := NewLuaDriverWithPolicy(path, env, policy)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Cleanup()
			before := requests.Load()
			if err := d.Init(context.Background(), map[string]any{"url": tc.url}); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.allowed {
				want = 1
			}
			if value, _, ok := tel.LatestMetric("oauth", "post_ok"); !ok || value != float64(want) {
				t.Fatalf("HTTP result = %v, present=%v, want %d", value, ok, want)
			}
			wantRequests := want
			if strings.HasPrefix(tc.name, "redirect_") {
				wantRequests = 1
			}
			if got := requests.Load() - before; got != int32(wantRequests) {
				t.Fatalf("HTTP requests = %d, want %d", got, wantRequests)
			}
			if deviceRequests.Load() != 0 {
				t.Fatal("OAuth exception reached a device write path")
			}
			// A write scope must not turn a read-only OAuth grant into a device
			// write grant, even when local HTTP write capability is configured.
			env.WithHTTPAllowWrite()
			env.writePhase = "command"
			env.writeDeadline = time.Now().Add(time.Minute)
			if err := env.allowWrite("http.post"); err == nil {
				t.Fatal("read-only POST escaped through command write scope")
			}
			if env.writeAttempts != 0 {
				t.Fatal("auth or rejected device POST spent the write budget")
			}
		})
	}
}

func TestReadOnlyDriverMaySignInOutsideAWriteScope(t *testing.T) {
	env := &HostEnv{RuntimePolicy: readOnlyAuthPostPolicy("/oauth/token")}

	// No write scope is open -- this is what init and poll look like.
	if err := env.allowWrite("http.post"); err == nil {
		t.Fatal("allowWrite should still refuse a POST outside a write scope")
	}
	if !env.allowAuthPost("https://api.myuplink.com/oauth/token") {
		t.Fatal("the declared sign-in must be allowed from init or poll")
	}
	// A token refresh is driven by expiry, not by a caller, so it must not
	// spend the write budget that a real command depends on.
	if env.writeAttempts != 0 {
		t.Fatalf("sign-in consumed the write budget: %d", env.writeAttempts)
	}
}

func TestSignInExemptionIsConfinedToTheDeclaredPath(t *testing.T) {
	env := &HostEnv{RuntimePolicy: readOnlyAuthPostPolicy("/oauth/token")}

	for _, url := range []string{
		"https://api.myuplink.com/v2/devices/1/points",   // the write it must not do
		"https://api.myuplink.com/oauth/token/../device", // no path games
		"https://api.myuplink.com/oauth",
		"https://evil.example/oauth/token/extra",
		"not a url at all",
	} {
		if env.allowAuthPost(url) {
			t.Errorf("POST to %q must not pass as authentication", url)
		}
	}

	// A query string is part of the request, not the path.
	if !env.allowAuthPost("https://api.myuplink.com/oauth/token?x=1") {
		t.Error("a query string must not defeat the declared path")
	}
}

func TestSignInExemptionRequiresBeingDeclared(t *testing.T) {
	// Undeclared: the ordinary write rules apply, which is every driver today.
	env := &HostEnv{RuntimePolicy: readOnlyAuthPostPolicy("")}
	if env.allowAuthPost("https://api.myuplink.com/oauth/token") {
		t.Error("a driver that declared nothing must not be exempt")
	}

	// Declared but not read-only: a controlling driver has no exemption to
	// take, and must not gain a POST path that skips the write scope.
	control := readOnlyAuthPostPolicy("/oauth/token")
	control.ReadOnly = false
	env = &HostEnv{RuntimePolicy: control}
	if env.allowAuthPost("https://api.myuplink.com/oauth/token") {
		t.Error("a controlling driver must not use the read-only exemption")
	}

	// Declared, read-only, but the permission was never granted.
	missing := readOnlyAuthPostPolicy("/oauth/token")
	missing.Permissions = map[string]bool{"http.get": true}
	env = &HostEnv{RuntimePolicy: missing}
	if env.allowAuthPost("https://api.myuplink.com/oauth/token") {
		t.Error("the exemption must still require http.post to be granted")
	}

	// An unmanaged driver has no policy and is unaffected either way.
	env = &HostEnv{}
	if env.allowAuthPost("https://api.myuplink.com/oauth/token") {
		t.Error("a driver with no runtime policy must not report an exemption")
	}
}
