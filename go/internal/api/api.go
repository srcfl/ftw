// Package api is the HTTP surface for forty-two-watts: control endpoints,
// telemetry queries, config get/set, battery-model introspection, self-tune
// orchestration, static file serving for the web UI.
//
// All responses are JSON (or raw file content for /static). All mutation
// endpoints accept JSON bodies. No WebSockets yet — clients poll.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/battery"
	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/control"
	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/evcloud"
	"github.com/frahlg/forty-two-watts/go/internal/events"
	"github.com/frahlg/forty-two-watts/go/internal/forecast"
	"github.com/frahlg/forty-two-watts/go/internal/ha"
	"github.com/frahlg/forty-two-watts/go/internal/loadmodel"
	"github.com/frahlg/forty-two-watts/go/internal/loadpoint"
	"github.com/frahlg/forty-two-watts/go/internal/mpc"
	"github.com/frahlg/forty-two-watts/go/internal/notifications"
	"github.com/frahlg/forty-two-watts/go/internal/p2p"
	"github.com/frahlg/forty-two-watts/go/internal/prices"
	"github.com/frahlg/forty-two-watts/go/internal/pvmodel"
	"github.com/frahlg/forty-two-watts/go/internal/scanner"
	"github.com/frahlg/forty-two-watts/go/internal/selftune"
	"github.com/frahlg/forty-two-watts/go/internal/selfupdate"
	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
	v2xpolicy "github.com/frahlg/forty-two-watts/go/internal/v2x"
)

const (
	// evPasswordKey is the state.db key for the EV charger password
	// (stored outside config.yaml for security).
	evPasswordKey = "ev_charger_password"
	// maskedPlaceholder is sent to the UI to indicate a password is set
	// without revealing the actual value.
	maskedPlaceholder = "••••••••"
)

// InstanceSigner signs the owner-access instance descriptor with the Pi's
// self-sovereign ES256 identity. *nova.Identity satisfies it. PublicKeyHex
// returns the uncompressed P-256 public key (X||Y, 128 lowercase hex chars);
// SignRawHex returns the raw r||s 64-byte signature as a 128-char hex string
// (the handler re-encodes it to base64url for the wire). Declared as an
// interface here so internal/api does not import internal/nova (matches the
// relaySigner pattern in cmd/forty-two-watts/owner_relay_register.go).
type InstanceSigner interface {
	PublicKeyHex() string
	SignRawHex(msg string) (string, error)
}

// Deps is the full set of runtime dependencies the API handlers need.
// One instance is shared across all handlers; mutations use the contained
// mutexes from each package.
type Deps struct {
	Tel *telemetry.Store
	// LogRing is the in-memory log buffer wired in main.go. Nil makes
	// /api/drivers/{name}/logs and /api/support/dump return 503.
	LogRing       *telemetry.LogRing
	Ctrl          *control.State
	CtrlMu        *sync.Mutex
	State         *state.Store
	CapMu         *sync.RWMutex
	Capacities    map[string]float64 // driver → battery_capacity_wh
	CfgMu         *sync.RWMutex
	Cfg           *config.Config
	ConfigPath    string
	DriverDir     string // where to scan for Lua drivers (default: <config-dir>/drivers)
	UserDriverDir string // persistent user-drivers overlay; searched before DriverDir
	Models        map[string]*battery.Model
	ModelsMu      *sync.Mutex
	SelfTune      *selftune.Coordinator
	DtS           float64                                   // control interval seconds (for model τ / age displays)
	SaveConfig    func(path string, c *config.Config) error // injection for testability
	WebDir        string                                    // static assets root (default "web")
	ColdDir       string                                    // cold-storage root for parquet rolloff; empty disables cold fallback
	// SnapshotDir is where pre-update snapshots of state.db + config.yaml
	// are written by the self-update flow. Defaults to
	// `<cold_dir_parent>/snapshots`; main.go is responsible for passing
	// an absolute, writable path. Empty disables the snapshot step —
	// updates proceed as before, the UI surfaces that no rollback point
	// was captured so the operator can decide whether to continue.
	SnapshotDir string

	// Optional: spot prices + weather forecast services. Nil if disabled.
	Prices   *prices.Service
	Forecast *forecast.Service

	// Optional: MPC planner. Nil if disabled.
	MPC *mpc.Service

	// Optional: PV digital-twin self-learner.
	PVModel *pvmodel.Service

	// Optional: load digital-twin self-learner.
	LoadModel *loadmodel.Service

	// Optional: EV loadpoints (Phase 3 observable skeleton; Phase 4
	// wires these into MPC decision surface).
	Loadpoints *loadpoint.Manager

	// LoadpointCtrl is the dispatch controller. The diagnostics
	// endpoint POST /api/loadpoints/{id}/manual_hold uses it to
	// install a temporary override that bypasses the MPC budget
	// path until expiry. Nil disables the endpoint.
	LoadpointCtrl *loadpoint.Controller

	// Optional: HA MQTT bridge (nil if disabled).
	HA *ha.Bridge

	// Driver registry — used by lifecycle endpoints (restart/disable/enable)
	// and EV command dispatch. Nil disables those endpoints (returns 503).
	Registry *drivers.Registry
	// Factories mirrored from the runtime registry so /api/drivers/test can
	// run a short-lived probe without persisting config.
	DriverMQTTFactory   func(name string, c *config.MQTTConfig) (drivers.MQTTCap, error)
	DriverModbusFactory func(name string, c *config.ModbusConfig) (drivers.ModbusCap, error)
	DriverARPLookup     func(host string) (mac string, ok bool)

	// Optional: background version-check + updater-sidecar dispatch.
	// Nil disables every /api/version/* endpoint (returns 503).
	SelfUpdate *selfupdate.Checker

	// Events is the shared pub/sub bus. Nil is a safe no-op for
	// handlers that publish (e.g. /api/notifications/test).
	Events *events.Bus

	// Optional: outbound push-notification service. Nil disables
	// /api/notifications/* endpoints.
	Notifications *notifications.Service

	// Restart triggers a graceful process restart from POST /api/restart.
	// Implementations:
	//   - production (docker compose): dispatch to the ftw-updater sidecar
	//     so the running container is force-recreated against the same
	//     image — exact same code path as the post-update restart.
	//   - dev / systemd: signal main() to return with a non-zero exit
	//     code; docker (unless-stopped) and systemd (on-failure) bring
	//     the binary back up.
	// nil disables /api/restart (returns 503).
	Restart func(ctx context.Context) error

	Version string

	// PairStore holds the currently-active ftw-pair sidecar session (if
	// any). Nil is safe — routes are still registered; GET returns 404.
	// T20/T21 can reach in via deps.PairStore for SSE heartbeat support.
	PairStore *PairStatusStore

	// ---- Owner remote access (Phase 3, WebAuthn passkey) ----

	// OwnerAccessRPID is the WebAuthn Relying Party ID for owner-access
	// passkeys. Must match the hostname the browser sees. Defaults to
	// "home.fortytwowatts.com"; override to "localhost" for local dev.
	OwnerAccessRPID string

	// OwnerAccessOrigins is the list of permitted browser origins for
	// owner-access ceremonies. Defaults to ["https://"+OwnerAccessRPID].
	OwnerAccessOrigins []string

	// OwnerAccessLANBypass, when true, treats any request from a loopback
	// host (127.0.0.1, localhost, ::1) as already authenticated. Used by
	// the LAN-served dashboard so the operator doesn't have to re-auth at
	// home. NEVER enable when the server is reachable from the public
	// internet directly (without the relay in front).
	OwnerAccessLANBypass bool

	// TunnelMarker is a per-process random secret. The relay long-poll
	// reverse-proxy (cmd/forty-two-watts/owner_relay_register.go) sets it
	// as the X-FTW-Tunnel header on every request it forwards from the
	// relay to the local API server. A request carrying this exact value
	// is therefore known to have arrived via the relay tunnel (remote) and
	// MUST NOT inherit LAN-bypass — even though it lands on a loopback host.
	// Empty disables tunnel detection (pure-LAN deployments with no relay).
	TunnelMarker string

	// SiteIdentityPubHex is the uncompressed P-256 public key (X||Y, 128 hex
	// chars) of this Pi's self-sovereign ES256 identity — generated on first
	// boot regardless of Nova (see cmd/forty-two-watts/main.go). Empty if
	// identity load failed; the /api/identity endpoint then returns 503.
	SiteIdentityPubHex string

	// SiteID is this Pi's stable owner-routing identifier. New installs use an
	// opaque high-entropy "site:<token>"; legacy installs may keep "site:<name>"
	// until their encrypted directory is migrated. Published via /api/identity so
	// the browser can pin it and rebuild the canonical DTLS fingerprint signing
	// string (which binds the signature to this site).
	SiteID string

	// RelayBaseURL is the base URL of the owner-access relay this Pi registers
	// with. main.go defaults it to the official relay after remote_access opt-in
	// and lets FTW_RELAY_URL override it for self-hosted/dev relays. Used to
	// self-publish the signed instance descriptor to PUT
	// {RelayBaseURL}/bootstrap/{site_id} during the brief first-enrollment window
	// (see bootstrap_publish.go). Empty means setup links cannot be published.
	RelayBaseURL string

	// InstanceSigner is the Pi's self-sovereign ES256 identity used to sign the
	// owner-access instance descriptor (GET /api/owner-access/instance-descriptor)
	// over the P2P channel. Satisfied by *nova.Identity — the same key behind
	// SiteIdentityPubHex. Nil when identity load failed on boot; the descriptor
	// endpoint then returns 503.
	InstanceSigner InstanceSigner

	// P2P is the Pi-side WebRTC manager (Phase 5). It answers browser SDP
	// offers (POST /api/p2p/offer) and serves the resulting direct DataChannel
	// with a p2p.Bridge over the ungated API mux (injected via SetLocalAPI in
	// main.go after New). Nil is safe — the offer endpoint returns 503.
	P2P *p2p.Manager

	// ownerAccess is the lazy-initialised ceremony + session map. Built
	// on first request via Server.ownerAccess().
	ownerAccess *ownerAccessState

	// PairSelfExe overrides the binary path used by POST /api/pair/start to
	// spawn child pair sessions. Empty means "use os.Executable()". Tests
	// inject "/bin/true" (or a fake echo binary) here so they don't actually
	// launch a sidecar.
	PairSelfExe string
}

// Server wraps the http.ServeMux and adds shared middleware (logging,
// no-cache headers on static assets).
type Server struct {
	deps *Deps
	mux  *http.ServeMux

	// mu protects lazy-initialised per-server caches (currently just
	// the owner-access ceremony state on Deps.ownerAccess).
	mu sync.Mutex

	// dailyCache memoizes per-local-day energy totals keyed by "YYYY-MM-DD".
	// Past days are immutable once the day ends, so we only ever recompute
	// today. Lives for process lifetime; /api/energy/daily?days=31 drops from
	// ~30 SQL round-trips with ~500k rows shipped to Go to at most one.
	dailyCacheMu sync.Mutex
	dailyCache   map[string]state.DayEnergy

	// savingsCache mirrors dailyCache for /api/savings/daily. Same
	// immutable-past-day rationale. Lazily allocated on first request
	// because the savings endpoint is opt-in (no-op without a configured
	// price zone), so most boots never need the map.
	savingsCacheMu sync.Mutex
	savingsCache   map[string]daySavings

	versionUpdateMu sync.Mutex
}

// New creates a new API server.
func New(deps *Deps) *Server {
	if deps.Version == "" {
		deps.Version = "dev"
	}
	if deps.WebDir == "" {
		deps.WebDir = "web"
	}
	if deps.PairStore == nil {
		deps.PairStore = NewPairStatusStore()
	}
	s := &Server{
		deps:       deps,
		mux:        http.NewServeMux(),
		dailyCache: make(map[string]state.DayEnergy),
	}
	s.routes()
	return s
}

// Handler returns the http.Handler suitable for `http.ListenAndServe`.
// The mux is wrapped by the owner auth-gate so remote (relay-tunnelled)
// requests can't reach the dashboard or control endpoints without a passkey
// session; genuine LAN/loopback requests pass via LAN-bypass.
func (s *Server) Handler() http.Handler { return s.gate(s.mux) }

