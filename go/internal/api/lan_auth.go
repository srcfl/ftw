package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/srcfl/ftw/go/internal/state"
)

const (
	lanAuthPasswordKey = "lan_auth_password"
	// Any password the owner chooses is accepted, however short: the page
	// warns about a short one but does not refuse it (Fredrik, 25 October
	// 2026). The upper bound only keeps the hashed input small.
	maxLANPasswordLen = 512

	lanGuessLimit    = 5
	lanGuessCooldown = 30 * time.Second

	lanSessionCookieName = "ftw_lan"
	lanSessionBytes      = 32
	lanSessionTTL        = 30 * 24 * time.Hour

	// Encoded in the stored hash so a later bump still verifies old rows.
	lanArgonTime    uint32 = 3
	lanArgonMemory  uint32 = 64 * 1024
	lanArgonThreads uint8  = 4
	lanArgonKeyLen  uint32 = 32
	lanArgonSaltLen        = 16
)

type lanSecretContextKey struct{}

func withLANSecret(ctx context.Context, ok bool) context.Context {
	return context.WithValue(ctx, lanSecretContextKey{}, ok)
}

func lanSecretFrom(ctx context.Context) (ok bool, checked bool) {
	v := ctx.Value(lanSecretContextKey{})
	if v == nil {
		return false, false
	}
	ok, _ = v.(bool)
	return ok, true
}

var (
	lanGuessMu          sync.Mutex
	lanGuessFailures    int
	lanGuessLockedUntil time.Time
	lanGuessNow         = time.Now

	lanSessionMu  sync.Mutex
	lanSessions   = map[string]lanSession{}
	lanSessionNow = time.Now
)

type lanSession struct {
	expires time.Time
}

