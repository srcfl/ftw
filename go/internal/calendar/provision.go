package calendar

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"

	"golang.org/x/crypto/bcrypt"
)

// GenerateToken returns a URL-safe random secret carrying nBytes of entropy.
// Used by main.go to mint the managed Radicale password on first enable.
func GenerateToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// provisionHtpasswd writes the managed Radicale credential — "username:bcrypt"
// — to the htpasswd file Radicale reads, so the operator never runs `htpasswd`
// by hand. Idempotent (it rewrites the single managed line each call) and
// fail-soft: when the target directory is absent (e.g. a raw-binary deploy
// without the shared ./radicale/config mount) it logs and returns, leaving any
// operator-managed users file untouched.
func (s *Service) provisionHtpasswd() {
	s.mu.RLock()
	manage, path, user, pass := s.manageCreds, s.htpasswdPath, s.username, s.password
	s.mu.RUnlock()
	if !manage || path == "" || user == "" || pass == "" {
		return
	}
	dir := filepath.Dir(path)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		slog.Warn("caldav: manage_credentials is on but the htpasswd directory is not present; "+
			"skipping credential provisioning (mount ./radicale/config into 42W, or set caldav.htpasswd_path)",
			"dir", dir)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		slog.Warn("caldav: bcrypt hashing failed; skipping htpasswd provisioning", "err", err)
		return
	}
	// Atomic replace so Radicale never reads a half-written file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(user+":"+string(hash)+"\n"), 0o600); err != nil {
		slog.Warn("caldav: failed to write htpasswd temp file", "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		slog.Warn("caldav: failed to install htpasswd file", "err", err)
		_ = os.Remove(tmp)
		return
	}
	slog.Info("caldav: wrote managed Radicale credential", "path", path, "user", user)
}