func (s *Server) routes() {
	// ---- JSON endpoints ----
	s.handle("GET  /api/health", s.handleHealth)
	s.handle("GET  /api/status", s.handleStatus)
	s.handle("GET  /api/system/info", s.handleSysInfo)
	s.handle("POST /api/p2p/offer", s.handleP2POffer)
	s.handle("GET  /api/config", s.handleGetConfig)
	s.handle("POST /api/config", s.handlePostConfig)
	s.handle("POST /api/drivers/verify_tesla", s.handleVerifyTesla)
	s.handle("GET /api/oauth/myuplink/start", s.handleMyUplinkOAuthStart)
	s.handle("GET /api/oauth/myuplink/callback", s.handleMyUplinkOAuthCallback)
	s.handle("POST /api/oauth/myuplink/exchange", s.handleMyUplinkOAuthExchange)
	s.handle("GET  /api/mode", s.handleGetMode)
	s.handle("POST /api/mode", s.handleSetMode)
	s.handle("POST /api/target", s.handleSetTarget)
	s.handle("POST /api/peak_limit", s.handleSetPeakLimit)
	s.handle("POST /api/peak_import_ceiling", s.handleSetPeakImportCeiling)
	s.handle("POST /api/ev_charging", s.handleSetEVCharging)
	s.handle("POST /api/battery_covers_ev", s.handleSetBatteryCoversEV)
	s.handle("GET  /api/drivers", s.handleDrivers)
	s.handle("GET  /api/drivers/catalog", s.handleDriversCatalog)
	s.handle("POST /api/drivers/test", s.handleDriverTest)
	s.handle("GET  /api/drivers/{name}", s.handleDriverDetail)
	s.handle("GET  /api/drivers/{name}/logs", s.handleDriverLogs)
	s.handle("GET  /api/logs", s.handleGlobalLogs)
	s.handle("GET  /api/support/dump", s.handleSupportDump)
	s.handle("POST /api/drivers/{name}/restart", s.handleDriverRestart)
	s.handle("POST /api/drivers/{name}/disable", s.handleDriverDisable)
	s.handle("POST /api/drivers/{name}/enable", s.handleDriverEnable)
	s.handle("GET  /api/ha/status", s.handleHAStatus)
	s.handle("GET  /api/notifications/status", s.handleNotificationsStatus)
	s.handle("GET  /api/notifications/defaults", s.handleNotificationsDefaults)
	s.handle("GET  /api/notifications/history", s.handleNotificationsHistory)
	s.handle("POST /api/notifications/test", s.handleNotificationsTest)
	s.handle("GET  /api/battery_models", s.handleGetModels)
	s.handle("POST /api/battery_models/reset", s.handleResetModel)
	s.handle("POST /api/self_tune/start", s.handleSelfTuneStart)
	s.handle("GET  /api/self_tune/status", s.handleSelfTuneStatus)
	s.handle("POST /api/self_tune/cancel", s.handleSelfTuneCancel)
	s.handle("GET  /api/history", s.handleHistory)
	s.handle("GET  /api/energy/daily", s.handleEnergyDaily)
	s.handle("GET  /api/savings/daily", s.handleSavingsDaily)
	s.handle("GET  /api/prices", s.handlePrices)
	s.handle("GET  /api/forecast", s.handleForecast)
	s.handle("GET  /api/mpc/plan", s.handleMPCPlan)
	s.handle("POST /api/mpc/replan", s.handleMPCReplan)
	s.handle("GET  /api/mpc/diagnose", s.handleMPCDiagnose)
	s.handle("GET  /api/mpc/diagnose/history", s.handleMPCDiagnoseHistory)
	s.handle("GET  /api/mpc/diagnose/at", s.handleMPCDiagnoseAt)
	s.handle("GET  /api/pvmodel", s.handlePVModel)
	s.handle("POST /api/pvmodel/reset", s.handlePVModelReset)
	s.handle("GET  /api/loadmodel", s.handleLoadModel)
	s.handle("POST /api/loadmodel/profile", s.handleLoadModelProfile)
	s.handle("POST /api/loadmodel/reset", s.handleLoadModelReset)
	s.handle("GET  /api/research/load/dump", s.handleLoadResearchDump)
	s.handle("GET  /api/series", s.handleSeries)
	s.handle("GET  /api/series/catalog", s.handleSeriesCatalog)
	s.handle("GET  /api/devices", s.handleDevices)
	s.handle("GET  /api/scan", s.handleScan)
	s.handle("GET  /api/ev/status", s.handleEVStatus)
	s.handle("POST /api/ev/command", s.handleEVCommand)
	s.handle("GET  /api/v2x/policy", s.handleV2XPolicy)
	s.handle("POST /api/v2x/command", s.handleV2XCommand)
	s.handle("POST /api/ev/chargers", s.handleEVChargers)
	s.handle("GET  /api/ev/providers", s.handleEVProviders)
	s.handle("GET  /api/loadpoints", s.handleLoadpoints)
	s.handle("POST /api/loadpoints/{id}/target", s.handleLoadpointTarget)
	s.handle("POST /api/loadpoints/{id}/soc", s.handleLoadpointSoC)
	s.handle("POST /api/loadpoints/{id}/force_start", s.handleLoadpointForceStart)
	s.handle("POST /api/loadpoints/{id}/manual_hold", s.handleLoadpointManualHold)
	s.handle("DELETE /api/loadpoints/{id}/manual_hold", s.handleLoadpointManualHoldClear)
	s.handle("GET  /api/loadpoints/{id}/manual_hold", s.handleLoadpointManualHoldGet)
	s.handle("POST /api/battery/manual_hold", s.handleBatteryManualHold)
	s.handle("DELETE /api/battery/manual_hold", s.handleBatteryManualHoldClear)
	s.handle("GET  /api/battery/manual_hold", s.handleBatteryManualHoldGet)
	s.handle("POST /api/pv/manual_hold", s.handlePVManualHold)
	s.handle("DELETE /api/pv/manual_hold", s.handlePVManualHoldClear)
	s.handle("GET  /api/pv/manual_hold", s.handlePVManualHoldGet)
	s.handle("GET  /api/version/check", s.handleVersionCheck)
	s.handle("POST /api/version/skip", s.handleVersionSkip)
	s.handle("POST /api/version/unskip", s.handleVersionUnskip)
	s.handle("POST /api/version/update", s.handleVersionUpdate)
	s.handle("POST /api/version/restart", s.handleVersionRestart)
	s.handle("GET  /api/version/update/status", s.handleVersionUpdateStatus)
	s.handle("GET  /api/version/snapshots", s.handleVersionSnapshots)
	s.handle("DELETE /api/version/snapshots/{id}", s.handleVersionSnapshotDelete)
	s.handle("POST /api/version/rollback", s.handleVersionRollback)
	s.handle("POST /api/restart", s.handleRestart)

	// ---- Pair sidecar endpoints ----
	// Pass the self-exe path so POST /api/pair/start can spawn "self pair ..."
	// as a detached child. Tests inject a fake path via deps.PairSelfExe.
	selfExe := s.deps.PairSelfExe
	if selfExe == "" {
		selfExe = resolvedSelfExe()
	}
	RegisterPairRoutes(s.mux, s.deps.PairStore, selfExe, s.authorizeOwnerManage)

	// ---- Owner remote access (Phase 3, WebAuthn passkey) ----
	s.handle("GET  /api/owner-access/enroll-pin", s.handleOwnerEnrollPin)
	s.handle("POST /api/owner-access/enroll/start", s.handleOwnerEnrollStart)
	s.handle("POST /api/owner-access/enroll/finish", s.handleOwnerEnrollFinish)
	s.handle("POST /api/owner-access/login/start", s.handleOwnerLoginStart)
	s.handle("POST /api/owner-access/login/finish", s.handleOwnerLoginFinish)
	s.handle("GET  /api/owner-access/devices", s.handleOwnerDevicesList)
	s.handle("DELETE /api/owner-access/devices/{credential_id_b64}", s.handleOwnerDeviceDelete)
	s.handle("GET  /api/owner-access/browser-keys", s.handleOwnerBrowserKeysList)
	s.handle("DELETE /api/owner-access/browser-keys/{browser_key_id}", s.handleOwnerBrowserKeyDelete)
	s.handle("GET  /api/owner-access/sessions", s.handleOwnerSessionsList)
	s.handle("DELETE /api/owner-access/sessions/{session_id}", s.handleOwnerSessionDelete)
	s.handle("GET  /api/owner-access/whoami", s.handleOwnerWhoami)
	s.handle("POST /api/owner-access/logout", s.handleOwnerLogout)
	// C3 — silent device-key PoP login over the P2P channel (open, pre-session).
	s.handle("GET  /api/owner-access/device-challenge", s.handleOwnerDeviceChallenge)
	s.handle("POST /api/owner-access/device-pop", s.handleOwnerDevicePoP)
	// Multi-tenant home route: Pi-signed instance descriptor, owner-authed,
	// served over the P2P channel (see api_owner_instance_descriptor.go).
	s.handle("GET  /api/owner-access/instance-descriptor", s.handleOwnerInstanceDescriptor)

	// ---- Self-sovereign site identity (Phase 2) ----
	s.handle("GET  /api/identity", s.handleIdentity)

	// ---- Static web UI ----
	// Everything not matched above falls through to the static server.
	s.mux.HandleFunc("/", s.handleStatic)
}

// handle wires "METHOD path" to a handler. Uses Go 1.22+ method-scoped
// routing so GET + POST on the same path can be registered independently.
func (s *Server) handle(methodPath string, h http.HandlerFunc) {
	parts := strings.SplitN(strings.TrimSpace(methodPath), " ", 2)
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	method, path := parts[0], parts[1]
	s.mux.HandleFunc(method+" "+path, h)
	_ = fmt.Sprintf // keep fmt import used elsewhere
}

// ---- Common helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("empty body")
	}
	return json.Unmarshal(body, v)
}

// ---- /api/health ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	health := s.deps.Tel.AllHealth()
	var ok, deg, off int
	for _, h := range health {
		switch h.Status {
		case telemetry.StatusOk:
			ok++
		case telemetry.StatusDegraded:
			deg++
		case telemetry.StatusOffline:
			off++
		}
	}
	status := "ok"
	if off > 0 {
		status = "degraded"
	}
	resp := map[string]any{
		"status":           status,
		"drivers_ok":       ok,
		"drivers_degraded": deg,
		"drivers_offline":  off,
	}
	// storage: surface DB corruption-recovery events from this boot so a
	// corrupt database is never a silent, blank-dashboard failure again.
	if s.deps.State != nil {
		storage := map[string]any{"state": "ok", "cache": "ok"}
		for _, ev := range s.deps.State.HealEvents() {
			storage[ev.Tier] = ev.Action // "rebuilt" | "restored"
			storage["last_event_ms"] = ev.AtMs
			storage["detail"] = ev.Detail
		}
		resp["storage"] = storage
	}
	writeJSON(w, 200, resp)
}

// ---- /api/status ----

