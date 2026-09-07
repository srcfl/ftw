package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appproto"
)

func TestRetiredCalendarEndpointsAreNotServed(t *testing.T) {
	srv := New(&Deps{Version: "test", WebDir: t.TempDir()})
	for _, path := range []string{"/api/caldav/status", "/api/caldav/credentials"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", method, path, rec.Code)
			}
		}
	}
}

func TestRetiredCalendarIsUnknownToAppSessions(t *testing.T) {
	for _, role := range []string{apiauth.RoleOwner, apiauth.RoleViewer} {
		for _, path := range []string{"/api/caldav/status", "/api/caldav/credentials"} {
			rig := newTieredSession(t, role)
			rig.send(t, appproto.MsgAPIReq, 1, appproto.APIReq{Method: appproto.APIGet, Path: path, StepUp: true})
			refusal := decode[appproto.ErrorBody](t, rig.frames.await(t, appproto.MsgError))
			if refusal.Code != appproto.ErrUnknownOp || rig.frames.has(appproto.MsgAPIHead) {
				t.Fatalf("%s %s: %+v", role, path, refusal)
			}
		}
	}
}
