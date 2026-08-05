package drivers

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// A driver that can be commanded says so here, in terms an operator UI can
// render without knowing the driver: what the command is called, what one
// input looks like, and what counts as proof the device took it.
//
// Core's own dispatch verbs (battery, curtail) are not declared. They are
// wired into the control tick and are not the operator's to press. This is
// for the commands that have no other way to reach a person — a heat curve
// offset, a mode selection — which today reach nobody at all.
//
// The signed package manifest carries the same shape in RuntimeCommand, but
// only for drivers that ship through the signed channel. A bundled or local
// driver has no policy, so this is where its declaration lives.
type CatalogControl struct {
	ID       string              `json:"id"`
	Label    string              `json:"label,omitempty"`
	Evidence string              `json:"evidence,omitempty"`
	Input    CatalogControlInput `json:"input"`
}

// CatalogControlInput describes the single value a control carries. Min, max
// and step are pointers because zero is a real bound: a 0..100 percentage and
// an undeclared range must not read the same.
type CatalogControlInput struct {
	Type string   `json:"type"`
	Min  *float64 `json:"min,omitempty"`
	Max  *float64 `json:"max,omitempty"`
	Step *float64 `json:"step,omitempty"`
	Unit string   `json:"unit,omitempty"`
}

var controlEvidence = map[string]bool{"readback": true, "write_ack": true}

// ControlsForDriver returns the controls declared by the catalog entry for
// luaPath, matching the way IsEVOrVehicleDriver and IsReadOnlyDriver already
// do: portable path first, then filename. Operators write the lua path both
// ways — `drivers/heishamon.lua` and `/data/heishamon.lua` are both ordinary
// — and a control that appears for one spelling and not the other would look
// like the driver, not the lookup.
func ControlsForDriver(catalog []CatalogEntry, luaPath string) []CatalogControl {
	if luaPath == "" {
		return nil
	}
	wantPath := filepath.ToSlash(filepath.Clean(luaPath))
	wantFilename := filepath.Base(wantPath)
	// A filename is only a fallback. When two sources contain the same
	// filename, the configured path is the only safe identity we have.
	for _, e := range catalog {
		if strings.EqualFold(filepath.ToSlash(filepath.Clean(e.Path)), wantPath) {
			return e.Controls
		}
	}
	for _, e := range catalog {
		if strings.EqualFold(e.Filename, wantFilename) {
			return e.Controls
		}
	}
	return nil
}

// pickControls reads the DRIVER block's `controls` list.
//
// The existing pick helpers stop at the first closing brace, which is why
// this does its own brace matching rather than extending them. Nothing else
// in the block nests, and making pickKVBlock nest would change how every
// field is read for the sake of one that did not exist yet.
//
// A declaration the UI cannot render is dropped rather than surfaced half
// formed: no id, an input type outside the vocabulary, or a number without
// both bounds. A slider with one end is not a control, it is a guess.
func pickControls(block string) []CatalogControl {
	body := nestedBlock(stripLuaComments(block), "controls")
	if body == "" {
		return nil
	}
	var out []CatalogControl
	for _, entry := range topLevelTables(body) {
		control := CatalogControl{
			ID:       fieldString(entry, "id"),
			Label:    fieldString(entry, "label"),
			Evidence: fieldString(entry, "evidence"),
		}
		if control.ID == "" || !validControlToken(control.ID) {
			continue
		}
		if control.Evidence != "" && !controlEvidence[control.Evidence] {
			continue
		}
		input := nestedBlock(entry, "input")
		if input == "" {
			continue
		}
		control.Input = CatalogControlInput{
			Type: fieldString(input, "type"),
			Unit: fieldString(input, "unit"),
			Min:  fieldNumber(input, "min"),
			Max:  fieldNumber(input, "max"),
			Step: fieldNumber(input, "step"),
		}
		switch control.Input.Type {
		case "number":
			if control.Input.Min == nil || control.Input.Max == nil ||
				*control.Input.Min >= *control.Input.Max {
				continue
			}
		case "boolean", "string":
		default:
			continue
		}
		out = append(out, control)
	}
	return out
}