func statusDriverOnline(tel *telemetry.Store, name string) bool {
	if tel == nil || name == "" {
		return false
	}
	h := tel.DriverHealth(name)
	return h != nil && h.IsOnline()
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.deps.CtrlMu.Lock()
	ctrl := *s.deps.Ctrl // copy for consistency
	lastTargets := append([]control.DispatchTarget{}, s.deps.Ctrl.LastTargets...)
	s.deps.CtrlMu.Unlock()

	s.deps.CapMu.RLock()
	caps := make(map[string]float64, len(s.deps.Capacities))
	for k, v := range s.deps.Capacities {
		caps[k] = v
	}
	s.deps.CapMu.RUnlock()

	// Aggregate live readings. Offline readings stay in telemetry so detailed
	// driver views can show the last known value, but they must not leak into
	// the live site balance.
	gridW := 0.0
	haveGrid := false
	if statusDriverOnline(s.deps.Tel, ctrl.SiteMeterDriver) {
		if r := s.deps.Tel.Get(ctrl.SiteMeterDriver, telemetry.DerMeter); r != nil {
			gridW = r.SmoothedW
			haveGrid = true
		}
	}
	if !haveGrid && ctrl.SiteMeterDriver == "" {
		// Preserve the historical "no configured site meter" behaviour for
		// development setups: report zero rather than treating it as stale data.
		haveGrid = true
	}
	var pvW, batW float64
	for _, r := range s.deps.Tel.ReadingsByType(telemetry.DerPV) {
		if !statusDriverOnline(s.deps.Tel, r.Driver) {
			continue
		}
		pvW += r.SmoothedW
	}
	for _, r := range s.deps.Tel.ReadingsByType(telemetry.DerBattery) {
		if !statusDriverOnline(s.deps.Tel, r.Driver) {
			continue
		}
		batW += r.SmoothedW
	}

	// Load = house-only consumption in site convention (+ into site):
	//   meter = load + ev + v2x + battery + pv
	//   so load = grid - bat - pv - ev - v2x
	// Subtracting vehicle charging/storage keeps the load signal (and
	// the loadmodel trained on it) reflecting the house, not "house + car".
	evW := s.deps.Tel.SumOnlineEVW()
	v2xW := s.deps.Tel.SumOnlineV2XW()
	loadW := 0.0
	if haveGrid {
		rawLoad := gridW - batW - pvW - evW - v2xW
		loadW = s.deps.Tel.UpdateLoad(rawLoad)
		if loadW < 0 {
			loadW = 0
		}
	}

	// Weighted average SoC by capacity
	var totalCap, weightedSoC float64
	for _, b := range s.deps.Tel.ReadingsByType(telemetry.DerBattery) {
		if !statusDriverOnline(s.deps.Tel, b.Driver) {
			continue
		}
		cap, ok := caps[b.Driver]
		if !ok {
			continue
		}
		totalCap += cap
		soc := 0.0
		if b.SoC != nil {
			soc = *b.SoC
		}
		weightedSoC += soc * cap
	}
	avgSoC := 0.0
	if totalCap > 0 {
		avgSoC = weightedSoC / totalCap
	}

	// Per-driver details
	drivers := make(map[string]any)
	for name, h := range s.deps.Tel.AllHealth() {
		d := map[string]any{
			"status":             h.Status.String(),
			"consecutive_errors": h.ConsecutiveErrors,
			"tick_count":         h.TickCount,
		}
		if h.LastError != "" {
			d["last_error"] = h.LastError
		}
		if r := s.deps.Tel.Get(name, telemetry.DerMeter); r != nil {
			d["meter_w"] = r.SmoothedW
		}
		if r := s.deps.Tel.Get(name, telemetry.DerPV); r != nil {
			d["pv_w"] = r.SmoothedW
		}
		if r := s.deps.Tel.Get(name, telemetry.DerBattery); r != nil {
			d["bat_w"] = r.SmoothedW
			if r.SoC != nil {
				d["bat_soc"] = *r.SoC
			}
		}
		// Vehicle (DerVehicle) — read-only BMS readings emitted by
		// drivers like tesla_vehicle.lua. Surfaced so the per-driver
		// card can render SoC + charge_limit + charging_state. RawW
		// is always 0 for vehicle readings (no power channel).
		if r := s.deps.Tel.Get(name, telemetry.DerVehicle); r != nil {
			var v struct {
				SoC                  *float64 `json:"soc"`
				ChargeLimitPct       *float64 `json:"charge_limit_pct"`
				ChargingState        *string  `json:"charging_state"`
				TimeToFullMin        *int     `json:"time_to_full_min"`
				ChargeAmps           *float64 `json:"charge_amps"`
				ChargerActualCurrent *float64 `json:"charger_actual_current"`
				Stale                *bool    `json:"stale"`
			}
			if r.Data != nil && json.Unmarshal(r.Data, &v) == nil {
				if v.SoC != nil {
					d["vehicle_soc"] = *v.SoC
				}
				if v.ChargeLimitPct != nil {
					d["vehicle_charge_limit_pct"] = *v.ChargeLimitPct
				}
				if v.ChargingState != nil {
					d["vehicle_charging_state"] = *v.ChargingState
				}
				if v.TimeToFullMin != nil {
					d["vehicle_time_to_full_min"] = *v.TimeToFullMin
				}
				if v.ChargeAmps != nil {
					d["vehicle_charge_amps"] = *v.ChargeAmps
				}
				if v.ChargerActualCurrent != nil {
					d["vehicle_charger_actual_current"] = *v.ChargerActualCurrent
				}
				if v.Stale != nil {
					d["vehicle_stale"] = *v.Stale
				}
			}
		}
		if r := s.deps.Tel.Get(name, telemetry.DerEV); r != nil {
			d["ev_w"] = r.SmoothedW
			// Surface the structured fields the driver put in Data so the
			// UI can render a proper EV card (plug state, reason, limits).
			// All labels are rendered by the driver itself — the UI
			// just displays strings verbatim. Codes are also surfaced
			// for anyone who wants to filter/route on them.
			var ev struct {
				Connected            *bool    `json:"connected"`
				Charging             *bool    `json:"charging"`
				SessionWh            *float64 `json:"session_wh"`
				OpMode               *int     `json:"op_mode"`
				StateLabel           *string  `json:"state_label"`
				ReasonNoCurrent      *int     `json:"reason_no_current"`
				ReasonNoCurrentLabel *string  `json:"reason_no_current_label"`
				IsOnline             *bool    `json:"is_online"`
				CableLocked          *bool    `json:"cable_locked"`
				MaxA                 *float64 `json:"max_a"`
				Phases               *int     `json:"phases"`
				ActualAmpsPerPhase   *float64 `json:"actual_amps_per_phase"`
			}
			if r.Data != nil && json.Unmarshal(r.Data, &ev) == nil {
				if ev.Connected != nil {
					d["ev_connected"] = *ev.Connected
				}
				if ev.Charging != nil {
					d["ev_charging"] = *ev.Charging
				}
				if ev.SessionWh != nil {
					d["ev_session_wh"] = *ev.SessionWh
				}
				if ev.OpMode != nil {
					d["ev_op_mode"] = *ev.OpMode
				}
				if ev.StateLabel != nil {
					d["ev_state_label"] = *ev.StateLabel
				}
				if ev.ReasonNoCurrent != nil {
					d["ev_reason_no_current"] = *ev.ReasonNoCurrent
				}
				if ev.ReasonNoCurrentLabel != nil {
					d["ev_reason_no_current_label"] = *ev.ReasonNoCurrentLabel
				}
				if ev.IsOnline != nil {
					d["ev_is_online"] = *ev.IsOnline
				}
				if ev.CableLocked != nil {
					d["ev_cable_locked"] = *ev.CableLocked
				}
				if ev.MaxA != nil {
					d["ev_max_a"] = *ev.MaxA
				}
				if ev.Phases != nil {
					d["ev_phases"] = *ev.Phases
				}
				if ev.ActualAmpsPerPhase != nil {
					d["ev_actual_amps_per_phase"] = *ev.ActualAmpsPerPhase
				}
			}
		}
		if r := s.deps.Tel.Get(name, telemetry.DerV2X); r != nil {
			d["v2x_w"] = r.SmoothedW
			if r.SoC != nil {
				d["v2x_vehicle_soc"] = *r.SoC
			}
			var v2x struct {
				Connected          *bool    `json:"connected"`
				PlugConnected      *bool    `json:"plug_connected"`
				VehicleSoC         *float64 `json:"vehicle_soc"`
				VehicleSoCFract    *float64 `json:"vehicle_soc_fract"`
				DCW                *float64 `json:"dc_w"`
				DCV                *float64 `json:"dc_v"`
				DCA                *float64 `json:"dc_a"`
				SessionChargeWh    *float64 `json:"session_charge_wh"`
				SessionDischargeWh *float64 `json:"session_discharge_wh"`
				TotalChargeWh      *float64 `json:"total_charge_wh"`
				TotalDischargeWh   *float64 `json:"total_discharge_wh"`
				ChargePowerMinW    *float64 `json:"charge_power_min_w"`
				ChargePowerMaxW    *float64 `json:"charge_power_max_w"`
				DischargePowerMinW *float64 `json:"discharge_power_min_w"`
				DischargePowerMaxW *float64 `json:"discharge_power_max_w"`
				EVMaxEnergyReqWh   *float64 `json:"ev_max_energy_req_wh"`
				EVMinEnergyReqWh   *float64 `json:"ev_min_energy_req_wh"`
				CapacityWh         *float64 `json:"capacity_wh"`
				RatedPowerW        *float64 `json:"rated_power_w"`
				Status             *string  `json:"status"`
				ControlMode        *string  `json:"control_mode"`
				Protocol           *string  `json:"protocol"`
			}
			if r.Data != nil && json.Unmarshal(r.Data, &v2x) == nil {
				if v2x.Connected != nil {
					d["v2x_connected"] = *v2x.Connected
				} else if v2x.PlugConnected != nil {
					d["v2x_connected"] = *v2x.PlugConnected
				}
				if v2x.VehicleSoC != nil {
					d["v2x_vehicle_soc"] = *v2x.VehicleSoC
				} else if v2x.VehicleSoCFract != nil {
					d["v2x_vehicle_soc"] = *v2x.VehicleSoCFract
				}
				if v2x.DCW != nil {
					d["v2x_dc_w"] = *v2x.DCW
				}
				if v2x.DCV != nil {
					d["v2x_dc_v"] = *v2x.DCV
				}
				if v2x.DCA != nil {
					d["v2x_dc_a"] = *v2x.DCA
				}
				if v2x.SessionChargeWh != nil {
					d["v2x_session_charge_wh"] = *v2x.SessionChargeWh
				}
				if v2x.SessionDischargeWh != nil {
					d["v2x_session_discharge_wh"] = *v2x.SessionDischargeWh
				}
				if v2x.TotalChargeWh != nil {
					d["v2x_total_charge_wh"] = *v2x.TotalChargeWh
				}
				if v2x.TotalDischargeWh != nil {
					d["v2x_total_discharge_wh"] = *v2x.TotalDischargeWh
				}
				if v2x.ChargePowerMinW != nil {
					d["v2x_charge_power_min_w"] = *v2x.ChargePowerMinW
				}
				if v2x.ChargePowerMaxW != nil {
					d["v2x_charge_power_max_w"] = *v2x.ChargePowerMaxW
				}
				if v2x.DischargePowerMinW != nil {
					d["v2x_discharge_power_min_w"] = *v2x.DischargePowerMinW
				}
				if v2x.DischargePowerMaxW != nil {
					d["v2x_discharge_power_max_w"] = *v2x.DischargePowerMaxW
				}
				if v2x.EVMaxEnergyReqWh != nil {
					d["v2x_ev_max_energy_req_wh"] = *v2x.EVMaxEnergyReqWh
				}
				if v2x.EVMinEnergyReqWh != nil {
					d["v2x_ev_min_energy_req_wh"] = *v2x.EVMinEnergyReqWh
				}
				if v2x.CapacityWh != nil {
					d["v2x_capacity_wh"] = *v2x.CapacityWh
				}
				if v2x.RatedPowerW != nil {
					d["v2x_rated_power_w"] = *v2x.RatedPowerW
				}
				if v2x.Status != nil {
					d["v2x_status"] = *v2x.Status
				}
				if v2x.ControlMode != nil {
					d["v2x_control_mode"] = *v2x.ControlMode
				}
				if v2x.Protocol != nil {
					d["v2x_protocol"] = *v2x.Protocol
				}
			}
		}
		drivers[name] = d
	}
	// Merge config drivers that aren't in the registry so the UI can
	// render them with a Restart or Enable button. A driver can be absent
	// from the registry because (a) it's disabled in yaml, or (b) the
	// initial Add failed (e.g. cloud auth error). Running drivers already
	// populated above take precedence.
	s.deps.CfgMu.RLock()
	for _, dc := range s.deps.Cfg.Drivers {
		if _, ok := drivers[dc.Name]; ok {
			continue
		}
		if dc.Disabled {
			drivers[dc.Name] = map[string]any{
				"status":   "disabled",
				"disabled": true,
			}
		} else {
			// Configured but not running — spawn probably failed. Show
			// as offline so the user sees it and can Restart.
			drivers[dc.Name] = map[string]any{
				"status":      "offline",
				"not_running": true,
			}
		}
	}
	s.deps.CfgMu.RUnlock()

	// Dispatch targets
	dispatch := make([]map[string]any, 0, len(lastTargets))
	for _, t := range lastTargets {
		dispatch = append(dispatch, map[string]any{
			"driver":   t.Driver,
			"target_w": t.TargetW,
			"clamped":  t.Clamped,
		})
	}

	var pvPredictW, loadPredictW float64
	if s.deps.PVModel != nil {
		pvPredictW = -s.deps.PVModel.PredictNow() // site-sign: negative
	}
	if s.deps.LoadModel != nil {
		loadPredictW = s.deps.LoadModel.Predict(time.Now())
	}

	// Energy today: integrate history points since midnight local time.
	// Keep this on the SQL aggregate path instead of loading raw history
	// rows: /api/status polls every 2 s, and reading every sample since
	// midnight made late-day dashboard loads visibly stall on Pi-sized DBs.
	//
	// Current slot: same integration over the fixed local 15-minute
	// settlement window (00/15/30/45). This is deliberately observational:
	// it lets the UI show whether second-to-second import/export is material
	// over the billing window without changing dispatch semantics.
	var energyToday map[string]any
	var energyCurrentSlot map[string]any
	if s.deps.State != nil {
		now := time.Now()
		midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		d, err := s.deps.State.DailyEnergy(midnight.UnixMilli(), now.UnixMilli())
		if err == nil {
			// Only surface today's totals once at least one integration
			// interval exists. Right after local midnight the range has
			// 0–1 history rows, so the SQL COALESCEs every sum to a vacuous
			// 0; rendering that as a hard "0 Wh" looks like a real reading
			// instead of "no data yet". Mirror the old `len(pts) > 1` guard.
			if d.Intervals > 0 {
				energyToday = map[string]any{
					"import_wh":         d.ImportWh,
					"export_wh":         d.ExportWh,
					"pv_wh":             d.PVWh,
					"bat_charged_wh":    d.BatChargedWh,
					"bat_discharged_wh": d.BatDischargedWh,
					"load_wh":           d.LoadWh,
				}
			}
		} else {
			slog.Warn("failed to integrate today's energy", "err", err)
		}
		slot, err := currentGridEnergySlot(s.deps.State, now)
		if err == nil {
			energyCurrentSlot = slot
		} else {
			slog.Warn("failed to integrate current grid energy slot", "err", err)
		}
	}

	// Fuse + site meter details (used by the dashboard to render per-phase
	// amperage bars). We expose the fuse config verbatim so the frontend
	// doesn't need a second /api/config fetch, and pull per-phase readings
	// from the site meter driver's raw emit payload.
	s.deps.CfgMu.RLock()
	troubleshootingMode := s.deps.Cfg.Site.TroubleshootingMode
	fuseCfg := map[string]any{
		"max_amps": s.deps.Cfg.Fuse.MaxAmps,
		"phases":   s.deps.Cfg.Fuse.Phases,
		"voltage":  s.deps.Cfg.Fuse.Voltage,
	}
	s.deps.CfgMu.RUnlock()

	phaseAmps := siteMeterPhaseAmps(s.deps.Tel, ctrl.SiteMeterDriver)
	var v2xGridW *float64
	if haveGrid {
		gridCopy := gridW
		v2xGridW = &gridCopy
	}
	v2xPolicy := s.v2xPolicyStatus(v2xGridW)

	resp := map[string]any{
		"version":               s.deps.Version,
		"mode":                  ctrl.Mode,
		"troubleshooting_mode":  troubleshootingMode,
		"plan_stale":            ctrl.PlanStale,
		"grid_w":                gridW,
		"pv_w":                  pvW,
		"pv_w_predicted":        pvPredictW,
		"bat_w":                 batW,
		"ev_w":                  evW,
		"v2x_w":                 v2xW,
		"load_w":                loadW,
		"load_w_predicted":      loadPredictW,
		"bat_soc":               avgSoC,
		"grid_target_w":         ctrl.GridTargetW,
		"peak_limit_w":          ctrl.PeakLimitW,
		"peak_import_ceiling_w": ctrl.PeakImportCeilingW,
		"ev_charging_w":         ctrl.EVChargingW,
		"battery_covers_ev":     ctrl.BatteryCoversEV,
		// True when an EV charger password is persisted in state.db. The
		// Settings UI uses this to show a "credentials saved" badge so the
		// operator can tell apart "never entered" from "saved but masked".
		"ev_credentials_saved": func() bool {
			if s.deps.State == nil {
				return false
			}
			pw, ok := s.deps.State.LoadConfig(evPasswordKey)
			return ok && pw != ""
		}(),
		"fuse":         fuseCfg,
		"phase_amps":   phaseAmps,
		"phase_powers": siteMeterPhasePowers(s.deps.Tel, ctrl.SiteMeterDriver),
		"v2x_policy":   v2xPolicy,
		"drivers":      drivers,
		"dispatch":     dispatch,
		// Observability counters for the per-slot Wh tracker. Pure
		// diagnostic — incremented when actual fleet delivery diverges
		// from the plan's BatteryEnergyWh by > 50 % (over) or < 50 %
		// (under). Idle slots (|planned| ≤ 50 Wh) are ignored.
		"slot_delivery_stats": ctrl.SlotDeliveryStats,
	}
	if energyToday != nil || energyCurrentSlot != nil {
		energy := map[string]any{}
		if energyToday != nil {
			energy["today"] = energyToday
		}
		if energyCurrentSlot != nil {
			energy["current_slot"] = energyCurrentSlot
		}
		resp["energy"] = energy
	}
	writeJSON(w, 200, resp)
}

// currentGridEnergySlot integrates per-direction grid energy across the
// active 15-minute settlement window. Under 15-min settlement the bill is
//
//	import_wh × import_price  +  export_wh × export_price
//
// — import and export are independent accumulators, never netted.
// `net_wh` is kept as a backwards-compat observational delta only; UI and
// downstream consumers MUST render or price import_wh and export_wh
// separately, never `net_wh` alone.
func currentGridEnergySlot(st *state.Store, now time.Time) (map[string]any, error) {
	slotStart := now.Truncate(15 * time.Minute)
	slotEnd := slotStart.Add(15 * time.Minute)
	d, err := st.DailyEnergy(slotStart.UnixMilli(), now.UnixMilli())
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"slot_start_ms": slotStart.UnixMilli(),
		"slot_end_ms":   slotEnd.UnixMilli(),
		"elapsed_s":     now.Sub(slotStart).Seconds(),
		"import_wh":     d.ImportWh,
		"export_wh":     d.ExportWh,
		// Observational only — see comment above. Do not bill against this.
		"net_wh": d.ImportWh - d.ExportWh,
	}, nil
}

