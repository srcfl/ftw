package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// POST /api/drivers/verify_tesla — operator-facing "Verify connection"
// button for the Tesla vehicle driver config form. Accepts {ip, vin}
// exactly matching the driver's own config fields, issues a single
// GET to the proxy's vehicle_data endpoint from the backend (avoids
// browser CORS), and returns a summary the UI can render inline.
//
// Hardened against SSRF: the `ip` param is constrained to RFC1918
// private space (10/8, 172.16/12, 192.168/16), link-local is rejected,
// loopback is rejected, ports are restricted to 80/443/8080, VIN is
// regex-validated to the standard 17-char pattern, redirects are
// refused, and the upstream body is never reflected back to the caller.
// See PR #184 review S1.
type verifyTeslaRequest struct {
	IP  string `json:"ip"`
	VIN string `json:"vin"`
}

type verifyTeslaResponse struct {
	OK             bool    `json:"ok"`
	Error          string  `json:"error,omitempty"`
	URL            string  `json:"url,omitempty"`
	Status         int     `json:"status,omitempty"`
	SoCPct         float64 `json:"soc_pct,omitempty"`
	ChargeLimitPct float64 `json:"charge_limit_pct,omitempty"`
	ChargingState  string  `json:"charging_state,omitempty"`
}

// validVINRe is the standard 17-character VIN charset: A-HJ-NPR-Z0-9
// (excludes I, O, Q to avoid confusion with 0/1). Applied even though
// Teslas pattern-match narrower because an attacker could try to abuse
// a relaxed validation to smuggle `/` or `?` into the URL path.
var validVINRe = regexp.MustCompile(`^[A-HJ-NPR-Z0-9]{17}$`)

// allowedVerifyPorts is the tiny set of ports we'll probe on the
// supposed proxy host. TeslaBLEProxy defaults to 8080 in every
// deployment we've seen; 80/443 are included for completeness in case
// someone fronted it with a reverse proxy. Locked down to these three
// so the endpoint can't be used to scan arbitrary LAN services.
var allowedVerifyPorts = map[int]bool{80: true, 443: true, 8080: true}

// errRedirectsForbidden ends a redirect chain before the backend follows
// a Location header to an unintended target (another SSRF vector).
var errRedirectsForbidden = errors.New("redirects disabled")

// maxVerifyBody caps how much of the proxy response we read. Tesla
// vehicle_data responses are typically 5-15 KB; 1 MB is pessimistic
// headroom. Prevents a malicious proxy from stalling the handler.
const maxVerifyBody = 1 << 20

// verifyMinInterval is the minimum gap between consecutive
// /api/drivers/verify_tesla calls for the SAME vin. Anything closer
// gets a 429. The aim is to make this endpoint useless as a "keep my
// car awake" pump for a same-origin XSS or a tab the operator left
// open: each call costs the vehicle's 12 V battery a measurable amount
// of energy when wakeup=true is attached.
const verifyMinInterval = 30 * time.Second

// verifyWakeupCooldown gates the `wakeup=true` query parameter. The
// first call (or one issued ≥ this far after the previous) wakes the
// car over BLE; calls inside the window omit wakeup=true and the
// proxy serves cached data. The user-facing flow ("press Verify")
// works either way — the proxy cache is fresh enough to confirm
// reachability + correct VIN — but the BLE radio doesn't get hammered.
const verifyWakeupCooldown = 5 * time.Minute

// verifyTracker is a small per-vin rate-limit + last-wakeup tracker.
// Persists for the process lifetime; resets on restart, which is fine
// because operators can always wait 30 s if they really need to retry.
var verifyTracker = struct {
	mu          sync.Mutex
	lastCall    map[string]time.Time
	lastWakeup  map[string]time.Time
}{
	lastCall:   map[string]time.Time{},
	lastWakeup: map[string]time.Time{},
}

// shouldWakeup returns true if this verify call should attach
// wakeup=true. Updates lastWakeup as a side effect when it returns
// true. Holds the tracker lock briefly.
func shouldWakeup(vin string, now time.Time) bool {
	verifyTracker.mu.Lock()
	defer verifyTracker.mu.Unlock()
	prev, ok := verifyTracker.lastWakeup[vin]
	if !ok || now.Sub(prev) >= verifyWakeupCooldown {
		verifyTracker.lastWakeup[vin] = now
		return true
	}
	return false
}