func lanSessionCookieValue(r *http.Request) (string, bool) {
	c, err := r.Cookie(lanSessionCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

func lanSessionValid(token string) bool {
	if token == "" {
		return false
	}
	lanSessionMu.Lock()
	defer lanSessionMu.Unlock()
	digest := lanSessionDigest(token)
	sess, ok := lanSessions[digest]
	if !ok {
		return false
	}
	if !sess.expires.After(lanSessionNow()) {
		delete(lanSessions, digest)
		return false
	}
	return true
}

func issueLANSession() (string, error) {
	raw := make([]byte, lanSessionBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	lanSessionMu.Lock()
	lanSessions[lanSessionDigest(token)] = lanSession{expires: lanSessionNow().Add(lanSessionTTL)}
	lanSessionMu.Unlock()
	return token, nil
}

func dropLANSession(token string) {
	if token == "" {
		return
	}
	lanSessionMu.Lock()
	delete(lanSessions, lanSessionDigest(token))
	lanSessionMu.Unlock()
}

func lanSessionDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func restoreLANSessions(st *state.Store) {
	if st == nil {
		return
	}
	lanSessionMu.Lock()
	defer lanSessionMu.Unlock()
	sessions, err := st.LoadLANSessions(lanSessionNow().UnixMilli())
	if err != nil {
		slog.Warn("LAN sessions could not be restored")
		return
	}
	for _, session := range sessions {
		lanSessions[session.Digest] = lanSession{expires: time.UnixMilli(session.ExpiresMS)}
	}
}

func dropAllLANSessions() {
	lanSessionMu.Lock()
	lanSessions = map[string]lanSession{}
	lanSessionMu.Unlock()
}

func setLANSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     lanSessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// matchLANSecret reports whether secret is the house password without
// touching the guess limiter. Authenticate uses this when the Bearer was
// already compared to FTW_API_TOKEN; a token miss is not a house guess.
func matchLANSecret(verify func(string) bool, secret string) bool {
	return verify != nil && verify(secret)
}

// admitLANSecret is the process-global guess limiter for the house password.
// Five failed VerifyLANSecret calls lock every further attempt, including
// the right password, for 30s. The clock is swapped in tests.
//
// Call this for an explicit house-password guess (login, or a LAN Bearer
// when no API token is configured). Do not call it for a Bearer that was
// compared to FTW_API_TOKEN.
func admitLANSecret(verify func(string) bool, secret string) bool {
	lanGuessMu.Lock()
	defer lanGuessMu.Unlock()
	now := lanGuessNow()
	if !lanGuessLockedUntil.IsZero() && now.Before(lanGuessLockedUntil) {
		return false
	}
	if !lanGuessLockedUntil.IsZero() {
		lanGuessFailures = 0
		lanGuessLockedUntil = time.Time{}
	}
	if verify == nil || !verify(secret) {
		lanGuessFailures++
		if lanGuessFailures >= lanGuessLimit {
			lanGuessLockedUntil = now.Add(lanGuessCooldown)
		}
		return false
	}
	lanGuessFailures = 0
	lanGuessLockedUntil = time.Time{}
	return true
}

// VerifyStoredLANSecret reports whether secret matches the argon2id hash
// in state.db. It does not rate-limit; Authenticate does that.
func VerifyStoredLANSecret(st *state.Store, secret string) bool {
	if st == nil || secret == "" {
		return false
	}
	encoded, ok := st.LoadConfig(lanAuthPasswordKey)
	if !ok || encoded == "" {
		return false
	}
	return verifyLANPassword(encoded, secret)
}

func lanPasswordConfigured(st *state.Store) bool {
	if st == nil {
		return false
	}
	encoded, ok := st.LoadConfig(lanAuthPasswordKey)
	return ok && encoded != ""
}

func hashLANPassword(password string) (string, error) {
	salt := make([]byte, lanArgonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, lanArgonTime, lanArgonMemory, lanArgonThreads, lanArgonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, lanArgonMemory, lanArgonTime, lanArgonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// HashInitialLANPassword lets the first-run server establish the owner's
// lock before the normal API starts. It grants no password-change authority.
func HashInitialLANPassword(password string) (string, error) {
	if password == "" || utf8.RuneCountInString(password) > maxLANPasswordLen {
		return "", fmt.Errorf("house password must be between 1 and %d characters", maxLANPasswordLen)
	}
	return hashLANPassword(password)
}

func verifyLANPassword(encoded, password string) bool {
	salt, want, timeCost, memory, threads, keyLen, ok := parseLANPasswordHash(encoded)
	if !ok {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, timeCost, memory, threads, keyLen)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func parseLANPasswordHash(encoded string) (salt, key []byte, timeCost, memory uint32, threads uint8, keyLen uint32, ok bool) {
	// $argon2id$v=19$m=65536,t=3,p=4$<salt>$<key>
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, 0, 0, 0, 0, false
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return nil, nil, 0, 0, 0, 0, false
	}
	var t, m, p uint64
	for _, field := range strings.Split(parts[3], ",") {
		name, raw, found := strings.Cut(field, "=")
		if !found {
			return nil, nil, 0, 0, 0, 0, false
		}
		n, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return nil, nil, 0, 0, 0, 0, false
		}
		switch name {
		case "m":
			m = n
		case "t":
			t = n
		case "p":
			p = n
		default:
			return nil, nil, 0, 0, 0, 0, false
		}
	}
	if t == 0 || m == 0 || p == 0 || p > 255 {
		return nil, nil, 0, 0, 0, 0, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return nil, nil, 0, 0, 0, 0, false
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return nil, nil, 0, 0, 0, 0, false
	}
	return salt, key, uint32(t), uint32(m), uint8(p), uint32(len(key)), true
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	lanAuth := false
	if s.deps.Cfg != nil && s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
		lanAuth = s.deps.Cfg.API.LANAuth
		s.deps.CfgMu.RUnlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lan_auth":       lanAuth,
		"home_assistant": HomeAssistantIngress(r),
		"configured":     lanPasswordConfigured(s.deps.State),
	})
}

type lanAuthLoginRequest struct {
	Password string `json:"password"`
}

// Keep storage diagnostics free of database paths and session credentials.
func lanSessionStoreErrorCategory(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		switch coded.Code() & 0xff {
		case 5:
			return "sqlite_busy"
		case 6:
			return "sqlite_locked"
		}
	}
	return "storage_error"
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	var req lanAuthLoginRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	// Serialize admission with password changes, so an old-password login
	// cannot add a session after revocation. Do not hold this lock while
	// reading an unauthenticated client's body: it also guards Settings.
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	if s.deps.State == nil || s.deps.Cfg == nil || s.deps.CfgMu == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "config store unavailable"})
		return
	}
	s.deps.CfgMu.RLock()
	lanAuth := s.deps.Cfg.API.LANAuth
	s.deps.CfgMu.RUnlock()
	if !lanAuth {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "LAN auth is not enabled"})
		return
	}
	if !lanPasswordConfigured(s.deps.State) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "LAN password is not configured"})
		return
	}
	st := s.deps.State
	encoded, found, err := st.ConfigValue(lanAuthPasswordKey)
	if err != nil || !found {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "password store unavailable"})
		return
	}
	if !admitLANSecret(func(secret string) bool {
		return verifyLANPassword(encoded, secret)
	}, req.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid password"})
		return
	}
	if status, msg := startLANSession(w, st, encoded); status != 0 {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// startLANSession signs this browser in after it proved the house password,
// whose stored hash is encoded. It sets the session cookie, or returns the
// status and reason to refuse with.
func startLANSession(w http.ResponseWriter, st *state.Store, encoded string) (int, string) {
	token, err := issueLANSession()
	if err != nil {
		return http.StatusInternalServerError, "could not create session"
	}
	lanSessionMu.Lock()
	now := lanSessionNow()
	lanSessionMu.Unlock()
	if err := st.SaveLANSession(lanSessionDigest(token), now.Add(lanSessionTTL).UnixMilli(), now.UnixMilli(), encoded); err != nil {
		dropLANSession(token)
		slog.Warn("LAN session could not be saved", "category", lanSessionStoreErrorCategory(err))
		return http.StatusServiceUnavailable, "could not save session"
	}
	setLANSessionCookie(w, token, int(lanSessionTTL/time.Second))
	return 0, ""
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := lanSessionCookieValue(r); ok {
		if s.deps.State == nil || s.deps.State.DeleteLANSession(lanSessionDigest(token)) != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "could not revoke session"})
			return
		}
		dropLANSession(token)
	}
	// MaxAge < 0 emits Max-Age=0 so the browser drops ftw_lan.
	setLANSessionCookie(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type lanAuthPasswordRequest struct {
	Password string `json:"password"`
	Enabled  *bool  `json:"enabled"`
}

// localBrowser is a direct request on the home network or the box itself:
// no app session, no proxy, and a local host name. Only such a request gets
// the sign-in cookie.
func localBrowser(r *http.Request) bool {
	auth, err := parseAuthority(r.Host)
	return err == nil && isLocalAuthority(auth) && isLocalClient(r.RemoteAddr) &&
		!hasForwardingHeader(r.Header) && !appLinkOverSession(r)
}

func (s *Server) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()
	if s.deps.State == nil || s.deps.Cfg == nil || s.deps.CfgMu == nil || s.deps.SaveConfig == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "config store unavailable"})
		return
	}
	var req lanAuthPasswordRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled is required"})
		return
	}

	s.deps.CfgMu.RLock()
	cfgCopy := *s.deps.Cfg
	currentlyOn := s.deps.Cfg.API.LANAuth
	s.deps.CfgMu.RUnlock()
	proved, _ := lanSecretFrom(r.Context())
	// Authenticate let this request in before the write lock: if the lock
	// came on in between, a LAN request that proved nothing would replace
	// the password the first browser just chose. The box itself, an app
	// session and a remote token caller have their own proof.
	if currentlyOn && !proved && isLocalClient(r.RemoteAddr) && !isLoopbackClient(r.RemoteAddr) && !appLinkOverSession(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ftw-lan"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "valid LAN password required"})
		return
	}
	enabled := *req.Enabled
	if enabled {
		// While the lock is off, whoever reaches this route is already an
		// owner: they may change any setting and pair a phone as an owner
		// (appLinkOwnerMintAllowed). Setting the first password gives them
		// nothing new and closes the door behind them, so the address they
		// call from does not matter: the box itself, a LAN device, or a
		// published Docker port. A visitor who gets there first locks the
		// owner out of Settings, which the owner sees at once and undoes on
		// the box with `ftw password reset` (Fredrik, 25 October 2026).
		// The app does not turn it on: the password is typed on the box's
		// own page.
		if !currentlyOn && appLinkOverSession(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "turn the house password on from the box's own page, not through the app",
			})
			return
		}
		already := lanPasswordConfigured(s.deps.State)
		switch {
		case req.Password == "":
			if !already {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "password required to enable LAN auth"})
				return
			}
		case utf8.RuneCountInString(req.Password) > maxLANPasswordLen:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("password must be at most %d characters", maxLANPasswordLen),
			})
			return
		default:
			encoded, err := hashLANPassword(req.Password)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not hash password"})
				return
			}
			cfgCopy.LANPasswordHash = encoded
		}
	}

	if enabled && req.Password == "" {
		var err error
		cfgCopy.LANPasswordHash, _, err = s.deps.State.ConfigValue(lanAuthPasswordKey)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "read saved password: " + err.Error()})
			return
		}
	}
	cfgCopy.API.LANAuth = enabled
	if !enabled {
		cfgCopy.LANPasswordHash = ""
	}
	if err := s.deps.SaveConfig(s.deps.ConfigPath, &cfgCopy); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save failed: " + err.Error()})
		return
	}
	s.deps.CfgMu.Lock()
	*s.deps.Cfg = cfgCopy
	s.deps.CfgMu.Unlock()
	dropAllLANSessions()
	if err := s.deps.State.DeleteAllLANSessions(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "could not revoke sessions"})
		return
	}
	switch {
	case !enabled:
		slog.Info("audit: house password turned off", "from", remoteHost(r))
	case req.Password != "" && currentlyOn:
		slog.Info("audit: house password changed", "from", remoteHost(r))
	case req.Password != "":
		slog.Info("audit: house password turned on", "from", remoteHost(r))
	}
	// A browser that chose the first password, or changed it while signed
	// in, stays signed in and goes on without typing it again. The box
	// itself may replace a password it never proved, as it may change any
	// setting, but that earns no sign-in. A failed sign-in only means the
	// page asks for the password; the lock is already on.
	signedIn := false
	if enabled && req.Password != "" && (!currentlyOn || proved) && localBrowser(r) {
		status, _ := startLANSession(w, s.deps.State, cfgCopy.LANPasswordHash)
		signedIn = status == 0
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"lan_auth":   enabled,
		"configured": lanPasswordConfigured(s.deps.State),
		"signed_in":  signedIn,
	})
}