// siteMeterPhaseAmps pulls per-phase L1/L2/L3 current (in amps) from the
// site meter driver's emit payload. Returns an empty slice if the site
// meter isn't reporting per-phase data — the frontend falls back to a
// total-amps bar in that case. Signed: negative = export on that phase.
func siteMeterPhaseAmps(tel *telemetry.Store, siteMeter string) []float64 {
	if siteMeter == "" {
		return nil
	}
	r := tel.Get(siteMeter, telemetry.DerMeter)
	if r == nil || len(r.Data) == 0 {
		return nil
	}
	var payload struct {
		L1A *float64 `json:"l1_a"`
		L2A *float64 `json:"l2_a"`
		L3A *float64 `json:"l3_a"`
	}
	if err := json.Unmarshal(r.Data, &payload); err != nil {
		return nil
	}
	out := make([]float64, 0, 3)
	if payload.L1A != nil {
		out = append(out, *payload.L1A)
	}
	if payload.L2A != nil {
		out = append(out, *payload.L2A)
	}
	if payload.L3A != nil {
		out = append(out, *payload.L3A)
	}
	return out
}

// siteMeterPhasePowers pulls per-phase L1/L2/L3 active power (W) from
// the site meter driver's emit payload. Mirrors siteMeterPhaseAmps —
// signed values, negative = export on that phase. UI uses these to
// display a per-phase W reading next to the per-phase A bar so the
// operator can see one phase importing while another exports
// (typical when a 1Φ EV is on L1 and PV is balanced across L2/L3).
func siteMeterPhasePowers(tel *telemetry.Store, siteMeter string) []float64 {
	if siteMeter == "" {
		return nil
	}
	r := tel.Get(siteMeter, telemetry.DerMeter)
	if r == nil || len(r.Data) == 0 {
		return nil
	}
	var payload struct {
		L1W *float64 `json:"l1_w"`
		L2W *float64 `json:"l2_w"`
		L3W *float64 `json:"l3_w"`
	}
	if err := json.Unmarshal(r.Data, &payload); err != nil {
		return nil
	}
	out := make([]float64, 0, 3)
	if payload.L1W != nil {
		out = append(out, *payload.L1W)
	}
	if payload.L2W != nil {
		out = append(out, *payload.L2W)
	}
	if payload.L3W != nil {
		out = append(out, *payload.L3W)
	}
	return out
}

// ---- /api/config ----

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	s.deps.CfgMu.RLock()
	cfg := *s.deps.Cfg
	s.deps.CfgMu.RUnlock()
	masked := cfg.MaskSecrets()
	// Strip resolved driver paths back to config-relative form so the UI
	// doesn't display (and round-trip) paths like "../drivers/foo.lua".
	masked.UnresolveDriverPaths(filepath.Dir(s.deps.ConfigPath))
	// EV charger password lives in state.db, not YAML. Signal to the UI
	// that a password is set by using a masked placeholder (MaskSecrets
	// blanked it to "").
	if masked.EVCharger != nil {
		if pw, ok := s.deps.State.LoadConfig(evPasswordKey); ok && pw != "" {
			cp := *masked.EVCharger
			cp.Password = maskedPlaceholder
			masked.EVCharger = &cp
		}
	}
	// Mask driver-declared config_secrets (e.g. sonnen api_token) so
	// the UI never sees the plaintext token in /api/config. The
	// settings tab renders an empty input + "Saved" badge; on POST the
	// PreserveMaskedSecrets pass restores the real value when the
	// browser sends back the placeholder (or an empty string).
	maskDriverConfigSecrets(&masked, s.driverSecretKeys())
	writeJSON(w, 200, masked)
}

// driverSecretKeys returns a map[lua-path]→[]secret-key built from the
// drivers/ catalog. Used by handleGetConfig + handlePostConfig to scope
// which `Driver.Config[*]` keys participate in the mask/restore cycle.
// On catalog read errors returns nil — handlers then skip the secrets
// pass entirely (fail-open: they still mask the structured fields).
func (s *Server) driverSecretKeys() map[string][]string {
	dir := s.deps.DriverDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(s.deps.ConfigPath), "drivers")
	}
	entries, err := drivers.LoadCatalogMulti(s.deps.UserDriverDir, dir)
	if err != nil {
		return nil
	}
	out := make(map[string][]string, len(entries))
	for _, e := range entries {
		if len(e.ConfigSecrets) == 0 {
			continue
		}
		path := filepath.ToSlash(e.Path)
		out[path] = e.ConfigSecrets
		base := filepath.ToSlash(filepath.Base(dir))
		if rel, ok := strings.CutPrefix(path, base+"/"); ok {
			// Config round-trips paths resolved via -drivers as
			// "drivers/<rel>" regardless of the actual directory name.
			// Keep catalog secret matching on that portable alias too.
			out[filepath.ToSlash(filepath.Join("drivers", rel))] = e.ConfigSecrets
		}
	}
	return out
}

// maskDriverConfigSecrets walks each driver in `cfg.Drivers` and, for
// every key listed in the catalog's config_secrets for that driver,
// replaces a non-empty stored value with maskedPlaceholder. Mirrors
// MaskSecrets for fields the config package can't know about (the
// catalog isn't a config-package dependency on purpose).
func maskDriverConfigSecrets(cfg *config.Config, secretsByLua map[string][]string) {
	if cfg == nil || len(secretsByLua) == 0 {
		return
	}
	for i := range cfg.Drivers {
		keys := secretsByLua[cfg.Drivers[i].Lua]
		if len(keys) == 0 || cfg.Drivers[i].Config == nil {
			continue
		}
		// Defensive copy so we don't mutate the live cfg.Drivers map
		// (callers pass a value copy of Config, but the inner Config
		// map is by-reference).
		cp := make(map[string]any, len(cfg.Drivers[i].Config))
		for k, v := range cfg.Drivers[i].Config {
			cp[k] = v
		}
		for _, k := range keys {
			if v, ok := cp[k]; ok {
				if s, ok := v.(string); ok && s != "" {
					cp[k] = maskedPlaceholder
				}
			}
		}
		cfg.Drivers[i].Config = cp
	}
}

// restoreDriverConfigSecrets is the symmetric POST-side step: for each
// driver, any catalog-declared secret value the UI sent back as the
// masked placeholder OR as an empty string (with a non-empty existing
// value) gets restored from `existing`. Without this, blanking the
// password input in the Settings tab would clobber the saved token.
func restoreDriverConfigSecrets(incoming, existing *config.Config, secretsByLua map[string][]string) {
	if incoming == nil || existing == nil || len(secretsByLua) == 0 {
		return
	}
	for i := range incoming.Drivers {
		keys := secretsByLua[incoming.Drivers[i].Lua]
		if len(keys) == 0 {
			continue
		}
		// Match the existing driver by Name (same key PreserveMaskedSecrets uses).
		var ed *config.Driver
		for j := range existing.Drivers {
			if existing.Drivers[j].Name == incoming.Drivers[i].Name {
				ed = &existing.Drivers[j]
				break
			}
		}
		if ed == nil || ed.Config == nil {
			continue
		}
		if incoming.Drivers[i].Config == nil {
			incoming.Drivers[i].Config = map[string]any{}
		}
		for _, k := range keys {
			existingV, hasE := ed.Config[k]
			if !hasE {
				continue
			}
			existingS, _ := existingV.(string)
			if existingS == "" {
				continue
			}
			incomingV, hasI := incoming.Drivers[i].Config[k]
			incomingS, _ := incomingV.(string)
			if !hasI || incomingS == "" || incomingS == maskedPlaceholder {
				incoming.Drivers[i].Config[k] = existingS
			}
		}
	}
}

func (s *Server) handlePostConfig(w http.ResponseWriter, r *http.Request) {
	var newCfg config.Config
	if err := readJSON(r, &newCfg); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid config: " + err.Error()})
		return
	}
	// Preserve secrets the UI sent back as empty (masked) values.
	s.deps.CfgMu.RLock()
	newCfg.PreserveMaskedSecrets(s.deps.Cfg)
	// Restore catalog-declared driver secrets (api_token etc.) the UI
	// returned as maskedPlaceholder or empty. Same semantics as
	// PreserveMaskedSecrets but scoped to keys the driver itself
	// declared via DRIVER.config_secrets — keeps config-package
	// catalog-agnostic.
	restoreDriverConfigSecrets(&newCfg, s.deps.Cfg, s.driverSecretKeys())
	s.deps.CfgMu.RUnlock()

	// EV charger password lives in state.db instead of config.yaml. Empty
	// or the masked placeholder means "keep existing"; a new value means
	// the user typed a real password. Defer the state write until after
	// validation + config save succeed so a rejected config cannot rotate
	// credentials behind the operator's back.
	var evPasswordToPersist string
	var persistEVPassword bool
	if newCfg.EVCharger != nil {
		pw := newCfg.EVCharger.Password
		if pw != "" && pw != maskedPlaceholder {
			evPasswordToPersist = pw
			persistEVPassword = true
		} else if stored, ok := s.deps.State.LoadConfig(evPasswordKey); ok {
			// Restore the real password into the candidate config so the
			// config-reload watcher sees it on the next apply.
			newCfg.EVCharger.Password = stored
		}
	}

	if err := newCfg.Validate(); err != nil {
		writeJSON(w, 400, map[string]string{"error": "validation: " + err.Error()})
		return
	}
	// Diff against the live config BEFORE we mutate the shared pointer —
	// otherwise the comparison would always come back empty.
	s.deps.CfgMu.RLock()
	oldCfg := *s.deps.Cfg
	s.deps.CfgMu.RUnlock()
	restartReasons := config.RestartRequiredFor(&oldCfg, &newCfg)

	// Persist atomically (Password has yaml:"-" so it won't appear in YAML)
	if err := s.deps.SaveConfig(s.deps.ConfigPath, &newCfg); err != nil {
		writeJSON(w, 500, map[string]string{"error": "save failed: " + err.Error()})
		return
	}
	if persistEVPassword {
		if err := s.deps.State.SaveConfig(evPasswordKey, evPasswordToPersist); err != nil {
			slog.Warn("failed to persist ev_charger_password", "err", err)
		}
	}
	// Apply control-level changes immediately (file watcher will also pick
	// this up but we're snappier).
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.SetGridTarget(newCfg.Site.GridTargetW)
	s.deps.Ctrl.GridToleranceW = newCfg.Site.GridToleranceW
	s.deps.Ctrl.SlewRateW = newCfg.Site.SlewRateW
	s.deps.Ctrl.MinDispatchIntervalS = newCfg.Site.MinDispatchIntervalS
	s.deps.Ctrl.PVSurplusAbsorbSoCCapPct = newCfg.Site.PVSurplusAbsorbSoCCapPct
	s.deps.Ctrl.PVSurplusAbsorbThresholdW = newCfg.Site.PVSurplusAbsorbThresholdW
	s.deps.CtrlMu.Unlock()
	if s.deps.Registry != nil {
		s.deps.Registry.Reload(r.Context(), newCfg.Drivers, newCfg.Site.TroubleshootingMode)
	}
	// Update shared cfg pointer
	s.deps.CfgMu.Lock()
	*s.deps.Cfg = newCfg
	s.deps.CfgMu.Unlock()
	slog.Info("config updated via API", "restart_required", len(restartReasons) > 0)
	writeJSON(w, 200, map[string]any{
		"status":           "ok",
		"restart_required": len(restartReasons) > 0,
		"restart_reasons":  restartReasons,
	})
}

// ---- /api/mode ----

func (s *Server) handleGetMode(w http.ResponseWriter, r *http.Request) {
	s.deps.CtrlMu.Lock()
	defer s.deps.CtrlMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"mode":          s.deps.Ctrl.Mode,
		"grid_target_w": s.deps.Ctrl.GridTargetW,
	})
}

func (s *Server) handleSetMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	m := control.Mode(req.Mode)
	// Validate against the canonical mode list. control.AllModes is the
	// single source of truth — the same list the HA discovery `select`
	// options derive from — so the validator and the HA bridge can't drift.
	if !control.IsValidMode(m) {
		writeJSON(w, 400, map[string]string{"error": "unknown mode: " + req.Mode})
		return
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.Mode = m
	// An explicit mode change is a reset signal: drop any active
	// battery manual hold so the new mode takes effect on the very
	// next dispatch tick. Mirrors the loadpoint manual_hold UX.
	s.deps.Ctrl.ClearBatteryManualHold()
	// Reset the PI integrator. The integral accumulated under the
	// previous mode's error signal is meaningless to the new mode
	// — keeping it caused integrator windup → wrong-direction stuck
	// output across the 2026-05-24 evening mode switch (live
	// regression: discharged the fleet to 7 % overnight while the
	// PI integral was pinned in the wrong direction). Mode change
	// is a discrete event; start the new regime from a clean PI.
	if s.deps.Ctrl.PI != nil {
		s.deps.Ctrl.PI.Reset()
	}
	s.deps.CtrlMu.Unlock()
	if err := s.deps.State.SaveConfig("mode", req.Mode); err != nil {
		slog.Warn("failed to persist mode", "err", err)
	}
	// Propagate to MPC if switching to a planner mode and force an
	// immediate replan. control.PlannerMPCMode is the single source of the
	// ModePlanner* → mpc.Mode mapping; ok is false for non-planner modes (and
	// for any planner mode that hasn't been wired into the mapping), so an
	// unmapped mode skips the MPC push instead of silently coercing it to the
	// zero-value mpc.Mode("").
	if mm, ok := control.PlannerMPCMode(m); ok && s.deps.MPC != nil {
		s.deps.MPC.SetMode(r.Context(), mm)
	}
	writeJSON(w, 200, map[string]string{"status": "ok", "mode": req.Mode})
}

// ---- /api/target ----

