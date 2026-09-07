package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/mpc"
)

func TestMPCDisabledEndpointsNameTheSkipReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		deps *Deps
		want string
	}{
		{
			name: "planner off",
			deps: &Deps{Cfg: &config.Config{}},
			want: mpc.ReasonPlannerDisabled,
		},
		{
			name: "no price provider",
			deps: &Deps{
				Cfg: &config.Config{
					Planner: &config.Planner{Enabled: true},
					Price:   &config.Price{Provider: "none"},
				},
				Capacities: map[string]float64{"battery": 9600},
			},
			want: mpc.ReasonNoPriceProvider,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := New(tc.deps)
			for _, path := range []string{"/api/mpc/plan", "/api/mpc/diagnose"} {
				req := httptest.NewRequest(http.MethodGet, path, nil)
				rr := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					t.Fatalf("%s: status %d, body %s", path, rr.Code, rr.Body.String())
				}
				var body struct {
					Enabled bool   `json:"enabled"`
					Reason  string `json:"reason"`
				}
				if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
					t.Fatalf("%s: decode: %v", path, err)
				}
				if body.Enabled {
					t.Fatalf("%s: enabled=true, want false", path)
				}
				if body.Reason != tc.want {
					t.Fatalf("%s: reason=%q, want %q", path, body.Reason, tc.want)
				}
			}
		})
	}
}
