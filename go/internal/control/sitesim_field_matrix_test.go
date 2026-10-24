//go:build !race

// The race detector makes the matrix 15 times slower (826 s of CPU) and finds
// nothing in it: each case runs on one goroutine. TestSiteSimScenarios still
// runs under -race.

package control

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// fieldKnownFile lists the rows that fail on master: case, kind and the fix
// that turns it green. A listed kind that passes fails the test until its
// line goes, and so does a failure the file does not list.
// FTW_FIELD_KNOWN=write rewrites it from a run and keeps each line's label.
const fieldKnownFile = "testdata/sitesim_field_known.txt"

type fieldKnown struct {
	header []string
	fixes  map[string]map[string]string // case, kind: fix
}

func readFieldKnown(t *testing.T) fieldKnown {
	t.Helper()
	data, err := os.ReadFile(fieldKnownFile)
	if err != nil {
		t.Fatal(err)
	}
	k := fieldKnown{fixes: map[string]map[string]string{}}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			k.header = append(k.header, line)
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			t.Fatalf("%s:%d: want case, kind and fix: %q", fieldKnownFile, i+1, line)
		}
		if k.fixes[f[0]] == nil {
			k.fixes[f[0]] = map[string]string{}
		}
		k.fixes[f[0]][f[1]] = f[2]
	}
	return k
}

func (k fieldKnown) write(t *testing.T, results []fieldResult) {
	t.Helper()
	lines := slices.Clone(k.header)
	for _, r := range results {
		for _, kind := range r.failed {
			fix := k.fixes[r.name][kind]
			if fix == "" {
				fix = "new"
			}
			lines = append(lines, r.name+" "+kind+" "+fix)
		}
	}
	if err := os.WriteFile(fieldKnownFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fieldResult is one case's broken invariants.
type fieldResult struct {
	name   string
	failed []string
}

func TestSiteSimFieldMatrix(t *testing.T) {
	cases, known := fieldCases(), readFieldKnown(t)
	writeKnown := os.Getenv("FTW_FIELD_KNOWN") == "write"
	names := map[string]bool{}
	for _, c := range cases {
		names[c.name()] = true
	}
	for name := range known.fixes {
		if !names[name] {
			t.Errorf("%s names %s, which the matrix no longer has", fieldKnownFile, name)
		}
	}
	var mu sync.Mutex
	var results []fieldResult
	t.Cleanup(func() {
		sort.Slice(results, func(i, j int) bool { return results[i].name < results[j].name })
		byKind, failing := map[string]int{}, 0
		for _, r := range results {
			if len(r.failed) > 0 {
				failing++
			}
			for _, k := range r.failed {
				byKind[k]++
			}
		}
		t.Logf("field matrix: %d cases, %d fail an invariant; by kind %v", len(results), failing, byKind)
		if writeKnown && len(results) == len(cases) {
			known.write(t, results)
		}
	})
	for _, c := range cases {
		t.Run(c.name(), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var trace func(string, ...any)
				if os.Getenv("FTW_FIELD_TRACE") == c.name() {
					trace = t.Logf
				}
				s, f := runFieldCase(c, trace)
				failed, fixes := slices.Sorted(maps.Keys(f.failures)), known.fixes[c.name()]
				var problems []string
				for _, k := range failed {
					if fix, ok := fixes[k]; ok {
						t.Logf("fails as known (%s): %s", fix, f.failures[k])
					} else {
						problems = append(problems, f.failures[k])
					}
				}
				for _, k := range slices.Sorted(maps.Keys(fixes)) {
					if _, ok := f.failures[k]; !ok {
						problems = append(problems, fmt.Sprintf("%s passes now: delete its known line (%s)", k, fixes[k]))
					}
				}
				mu.Lock()
				results = append(results, fieldResult{name: c.name(), failed: failed})
				mu.Unlock()
				t.Logf("peak phase %.1f A, delivered %.1f kWh, car idle with room %v", s.peakA, f.deliveredWh/1000, s.idleWithRoom)
				switch {
				case len(problems) > 0 && writeKnown:
					t.Log(strings.Join(problems, "\n"))
				case len(problems) > 0:
					t.Fatal(strings.Join(problems, "\n"))
				}
			})
		})
	}
}