func (s *Server) handleSetTarget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		GridTargetW float64 `json:"grid_target_w"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.SetGridTarget(req.GridTargetW)
	s.deps.CtrlMu.Unlock()
	if err := s.deps.State.SaveConfig("grid_target_w", strconv.FormatFloat(req.GridTargetW, 'f', 1, 64)); err != nil {
		slog.Warn("failed to persist grid_target_w", "err", err)
	}
	slog.Info("grid target changed", "w", req.GridTargetW)
	writeJSON(w, 200, map[string]any{"status": "ok", "grid_target_w": req.GridTargetW})
}

// ---- /api/peak_limit ----

func (s *Server) handleSetPeakLimit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeakLimitW float64 `json:"peak_limit_w"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.PeakLimitW = req.PeakLimitW
	s.deps.CtrlMu.Unlock()
	writeJSON(w, 200, map[string]any{"status": "ok", "peak_limit_w": req.PeakLimitW})
}

// ---- /api/peak_import_ceiling ----
//
// Hard import ceiling enforced in every mode. Default 0 = disabled. When
// > 0, dispatch's import-side clamps (joint EV/battery allocator,
// applyFuseGuard import branch, forceFuseDischarge) use min(fuse, peak)
// as the binding threshold. Persisted in state.db so the operator's
// tariff stays armed across restarts. See control.State.PeakImportCeilingW
// for the full rationale.
func (s *Server) handleSetPeakImportCeiling(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeakImportCeilingW float64 `json:"peak_import_ceiling_w"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.PeakImportCeilingW < 0 {
		writeJSON(w, 400, map[string]string{"error": "peak_import_ceiling_w must be ≥ 0"})
		return
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.PeakImportCeilingW = req.PeakImportCeilingW
	s.deps.CtrlMu.Unlock()
	if s.deps.State != nil {
		_ = s.deps.State.SaveConfig("peak_import_ceiling_w", strconv.FormatFloat(req.PeakImportCeilingW, 'f', 1, 64))
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "peak_import_ceiling_w": req.PeakImportCeilingW})
}

// ---- /api/ev_charging ----

func (s *Server) handleSetEVCharging(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PowerW float64 `json:"power_w"`
		Active bool    `json:"active"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.SetManualEVCharging(req.PowerW, req.Active)
	s.deps.CtrlMu.Unlock()
	writeJSON(w, 200, map[string]any{"status": "ok", "ev_charging_w": req.PowerW})
}

// ---- /api/battery_covers_ev ----
//
// When enabled, dispatch skips its usual subtraction of EVChargingW from
// the meter reading so batteries discharge into the EV. Default off
// preserves the traditional "battery never feeds the car" behaviour.
// See control.State.BatteryCoversEV for the full rationale.
func (s *Server) handleSetBatteryCoversEV(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.CtrlMu.Lock()
	s.deps.Ctrl.BatteryCoversEV = req.Enabled
	s.deps.CtrlMu.Unlock()
	if s.deps.State != nil {
		val := "false"
		if req.Enabled {
			val = "true"
		}
		_ = s.deps.State.SaveConfig("battery_covers_ev", val)
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "battery_covers_ev": req.Enabled})
}

// ---- /api/drivers ----

func (s *Server) handleDrivers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.deps.Tel.AllHealth())
}

// GET /api/drivers/catalog — list of available drivers from the
// drivers/ directory, parsed from each .lua file's DRIVER metadata.
// Used by the Settings UI to offer an "Add from catalog" dropdown.
func (s *Server) handleDriversCatalog(w http.ResponseWriter, r *http.Request) {
	// Default is next to the config file; overridable via -drivers for
	// deployments (Docker) where drivers live in the image, not the data volume.
	dir := s.deps.DriverDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(s.deps.ConfigPath), "drivers")
	}
	// User-drivers dir (persistent volume) takes precedence over bundled dir.
	entries, err := drivers.LoadCatalogMulti(s.deps.UserDriverDir, dir)
	if err != nil {
		writeJSON(w, 200, map[string]any{"path": dir, "entries": []any{}, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"path": dir, "entries": entries})
}

// POST /api/drivers/{name}/restart — stop + re-add the driver so it
// re-runs driver_init. Useful to force a cloud driver to re-authenticate
// after you've updated credentials without restarting the whole process.
func (s *Server) handleDriverRestart(w http.ResponseWriter, r *http.Request) {
	if s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "registry unavailable"})
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "missing driver name"})
		return
	}
	// Look up the latest config from the in-memory cfg so the restart
	// picks up anything the UI just changed (e.g. a new EV password
	// injected from state.db on config POST).
	s.deps.CfgMu.RLock()
	var cfg *config.Driver
	for i := range s.deps.Cfg.Drivers {
		if s.deps.Cfg.Drivers[i].Name == name {
			c := s.deps.Cfg.Drivers[i]
			cfg = &c
			break
		}
	}
	s.deps.CfgMu.RUnlock()
	if cfg == nil {
		// Fall back to whatever the registry has — still lets you restart
		// a driver that isn't in cfg.yaml (e.g. injected EV charger).
		if err := s.deps.Registry.RestartByName(r.Context(), name); err != nil {
			writeJSON(w, 404, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "restarted", "source": "registry"})
		return
	}
	if err := s.deps.Registry.Restart(r.Context(), *cfg); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "restarted", "source": "config"})
}

// POST /api/drivers/{name}/disable — set the Disabled flag in the
// config and persist, stopping the running driver. Survives restarts.
func (s *Server) handleDriverDisable(w http.ResponseWriter, r *http.Request) {
	s.setDriverDisabled(w, r, true)
}

// POST /api/drivers/{name}/enable — clear the Disabled flag and spawn
// the driver (if it's not already running).
func (s *Server) handleDriverEnable(w http.ResponseWriter, r *http.Request) {
	s.setDriverDisabled(w, r, false)
}

func (s *Server) setDriverDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	if s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "registry unavailable"})
		return
	}
	name := r.PathValue("name")
	if name == "" {
		writeJSON(w, 400, map[string]string{"error": "missing driver name"})
		return
	}
	s.deps.CfgMu.Lock()
	found := false
	for i := range s.deps.Cfg.Drivers {
		if s.deps.Cfg.Drivers[i].Name == name {
			s.deps.Cfg.Drivers[i].Disabled = disabled
			found = true
			break
		}
	}
	if !found {
		s.deps.CfgMu.Unlock()
		writeJSON(w, 404, map[string]string{"error": "driver not found in config"})
		return
	}
	cfgCopy := *s.deps.Cfg
	s.deps.CfgMu.Unlock()

	// Persist to disk so the change survives restart.
	if err := s.deps.SaveConfig(s.deps.ConfigPath, &cfgCopy); err != nil {
		writeJSON(w, 500, map[string]string{"error": "save failed: " + err.Error()})
		return
	}
	// Apply immediately via Reload — it filters disabled drivers and
	// stops running ones, or re-adds the newly-enabled one.
	s.deps.Registry.Reload(r.Context(), cfgCopy.Drivers, cfgCopy.Site.TroubleshootingMode)

	action := "disabled"
	if !disabled {
		action = "enabled"
	}
	writeJSON(w, 200, map[string]string{"status": action, "driver": name})
}

// GET /api/ha/status — is the HA MQTT bridge connected?
// Used by the Settings UI to show a live connection indicator
// instead of silently relying on "it's saved".
func (s *Server) handleHAStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.HA == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, 200, map[string]any{
		"enabled":           true,
		"connected":         s.deps.HA.IsConnected(),
		"broker":            s.deps.HA.BrokerAddr(),
		"last_publish_ms":   s.deps.HA.LastPublishMs(),
		"sensors_announced": s.deps.HA.SensorsAnnounced(),
	})
}

// ---- /api/battery_models ----

func (s *Server) handleGetModels(w http.ResponseWriter, r *http.Request) {
	s.deps.ModelsMu.Lock()
	defer s.deps.ModelsMu.Unlock()
	out := make(map[string]any, len(s.deps.Models))
	for name, m := range s.deps.Models {
		out[name] = map[string]any{
			"tau_s":                 m.TimeConstantS(s.deps.DtS),
			"gain":                  m.SteadyStateGain(),
			"deadband_w":            m.DeadbandW,
			"n_samples":             m.NSamples,
			"confidence":            m.Confidence(),
			"health_score":          m.HealthScore(),
			"health_drift_per_day":  m.HealthDriftPerDay(),
			"baseline_gain":         m.BaselineGain,
			"baseline_tau_s":        m.BaselineTauS,
			"last_calibrated_ts_ms": m.LastCalibrated,
			"last_updated_ts_ms":    m.LastUpdatedMs,
			"max_charge_curve":      m.MaxChargeCurve,
			"max_discharge_curve":   m.MaxDischargeCurve,
			"a":                     m.A,
			"b":                     m.B,
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleResetModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Battery string `json:"battery"`
		All     bool   `json:"all"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.ModelsMu.Lock()
	defer s.deps.ModelsMu.Unlock()
	var reset []string
	if req.All {
		for name := range s.deps.Models {
			s.deps.Models[name] = battery.New(name)
			reset = append(reset, name)
		}
	} else if req.Battery != "" {
		if _, ok := s.deps.Models[req.Battery]; !ok {
			writeJSON(w, 404, map[string]string{"error": "battery not found: " + req.Battery})
			return
		}
		s.deps.Models[req.Battery] = battery.New(req.Battery)
		reset = append(reset, req.Battery)
	} else {
		writeJSON(w, 400, map[string]string{"error": "provide 'battery' or 'all'"})
		return
	}
	// Persist fresh models
	for _, name := range reset {
		if m, ok := s.deps.Models[name]; ok {
			if data, err := json.Marshal(m); err == nil {
				if err := s.deps.State.SaveBatteryModel(name, string(data)); err != nil {
					slog.Warn("failed to persist battery model", "battery", name, "err", err)
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{"reset": reset})
}

// ---- /api/self_tune/* ----

func (s *Server) handleSelfTuneStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Batteries []string `json:"batteries"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.ModelsMu.Lock()
	err := s.deps.SelfTune.Start(req.Batteries, s.deps.Models, s.deps.DtS)
	s.deps.ModelsMu.Unlock()
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	slog.Info("self-tune started", "batteries", req.Batteries)
	writeJSON(w, 200, map[string]any{"status": "started", "batteries": req.Batteries})
}

func (s *Server) handleSelfTuneStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.deps.SelfTune.Status())
}

func (s *Server) handleSelfTuneCancel(w http.ResponseWriter, r *http.Request) {
	s.deps.SelfTune.Cancel()
	slog.Info("self-tune cancelled")
	writeJSON(w, 200, map[string]string{"status": "cancelled"})
}

// ---- /api/history ----

// handleHistory returns time-series points from state DB.
// Query params: range=5m|15m|1h|6h|24h|3d, points=N
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	rangeStr := r.URL.Query().Get("range")
	if rangeStr == "" {
		rangeStr = "5m"
	}
	pointsStr := r.URL.Query().Get("points")
	points := 200
	if pointsStr != "" {
		if n, err := strconv.Atoi(pointsStr); err == nil && n > 0 {
			points = n
		}
	}

	windowMs := parseRange(rangeStr)
	nowMs := time.Now().UnixMilli()
	since := nowMs - windowMs
	rows, err := s.deps.State.LoadHistory(since, nowMs, points)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// Row.JSON is a blob from record_history; deserialize if valid
		var inner map[string]any
		if row.JSON != "" {
			_ = json.Unmarshal([]byte(row.JSON), &inner)
		}
		if inner == nil {
			inner = map[string]any{}
		}
		inner["ts"] = row.TsMs
		// Fill from flat columns for charting
		inner["grid_w"] = row.GridW
		inner["pv_w"] = row.PVW
		inner["bat_w"] = row.BatW
		inner["load_w"] = row.LoadW
		inner["bat_soc"] = row.BatSoC
		items = append(items, inner)
	}
	writeJSON(w, 200, map[string]any{"items": items, "range": rangeStr})
}

func parseRange(s string) int64 {
	switch s {
	case "5m":
		return 5 * 60 * 1000
	case "15m":
		return 15 * 60 * 1000
	case "1h":
		return 60 * 60 * 1000
	case "6h":
		return 6 * 60 * 60 * 1000
	case "24h":
		return 24 * 60 * 60 * 1000
	case "48h":
		return 48 * 60 * 60 * 1000
	case "3d":
		return 3 * 24 * 60 * 60 * 1000
	}
	return 5 * 60 * 1000
}

// ---- /api/energy/daily ----
//
// Query params: days=N (default 7, capped at 90)
// Response: {"days": [{"day":"YYYY-MM-DD","import_wh":..., "export_wh":...,
//
//	"pv_wh":..., "bat_charged_wh":..., "bat_discharged_wh":...,
//	"load_wh":...}], "tz": "Local"}
//
// Buckets are local-day. Today is the last entry. Mirrors the integration
// loop in handleStatus's energy-today block — same site convention, same
// W*dt math — but per local day across the requested range. Designed for
// the dashboard's history cards (Imported / Consumed / Exported).
func (s *Server) handleEnergyDaily(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 200, map[string]any{"days": []any{}})
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > 90 {
		days = 90
	}
	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	// Walk day-by-day: past days hit the immutable-day cache, today always
	// recomputes (in-progress). Each cache miss / today fire is one SQL
	// round-trip that returns a single pre-integrated row — no raw samples
	// shipped to Go.
	out := make([]map[string]any, 0, days)
	for i := days - 1; i >= 0; i-- {
		dayStart := todayMidnight.AddDate(0, 0, -i)
		dayKey := dayStart.Format("2006-01-02")
		isToday := i == 0

		var de state.DayEnergy
		if isToday {
			d, err := s.deps.State.DailyEnergy(dayStart.UnixMilli(), now.UnixMilli())
			if err != nil {
				slog.Error("handleEnergyDaily: DailyEnergy failed", "err", err, "day", dayKey)
				http.Error(w, "history load failed", http.StatusInternalServerError)
				return
			}
			de = d
		} else {
			// Two-tier cache: in-memory first, then the persistent
			// energy_daily table. The persistent layer survives
			// restarts — the 2026-05-25 baseline was 25 s for
			// days=30 cold-start because every closed day re-ran a
			// per-day DailyEnergy SQL pass; with the table populated
			// the same query reduces to N PK lookups.
			s.dailyCacheMu.Lock()
			cached, ok := s.dailyCache[dayKey]
			s.dailyCacheMu.Unlock()
			if ok {
				de = cached
			} else if persisted, present, err := s.deps.State.LoadDailyEnergy(dayKey); err == nil && present {
				de = persisted
				s.dailyCacheMu.Lock()
				s.dailyCache[dayKey] = de
				s.dailyCacheMu.Unlock()
			} else {
				dayEnd := dayStart.AddDate(0, 0, 1)
				d, err := s.deps.State.DailyEnergy(dayStart.UnixMilli(), dayEnd.UnixMilli())
				if err != nil {
					slog.Error("handleEnergyDaily: DailyEnergy failed", "err", err, "day", dayKey)
					http.Error(w, "history load failed", http.StatusInternalServerError)
					return
				}
				de = d
				s.dailyCacheMu.Lock()
				s.dailyCache[dayKey] = de
				s.dailyCacheMu.Unlock()
				// Persist for next restart. Closed days only —
				// today is excluded via the isToday branch above.
				// Best-effort: a write failure is logged but not
				// surfaced to the operator since the in-memory
				// cache still serves this request.
				if err := s.deps.State.SaveDailyEnergy(dayKey, de); err != nil {
					slog.Warn("handleEnergyDaily: persist daily aggregate failed", "err", err, "day", dayKey)
				}
			}
		}

		out = append(out, map[string]any{
			"day":               dayKey,
			"import_wh":         de.ImportWh,
			"export_wh":         de.ExportWh,
			"pv_wh":             de.PVWh,
			"bat_charged_wh":    de.BatChargedWh,
			"bat_discharged_wh": de.BatDischargedWh,
			"load_wh":           de.LoadWh,
		})
	}
	writeJSON(w, 200, map[string]any{"days": out, "tz": loc.String()})
}

