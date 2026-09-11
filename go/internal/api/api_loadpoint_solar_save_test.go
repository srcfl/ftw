package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func TestSolarPreferenceStorageFailureKeepsChoiceAndRetrySaves(t *testing.T) {
	for _, previous := range []bool{false, true} {
		t.Run(strconv.FormatBool(previous), func(t *testing.T) {
			m := loadpoint.NewManager()
			m.Load([]loadpoint.Config{{ID: "garage", DriverName: "easee", SurplusOnly: previous}})
			saved, fail := previous, true
			m.SetSurplusOnlySaver(func(_ string, v bool) error {
				if fail {
					return errors.New("disk full")
				}
				saved = v
				return nil
			})
			srv := New(&Deps{Loadpoints: m})
			request := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/api/loadpoints/garage/target",
					strings.NewReader(`{"surplus_only":`+strconv.FormatBool(!previous)+`}`))
				r.Header.Set("Content-Type", "application/json")
				rr := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rr, r)
				return rr
			}
			rr := request()
			if rr.Code != http.StatusInternalServerError || !strings.Contains(rr.Body.String(), "previous choice is unchanged") {
				t.Fatalf("failed save returned %d: %s", rr.Code, rr.Body.String())
			}
			if s, _ := m.State("garage"); s.SurplusOnly != previous || saved != previous {
				t.Fatalf("failure changed solar preference: state=%v saved=%v", s.SurplusOnly, saved)
			}
			fail = false
			if rr = request(); rr.Code != http.StatusOK {
				t.Fatalf("retry returned %d: %s", rr.Code, rr.Body.String())
			}
			if s, _ := m.State("garage"); s.SurplusOnly == previous || saved != s.SurplusOnly {
				t.Fatalf("retry did not save the active preference: state=%v saved=%v", s.SurplusOnly, saved)
			}
		})
	}
}
