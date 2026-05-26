package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// SSRF + input-validation regression tests for /api/drivers/verify_tesla.
// These lock in the security boundary the endpoint exists to enforce —
// remove a guard, the matching test fails. PR #184 review S1 is the
// origin of every constraint asserted here.

// resetVerifyTracker wipes the per-process rate-limit/wakeup state so
// each test starts clean. The tracker is a package var; concurrent
// tests must not run in parallel against it.
func resetVerifyTracker() {
	verifyTracker.mu.Lock()
	verifyTracker.lastCall = map[string]time.Time{}
	verifyTracker.lastWakeup = map[string]time.Time{}
	verifyTracker.mu.Unlock()
}

func TestValidateProxyIPRejectsPublicAndSpecial(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		expErr string
	}{
		{"public dns ipv4", "8.8.8.8", "public IP not permitted"},
		{"loopback v4", "127.0.0.1", "loopback"},
		{"link-local v4", "169.254.10.10", "link-local"},
		{"ipv6", "::1", "IPv6 not supported"},
		{"hostname rejected", "tesla.local", "invalid IP literal"},
		{"port not allowed", "192.168.1.50:22", "port 22 not permitted"},
		{"port 6379", "192.168.1.50:6379", "port 6379 not permitted"},
		{"empty", "", "invalid IP literal"},
		{"path-smuggle", "192.168.1.50/admin", "invalid IP literal"},
		// Carrier-grade NAT (100.64/10) is intentionally NOT in the
		// RFC1918 set — should be rejected as public.
		{"cgn 100.64", "100.64.1.1", "public IP not permitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := validateProxyIP(tc.input)
			if err == nil {
				t.Fatalf("validateProxyIP(%q) accepted, expected error containing %q",
					tc.input, tc.expErr)
			}
			if !strings.Contains(err.Error(), tc.expErr) {
				t.Errorf("validateProxyIP(%q) err = %v, want substring %q",
					tc.input, err, tc.expErr)
			}
		})
	}
}