// nestedBlock returns the body of `name = { … }`, matching braces so an inner
// table does not end the outer one. Braces inside string literals are skipped;
// a topic or a label is allowed to contain one.
func nestedBlock(block, name string) string {
	loc := regexp.MustCompile(regexp.QuoteMeta(name) + `\s*=\s*\{`).FindStringIndex(block)
	if loc == nil {
		return ""
	}
	depth := 0
	inString := false
	for i := loc[1] - 1; i < len(block); i++ {
		switch block[i] {
		case '"':
			// Lua has no escaped quote in the metadata this reads, and a
			// backslash before one would be a driver bug, not a string.
			inString = !inString
		case '{':
			if !inString {
				depth++
			}
		case '}':
			if inString {
				continue
			}
			depth--
			if depth == 0 {
				return block[loc[1]:i]
			}
		}
	}
	return ""
}

// stripLuaComments blanks comments without changing quoted strings or line
// breaks. The catalog parser is deliberately not a Lua interpreter, so it
// must remove a commented declaration before the lightweight table matcher
// sees it. Both line comments and long-bracket comments are valid Lua.
func stripLuaComments(src string) string {
	out := []byte(src)
	for i := 0; i < len(src); {
		switch src[i] {
		case '"':
			i++
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
					continue
				}
				if src[i] == '"' {
					i++
					break
				}
				i++
			}
			continue
		case '-':
			if i+1 >= len(src) || src[i+1] != '-' {
				i++
				continue
			}
			if end, level, ok := luaLongBracketOpen(src, i+2); ok {
				j := end
				for j < len(src) {
					if close, ok := luaLongBracketClose(src, j, level); ok {
						j = close
						break
					}
					j++
				}
				blankLuaComment(out, i, j)
				i = j
				continue
			}
			j := i
			for j < len(src) && src[j] != '\n' {
				j++
			}
			blankLuaComment(out, i, j)
			i = j
			continue
		default:
			i++
		}
	}
	return string(out)
}

func blankLuaComment(out []byte, start, end int) {
	if end > len(out) {
		end = len(out)
	}
	for i := start; i < end; i++ {
		if out[i] != '\n' && out[i] != '\r' {
			out[i] = ' '
		}
	}
}

func luaLongBracketOpen(src string, start int) (end, level int, ok bool) {
	if start >= len(src) || src[start] != '[' {
		return 0, 0, false
	}
	i := start + 1
	for i < len(src) && src[i] == '=' {
		i++
		level++
	}
	if i >= len(src) || src[i] != '[' {
		return 0, 0, false
	}
	return i + 1, level, true
}

func luaLongBracketClose(src string, start, level int) (end int, ok bool) {
	if start >= len(src) || src[start] != ']' {
		return 0, false
	}
	i := start + 1
	for n := 0; n < level && i < len(src) && src[i] == '='; n++ {
		i++
	}
	if i >= len(src) || src[i] != ']' {
		return 0, false
	}
	return i + 1, true
}

// topLevelTables splits a list body into its `{ … }` entries, ignoring braces
// nested inside each entry.
func topLevelTables(body string) []string {
	var out []string
	depth, start := 0, -1
	inString := false
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '"':
			inString = !inString
		case '{':
			if inString {
				continue
			}
			if depth == 0 {
				start = i + 1
			}
			depth++
		case '}':
			if inString {
				continue
			}
			depth--
			if depth == 0 && start >= 0 {
				out = append(out, body[start:i])
				start = -1
			}
		}
	}
	return out
}

// The pick helpers in catalog.go anchor to the start of a line, which is
// right for the DRIVER block's own fields and wrong here: a control is
// commonly written inline, `{ id = "x", input = { type = "number" } }`, where
// only the first field of each table begins a line. These match a field
// wherever it sits, while still requiring a full name so `min` does not hit
// inside `minimum`.
func fieldString(block, name string) string {
	re := regexp.MustCompile(`(?:^|[\s,{])` + regexp.QuoteMeta(name) + `\s*=\s*"([^"]*)"`)
	m := re.FindStringSubmatch(block)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func fieldNumber(block, name string) *float64 {
	// The trailing delimiter is part of the match so a value such as 1e6foo
	// cannot be accepted as the prefix 1. Decimal exponent notation is valid
	// Lua metadata; malformed values are rejected rather than shortened.
	luaNumber := `[-+]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][-+]?[0-9]+)?`
	re := regexp.MustCompile(`(?:^|[\s,{])` + regexp.QuoteMeta(name) + `\s*=(\s*)` + `(` + luaNumber + `)([^A-Za-z0-9_.]|$)`)
	m := re.FindStringSubmatch(block)
	if len(m) < 3 {
		return nil
	}
	value, err := strconv.ParseFloat(m[2], 64)
	if err != nil {
		return nil
	}
	return &value
}
