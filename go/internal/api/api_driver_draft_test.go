package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
)

// A real registry, so a draft is only green if the edited file actually loads
// and the driver actually comes back on it.
func draftServer(t *testing.T) (*Server, string, string, *config.Config) {
	t.Helper()
	bundled := t.TempDir()
	user := t.TempDir()
	writeSourceDriver(t, bundled, "demo.lua", "demo", "1.0.0",
		"function driver_init(cfg) end\nfunction driver_poll() return 1000 end\n")

	reg := drivers.NewRegistry(nil)
	t.Cleanup(reg.ShutdownAll)
	cfg := &config.Config{Drivers: []config.Driver{
		{Name: "demo-1", Lua: filepath.Join(bundled, "demo.lua")},
	}}
	srv := New(&Deps{
		DriverDir: bundled, UserDriverDir: user,
		Registry: reg, Cfg: cfg, CfgMu: &sync.RWMutex{},
	})
	return srv, bundled, user, cfg
}

// runningLua is the path the configured driver would be restarted from.
func runningLua(cfg *config.Config) string { return cfg.Drivers[0].Lua }

func postDraft(t *testing.T, srv *Server, id, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/"+id+"/draft", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func postDraftAction(t *testing.T, srv *Server, id, action string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/"+id+"/draft/"+action, nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

const draftLua = "DRIVER = {\n  id = \"demo\",\n  version = \"1.0.0\",\n" +
	"  protocols = { \"modbus\" },\n}\nfunction driver_init(cfg) end\nfunction driver_poll() return 99 end\n"

func TestDraftRunsTheEditedDriverAndRestartsIt(t *testing.T) {
	srv, _, user, cfg := draftServer(t)

	code, body := postDraft(t, srv, "demo", `{"lua":`+quote(draftLua)+`,"minutes":5}`)
	if code != 200 {
		t.Fatalf("draft = %d %v", code, body)
	}
	if body["status"] != "running" || body["minutes"] != float64(5) {
		t.Fatalf("draft body = %+v", body)
	}
	// The overlay is searched before the bundled copy, so writing there is
	// what makes the edit the driver that runs.
	live, err := os.ReadFile(filepath.Join(user, "demo.lua"))
	if err != nil || !strings.Contains(string(live), "return 99") {
		t.Fatalf("overlay = %q, %v", live, err)
	}
	// The overlay search runs once, at config load, so cfg.Lua still pointed
	// at the bundled copy. Without repointing it the driver restarts on the
	// file it was already running and the edit does nothing.
	if got := runningLua(cfg); got != filepath.Join(user, "demo.lua") {
		t.Fatalf("running lua = %q, want the draft in the overlay", got)
	}
}

func TestDraftRevertsOnItsOwnWhenTheWindowPasses(t *testing.T) {
	srv, bundled, user, cfg := draftServer(t)
	if code, body := postDraft(t, srv, "demo", `{"lua":`+quote(draftLua)+`,"minutes":1}`); code != 200 {
		t.Fatalf("draft = %d %v", code, body)
	}

	// Walking away is the common case, so expiry is what the promise rests on.
	srv.expireDraft("demo.lua")

	if _, err := os.Stat(filepath.Join(user, "demo.lua")); !os.IsNotExist(err) {
		t.Fatalf("overlay still holds the draft: %v", err)
	}
	if got := runningLua(cfg); got != filepath.Join(bundled, "demo.lua") {
		t.Fatalf("running lua = %q, want the bundled copy back", got)
	}
}

func TestDraftRevertRestoresAnOperatorsOwnOverride(t *testing.T) {
	srv, _, user, _ := draftServer(t)
	// An override that existed before the edit must come back, not be deleted
	// along with the draft that was laid on top of it.
	own := "DRIVER = {\n  id = \"demo\",\n  version = \"1.0.0\",\n}\nfunction driver_poll() return 7 end\n"
	if err := os.WriteFile(filepath.Join(user, "demo.lua"), []byte(own), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, body := postDraft(t, srv, "demo", `{"lua":`+quote(draftLua)+`}`); code != 200 {
		t.Fatalf("draft = %d %v", code, body)
	}
	if code, body := postDraftAction(t, srv, "demo", "revert"); code != 200 {
		t.Fatalf("revert = %d %v", code, body)
	}

	restored, err := os.ReadFile(filepath.Join(user, "demo.lua"))
	if err != nil || !strings.Contains(string(restored), "return 7") {
		t.Fatalf("restored = %q, %v; want the operator's own file back", restored, err)
	}
}

func TestKeepingADraftStopsTheClock(t *testing.T) {
	srv, _, user, _ := draftServer(t)
	if code, _ := postDraft(t, srv, "demo", `{"lua":`+quote(draftLua)+`,"minutes":1}`); code != 200 {
		t.Fatal("draft did not start")
	}
	if code, body := postDraftAction(t, srv, "demo", "keep"); code != 200 {
		t.Fatalf("keep = %d %v", code, body)
	}

	// Kept means an ordinary override: expiring now must not undo it, and a
	// restart must not either.
	srv.expireDraft("demo.lua")
	srv.RevertDraftsOnStart()
	kept, err := os.ReadFile(filepath.Join(user, "demo.lua"))
	if err != nil || !strings.Contains(string(kept), "return 99") {
		t.Fatalf("kept draft = %q, %v", kept, err)
	}
}

// An operator copies their own override in as themselves; the gateway runs as
// its own user. On a real Raspberry Pi that meant the draft could not be
// written at all -- "permission denied" on a file the gateway does not own,
// even though it owns the directory. Rename needs the directory, not the file.
func TestDraftReplacesAnOverrideItDoesNotOwn(t *testing.T) {
	srv, _, user, _ := draftServer(t)
	override := filepath.Join(user, "demo.lua")
	own := "DRIVER = {\n  id = \"demo\",\n  version = \"1.0.0\",\n}\n" +
		"function driver_init(cfg) end\nfunction driver_poll() return 7 end\n"
	if err := os.WriteFile(override, []byte(own), 0o444); err != nil {
		t.Fatal(err)
	}

	if code, body := postDraft(t, srv, "demo", `{"lua":`+quote(draftLua)+`}`); code != 200 {
		t.Fatalf("draft over a read-only override = %d %v", code, body)
	}
	live, err := os.ReadFile(override)
	if err != nil || !strings.Contains(string(live), "return 99") {
		t.Fatalf("overlay = %q, %v", live, err)
	}

	// And it still comes back, so the operator does not lose their file.
	if code, _ := postDraftAction(t, srv, "demo", "revert"); code != 200 {
		t.Fatal("revert failed")
	}
	restored, err := os.ReadFile(override)
	if err != nil || !strings.Contains(string(restored), "return 7") {
		t.Fatalf("restored = %q, %v", restored, err)
	}
}

// Keep removes the record; a timer that fires in the same instant must not
// then delete the file the operator just chose to keep.
func TestAnExpiryRacingKeepDoesNotDeleteTheKeptFile(t *testing.T) {
	srv, _, user, _ := draftServer(t)
	if code, _ := postDraft(t, srv, "demo", `{"lua":`+quote(draftLua)+`,"minutes":1}`); code != 200 {
		t.Fatal("draft did not start")
	}
	if code, _ := postDraftAction(t, srv, "demo", "keep"); code != 200 {
		t.Fatal("keep failed")
	}

	// The timer was disarmed, but a firing already in flight lands here.
	srv.expireDraft("demo.lua")

	if _, err := os.Stat(filepath.Join(user, "demo.lua")); err != nil {
		t.Fatalf("the kept file was deleted by a late expiry: %v", err)
	}
}

// The timer dies with the process, so the record on disk is what makes the
// window mean anything across a restart.
func TestARestartUndoesADraftLeftRunning(t *testing.T) {
	srv, _, user, _ := draftServer(t)
	if code, _ := postDraft(t, srv, "demo", `{"lua":`+quote(draftLua)+`,"minutes":60}`); code != 200 {
		t.Fatal("draft did not start")
	}

	srv.RevertDraftsOnStart()

	if _, err := os.Stat(filepath.Join(user, "demo.lua")); !os.IsNotExist(err) {
		t.Fatal("a draft survived a restart; the window would never expire")
	}
}

func TestDraftRefusesSourceThatCannotRun(t *testing.T) {
	srv, bundled, user, cfg := draftServer(t)

	broken := "DRIVER = {\n  id = \"demo\",\n" // never closed
	code, body := postDraft(t, srv, "demo", `{"lua":`+quote(broken)+`}`)
	if code != 422 || !strings.Contains(toString(body["error"]), "does not compile") {
		t.Fatalf("broken draft = %d %v", code, body)
	}

	// A driver may not take another driver's slot by renaming itself.
	wrongID := strings.Replace(draftLua, `id = "demo"`, `id = "somethingelse"`, 1)
	code, body = postDraft(t, srv, "demo", `{"lua":`+quote(wrongID)+`}`)
	if code != 422 || !strings.Contains(toString(body["error"]), "declares id") {
		t.Fatalf("renamed draft = %d %v", code, body)
	}

	if _, err := os.Stat(filepath.Join(user, "demo.lua")); !os.IsNotExist(err) {
		t.Fatal("a refused draft reached the overlay")
	}
	if got := runningLua(cfg); got != filepath.Join(bundled, "demo.lua") {
		t.Fatalf("running lua = %q; a refused draft moved the driver", got)
	}
}

func TestDraftWindowIsBounded(t *testing.T) {
	srv, _, _, _ := draftServer(t)
	for _, minutes := range []int{-1, 61, 1440} {
		body := `{"lua":` + quote(draftLua) + `,"minutes":` + itoa(minutes) + `}`
		if code, _ := postDraft(t, srv, "demo", body); code == 200 && minutes != -1 {
			t.Fatalf("minutes=%d was accepted", minutes)
		}
	}
}

// A draft that stops the driver from starting is not something to leave
// running: the edit is undone before the error is reported.
func TestADraftThatWillNotStartIsUndoneImmediately(t *testing.T) {
	srv, _, user, _ := draftServer(t)
	// Compiles and keeps its id, so it passes validation -- and then throws on
	// the way up, which is exactly the edit worth protecting against.
	throws := "DRIVER = {\n  id = \"demo\",\n  version = \"1.0.0\",\n}\n" +
		"function driver_init(cfg) error(\"nope\") end\nfunction driver_poll() return 1 end\n"

	code, _ := postDraft(t, srv, "demo", `{"lua":`+quote(throws)+`}`)
	if code != 502 {
		t.Fatalf("failed restart = %d, want 502", code)
	}
	if _, err := os.Stat(filepath.Join(user, "demo.lua")); !os.IsNotExist(err) {
		t.Fatal("a draft that would not start was left in place")
	}
}

func TestDraftStatusReportsTheRemainingWindow(t *testing.T) {
	srv, _, _, _ := draftServer(t)
	before := time.Now().UnixMilli()
	if code, _ := postDraft(t, srv, "demo", `{"lua":`+quote(draftLua)+`,"minutes":5}`); code != 200 {
		t.Fatal("draft did not start")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/drivers/demo/draft", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)

	// So the countdown survives a reload of the page rather than living only
	// in the tab that started it.
	if out["running"] != true {
		t.Fatalf("status = %+v", out)
	}
	expires, _ := out["expires_at_ms"].(float64)
	if int64(expires) <= before {
		t.Fatalf("expires_at_ms = %v, want a time in the future", expires)
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

func quote(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

func itoa(n int) string {
	out, _ := json.Marshal(n)
	return string(out)
}
