package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func lint(t *testing.T, srv *Server, id, source string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"lua": source})
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/"+id+"/lint", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return out
}

func problemsOf(out map[string]any) []map[string]any {
	raw, _ := out["problems"].([]any)
	var problems []map[string]any
	for _, p := range raw {
		if m, ok := p.(map[string]any); ok {
			problems = append(problems, m)
		}
	}
	return problems
}

func lintServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	writeSourceDriver(t, dir, "demo.lua", "demo", "1.0.0", "function driver_poll() return 1 end\n")
	return New(&Deps{DriverDir: dir})
}

// The point of asking the server is that this is the parser which decides
// whether the driver starts. Ace's own linter is a different implementation
// and can disagree.
func TestLintReportsTheLineASyntaxErrorIsOn(t *testing.T) {
	srv := lintServer(t)
	source := "local a = 1\nlocal b = 2\nif a == b then\n  print(\"x\"\nend\n"

	out := lint(t, srv, "demo", source)
	if out["ok"] != false {
		t.Fatalf("broken source linted ok: %+v", out)
	}
	problems := problemsOf(out)
	if len(problems) == 0 {
		t.Fatal("no problem reported for source that does not compile")
	}
	// Without a line the operator has to hunt through 35 kB by eye.
	if line, _ := problems[0]["line"].(float64); line < 4 {
		t.Fatalf("line = %v, want the unclosed call on line 4 or after", line)
	}
	if msg, _ := problems[0]["message"].(string); msg == "" || strings.Contains(msg, ".lua:") {
		t.Fatalf("message = %q; the chunk name is noise on screen", msg)
	}
}

func TestLintAcceptsADriverThatCompiles(t *testing.T) {
	srv := lintServer(t)
	source := "DRIVER = { id = \"demo\" }\nfunction driver_init(cfg) end\nfunction driver_poll() return 1 end\n"

	out := lint(t, srv, "demo", source)
	if out["ok"] != true {
		t.Fatalf("valid driver rejected: %+v", out)
	}
	if len(problemsOf(out)) != 0 {
		t.Fatalf("clean driver reported problems: %+v", problemsOf(out))
	}
}

// These are warnings, not errors: a draft mid-edit may not have its
// entrypoints back yet, and refusing to lint at all would be unhelpful.
func TestLintWarnsAboutWhatWouldStopTheDriverStarting(t *testing.T) {
	srv := lintServer(t)
	out := lint(t, srv, "demo", "DRIVER = { id = \"demo\" }\nlocal x = 1\n")

	if out["ok"] != true {
		t.Fatal("source that compiles should lint ok even when it is not yet a driver")
	}
	var messages []string
	for _, p := range problemsOf(out) {
		if p["severity"] != "warning" {
			t.Fatalf("severity = %v, want warning", p["severity"])
		}
		messages = append(messages, p["message"].(string))
	}
	joined := strings.Join(messages, " | ")
	if !strings.Contains(joined, "driver_init") || !strings.Contains(joined, "driver_poll") {
		t.Fatalf("warnings = %q, want both missing entrypoints named", joined)
	}
}

// Running it would be refused for this reason, so saying it while editing
// saves a round trip through a failed draft.
func TestLintWarnsWhenTheDriverRenamesItself(t *testing.T) {
	srv := lintServer(t)
	source := "DRIVER = {\n  id = \"somethingelse\",\n}\n" +
		"function driver_init(cfg) end\nfunction driver_poll() return 1 end\n"

	out := lint(t, srv, "demo", source)
	var found bool
	for _, p := range problemsOf(out) {
		if strings.Contains(p["message"].(string), "somethingelse") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no warning about the renamed id: %+v", problemsOf(out))
	}
}

func TestLintRefusesSomethingTooLargeToBeADriver(t *testing.T) {
	srv := lintServer(t)
	body, _ := json.Marshal(map[string]string{"lua": strings.Repeat("x", maxDriverSourceBytes+1)})
	req := httptest.NewRequest(http.MethodPost, "/api/drivers/demo/lint", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != 400 {
		t.Fatalf("oversized lint = %d, want 400", rr.Code)
	}
}

func TestSplitLuaErrorKeepsTheLineAndDropsTheChunkName(t *testing.T) {
	// What gopher-lua actually writes. The chunk name and column are noise
	// next to a gutter marker; the token is what makes it actionable.
	line, message := splitLuaError("<string> line:5(column:3) near 'end':   syntax error")
	if line != 5 {
		t.Fatalf("line = %d, want 5", line)
	}
	if message != "syntax error near 'end'" {
		t.Fatalf("message = %q", message)
	}

	// End-of-file errors carry column -1 and no token.
	if line, message := splitLuaError("<string> line:9(column:-1) near '<eof>':   syntax error"); line != 9 ||
		!strings.Contains(message, "eof") {
		t.Fatalf("eof error = %d %q", line, message)
	}

	// The plain form is still understood.
	if line, message := splitLuaError("driver.lua:12: unexpected symbol"); line != 12 ||
		message != "unexpected symbol" {
		t.Fatalf("plain form = %d %q", line, message)
	}

	// Anything it cannot place still reaches the operator intact rather than
	// being swallowed.
	if line, message := splitLuaError("something unplaceable"); line != 0 || message != "something unplaceable" {
		t.Fatalf("unparseable = %d %q", line, message)
	}
}