// rateLimitVerify returns a non-nil time.Duration when the call is too
// soon and should be rejected; the duration is how long the caller
// must wait. Updates lastCall on success.
func rateLimitVerify(vin string, now time.Time) (time.Duration, bool) {
	verifyTracker.mu.Lock()
	defer verifyTracker.mu.Unlock()
	prev, ok := verifyTracker.lastCall[vin]
	if ok && now.Sub(prev) < verifyMinInterval {
		return verifyMinInterval - now.Sub(prev), false
	}
	verifyTracker.lastCall[vin] = now
	return 0, true
}

// validateProxyIP accepts "host" or "host:port" where host is an IPv4
// literal in RFC1918 space. Hostnames are rejected — we want no DNS
// step between parse and connect, because a DNS rebind can change the
// answer between verification and connection.
func validateProxyIP(s string) (string, int, error) {
	host, port := splitHostPort(s, 8080)
	if !allowedVerifyPorts[port] {
		return "", 0, fmt.Errorf("port %d not permitted (allowed: 80, 443, 8080)", port)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", 0, fmt.Errorf("invalid IP literal %q (hostnames not allowed)", host)
	}
	if ip.To4() == nil {
		return "", 0, fmt.Errorf("IPv6 not supported")
	}
	if ip.IsLoopback() {
		return "", 0, fmt.Errorf("loopback address not permitted")
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return "", 0, fmt.Errorf("link-local address not permitted")
	}
	if !isPrivateIPv4(ip) {
		return "", 0, fmt.Errorf("public IP not permitted (RFC1918 only)")
	}
	return ip.String(), port, nil
}

// isPrivateIPv4 matches 10/8, 172.16/12, 192.168/16. Intentionally
// does NOT match 169.254/16 (link-local — caller blocks separately),
// 100.64/10 (carrier-grade NAT), or RFC6598 space.
func isPrivateIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	switch {
	case ip4[0] == 10:
		return true
	case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
		return true
	case ip4[0] == 192 && ip4[1] == 168:
		return true
	}
	return false
}