// ---- /api/prices ----
//
// Query params:
//
//	range=24h|48h|3d  — window starting NOW unless since_ms given
//	since_ms=…        — explicit start
//	until_ms=…        — explicit end (default: now + 48h)
//
// Response: {"zone": "...", "items": [{slot_ts_ms, slot_len_min, spot_ore_kwh, total_ore_kwh, ...}]}
func (s *Server) handlePrices(w http.ResponseWriter, r *http.Request) {
	if s.deps.Prices == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "enabled": false})
		return
	}
	q := r.URL.Query()
	nowMs := time.Now().UnixMilli()
	var since, until int64
	since = nowMs - 1*3600*1000  // default 1h lookback
	until = nowMs + 48*3600*1000 // default 48h lookahead
	if v := q.Get("since_ms"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n
		}
	}
	if v := q.Get("until_ms"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			until = n
		}
	}
	if rng := q.Get("range"); rng != "" {
		since = nowMs
		until = nowMs + parseRange(rng)
	}
	rows, err := s.deps.Prices.Load(since, until)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"zone":    s.deps.Prices.Zone,
		"items":   rows,
		"enabled": true,
	})
}

// ---- /api/forecast ----
//
// Query params: range=24h|48h|3d (default 48h lookahead).
// Response: {"items": [{slot_ts_ms, cloud_cover_pct, temp_c, pv_w_estimated, ...}]}
func (s *Server) handleForecast(w http.ResponseWriter, r *http.Request) {
	if s.deps.Forecast == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "enabled": false})
		return
	}
	q := r.URL.Query()
	nowMs := time.Now().UnixMilli()
	since, until := nowMs-time.Hour.Milliseconds(), nowMs+48*3600*1000
	if rng := q.Get("range"); rng != "" {
		since = nowMs
		until = nowMs + parseRange(rng)
	}
	rows, err := s.deps.Forecast.Load(since, until)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"items": rows, "enabled": true})
}

// ---- MPC planner ----

func (s *Server) handleMPCPlan(w http.ResponseWriter, r *http.Request) {
	if s.deps.MPC == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	plan := s.deps.MPC.Latest()
	at, reason := s.deps.MPC.LastReplanInfo()
	meta := map[string]any{
		"last_replan_ms":     at.UnixMilli(),
		"last_replan_reason": reason,
	}
	if plan == nil {
		writeJSON(w, 200, map[string]any{"enabled": true, "plan": nil, "meta": meta})
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": true, "plan": plan, "meta": meta})
}

func (s *Server) handleMPCReplan(w http.ResponseWriter, r *http.Request) {
	if s.deps.MPC == nil {
		writeJSON(w, 400, map[string]string{"error": "mpc disabled"})
		return
	}
	plan := s.deps.MPC.Replan(r.Context())
	writeJSON(w, 200, map[string]any{"enabled": true, "plan": plan})
}

// handleMPCDiagnose exposes the full per-slot context of the most
// recent Optimize call: inputs (price, PV, load, confidence) joined
// with outputs (battery, grid, SoC, cost, reason) plus the Params the
// DP was parameterized with. Lets operators answer "why did the
// planner decide X at 21:00?" without shelling into the host.
func (s *Server) handleMPCDiagnose(w http.ResponseWriter, r *http.Request) {
	if s.deps.MPC == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	diag := s.deps.MPC.Diagnose()
	if diag == nil {
		writeJSON(w, 200, map[string]any{"enabled": true, "diagnostic": nil})
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": true, "diagnostic": diag})
}

// handleMPCDiagnoseHistory lists persisted replan snapshots as
// lightweight summaries for the timeline UI. The full per-slot JSON
// blob isn't included — call /api/mpc/diagnose/at?ts=<ms> for that.
//
// Query params:
//
//	since  unix-ms; default "now − 7d"
//	until  unix-ms; default now
//	limit  max rows returned; default 500, cap 5000
//
// Falls back to the cold-storage parquet files when the requested
// window extends beyond DiagnosticsRecentRetention — keeps the UI
// working for year-old incidents without a separate code path.
func (s *Server) handleMPCDiagnoseHistory(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	now := time.Now().UnixMilli()
	since := now - 7*24*3600*1000
	until := now
	if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n
		}
	}
	if v := r.URL.Query().Get("until"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			until = n
		}
	}
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
			if limit > 5000 {
				limit = 5000
			}
		}
	}
	summaries, err := s.deps.State.LoadDiagnosticsInRange(since, until, limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	// If the query window extends before the SQLite retention and we
	// have a cold-storage root configured, top up with Parquet. After
	// merging we re-sort newest-first and re-apply the caller's limit
	// so the combined response honours the same contract as the
	// pure-hot path (otherwise cold rows would append at the tail and
	// break the "newest first" promise + could overshoot `limit`).
	coldDir := s.deps.ColdDir
	if coldDir != "" {
		hotCutoff := now - int64(state.DiagnosticsRecentRetention/time.Millisecond)
		if since < hotCutoff {
			coldUntil := hotCutoff
			if until < coldUntil {
				coldUntil = until
			}
			cold, cerr := s.deps.State.LoadDiagnosticsFromParquet(coldDir, since, coldUntil)
			if cerr == nil && len(cold) > 0 {
				summaries = append(summaries, cold...)
				sort.Slice(summaries, func(i, j int) bool {
					return summaries[i].TsMs > summaries[j].TsMs
				})
				if limit > 0 && len(summaries) > limit {
					summaries = summaries[:limit]
				}
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"enabled":   true,
		"snapshots": summaries,
	})
}

// handleMPCDiagnoseAt returns the snapshot active at ?ts=<ms> — the
// replan whose ts_ms is the largest one ≤ ts. That's the "plan that
// was driving the EMS at that moment" semantics, so a query at 02:07
// returns the 02:00 replan (not the 02:15 one that ran afterward).
// Falls through to Parquet when the hit isn't in the hot table.
func (s *Server) handleMPCDiagnoseAt(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	ts, err := strconv.ParseInt(r.URL.Query().Get("ts"), 10, 64)
	if err != nil || ts <= 0 {
		writeJSON(w, 400, map[string]string{"error": "ts (unix ms) required"})
		return
	}
	row, err := s.deps.State.LoadDiagnosticAt(ts)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if row == nil && s.deps.ColdDir != "" {
		row, err = s.deps.State.LoadDiagnosticFullFromParquet(s.deps.ColdDir, ts)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
	}
	if row == nil {
		writeJSON(w, 200, map[string]any{"enabled": true, "snapshot": nil})
		return
	}
	// Pass the stored JSON through raw so the client sees the exact
	// mpc.Diagnostic shape it would have gotten from /api/mpc/diagnose.
	// Wrapping in a typed struct would force an unmarshal + remarshal
	// that adds ~1 ms on a 2880-slot snapshot for no benefit.
	payload := map[string]any{
		"ts_ms":          row.TsMs,
		"reason":         row.Reason,
		"zone":           row.Zone,
		"total_cost_ore": row.TotalCostOre,
		"horizon_slots":  row.HorizonSlots,
		"diagnostic":     json.RawMessage(row.JSON),
	}
	writeJSON(w, 200, map[string]any{"enabled": true, "snapshot": payload})
}

// ---- Long-format time-series ----

// handleSeries: GET /api/series?driver=ferroamp&metric=battery_w&range=1h&points=600
// Returns one metric's time series for one driver. Useful for the metric
// browser UI and for ML training data exports.
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	driver := r.URL.Query().Get("driver")
	metric := r.URL.Query().Get("metric")
	if driver == "" || metric == "" {
		writeJSON(w, 400, map[string]string{"error": "driver and metric are required"})
		return
	}
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "1h"
	}
	points := 0
	if p := r.URL.Query().Get("points"); p != "" {
		if v, err := strconv.Atoi(p); err == nil {
			points = v
		}
	}
	windowMs := parseRange(rng)
	now := time.Now().UnixMilli()
	rows, err := s.deps.State.LoadSeries(driver, metric, now-windowMs, now, points)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, len(rows))
	for i, sm := range rows {
		out[i] = map[string]any{"ts": sm.TsMs, "v": sm.Value}
	}
	writeJSON(w, 200, map[string]any{
		"driver": driver, "metric": metric, "range": rng, "points": out,
	})
}

// handleSeriesCatalog: GET /api/series/catalog
// Lists the (driver, metric) tuples that have any samples recorded. UIs
// use this to enumerate available signals for charting / debugging.
func (s *Server) handleSeriesCatalog(w http.ResponseWriter, r *http.Request) {
	drivers, err := s.deps.State.DriverNames()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	metrics, err := s.deps.State.MetricNames()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{
		"drivers": drivers,
		"metrics": metrics,
	})
}

// handleDevices: GET /api/devices
// Returns every registered device with its hardware-stable identity. UIs
// surface this in driver cards (small "SN: ABC" line) and in Settings →
// Devices so the operator can see how each driver is identified.
func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.deps.State.AllDevices()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, len(devs))
	for i, d := range devs {
		out[i] = map[string]any{
			"device_id":     d.DeviceID,
			"driver_name":   d.DriverName,
			"make":          d.Make,
			"serial":        d.Serial,
			"mac":           d.MAC,
			"endpoint":      d.Endpoint,
			"first_seen_ms": d.FirstSeenMs,
			"last_seen_ms":  d.LastSeenMs,
		}
	}
	writeJSON(w, 200, map[string]any{"devices": out})
}

// ---- network scan ----

// handleScan: GET /api/scan
// Probes the local network for devices on common energy-protocol ports
// (Modbus 502, MQTT 1883, HTTP 80). Used by Settings → Scan and the
// bootstrap wizard.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	devices, err := scanner.Scan(r.Context())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, devices)
}

// ---- PV digital twin ----

func (s *Server) handlePVModel(w http.ResponseWriter, r *http.Request) {
	if s.deps.PVModel == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	m := s.deps.PVModel.Model()
	rd := s.deps.PVModel.ResidualDiagSnapshot()
	// Single-number "what is the planner doing right now" view: the
	// correction the MPC would apply to a slot 15 min out, after the
	// ramp-off + gates. Saves operators from interpreting mean × ramp
	// themselves.
	now := time.Now()
	currentCorrW := s.deps.PVModel.ResidualCorrect(now, now.Add(15*time.Minute), 0)
	writeJSON(w, 200, map[string]any{
		"enabled":                    true,
		"samples":                    m.Samples,
		"mae_w":                      m.MAE,
		"rated_w":                    m.RatedW,
		"quality":                    m.Quality(),
		"last_ms":                    m.LastMs,
		"forgetting":                 m.Forgetting,
		"beta":                       m.Beta,
		"pv_residual_correction_w":   currentCorrW,
		"pv_residual_sample_count":   rd.SampleCount,
		"pv_residual_mean_w":         rd.MeanW,
		"pv_residual_std_w":          rd.StdW,
		"pv_residual_window_minutes": rd.WindowMinutes,
	})
}

func (s *Server) handlePVModelReset(w http.ResponseWriter, r *http.Request) {
	if s.deps.PVModel == nil {
		writeJSON(w, 400, map[string]string{"error": "pvmodel disabled"})
		return
	}
	s.deps.PVModel.Reset()
	writeJSON(w, 200, map[string]string{"status": "reset"})
}

// ---- static ----

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		path = "/index.html"
	}
	if path == "/setup" {
		path = "/setup.html"
	}
	// Legacy dashboard moved behind /legacy; /next is now the default
	// at /. Keep /next working as a 301 so old bookmarks land correctly
	// without advertising two URLs for the same content.
	if path == "/next" || path == "/next.html" {
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
		return
	}
	if path == "/legacy" {
		path = "/legacy.html"
	}
	// Prevent path traversal
	clean := filepath.Clean(filepath.Join(s.deps.WebDir, path))
	absWeb, _ := filepath.Abs(s.deps.WebDir)
	absPath, _ := filepath.Abs(clean)
	if !strings.HasPrefix(absPath, absWeb+string(filepath.Separator)) {
		writeJSON(w, 403, map[string]string{"error": "forbidden"})
		return
	}
	// Always-revalidate so version bumps land immediately
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, clean)
}

