package drivers

import (
	"fmt"
	"io"
	net_http "net/http"
	"net/http/cookiejar"
	net_url "net/url"
	"sort"
	"strings"

	lua "github.com/yuin/gopher-lua"
	"golang.org/x/net/publicsuffix"
)

// host.http_request{method, url, headers?, body?} returns
// {status, headers, location, body} or (nil, error_string).
//
// It is for a driver that has to sign in through a web login before it can
// read: the status and Location come back to Lua instead of being followed or
// turned into an error, and session cookies live in a jar the host keeps for
// this driver. The jar is in memory only, so a driver restart starts a clean
// session. Lua never sees the cookies: Set-Cookie is left out of headers.
//
// Rules on top of the http_get/http_post ones:
//   - allowed_hosts must be non-empty, and the URL must be https;
//   - only GET and POST, with POST under the same write or sign-in gate as
//     http_post;
//   - redirects are never followed, so every hop is a separate call the
//     host checks against allowed_hosts;
//   - the jar stores and sends cookies only for allowed hosts.
//
// host.http_cookies_clear() empties the jar before a fresh sign-in.
func registerHTTPRequest(L *lua.LState, host *lua.LTable, env *HostEnv, base *net_http.Client, hostAllowed func(string) (bool, string)) {
	newJar := func() *allowedHostJar {
		inner, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
		return &allowedHostJar{inner: inner, allowed: hostAllowed}
	}
	jar := newJar()
	client := &net_http.Client{
		Timeout:   base.Timeout,
		Transport: base.Transport,
		Jar:       jar,
		CheckRedirect: func(*net_http.Request, []*net_http.Request) error {
			return net_http.ErrUseLastResponse
		},
	}

	host.RawSetString("http_cookies_clear", L.NewFunction(func(L *lua.LState) int {
		jar = newJar()
		client.Jar = jar
		return 0
	}))

	host.RawSetString("http_request", L.NewFunction(func(L *lua.LState) int {
		fail := func(msg string) int {
			L.Push(lua.LNil)
			L.Push(lua.LString(msg))
			return 2
		}
		opts := L.CheckTable(1)
		method := strings.ToUpper(lua.LVAsString(opts.RawGetString("method")))
		if method == "" {
			method = "GET"
		}
		rawURL := lua.LVAsString(opts.RawGetString("url"))
		if !env.HTTP {
			return fail("http: capability not granted")
		}
		if len(env.HTTPAllowedHosts) == 0 {
			return fail("http_request: requires a non-empty allowed_hosts")
		}
		switch method {
		case "GET":
			if !env.permissionAllowed("http.get") {
				return fail("http.get: permission not granted by signed package")
			}
		case "POST":
			if !env.allowAuthPost(rawURL) {
				if err := env.allowWrite("http.post"); err != nil {
					return fail(err.Error())
				}
			}
		default:
			return fail(fmt.Sprintf("http_request: method %q not supported (GET or POST)", method))
		}
		u, err := net_url.Parse(rawURL)
		if err != nil || !strings.EqualFold(u.Scheme, "https") {
			return fail("http_request: requires an https URL")
		}
		if ok, reason := hostAllowed(rawURL); !ok {
			return fail("http: " + reason)
		}

		var body io.Reader
		if v := opts.RawGetString("body"); v != lua.LNil {
			body = strings.NewReader(lua.LVAsString(v))
		}
		var req *net_http.Request
		if method == "GET" {
			req, err = net_http.NewRequestWithContext(luaCallContext(L), method, rawURL, body)
		} else {
			// Same ordering rule as http_post: do not cancel a request the
			// server may already have acted on.
			req, err = net_http.NewRequest(method, rawURL, body)
		}
		if err != nil {
			return fail(err.Error())
		}
		if headers, ok := opts.RawGetString("headers").(*lua.LTable); ok {
			headers.ForEach(func(k, v lua.LValue) {
				if ks, ok := k.(lua.LString); ok {
					req.Header.Set(string(ks), v.String())
				}
			})
		}
		resp, err := client.Do(req)
		if err != nil {
			return fail(err.Error())
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return fail(err.Error())
		}

		out := L.NewTable()
		out.RawSetString("status", lua.LNumber(resp.StatusCode))
		hdrs := L.NewTable()
		names := make([]string, 0, len(resp.Header))
		for name := range resp.Header {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			lower := strings.ToLower(name)
			if lower == "set-cookie" {
				continue
			}
			hdrs.RawSetString(lower, lua.LString(strings.Join(resp.Header[name], ", ")))
		}
		out.RawSetString("headers", hdrs)
		if loc, err := resp.Location(); err == nil {
			out.RawSetString("location", lua.LString(loc.String()))
		}
		out.RawSetString("body", lua.LString(string(data)))
		L.Push(out)
		return 1
	}))
}

// allowedHostJar keeps a driver's session cookies to the hosts it may reach
// over https. A cookie a server sets for any other host is dropped, and none
// is ever sent elsewhere.
type allowedHostJar struct {
	inner   *cookiejar.Jar
	allowed func(string) (bool, string)
}

func (j *allowedHostJar) ok(u *net_url.URL) bool {
	if u == nil || !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	ok, _ := j.allowed(u.String())
	return ok
}

func (j *allowedHostJar) SetCookies(u *net_url.URL, cookies []*net_http.Cookie) {
	if j.ok(u) {
		j.inner.SetCookies(u, cookies)
	}
}

func (j *allowedHostJar) Cookies(u *net_url.URL) []*net_http.Cookie {
	if !j.ok(u) {
		return nil
	}
	return j.inner.Cookies(u)
}
