package calendar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

func TestProvisionHtpasswdWritesBcrypt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users")
	mc := true
	s := New(config.CalDAV{
		Enabled: true, Username: "fortytwowatts", Password: "s3cr3t-pw",
		ManageCredentials: &mc, HtpasswdPath: path,
	}, &fakeLP{}, &fakeLM{}, "garage")

	s.provisionHtpasswd()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("htpasswd not written: %v", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 2)
	if len(parts) != 2 || parts[0] != "fortytwowatts" {
		t.Fatalf("unexpected htpasswd line: %q", string(data))
	}
	if err := bcrypt.CompareHashAndPassword([]byte(parts[1]), []byte("s3cr3t-pw")); err != nil {
		t.Fatalf("bcrypt hash does not verify the password: %v", err)
	}
	// Idempotent: a second call rewrites a single valid line.
	s.provisionHtpasswd()
	again, _ := os.ReadFile(path)
	if strings.Count(strings.TrimSpace(string(again)), "\n") != 0 {
		t.Fatalf("expected exactly one credential line, got: %q", string(again))
	}
}

func TestProvisionHtpasswdSkipsWhenDirMissing(t *testing.T) {
	mc := true
	path := filepath.Join(t.TempDir(), "missing", "users") // parent dir absent
	s := New(config.CalDAV{Enabled: true, Username: "u", Password: "p", ManageCredentials: &mc, HtpasswdPath: path}, &fakeLP{}, &fakeLM{}, "")
	s.provisionHtpasswd() // must be a fail-soft no-op, not a panic
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("must not create the htpasswd when its directory is missing")
	}
}

func TestGenerateTokenNonEmptyAndDistinct(t *testing.T) {
	a, err := GenerateToken(18)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateToken(18)
	if a == "" || a == b {
		t.Fatalf("tokens should be non-empty and distinct: %q vs %q", a, b)
	}
	if len(a) < 20 {
		t.Fatalf("token unexpectedly short: %q", a)
	}
}

func TestManagedUsernameDefault(t *testing.T) {
	mc := true
	s := New(config.CalDAV{Enabled: true, ManageCredentials: &mc}, &fakeLP{}, &fakeLM{}, "garage")
	if got := s.Credentials().Username; got != config.DefaultCalDAVUsername {
		t.Fatalf("managed username default: want %q, got %q", config.DefaultCalDAVUsername, got)
	}
}
