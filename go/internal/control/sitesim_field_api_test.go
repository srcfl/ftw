//go:build !race

package control_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The field matrix reads the EV verdict as the app does, from GET
// /api/loadpoints. The api package imports control, so only this external
// test package can build the server.
func init() {
	control.FieldVerdict = func(tel *telemetry.Store, lps *loadpoint.Manager, ctrl *loadpoint.Controller) func() (string, string) {
		srv := api.New(&api.Deps{Tel: tel, Loadpoints: lps, LoadpointCtrl: ctrl})
		return func() (string, string) {
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/loadpoints", nil))
			var body struct {
				Loadpoints []struct {
					Feedback *struct{ Status, Reason string } `json:"control_feedback"`
				} `json:"loadpoints"`
			}
			if json.Unmarshal(rr.Body.Bytes(), &body) != nil || len(body.Loadpoints) == 0 || body.Loadpoints[0].Feedback == nil {
				return "none", "no control_feedback"
			}
			return body.Loadpoints[0].Feedback.Status, body.Loadpoints[0].Feedback.Reason
		}
	}
}