// GET /api/ev/status — detailed EV charger state for the dashboard modal.
//
// Accepts an optional ?driver= to target a specific charger on multi-EV
// sites. When unset, falls back to the first DerEV reading. A named driver
// that isn't currently reporting telemetry returns 404 so the UI can
// distinguish "charger temporarily offline" from "no EV charger configured".
func (s *Server) handleEVStatus(w http.ResponseWriter, r *http.Request) {
	readings := s.deps.Tel.ReadingsByType(telemetry.DerEV)
	if len(readings) == 0 {
		writeJSON(w, 200, map[string]any{"connected": false})
		return
	}
	rd := readings[0]
	if want := r.URL.Query().Get("driver"); want != "" {
		found := false
		for _, candidate := range readings {
			if candidate.Driver == want {
				rd = candidate
				found = true
				break
			}
		}
		if !found {
			writeJSON(w, 404, map[string]string{"error": "unknown ev driver"})
			return
		}
	}
	resp := map[string]any{
		"driver":  rd.Driver,
		"w":       rd.RawW,
		"updated": rd.UpdatedAt,
	}
	if len(rd.Data) > 0 {
		var data map[string]any
		if err := json.Unmarshal(rd.Data, &data); err == nil {
			for k, v := range data {
				resp[k] = v
			}
		}
	}
	writeJSON(w, 200, resp)
}

// POST /api/ev/command — send a command to the EV charger driver.
//
// Action is validated against an allowlist of known Lua-driver verbs so the
// API doesn't silently 200-OK a typo'd action: the Lua command hook returns
// no value for unknown actions, which looks like success to the registry.
var validEVActions = map[string]bool{
	"ev_start":       true,
	"ev_pause":       true,
	"ev_resume":      true,
	"ev_set_current": true,
}

func (s *Server) handleEVCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Driver string `json:"driver"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	if !validEVActions[req.Action] {
		writeJSON(w, 400, map[string]string{"error": "unsupported action"})
		return
	}
	// Validate the request against telemetry (is the driver known?) before
	// checking Registry availability. Request-validation errors (400/404)
	// should win over service-readiness errors (503) so the client can tell
	// "you sent a bad name" apart from "the backend isn't wired up yet".
	if s.deps.Tel == nil {
		writeJSON(w, 503, map[string]string{"error": "telemetry not available"})
		return
	}
	readings := s.deps.Tel.ReadingsByType(telemetry.DerEV)
	if len(readings) == 0 {
		writeJSON(w, 404, map[string]string{"error": "no EV driver active"})
		return
	}
	driverName := readings[0].Driver
	if req.Driver != "" {
		found := false
		for _, rd := range readings {
			if rd.Driver == req.Driver {
				driverName = req.Driver
				found = true
				break
			}
		}
		if !found {
			writeJSON(w, 404, map[string]string{"error": "unknown ev driver"})
			return
		}
	}
	if s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "driver registry not available"})
		return
	}
	// Manual Start / Resume / Pause must "stick" against the next
	// dispatch tick — otherwise the controller's MPC-budget check (or
	// surplus-only clamp with no live PV) commands `power_w=0` on the
	// very next 5 s tick and the operator's click looks like a no-op.
	// Solution: set a no-expiry ManualHold on the matched loadpoint
	// before forwarding the action to the driver. Pause clears the
	// hold so the LP reverts to plan / PV-surplus / schedule. Pure
	// driver-targeted actions (ev_set_current) skip the hold path —
	// dispatch uses ev_set_current internally and a sticky hold there
	// would defeat the whole control loop.
	if req.Action == "ev_start" || req.Action == "ev_resume" || req.Action == "ev_pause" {
		applyManualEVHold(s.deps, driverName, req.Action)
	}
	payload, _ := json.Marshal(map[string]any{"action": req.Action})
	if err := s.deps.Registry.Send(r.Context(), driverName, payload); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

var validV2XActions = map[string]bool{
	"v2x_set_power": true,
	"v2x_stop":      true,
	"init":          true,
	"deinit":        true,
}

const maxManualV2XPowerW = 50000

func catalogHasCapability(entries []drivers.CatalogEntry, luaPath, capability string) bool {
	base := filepath.Base(luaPath)
	for _, entry := range entries {
		if entry.Path != luaPath && entry.Filename != base && filepath.Base(entry.Path) != base {
			continue
		}
		for _, cap := range entry.Capabilities {
			if cap == capability {
				return true
			}
		}
	}
	return false
}

func (s *Server) configuredV2XDrivers() map[string]bool {
	out := make(map[string]bool)
	if s.deps.Cfg == nil || s.deps.CfgMu == nil {
		return out
	}
	var cfgDrivers []config.Driver
	s.deps.CfgMu.RLock()
	cfgDrivers = append(cfgDrivers, s.deps.Cfg.Drivers...)
	s.deps.CfgMu.RUnlock()
	catalog, _ := drivers.LoadCatalogMulti(s.deps.UserDriverDir, s.deps.DriverDir)
	for _, d := range cfgDrivers {
		if d.Disabled || d.Name == "" {
			continue
		}
		if catalogHasCapability(catalog, d.Lua, telemetry.DerV2X.String()) ||
			strings.Contains(strings.ToLower(filepath.Base(d.Lua)), "v2x") {
			out[d.Name] = true
		}
	}
	return out
}

func (s *Server) liveV2XDrivers() map[string]bool {
	out := make(map[string]bool)
	if s.deps.Tel == nil {
		return out
	}
	for _, r := range s.deps.Tel.ReadingsByType(telemetry.DerV2X) {
		h := s.deps.Tel.DriverHealth(r.Driver)
		if h != nil && h.IsOnline() {
			out[r.Driver] = true
		}
	}
	return out
}

func pickV2XDriver(requested string, configured, live map[string]bool) (string, error) {
	if requested != "" {
		if configured[requested] || live[requested] {
			return requested, nil
		}
		return "", fmt.Errorf("unknown v2x driver")
	}
	candidates := make(map[string]bool)
	for name := range configured {
		candidates[name] = true
	}
	for name := range live {
		candidates[name] = true
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no V2X driver configured")
	}
	if len(candidates) > 1 {
		return "", fmt.Errorf("driver is required when multiple V2X drivers exist")
	}
	for name := range candidates {
		return name, nil
	}
	return "", fmt.Errorf("no V2X driver configured")
}

type v2xPolicyTelemetry struct {
	Connected          *bool    `json:"connected"`
	PlugConnected      *bool    `json:"plug_connected"`
	VehicleSoC         *float64 `json:"vehicle_soc"`
	VehicleSoCFract    *float64 `json:"vehicle_soc_fract"`
	CapacityWh         *float64 `json:"capacity_wh"`
	RatedPowerW        *float64 `json:"rated_power_w"`
	ChargePowerMaxW    *float64 `json:"charge_power_max_w"`
	DischargePowerMaxW *float64 `json:"discharge_power_max_w"`
}

func (s *Server) v2xPolicyConfig() *config.V2XPolicy {
	if s.deps == nil || s.deps.Cfg == nil || s.deps.CfgMu == nil {
		return nil
	}
	s.deps.CfgMu.RLock()
	defer s.deps.CfgMu.RUnlock()
	if s.deps.Cfg.V2X == nil {
		return nil
	}
	cp := *s.deps.Cfg.V2X
	return &cp
}

func (s *Server) v2xPolicyStatus(gridW *float64) map[string]any {
	policy := s.v2xPolicyConfig()
	policyOut := config.V2XPolicy{}
	if policy != nil {
		policyOut = *policy
	}

	driversOut := map[string]v2xpolicy.Envelope{}
	if s.deps == nil || s.deps.Tel == nil {
		return map[string]any{
			"policy":  policyOut,
			"drivers": driversOut,
		}
	}

	candidates := s.configuredV2XDrivers()
	for _, r := range s.deps.Tel.ReadingsByType(telemetry.DerV2X) {
		candidates[r.Driver] = true
	}
	if policy != nil && policy.DriverName != "" {
		candidates[policy.DriverName] = true
	}
	for name := range candidates {
		r := s.deps.Tel.Get(name, telemetry.DerV2X)
		driversOut[name] = v2xpolicy.Evaluate(policy, s.v2xSnapshot(name, r, gridW))
	}

	return map[string]any{
		"policy":  policyOut,
		"drivers": driversOut,
	}
}

func (s *Server) v2xSnapshot(name string, r *telemetry.DerReading, gridW *float64) v2xpolicy.Snapshot {
	snap := v2xpolicy.Snapshot{
		Driver: name,
		GridW:  gridW,
		Now:    time.Now(),
	}
	if s.deps != nil && s.deps.Tel != nil {
		if h := s.deps.Tel.DriverHealth(name); h != nil {
			snap.Online = h.IsOnline()
		}
	}
	if r == nil {
		return snap
	}
	if r.SoC != nil {
		soc := *r.SoC
		snap.SoC = &soc
	}
	var data v2xPolicyTelemetry
	if r.Data != nil && json.Unmarshal(r.Data, &data) == nil {
		if data.Connected != nil {
			connected := *data.Connected
			snap.Connected = &connected
		} else if data.PlugConnected != nil {
			connected := *data.PlugConnected
			snap.Connected = &connected
		}
		if snap.SoC == nil {
			if data.VehicleSoC != nil {
				soc := *data.VehicleSoC
				snap.SoC = &soc
			} else if data.VehicleSoCFract != nil {
				soc := *data.VehicleSoCFract
				snap.SoC = &soc
			}
		}
		if data.CapacityWh != nil {
			snap.CapacityWh = *data.CapacityWh
		}
		if data.RatedPowerW != nil {
			snap.RatedPowerW = *data.RatedPowerW
		}
		if data.ChargePowerMaxW != nil {
			snap.ChargePowerMaxW = *data.ChargePowerMaxW
		}
		if data.DischargePowerMaxW != nil {
			snap.DischargePowerMaxW = *data.DischargePowerMaxW
		}
	}
	return snap
}

func (s *Server) currentV2XGridW() *float64 {
	if s.deps == nil || s.deps.Tel == nil || s.deps.Ctrl == nil || s.deps.CtrlMu == nil {
		return nil
	}
	s.deps.CtrlMu.Lock()
	siteMeter := s.deps.Ctrl.SiteMeterDriver
	s.deps.CtrlMu.Unlock()
	if siteMeter == "" || !statusDriverOnline(s.deps.Tel, siteMeter) {
		return nil
	}
	if r := s.deps.Tel.Get(siteMeter, telemetry.DerMeter); r != nil {
		gridW := r.SmoothedW
		return &gridW
	}
	return nil
}

// GET /api/v2x/policy — read back the configured V2X policy plus the live
// allowed power envelope per V2X driver. This is observability only; automatic
// dispatch remains disabled until a later planner integration consumes it.
func (s *Server) handleV2XPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.v2xPolicyStatus(s.currentV2XGridW()))
}

// POST /api/v2x/command — send a signed W setpoint to a V2X charger driver.
// This is intentionally a thin manual/test surface; automatic optimizer
// dispatch is kept out until the V2X policy layer has reserve/departure
// constraints wired in.
func (s *Server) handleV2XCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string  `json:"action"`
		Driver string  `json:"driver"`
		PowerW float64 `json:"power_w"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	if req.Action == "" {
		req.Action = "v2x_set_power"
	}
	if !validV2XActions[req.Action] {
		writeJSON(w, 400, map[string]string{"error": "unsupported action"})
		return
	}
	if math.IsNaN(req.PowerW) || math.IsInf(req.PowerW, 0) {
		writeJSON(w, 400, map[string]string{"error": "power_w must be finite"})
		return
	}
	if math.Abs(req.PowerW) > maxManualV2XPowerW {
		writeJSON(w, 400, map[string]string{"error": "power_w outside allowed manual V2X range"})
		return
	}
	live := s.liveV2XDrivers()
	driverName, err := pickV2XDriver(req.Driver, s.configuredV2XDrivers(), live)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": err.Error()})
		return
	}
	if s.deps.Registry == nil {
		writeJSON(w, 503, map[string]string{"error": "driver registry not available"})
		return
	}
	action := req.Action
	powerW := req.PowerW
	if action == "v2x_stop" {
		action = "v2x_set_power"
		powerW = 0
	}
	// Only the v2x_set_power action carries a setpoint. init / deinit (and
	// the v2x_stop→set_power+0 remap above) are non-setpoint actions, so a
	// caller-supplied power_w must never leak through to the driver on
	// them — force it to 0. This also keeps the live-telemetry guard below
	// from being bypassed by a non-setpoint action smuggling a setpoint.
	if action != "v2x_set_power" {
		powerW = 0
	}
	if action == "v2x_set_power" && powerW != 0 && !live[driverName] {
		writeJSON(w, 409, map[string]string{"error": "v2x driver is not reporting live telemetry"})
		return
	}
	payload, _ := json.Marshal(map[string]any{"action": action, "power_w": powerW})
	if err := s.deps.Registry.Send(r.Context(), driverName, payload); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "driver": driverName, "power_w": powerW})
}

// applyManualEVHold pins / releases the loadpoint matching the given
// EV driver according to the operator action. Start / Resume install
// a no-expiry hold at the loadpoint's MaxChargeW so the next dispatch
// tick can't pull the wallbox back to 0. Pause clears the hold and
// the loadpoint immediately reverts to plan / surplus rules. No-op
// when no controller is wired or no loadpoint matches the driver —
// the action still forwards to the driver in those cases.
func applyManualEVHold(deps *Deps, driverName string, action string) {
	if deps == nil || deps.LoadpointCtrl == nil || deps.Loadpoints == nil {
		return
	}
	var lpID string
	var maxW float64
	for _, st := range deps.Loadpoints.States() {
		if st.DriverName == driverName {
			lpID = st.ID
			maxW = st.MaxChargeW
			break
		}
	}
	if lpID == "" {
		return
	}
	if action == "ev_pause" {
		deps.LoadpointCtrl.ClearManualHold(lpID)
		slog.Info("ev manual pause — cleared manual hold, reverting to plan", "lp", lpID)
		return
	}
	if maxW <= 0 {
		maxW = 11000 // 16 A × 3φ × 230 V fallback when the LP config didn't set it
	}
	// 100-year expiry serves as "sticky until the operator cancels".
	// Using time.Now() + a long delta rather than time.Time{} because
	// SetManualHold treats zero ExpiresAt as "delete" (controller.go:653).
	deps.LoadpointCtrl.SetManualHold(lpID, loadpoint.ManualHold{
		PowerW:    maxW,
		ExpiresAt: time.Now().Add(100 * 365 * 24 * time.Hour),
	})
	slog.Info("ev manual start/resume — installed sticky hold",
		"lp", lpID, "action", action, "hold_w", maxW)
}

