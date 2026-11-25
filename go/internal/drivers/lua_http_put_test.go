package drivers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// host.http_put shares http_patch's gates. These tests pin the verb on the
// wire and that the write grant, not the plain HTTP grant, lets it out. The
// sonnenBatterie JSON API v2 is the first user: it changes its operating mode
// only through PUT /api/v2/configurations and answers PATCH and POST there
// with 404.

func TestLuaHTTPPutSendsMethodBodyAndHeaders(t *testing.T) {
	var gotMethod, gotBody, gotCT, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"EM_OperatingMode":"2"}`))
	}))
	defer srv.Close()

	env := NewHostEnv("putter", telemetry.NewStore()).WithHTTP().WithHTTPAllowWrite()
	ok, errText := runHTTPWriteDriver(t, env, "http_put", srv.URL+"/api/v2/configurations")
	if !ok {
		t.Fatalf("put failed: %s", errText)
	}
	if gotMethod != "PUT" {
		t.Errorf("method = %q, want PUT", gotMethod)
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

// A driver may send a form body by naming its own Content-Type; the host's
// JSON default must not override it.
func TestLuaHTTPPutDriverContentTypeWins(t *testing.T) {
	var gotMethod, gotBody, gotCT, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotCT = r.Header.Get("Content-Type")
		gotToken = r.Header.Get("Auth-Token")
		_, _ = w.Write([]byte(`{"EM_OperatingMode":"2"}`))
	}))
	defer srv.Close()

	src := `
		function driver_init() end
		function driver_poll()
			local body, err = host.http_put("` + srv.URL + `/api/v2/configurations",
				"EM_OperatingMode=2",
				{ ["Content-Type"] = "application/x-www-form-urlencoded",
				  ["Auth-Token"] = "tok" })
			if err then
				host.log("info", "ERR:" .. tostring(err))
			else
				host.emit_metric("put_ok", 1)
			end
			return 60000
		end
		function driver_command() end
		function driver_default_mode() end
	`
	path := filepath.Join(t.TempDir(), "form.lua")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	env := NewHostEnv("form", telemetry.NewStore()).WithHTTP().WithHTTPAllowWrite()
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
	if v, _, found := env.Telemetry.LatestMetric("form", "put_ok"); !found || v != 1 {
		t.Fatal("form PUT did not succeed")
	}
	if gotMethod != "PUT" || gotBody != "EM_OperatingMode=2" {
		t.Errorf("got %s %q", gotMethod, gotBody)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q, want the driver's form type", gotCT)
	}
	if gotToken != "tok" {
		t.Errorf("Auth-Token = %q", gotToken)
	}
}

func TestLuaHTTPPutRequiresAllowWrite(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	env := NewHostEnv("readonly", telemetry.NewStore()).WithHTTP()
	ok, deniedBy := runHTTPWriteDriver(t, env, "http_put", srv.URL)
	if ok {
		t.Fatal("http_put succeeded without capabilities.http.allow_write")
	}
	if deniedBy != "denied_write_cap" {
		t.Errorf("denied by %q, want the allow_write gate", deniedBy)
	}
	if hits.Load() != 0 {
		t.Fatalf("server was reached %d times despite missing allow_write", hits.Load())
	}
}

func TestLuaHTTPPutWithoutHTTPCapability(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	env := NewHostEnv("nohttp", telemetry.NewStore()).WithHTTPAllowWrite()
	ok, deniedBy := runHTTPWriteDriver(t, env, "http_put", srv.URL)
	if ok {
		t.Fatal("http_put succeeded without the http capability")
	}
	if deniedBy != "denied_http_cap" {
		t.Errorf("denied by %q, want the http capability gate", deniedBy)
	}
	if hits.Load() != 0 {
		t.Fatalf("server was reached %d times", hits.Load())
	}
}

func TestLuaHTTPPutRefusesRedirects(t *testing.T) {
	for _, code := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var followUps atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				followUps.Add(1)
				_, _ = w.Write([]byte(`ok`))
			}))
			defer target.Close()
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL, code)
			}))
			defer redirector.Close()

			env := NewHostEnv("redirected", telemetry.NewStore()).WithHTTP().WithHTTPAllowWrite()
			ok, deniedBy := runHTTPWriteDriver(t, env, "http_put", redirector.URL)
			if ok {
				t.Fatal("a redirected PUT must not report success")
			}
			if deniedBy != "denied_redirect" {
				t.Errorf("denied by %q, want the redirect refusal", deniedBy)
			}
			if followUps.Load() != 0 {
				t.Fatalf("redirect target was reached %d times", followUps.Load())
			}
		})
	}
}

func TestLuaHTTPPutRespectsAllowlist(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	env := NewHostEnv("fenced", telemetry.NewStore()).WithHTTP().WithHTTPAllowWrite().
		WithHTTPAllowedHosts([]string{"10.99.99.99"})
	if ok, _ := runHTTPWriteDriver(t, env, "http_put", srv.URL); ok {
		t.Fatal("http_put escaped the allowed_hosts fence")
	}
	if hits.Load() != 0 {
		t.Fatalf("server was reached %d times despite the allowlist", hits.Load())
	}
}

func TestLuaHTTPPutBlockedInFingerprintProbe(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	env := NewHostEnv("probe", telemetry.NewStore()).WithHTTP().WithHTTPAllowWrite()
	env.ProbeReadOnly = true
	if ok, _ := runHTTPWriteDriver(t, env, "http_put", srv.URL); ok {
		t.Fatal("http_put succeeded in a read-only probe")
	}
	if hits.Load() != 0 {
		t.Fatalf("server was reached %d times from a read-only probe", hits.Load())
	}
}
