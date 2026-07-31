package homelinksession

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

const browserSessionSource = "../../../web/home-link-session.js"

// TestBrowserCeilingMatchesSessionLifetime keeps the two independent clocks
// honest. The gateway stamps an expiry with SessionLifetime and the browser
// checks it against its own clock plus MaxSessionLifetime, so the issued
// lifetime must stay below the browser ceiling by the skew allowance.
func TestBrowserCeilingMatchesSessionLifetime(t *testing.T) {
	if SessionLifetime >= MaxSessionLifetime {
		t.Fatalf("issued lifetime %s leaves no clock-skew headroom below %s",
			SessionLifetime, MaxSessionLifetime)
	}
	if MaxSessionLifetime-SessionLifetime != MaxClockSkew {
		t.Fatalf("headroom %s does not match the declared skew allowance %s",
			MaxSessionLifetime-SessionLifetime, MaxClockSkew)
	}

	source, err := os.ReadFile(browserSessionSource)
	if err != nil {
		t.Fatal(err)
	}
	ceiling := fmt.Sprintf("accept.expires_at_ms > now + %d * 60 * 1000",
		int(MaxSessionLifetime/time.Minute))
	if count := strings.Count(string(source), ceiling); count != 1 {
		t.Fatalf("%s states the browser ceiling %q %d times, want exactly once",
			browserSessionSource, ceiling, count)
	}
}
