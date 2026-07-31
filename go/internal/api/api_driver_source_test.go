package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSourceDriver(t *testing.T, dir, filename, id, version, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	src := "DRIVER = {\n" +
		"  id = \"" + id + "\",\n" +
		"  version = \"" + version + "\",\n" +
		"  protocols = { \"modbus\" },\n" +
		"}\n" + body
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(src), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
}

func getDriverSource(t *testing.T, srv *Server, id string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/drivers/"+id+"/source", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var body map[string]any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
	}
	return rr.Code, body
}

// The three overlays are searched in the order the driver resolver uses them,
// so the file returned is the file running. Showing the bundled copy while an
// override is what actually runs would send someone debugging the wrong code.
func TestDriverSourceReturnsTheFileThatIsRunning(t *testing.T) {
	bundled := t.TempDir()
	user := t.TempDir()
	writeSourceDriver(t, bundled, "demo.lua", "demo", "1.0.0", "function driver_poll() return 1 end\n")

	srv := New(&Deps{DriverDir: bundled, UserDriverDir: user})

	code, body := getDriverSource(t, srv, "demo")
	if code != 200 {
		t.Fatalf("bundled read = %d %v", code, body)
	}
	if body["source"] != "bundled" || body["version"] != "1.0.0" {
		t.Fatalf("bundled entry = %+v", body)
	}
	if lua, _ := body["lua"].(string); lua == "" || !strings.Contains(lua, "driver_poll") {
		t.Fatalf("bundled lua = %q", lua)
	}

	// An override shadows the channel and the build, so it must win here too.
	writeSourceDriver(t, user, "demo.lua", "demo", "1.0.0", "function driver_poll() return 2 end\n")
	code, body = getDriverSource(t, srv, "demo")
	if code != 200 {
		t.Fatalf("override read = %d %v", code, body)
	}
	if body["source"] != "local" {
		t.Fatalf("source = %v, want local once an override exists", body["source"])
	}
	if lua, _ := body["lua"].(string); !strings.Contains(lua, "return 2") {
		t.Fatalf("override lua = %q, want the operator's own file", lua)
	}
}

func TestDriverSourceReportsTheHashOfWhatItReturned(t *testing.T) {
	bundled := t.TempDir()
	writeSourceDriver(t, bundled, "demo.lua", "demo", "1.0.0", "function driver_poll() return 1 end\n")
	srv := New(&Deps{DriverDir: bundled})

	_, body := getDriverSource(t, srv, "demo")
	// The hash is what an edit is diffed against, so it has to describe the
	// bytes in this response rather than a manifest entry that may be stale.
	raw, err := os.ReadFile(filepath.Join(bundled, "demo.lua"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := body["bytes"], float64(len(raw)); got != want {
		t.Fatalf("bytes = %v, want %v", got, want)
	}
	if digest, _ := body["sha256"].(string); len(digest) != 64 {
		t.Fatalf("sha256 = %q", digest)
	}
}

func TestDriverSourceLinksToTheFileInTheRepository(t *testing.T) {
	bundled := t.TempDir()
	writeSourceDriver(t, bundled, "sungrow.lua", "sungrow", "1.5.0", "function driver_poll() return 1 end\n")
	srv := New(&Deps{DriverDir: bundled})

	_, body := getDriverSource(t, srv, "sungrow")
	want := "https://github.com/srcfl/device-drivers/blob/main/drivers/lua/sungrow.lua"
	if body["repository_url"] != want {
		t.Fatalf("repository_url = %v, want %v", body["repository_url"], want)
	}
}

func TestDriverSourceRefusesAnUnknownDriver(t *testing.T) {
	srv := New(&Deps{DriverDir: t.TempDir()})
	if code, _ := getDriverSource(t, srv, "absent"); code != 404 {
		t.Fatalf("unknown driver = %d, want 404", code)
	}
}

// A logical path is remote input on a managed entry. Only the base name is
// used, and only when it names a Lua file.
func TestDriverRepositoryURLIgnoresPathsItCannotVouchFor(t *testing.T) {
	for _, path := range []string{"", ".", "/", "drivers/", "drivers/evil.sh"} {
		if got := driverRepositoryURL(path); got != "" {
			t.Errorf("driverRepositoryURL(%q) = %q, want empty", path, got)
		}
	}
	if got := driverRepositoryURL("drivers/../../etc/passwd"); got != "" {
		t.Errorf("traversal produced %q", got)
	}
	if got := driverRepositoryURL("drivers/pixii.lua"); got != driverRepositoryBase+"pixii.lua" {
		t.Errorf("normal path produced %q", got)
	}
}
