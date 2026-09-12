package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A browser must refuse an ES module served with a non-JavaScript
// Content-Type once X-Content-Type-Options: nosniff is set — and FTW sets it.
// http.ServeFile alone asks the operating system's MIME table (the registry
// on Windows), so a host that maps .mjs to text/plain would break the
// vendored MapLibre import outright. The served type must come from the app.
func TestStaticAssetsServeWithPinnedContentTypes(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "vendor", "maplibre")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(sub, "maplibre-gl.mjs"): "export default {};",
		filepath.Join(dir, "app.js"):          "// classic script",
		filepath.Join(dir, "style.css"):       "body{}",
		filepath.Join(dir, "logo.svg"):        `<svg xmlns="http://www.w3.org/2000/svg"/>`,
	}
	for p, content := range files {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(&Deps{Version: "test", WebDir: dir})

	for path, want := range map[string]string{
		"/vendor/maplibre/maplibre-gl.mjs": "text/javascript",
		"/app.js":                          "text/javascript",
		"/style.css":                       "text/css",
		"/logo.svg":                        "image/svg+xml",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("%s: status = %d, want 200", path, rr.Code)
		}
		if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, want) {
			t.Errorf("%s: Content-Type = %q, want prefix %q", path, got, want)
		}
	}
}
