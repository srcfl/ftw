package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/srcfl/ftw/go/internal/state"
)

const (
	lanAuthPasswordKey = "lan_auth_password"
	minLANPasswordLen  = 10

	lanGuessLimit    = 5
	lanGuessCooldown = 30 * time.Second

	lanSessionCookieName = "ftw_lan"
	lanSessionBytes      = 32
	lanSessionTTL        = 12 * time.Hour

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
	sess, ok := lanSessions[token]
	if !ok {
		return false
	}
	if !sess.expires.After(lanSessionNow()) {
		delete(lanSessions, token)
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
	lanSessions[token] = lanSession{expires: lanSessionNow().Add(lanSessionTTL)}
	lanSessionMu.Unlock()
	return token, nil
}

func dropLANSession(token string) {
	if token == "" {
		return
	}
	lanSessionMu.Lock()
	delete(lanSessions, token)
	lanSessionMu.Unlock()
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

// admitLANSecret is the process-global guess limiter for the house password.
// Five failed VerifyLANSecret calls lock every further attempt, including
// the right password, for 30s. The clock is swapped in tests.
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

func (s *Server) handleAuthStatus(w http.ResponseWriter, _ *http.Request) {
	lanAuth := false
	if s.deps.Cfg != nil && s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
		lanAuth = s.deps.Cfg.API.LANAuth
		s.deps.CfgMu.RUnlock()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lan_auth":   lanAuth,
		"configured": lanPasswordConfigured(s.deps.State),
	})
}

type lanAuthLoginRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
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
	var req lanAuthLoginRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	st := s.deps.State
	if !admitLANSecret(func(secret string) bool {
		return VerifyStoredLANSecret(st, secret)
	}, req.Password) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid password"})
		return
	}
	token, err := issueLANSession()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create session"})
		return
	}
	setLANSessionCookie(w, token, int(lanSessionTTL/time.Second))
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if token, ok := lanSessionCookieValue(r); ok {
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
	s.deps.CfgMu.RUnlock()
	enabled := *req.Enabled
	if enabled {
		s.deps.CfgMu.RLock()
		currentlyOn := s.deps.Cfg.API.LANAuth
		s.deps.CfgMu.RUnlock()
		// While the lock is off every LAN peer is an owner. First enable
		// from the LAN is how a visitor sets their own password and
		// locks Settings. The box itself (loopback) is the only door
		// that may turn the lock on.
		if !currentlyOn && !isLoopbackClient(r.RemoteAddr) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "enable the house password from loopback inside the process. A published Docker port is not loopback — on Docker Desktop use docker compose -f docker-compose.macos.yml exec ftw, then curl http://127.0.0.1:8080",
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
		case len(req.Password) < minLANPasswordLen:
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("password must be at least %d characters", minLANPasswordLen),
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

	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"lan_auth":   enabled,
		"configured": lanPasswordConfigured(s.deps.State),
	})
}
