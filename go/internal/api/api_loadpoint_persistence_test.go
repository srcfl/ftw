package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

type blockedEVPersistence struct{ err error }

func (*blockedEVPersistence) LoadConfig(string) (string, bool) { return "", false }
func (*blockedEVPersistence) SaveConfig(string, string) error  { return nil }
func (p *blockedEVPersistence) Flush(context.Context) error    { return p.err }

func TestManualHoldDoesNotClaimSaveAfterPersistenceFailure(t *testing.T) {
	srv, ctrl := newManualHoldServer(t)
	srv.deps.Loadpoints.SetSessionStore(&blockedEVPersistence{err: errors.New("disk stalled")})
	ctrl.SetManualHoldSaver(func(id string, hold loadpoint.ManualHold, cleared bool) {
		_ = srv.deps.Loadpoints.PersistManualHold(id, hold, cleared)
	})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/manual_hold", strings.NewReader(`{"power_w":0,"hold_s":0}`))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "choice is active") {
		t.Fatal(rr.Code, rr.Body.String())
	}
	if hold, active := ctrl.GetManualHold("garage", srv.deps.Loadpoints.States()[0].TargetTime); !active || hold.PowerW != 0 {
		t.Fatal("a disk failure must not undo the active Stop", hold, active)
	}
}