// GET /api/ev/providers — return the descriptor for every registered EV
// charger provider. The wizard reads this to decide which transport +
// auth fields to render for the user's pick.
func (s *Server) handleEVProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, evcloud.Describe())
}

// POST /api/ev/chargers — probe a provider for the chargers reachable
// from the supplied config. Body is the EVCharger shape (provider +
// transport block + optional auth). For providers that need auth and
// the body omits Password, we fall back to the persisted
// ev_charger_password so the operator doesn't have to re-type it when
// they're just refreshing the picker.
func (s *Server) handleEVChargers(w http.ResponseWriter, r *http.Request) {
	var cfg config.EVCharger
	if err := readJSON(r, &cfg); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	cfg.Normalize()
	// Provider must come from the caller. The api stays vendor-agnostic
	// — the wizard's GET /api/ev/providers enumerates the registry, the
	// operator picks one, and that choice is what arrives here. Defaulting
	// to a specific brand would silently couple the api to one vendor.
	if cfg.Provider == "" {
		writeJSON(w, 400, map[string]string{"error": "provider required"})
		return
	}
	p, err := evcloud.Get(cfg.Provider)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	desc := p.Describe()
	if desc.NeedsAuth && cfg.Password == "" {
		if pw, ok := s.deps.State.LoadConfig(evPasswordKey); ok {
			cfg.Password = pw
		}
	}
	if err := cfg.Validate(); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if desc.NeedsAuth && cfg.Password == "" {
		writeJSON(w, 400, map[string]string{"error": "password required"})
		return
	}
	chargers, err := p.ListChargers(&cfg)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, chargers)
}

// GET /api/loadpoints returns the configured EV loadpoints with their
// current observable state. When a DerVehicle driver is online (e.g.
// tesla_vehicle.lua against TeslaBLEProxy), its real BMS SoC is
// overlaid onto the response and SoCSource flips from "inferred" to
// "vehicle" so the UI can render measured-vs-estimated honestly.
// Multi-vehicle households (one wallbox, multiple Teslas) are picked
// among by charging_state ranking — see decorateWithVehicle.
func (s *Server) handleLoadpoints(w http.ResponseWriter, r *http.Request) {
	if s.deps.Loadpoints == nil {
		writeJSON(w, 200, map[string]any{"enabled": false, "loadpoints": []any{}})
		return
	}
	states := s.deps.Loadpoints.States()
	if s.deps.Tel != nil {
		decorateLoadpointsWithVehicle(states, s.deps.Tel)
	}
	s.decorateLoadpointsWithManual(states)
	writeJSON(w, 200, map[string]any{
		"enabled":    true,
		"loadpoints": states,
	})
}

// decorateLoadpointsWithVehicle overlays the best-matching DerVehicle
// reading onto each plugged-in loadpoint state. Mutates the input
// slice in place. Picker (rank + freshness + bounds) lives in
// telemetry.PickBestVehicleForLoadpoint so main.go's MPC plumbing and
// this presentation path agree on which vehicle is "the one".
//
// Pairing is decided per loadpoint: when a loadpoint is currently
// delivering power, the pick is gated on charging_state ∈
// {Charging, Starting}. That prevents a second vehicle (parked at
// home, returning SoC, but not on this charger) from winning the
// pick on freshness alone and flipping the loadpoint's SoC source
// every tick — the failure mode observed with two Teslas in the same
// household.
//
// CurrentSoCPct is intentionally NOT overwritten with the BMS reading.
// The loadpoint controller uses CurrentSoCPct as its inference state;
// overlaying it from the BMS would mean the UI shows BMS truth while
// the controller's plan was computed from the inferred value the
// previous tick — a presentation lie. VehicleSoCPct exposes the BMS
// value separately; the frontend renders both and labels which one
// the controller used.
func decorateLoadpointsWithVehicle(states []loadpoint.State, tel *telemetry.Store) {
	if len(tel.ReadingsByType(telemetry.DerVehicle)) == 0 {
		// No vehicle drivers — mark every plugged-in lp as inferred.
		for i := range states {
			if states[i].PluggedIn {
				states[i].SoCSource = "inferred"
			}
		}
		return
	}
	now := time.Now()
	for i := range states {
		if !states[i].PluggedIn {
			continue
		}
		delivering := states[i].CurrentPowerW > loadpoint.DeliveringW
		pick := telemetry.PickBestVehicleForLoadpoint(tel, delivering, now)
		if pick.Driver == "" {
			states[i].SoCSource = "inferred"
			continue
		}
		states[i].VehicleDriver = pick.Driver
		states[i].VehicleSoCPct = pick.SoCPct
		states[i].VehicleChargeLimitPct = pick.ChargeLimitPct
		states[i].VehicleChargingState = pick.ChargingState
		states[i].VehicleStale = pick.Stale
		states[i].SoCSource = "vehicle"
	}
}

// POST /api/loadpoints/{id}/target sets user intent for an EV
// loadpoint: the SoC % the vehicle should reach by the target time.
// Triggers an MPC replan so the new target takes effect within one
// control cycle.
//
// Body: {"soc_pct": 80, "target_time_ms": 1745000000000}
//
// target_time_ms == 0 → no deadline (charge opportunistically).
func (s *Server) handleLoadpointTarget(w http.ResponseWriter, r *http.Request) {
	if s.deps.Loadpoints == nil {
		writeJSON(w, 404, map[string]string{"error": "loadpoints not configured"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	// All three fields are pointers so a caller can omit any of them
	// and the corresponding piece of loadpoint state is preserved.
	// This matters most for surplus_only-only flips (e.g. the "PV
	// surplus" checkbox in the UI), which used to silently zero the
	// SoC target + deadline because SoCPct/TargetTimeMs defaulted to
	// 0 and the handler unconditionally called SetTarget. To clear
	// the target the way the legacy client does, pass an explicit
	// `{"soc_pct": 0, "target_time_ms": 0}` — pointers to zero are
	// distinct from nil here.
	// Schedule uses json.RawMessage so the handler can distinguish three
	// states the regular struct-pointer trick can't: absent (leave alone),
	// null (clear), or object (set). encoding/json collapses absent/null
	// to nil for *struct pointers, which would lose the explicit-clear
	// signal the UI needs.
	var req struct {
		SoCPct       *float64        `json:"soc_pct,omitempty"`
		TargetTimeMs *int64          `json:"target_time_ms,omitempty"`
		SurplusOnly  *bool           `json:"surplus_only,omitempty"`
		Schedule     json.RawMessage `json:"schedule,omitempty"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if req.SoCPct == nil && req.TargetTimeMs == nil && req.SurplusOnly == nil && len(req.Schedule) == 0 {
		writeJSON(w, 400, map[string]string{"error": "no fields to update"})
		return
	}

	// Schedule first: when set, it implies target_soc_pct + target_time
	// values that SetTarget below will read back, so apply order matters.
	scheduleChanged := false
	if len(req.Schedule) > 0 {
		if bytes.Equal(bytes.TrimSpace(req.Schedule), []byte("null")) {
			if !s.deps.Loadpoints.ClearSchedule(id) {
				writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
				return
			}
		} else {
			var sched loadpoint.Schedule
			if err := json.Unmarshal(req.Schedule, &sched); err != nil {
				writeJSON(w, 400, map[string]string{"error": "invalid schedule: " + err.Error()})
				return
			}
			if sched.TimeOfDayMinUTC < 0 || sched.TimeOfDayMinUTC >= 1440 {
				writeJSON(w, 400, map[string]string{"error": "time_of_day_min_utc must be 0..1439"})
				return
			}
			if !s.deps.Loadpoints.SetSchedule(id, sched) {
				writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
				return
			}
			// Roll immediately so the upcoming SetTarget read-modify-write
			// sees the schedule-implied deadline, not stale state.
			s.deps.Loadpoints.RollSchedules(time.Now().UTC())
		}
		scheduleChanged = true
	}
	if req.SoCPct != nil || req.TargetTimeMs != nil {
		// SetTarget always takes both fields, so when the caller
		// omitted one we have to look up the existing value to
		// preserve it. Read-modify-write under the manager's lock
		// is two RLocks (states + setter) which is fine off the
		// hot path; the alternative would be a richer API surface.
		st, ok := s.deps.Loadpoints.State(id)
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
			return
		}
		soc := st.TargetSoCPct
		if req.SoCPct != nil {
			soc = *req.SoCPct
		}
		deadline := st.TargetTime
		if req.TargetTimeMs != nil {
			if *req.TargetTimeMs > 0 {
				deadline = time.UnixMilli(*req.TargetTimeMs).UTC()
			} else {
				deadline = time.Time{}
			}
		}
		if !s.deps.Loadpoints.SetTarget(id, soc, deadline) {
			writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
			return
		}
	}
	surplusDisabled := false
	if req.SurplusOnly != nil {
		prev, ok := s.deps.Loadpoints.SetSurplusOnly(id, *req.SurplusOnly)
		if !ok {
			writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
			return
		}
		// Disabling surplus_only is a planner regime change: the
		// terminal SoC credit flips from self-consumption back to the
		// arbitrage default (much higher), the grid-charge ban lifts,
		// and the LP may now be eligible for grid-arbitrage scheduling
		// (when target_soc_pct > 0). Force a synchronous replan with a
		// tagged reason so the new schedule is in place by the time
		// this HTTP response returns and the diagnose snapshot records
		// "why" the plan changed at this timestamp.
		if prev && !*req.SurplusOnly {
			surplusDisabled = true
		}
	}
	// Force-wake the bound vehicle on any schedule edit. Without this
	// the next plan + dispatch tick reads stale vehicle state — the
	// new schedule could be planning against an old SoC, old vehicle
	// charge_limit, or a "Complete" status that no longer reflects
	// reality. Fire-and-forget on a background goroutine so the API
	// stays snappy even when the BLE proxy is slow / asleep. Bounded
	// timeout so a hung wake never leaks. Bypasses the auto-wake
	// cooldown — the operator just told us they want a fresh read.
	if scheduleChanged && s.deps.LoadpointCtrl != nil {
		go func(lpID string) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := s.deps.LoadpointCtrl.RefreshVehicle(ctx, lpID); err != nil {
				slog.Warn("loadpoint refresh-vehicle failed", "lp", lpID, "err", err)
			}
		}(id)
	}
	if s.deps.MPC != nil {
		if surplusDisabled {
			slog.Info("loadpoint surplus_only disabled — forcing replan",
				"lp", id)
			// Synchronous + fresh context (the request context dies the
			// moment we writeJSON). Replan typically completes in
			// <100ms for current grid sizes; the API caller blocks
			// briefly and returns to a UI that can immediately fetch
			// /api/mpc/plan and see the new schedule.
			s.deps.MPC.ReplanWithReason(context.Background(), "surplus_only_disabled")
		} else if scheduleChanged {
			slog.Info("loadpoint schedule changed — forcing replan", "lp", id)
			s.deps.MPC.ReplanWithReason(context.Background(), "loadpoint_schedule_changed")
		} else {
			// Other field changes: replan is helpful but not load-
			// bearing — kick it off in the background so the API stays
			// snappy. The goroutine uses a fresh context for the same
			// reason as above (request ctx cancellation).
			go s.deps.MPC.ReplanWithReason(context.Background(), "loadpoint_target_changed")
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /api/loadpoints/{id}/soc lets the operator correct the
// inferred vehicle SoC. Most EV chargers are blind to the
// vehicle's BMS — without a vehicle-side API integration
// we have no way to know actual SoC. We infer from
// `plugin_soc_pct + delivered_wh / capacity`, but if the plug-in
// anchor was wrong the estimate drifts. This endpoint re-anchors so
// `current_soc_pct` equals the value the operator reads off their
// car, and future observations accumulate from there.
//
// Body: {"soc_pct": 60}
//
// Returns 409 if the loadpoint is unplugged (can't set SoC on a
// vehicle that isn't in the session).
func (s *Server) handleLoadpointSoC(w http.ResponseWriter, r *http.Request) {
	if s.deps.Loadpoints == nil {
		writeJSON(w, 404, map[string]string{"error": "loadpoints not configured"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	var req struct {
		SoCPct float64 `json:"soc_pct"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	// Confirm loadpoint exists before inspecting plug state.
	if _, ok := s.deps.Loadpoints.State(id); !ok {
		writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
		return
	}
	if !s.deps.Loadpoints.SetCurrentSoC(id, req.SoCPct) {
		writeJSON(w, 409, map[string]string{
			"error": "loadpoint not plugged in — SoC can only be set during an active session",
		})
		return
	}
	// Trigger replan so the corrected SoC feeds into the next plan.
	if s.deps.MPC != nil {
		go s.deps.MPC.Replan(r.Context())
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// GET /api/notifications/status — reports enabled + counters.
func (s *Server) handleNotificationsStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.Notifications == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, 200, s.deps.Notifications.Status())
}

// POST /api/notifications/test — dispatches a test message via the event
// bus so the core never reaches into the notifications service directly.
// Returns the dispatch error (if any) from the Reply channel.
func (s *Server) handleNotificationsTest(w http.ResponseWriter, r *http.Request) {
	if s.deps.Notifications == nil {
		writeJSON(w, 503, map[string]string{"error": "notifications not configured"})
		return
	}
	if s.deps.Events == nil {
		// No bus wired — fall back to direct call so the endpoint is
		// still usable in tests that don't spin up a bus.
		if err := s.deps.Notifications.SendTest(); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "sent"})
		return
	}
	reply := make(chan error, 1)
	s.deps.Events.Publish(events.NotificationTest{Reply: reply})
	select {
	case err := <-reply:
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "sent"})
	case <-time.After(11 * time.Second):
		writeJSON(w, 504, map[string]string{"error": "notification timeout"})
	}
}

// GET /api/notifications/defaults — exposes the built-in template
// strings so the settings UI can pre-fill inputs with exactly what the
// backend renders when the operator leaves a custom template blank.
func (s *Server) handleNotificationsDefaults(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, notifications.EventDefaults())
}

// GET /api/notifications/history?limit=N — recent notification dispatches
// persisted to state.notification_log. limit is clamped to [1, 500];
// defaults to 100.
func (s *Server) handleNotificationsHistory(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 200, []any{})
		return
	}
	limit := 100
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.deps.State.RecentNotifications(limit)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, rows)
}
