package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/fleetping"
)

func driverUsageServer(t *testing.T, usage *fleetping.Usage) *Server {
	t.Helper()
	dir := t.TempDir()
	lua := "DRIVER = {\n  id = \"pixii\",\n  name = \"Pixii\",\n  version = \"2.1.9\",\n}\n"
	for _, name := range []string{"pixii.lua"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(lua), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return New(&Deps{DriverDir: dir, DriverUsage: usage})
}

func readDriverCatalog(t *testing.T, srv *Server) (run map[string]int, entries []map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/drivers/catalog", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		RunBy   map[string]int   `json:"run_by"`
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.RunBy, body.Entries
}

func TestDriverCatalogSaysHowManyRunThatVersion(t *testing.T) {
	feed := `{"days":[{"date":"2026-11-07","drivers":{"pixii@2.1.9":1,"pixii@2.1.8":14}}]}`
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(feed)) }))
	defer cloud.Close()
	run, entries := readDriverCatalog(t, driverUsageServer(t, fleetping.NewUsage(cloud.URL, nil)))
	if run["pixii@2.1.8"] != 14 || len(entries) != 1 || entries[0]["run_by"] != float64(1) {
		t.Fatalf("run_by = %v, entries = %v", run, entries)
	}
}

func TestDriverCatalogHasNoCountWithoutTheFeed(t *testing.T) {
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer cloud.Close()
	run, entries := readDriverCatalog(t, driverUsageServer(t, fleetping.NewUsage(cloud.URL, nil)))
	if run != nil || len(entries) != 1 {
		t.Fatalf("run_by = %v, entries = %v", run, entries)
	}
	if _, ok := entries[0]["run_by"]; ok {
		t.Fatalf("an unknown count became a number: %v", entries[0])
	}
}