// handleVerifyTesla runs a one-off vehicle_data fetch against the
// configured TeslaBLEProxy. Mirrors exactly what the driver does on
// each poll, minus the emit. Errors are surfaced verbatim so the
// operator can distinguish "proxy unreachable" from "vehicle asleep"
// from "VIN not paired".
func (s *Server) handleVerifyTesla(w http.ResponseWriter, r *http.Request) {
	var req verifyTeslaRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, verifyTeslaResponse{Error: "invalid body: " + err.Error()})
		return
	}
	ipRaw := strings.TrimSpace(req.IP)
	vin := strings.ToUpper(strings.TrimSpace(req.VIN))
	if ipRaw == "" || vin == "" {
		writeJSON(w, 400, verifyTeslaResponse{Error: "both `ip` and `vin` are required"})
		return
	}
	if !validVINRe.MatchString(vin) {
		writeJSON(w, 400, verifyTeslaResponse{Error: "invalid VIN (must be 17 chars, A-HJ-NPR-Z and 0-9)"})
		return
	}

	host, port, err := validateProxyIP(ipRaw)
	if err != nil {
		writeJSON(w, 400, verifyTeslaResponse{Error: "invalid proxy ip: " + err.Error()})
		return
	}

	now := time.Now()
	if wait, ok := rateLimitVerify(vin, now); !ok {
		writeJSON(w, 429, verifyTeslaResponse{
			Error: fmt.Sprintf("too many verify attempts — wait %d s before retrying",
				int(wait.Seconds())+1),
		})
		return
	}

	wakeup := shouldWakeup(vin, now)
	urlStr := fmt.Sprintf("http://%s:%d/api/1/vehicles/%s/vehicle_data?endpoints=charge_state",
		host, port, vin)
	if wakeup {
		urlStr += "&wakeup=true"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		writeJSON(w, 500, verifyTeslaResponse{Error: "build request: " + err.Error(), URL: urlStr})
		return
	}
	httpReq.Header.Set("Accept", "application/json")

	// Client configured for SSRF resistance: no redirects (a compromised
	// proxy could send a 302 to a metadata endpoint), context-bound
	// timeout, no arbitrary transport sharing.
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errRedirectsForbidden
		},
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		// CheckRedirect rejection wraps errRedirectsForbidden in a
		// *url.Error whose .Err carries the upstream Location header —
		// reflecting that back to the caller would leak the redirect
		// target. Detect the wrapped sentinel and replace with a fixed
		// message. All other transport errors (timeout, conn refused,
		// TLS) are safe to surface verbatim — they don't carry
		// upstream-controlled content.
		if isRedirectError(err) {
			writeJSON(w, 200, verifyTeslaResponse{
				Error: "proxy attempted a redirect (refused) — TeslaBLEProxy should respond directly",
				URL:   urlStr,
			})
			return
		}
		writeJSON(w, 200, verifyTeslaResponse{Error: "request failed: " + err.Error(), URL: urlStr})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusRequestTimeout { // 408: car asleep
		writeJSON(w, 200, verifyTeslaResponse{
			Error:  "vehicle asleep (408) — the proxy reached your car but it did not wake in time. Try again in a few seconds.",
			URL:    urlStr,
			Status: resp.StatusCode,
		})
		return
	}
	if resp.StatusCode >= 400 {
		// Do NOT reflect the upstream body — even the status number on
		// its own is already a port-scan oracle, but the body can carry
		// metadata-service tokens on a successful SSRF hop. Fixed message.
		writeJSON(w, 200, verifyTeslaResponse{
			Error:  fmt.Sprintf("HTTP %d from proxy", resp.StatusCode),
			URL:    urlStr,
			Status: resp.StatusCode,
		})
		return
	}

	// Three response shapes in the wild:
	//
	//   1. TeslaBLEProxy: { response: { response: { charge_state: {…} } } }
	//      — the proxy wraps the upstream body once more. THIS is
	//      what the real-world deployment emits (tested locally).
	//   2. Tesla Owner API:  { response: { charge_state: {…} } }
	//   3. Bare:             { charge_state: {…} }
	//
	// We try all three and pick whichever yielded non-zero fields.
	type chargeState struct {
		BatteryLevel        float64 `json:"battery_level"`
		ChargeLimitSoC      float64 `json:"charge_limit_soc"`
		ChargingState       string  `json:"charging_state"`
		MinutesToFullCharge float64 `json:"minutes_to_full_charge"`
		TimeToFullCharge    float64 `json:"time_to_full_charge"`
	}
	var parsed struct {
		Response struct {
			Response struct {
				ChargeState chargeState `json:"charge_state"`
			} `json:"response"`
			ChargeState chargeState `json:"charge_state"`
		} `json:"response"`
		ChargeState chargeState `json:"charge_state"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxVerifyBody)).Decode(&parsed); err != nil {
		writeJSON(w, 200, verifyTeslaResponse{
			Error: "decode failed (upstream response not recognizable JSON)", URL: urlStr, Status: resp.StatusCode,
		})
		return
	}
	cs := parsed.Response.Response.ChargeState
	if cs.BatteryLevel == 0 && cs.ChargeLimitSoC == 0 && cs.ChargingState == "" {
		cs = parsed.Response.ChargeState
	}
	if cs.BatteryLevel == 0 && cs.ChargeLimitSoC == 0 && cs.ChargingState == "" {
		cs = parsed.ChargeState
	}
	if cs.BatteryLevel == 0 && cs.ChargeLimitSoC == 0 && cs.ChargingState == "" {
		writeJSON(w, 200, verifyTeslaResponse{
			Error: "proxy returned 200 but no charge_state fields — VIN may not be paired",
			URL:   urlStr, Status: resp.StatusCode,
		})
		return
	}
	writeJSON(w, 200, verifyTeslaResponse{
		OK:             true,
		URL:            urlStr,
		Status:         resp.StatusCode,
		SoCPct:         cs.BatteryLevel,
		ChargeLimitPct: cs.ChargeLimitSoC,
		ChargingState:  cs.ChargingState,
	})
}

// splitHostPort parses "host" or "host:port" and returns the port
// defaulting to `def` when no port is present. Uses net.SplitHostPort
// so bracketed IPv6 + edge cases behave like the rest of the stdlib.
// An explicit ":0", non-numeric, or out-of-range port falls back to
// def — there's no use case for port 0 in this endpoint and the
// operator probably meant the default.
func splitHostPort(s string, def int) (string, int) {
	if !strings.Contains(s, ":") {
		return s, def
	}
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return s, def
	}
	if portStr == "" {
		return host, def
	}
	var port int
	for _, r := range portStr {
		if r < '0' || r > '9' {
			return host, def
		}
		port = port*10 + int(r-'0')
		if port > 65535 {
			return host, def
		}
	}
	if port == 0 {
		return host, def
	}
	return host, port
}

// isRedirectError reports whether the error returned by http.Client.Do
// originated from our CheckRedirect rejection (vs a transport-level
// failure). The stdlib wraps the rejection in *url.Error whose Unwrap
// returns errRedirectsForbidden.
func isRedirectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errRedirectsForbidden) {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return errors.Is(ue.Err, errRedirectsForbidden)
	}
	return false
}
