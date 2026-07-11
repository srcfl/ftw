package calendar

import (
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

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
