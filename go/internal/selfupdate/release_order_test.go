package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

// A stable may be promoted after newer betas ship, so GitHub lists it above
// a higher beta. Beta boxes must still get the highest version.
func TestCheck_BetaPrefersNewerBetaListedBelowPromotedStable(t *testing.T) {
	releases := fakeReleasesServer(t, nil,
		fakeRelease{tag: "v0.158.0"},
		fakeRelease{tag: "v0.159.0-beta.1", prerelease: true},
		fakeRelease{tag: "v0.158.0-beta.3", prerelease: true},
	)
	for _, tc := range []struct {
		current string
		update  bool
	}{
		{"v0.157.2-beta.1", true},
		{"v0.158.0-beta.3", true},
		{"v0.159.0-beta.1", false},
	} {
		t.Run(tc.current, func(t *testing.T) {
			c := newCheckerOnFakes(t, tc.current, releases, newMemStore())
			if err := c.SetChannel(ChannelBeta); err != nil {
				t.Fatalf("SetChannel: %v", err)
			}
			info, err := c.Check(context.Background(), true)
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if info.Latest != "v0.159.0-beta.1" || info.UpdateAvailable != tc.update {
				t.Fatalf("latest=%q update=%v, want v0.159.0-beta.1 update=%v", info.Latest, info.UpdateAvailable, tc.update)
			}
		})
	}

	stable := newCheckerOnFakes(t, "v0.157.2", releases, newMemStore())
	info, err := stable.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("stable Check: %v", err)
	}
	if info.Channel != ChannelStable || info.Latest != "v0.158.0" || !info.UpdateAvailable {
		t.Fatalf("stable info = %+v", info)
	}
}

// pagedReleasesServer serves one page per request and links each page to
// the next, as GitHub does.
func pagedReleasesServer(t *testing.T, calls *atomic.Int32, pages ...[]fakeRelease) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page+1 < len(pages) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/releases?page=%d>; rel="next"`, srv.URL, page+1))
		}
		list := make([]map[string]any, 0, len(pages[page]))
		for _, rel := range pages[page] {
			list = append(list, rel.json())
		}
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheck_BetaWalksPastStableToTheFirstBetaAndStops(t *testing.T) {
	var calls atomic.Int32
	releases := pagedReleasesServer(t, &calls,
		[]fakeRelease{{tag: "v0.158.0"}},
		[]fakeRelease{{tag: "v0.159.0-beta.1", prerelease: true}},
		[]fakeRelease{{tag: "v0.157.0"}},
	)
	c := newCheckerOnFakes(t, "v0.158.0-beta.3", releases, newMemStore())
	if err := c.SetChannel(ChannelBeta); err != nil {
		t.Fatalf("SetChannel: %v", err)
	}
	info, err := c.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Latest != "v0.159.0-beta.1" || !info.UpdateAvailable {
		t.Fatalf("beta info = %+v", info)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("pages fetched = %d, want 2: the walk stops at the first beta", got)
	}
}
