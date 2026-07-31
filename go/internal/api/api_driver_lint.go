package api

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// The editor also lints as you type, using Ace's own luaparse. That is a
// different implementation from the one the driver runs under, and it can
// disagree. This endpoint asks gopher-lua — the parser that actually decides
// whether the driver will start — so a green tick never lies about that.
func (s *Server) handleDriverLint(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Lua string `json:"lua"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if len(body.Lua) > maxDriverSourceBytes {
		writeJSON(w, 400, map[string]string{"error": errDriverSourceTooLarge.Error()})
		return
	}

	problems := []map[string]any{}

	L := lua.NewState()
	defer L.Close()
	if _, err := L.LoadString(body.Lua); err != nil {
		line, message := splitLuaError(err.Error())
		problems = append(problems, map[string]any{
			"line": line, "severity": "error", "message": message,
		})
		// Nothing further is meaningful: the parse stopped here.
		writeJSON(w, 200, map[string]any{"ok": false, "problems": problems})
		return
	}

	// It compiles. Whether it is a usable driver is a separate question, and
	// the answers are warnings rather than errors: a draft mid-edit may not
	// have its entrypoints back yet.
	if entry := r.PathValue("id"); entry != "" {
		problems = append(problems, driverShapeProblems(body.Lua, entry)...)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "problems": problems})
}

// gopher-lua writes:
//
//	<string> line:5(column:3) near 'end':   syntax error
//
// The chunk name and the column are noise next to a marker in the gutter; the
// line places it and the token is what makes the message actionable.
var gopherLuaError = regexp.MustCompile(
	`line:(\d+)\(column:(-?\d+)\)\s*(?:near '([^']*)':)?\s*(.*)$`)

// Older builds and other callers use `chunk:line: message`, so that form is
// still understood rather than falling through to the raw string.
var plainLuaError = regexp.MustCompile(`^(?:[^:]*:)?(\d+):\s*(.+)$`)

func splitLuaError(raw string) (int, string) {
	raw = strings.TrimSpace(raw)
	if m := gopherLuaError.FindStringSubmatch(raw); len(m) == 5 {
		line, err := strconv.Atoi(m[1])
		if err == nil {
			message := strings.TrimSpace(m[4])
			if message == "" {
				message = "syntax error"
			}
			if token := m[3]; token != "" {
				message += " near '" + token + "'"
			}
			return line, message
		}
	}
	if m := plainLuaError.FindStringSubmatch(raw); len(m) == 3 {
		if line, err := strconv.Atoi(m[1]); err == nil {
			return line, strings.TrimSpace(m[2])
		}
	}
	return 0, raw
}

// driverShapeProblems reports what would stop this from working as a driver
// even though it parses: a missing entrypoint, or an identity that no longer
// matches the slot it is being edited in.
func driverShapeProblems(source, driverID string) []map[string]any {
	var out []map[string]any

	for _, required := range []string{"driver_init", "driver_poll"} {
		if !declaresFunction(source, required) {
			out = append(out, map[string]any{
				"line": 0, "severity": "warning",
				"message": "no " + required + "() — the driver will not start without it",
			})
		}
	}

	if id := declaredDriverID(source); id != "" && id != driverID {
		out = append(out, map[string]any{
			"line": 0, "severity": "warning",
			"message": "this declares id \"" + id + "\", but it is being edited as \"" +
				driverID + "\"; running it will be refused",
		})
	}
	return out
}

func declaresFunction(source, name string) bool {
	re := regexp.MustCompile(`(?m)^\s*(?:local\s+)?function\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	return re.MatchString(source)
}

var driverIDField = regexp.MustCompile(`(?m)^\s*id\s*=\s*"([^"]*)"`)

// declaredDriverID reads the id out of the DRIVER table without running the
// file. The catalog parser wants a path on disk, and a draft has none yet.
func declaredDriverID(source string) string {
	block := source
	if start := strings.Index(source, "DRIVER"); start >= 0 {
		block = source[start:]
	}
	if m := driverIDField.FindStringSubmatch(block); len(m) == 2 {
		return m[1]
	}
	return ""
}