func TestValidateProxyIPAcceptsRFC1918(t *testing.T) {
	cases := []struct {
		input    string
		wantHost string
		wantPort int
	}{
		{"10.0.0.1", "10.0.0.1", 8080},
		{"172.16.0.1", "172.16.0.1", 8080},
		{"172.31.255.255", "172.31.255.255", 8080},
		{"192.168.1.50", "192.168.1.50", 8080},
		{"192.168.1.50:80", "192.168.1.50", 80},
		{"192.168.1.50:443", "192.168.1.50", 443},
		{"192.168.1.50:8080", "192.168.1.50", 8080},
	}
	for _, tc := range cases {
		host, port, err := validateProxyIP(tc.input)
		if err != nil {
			t.Errorf("validateProxyIP(%q) err = %v, want ok", tc.input, err)
			continue
		}
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("validateProxyIP(%q) = (%s,%d), want (%s,%d)",
				tc.input, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

func TestSplitHostPortDefaultsAndEdges(t *testing.T) {
	cases := []struct {
		in       string
		def      int
		wantHost string
		wantPort int
	}{
		{"192.168.1.50", 8080, "192.168.1.50", 8080},
		{"192.168.1.50:1234", 8080, "192.168.1.50", 1234},
		// :0 must NOT silently become the default — operator probably
		// meant something specific; we treat zero as no port and fall
		// back, but document the behaviour explicitly via this test.
		{"192.168.1.50:0", 8080, "192.168.1.50", 8080},
		// Port overflow + non-numeric → fall back rather than panic.
		{"192.168.1.50:99999", 8080, "192.168.1.50", 8080},
		{"192.168.1.50:abc", 8080, "192.168.1.50", 8080},
		// Empty port after colon.
		{"192.168.1.50:", 8080, "192.168.1.50", 8080},
	}
	for _, tc := range cases {
		host, port := splitHostPort(tc.in, tc.def)
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("splitHostPort(%q,%d) = (%s,%d), want (%s,%d)",
				tc.in, tc.def, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

func TestValidVINRegex(t *testing.T) {
	good := []string{
		"5YJ3E1EA1KF000000",
		"7SAYGDEE9PA000000",
		"ABCDEFGHJKLMNPRST", // exactly 17, no I/O/Q
	}
	bad := []string{
		"",                      // empty
		"5YJ3E1EA1KF00000",      // 16 chars
		"5YJ3E1EA1KF0000000",    // 18 chars
		"5YJ3E1EA1KF00000I",     // contains I
		"5YJ3E1EA1KF00000O",     // contains O
		"5YJ3E1EA1KF00000Q",     // contains Q
		"5YJ3E1EA1KF00000/",     // contains slash (path-smuggle attempt)
		"5YJ3E1EA1KF00000?",     // contains ? (query smuggle)
		"5yj3e1ea1kf000000",     // lowercase (handler upper-cases first)
		"5YJ3E1EA1KF00 0000",    // contains space
	}
	for _, v := range good {
		if !validVINRe.MatchString(v) {
			t.Errorf("validVINRe rejected good VIN %q", v)
		}
	}
	for _, v := range bad {
		if validVINRe.MatchString(v) {
			t.Errorf("validVINRe accepted bad VIN %q", v)
		}
	}
}

func TestVerifyTeslaRejectsBadInput(t *testing.T) {
	resetVerifyTracker()
	srv := New(&Deps{})
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"empty body", `{}`, 400, "required"},
		{"no vin", `{"ip":"192.168.1.50"}`, 400, "required"},
		{"no ip", `{"vin":"5YJ3E1EA1KF000000"}`, 400, "required"},
		{"bad vin", `{"ip":"192.168.1.50","vin":"BADVIN"}`, 400, "invalid VIN"},
		{"public ip", `{"ip":"8.8.8.8","vin":"5YJ3E1EA1KF000000"}`, 400, "public IP"},
		{"loopback", `{"ip":"127.0.0.1","vin":"5YJ3E1EA1KF000000"}`, 400, "loopback"},
		{"port 22", `{"ip":"192.168.1.50:22","vin":"5YJ3E1EA1KF000000"}`, 400, "port 22"},
		{"hostname", `{"ip":"tesla.local","vin":"5YJ3E1EA1KF000000"}`, 400, "invalid IP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/drivers/verify_tesla",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)
			if rr.Code != tc.wantCode {
				t.Errorf("status = %d, want %d (body=%s)", rr.Code, tc.wantCode, rr.Body.String())
			}
			var resp verifyTeslaResponse
			_ = json.Unmarshal(rr.Body.Bytes(), &resp)
			if !strings.Contains(resp.Error, tc.wantErr) {
				t.Errorf("error = %q, want substring %q", resp.Error, tc.wantErr)
			}
		})
	}
}

// Tested at the helper level rather than through the full HTTP path —
// the handler issues a network call against an unreachable RFC1918 IP
// which would block the test for the full 30 s timeout. The rate-limit
// gate runs BEFORE the network call, so verifying it via the helper
// directly catches the same regression without the wait.
func TestRateLimitVerifyHelper(t *testing.T) {
	resetVerifyTracker()
	now := time.Now()
	if _, ok := rateLimitVerify("VIN_RL", now); !ok {
		t.Fatal("first call should pass the rate limiter")
	}
	if wait, ok := rateLimitVerify("VIN_RL", now.Add(1*time.Second)); ok {
		t.Errorf("second call within window should be rate-limited (wait=%v)", wait)
	}
	if _, ok := rateLimitVerify("VIN_RL", now.Add(verifyMinInterval+time.Second)); !ok {
		t.Errorf("call past window should pass")
	}
	if _, ok := rateLimitVerify("VIN_OTHER", now.Add(1*time.Second)); !ok {
		t.Errorf("different VIN should pass independently")
	}
}

func TestVerifyTeslaWakeupCooldown(t *testing.T) {
	resetVerifyTracker()
	now := time.Now()
	// First call wakes.
	if !shouldWakeup("VIN_A", now) {
		t.Error("first wakeup should be granted")
	}
	// Immediately retry → no wakeup.
	if shouldWakeup("VIN_A", now.Add(1*time.Second)) {
		t.Error("second wakeup within cooldown should be denied")
	}
	// Past the cooldown → wakeup again.
	if !shouldWakeup("VIN_A", now.Add(verifyWakeupCooldown+time.Second)) {
		t.Error("wakeup after cooldown should be granted")
	}
	// Different VIN tracked independently.
	if !shouldWakeup("VIN_B", now.Add(1*time.Second)) {
		t.Error("different VIN should track independently")
	}
}

func TestVerifyTrackerConcurrentSafe(t *testing.T) {
	// Smoke test that the mutex actually protects the maps under load —
	// catch regressions where someone "optimises" to atomics.
	resetVerifyTracker()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vin := "VIN_X"
			now := time.Now()
			_, _ = rateLimitVerify(vin, now)
			_ = shouldWakeup(vin, now)
		}(i)
	}
	wg.Wait()
	// No assertion beyond "did not panic / race".
}
