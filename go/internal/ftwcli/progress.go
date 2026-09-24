package ftwcli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// sample is one reading of a phase's progress.
type sample struct {
	key, label  string // key names the phase; label is what a person reads
	done, total int64
	unit        string // "bytes", "rows" or "" when the phase counts nothing
	detail      string
	quiet       bool // a short setup phase leaves no line when it ends
}

// meter shows the progress of one run.
//
// On a terminal each phase gets two lines: its name, and below it a bar or
// spinner with amount, rate, time left and elapsed time, redrawn in place.
// When the phase ends both become one ✓ line with its duration and average
// rate. Elsewhere, for logs and wrapping scripts, it writes a plain line
// when the phase changes and every logEvery, and a ✓ line at the end.
type meter struct {
	out   io.Writer
	e     env
	start time.Time

	cur        sample
	phaseStart time.Time
	firstDone  int64
	rate       float64 // smoothed units per second
	sampleAt   time.Time
	sampleDone int64
	named      bool // the phase's name line is on screen
	live       bool // the progress line is on screen
	drawnAt    time.Time
	loggedAt   time.Time
	spin       int
}

// ANSI control: move to the start of the line and clear it; move up one.
const (
	clearLine = "\r\x1b[2K"
	lineUp    = "\x1b[1A"
)

func newMeter(out io.Writer, e env) *meter {
	return &meter{out: out, e: e, start: e.now()}
}

func (m *meter) show(s sample) {
	now := m.e.now()
	if s.key != m.cur.key {
		m.finish(true)
		m.phaseStart = now
		if m.e.tty {
			fmt.Fprintf(m.out, "  %s\n", m.fit(s.label, m.e.width-2))
			m.named = true
		}
	}
	if s.key != m.cur.key || s.unit != m.cur.unit {
		m.firstDone, m.sampleDone, m.sampleAt, m.rate = s.done, s.done, now, 0
		m.loggedAt, m.drawnAt = time.Time{}, time.Time{}
	}
	if dt := now.Sub(m.sampleAt).Seconds(); dt >= 0.5 && s.done >= m.sampleDone {
		instant := float64(s.done-m.sampleDone) / dt
		if m.rate == 0 {
			m.rate = instant
		} else {
			m.rate = 0.7*m.rate + 0.3*instant
		}
		m.sampleAt, m.sampleDone = now, s.done
	}
	m.cur = s
	m.draw(now)
}

// finish closes the current phase with a line that says how it went.
func (m *meter) finish(ok bool) {
	if m.cur.key == "" {
		return
	}
	s, took := m.cur, m.e.now().Sub(m.phaseStart)
	m.erase()
	m.cur = sample{}
	if ok && s.quiet {
		return
	}
	summary := "in " + formatTook(took)
	if !ok {
		summary = "after " + formatTook(took)
	} else if s.unit != "" && s.done > 0 {
		summary = amount(s.unit, s.done) + " " + summary
		if secs := took.Seconds(); secs >= 1 && s.done > m.firstDone {
			summary += " (" + perSecond(s.unit, float64(s.done-m.firstDone)/secs) + ")"
		}
	}
	fmt.Fprintf(m.out, "%s %s  %s\n", m.symbol(ok), s.label, summary)
}

// recorded prints a phase Core reports as finished, with Core's timing. It
// replaces the live phase when that is the same one and otherwise goes
// above it.
func (m *meter) recorded(label, summary string) {
	same := m.cur.key != "" && m.cur.label == label
	m.erase()
	fmt.Fprintf(m.out, "%s %s  %s\n", m.symbol(true), label, summary)
	if same {
		m.cur = sample{}
		return
	}
	if m.cur.key != "" && m.e.tty {
		fmt.Fprintf(m.out, "  %s\n", m.fit(m.cur.label, m.e.width-2))
		m.named, m.drawnAt = true, time.Time{}
	}
}

// discard drops the live phase without a line of its own, when Core's
// record of the run already covers it.
func (m *meter) discard() {
	m.erase()
	m.cur = sample{}
}

// erase removes the live phase's lines from a terminal.
func (m *meter) erase() {
	if !m.e.tty {
		return
	}
	if m.live {
		fmt.Fprint(m.out, clearLine)
	}
	if m.named {
		fmt.Fprint(m.out, lineUp+clearLine)
	}
	m.live, m.named = false, false
}

func (m *meter) draw(now time.Time) {
	if !m.e.tty {
		if m.loggedAt.IsZero() || now.Sub(m.loggedAt) >= m.e.logEvery {
			fmt.Fprintf(m.out, "[%s] %s  %s\n", formatElapsed(now.Sub(m.start)), m.cur.label, m.numbers(now))
			m.loggedAt = now
		}
		return
	}
	if !m.drawnAt.IsZero() && now.Sub(m.drawnAt) < 100*time.Millisecond {
		return
	}
	m.drawnAt = now
	m.spin++
	indicator := m.spinner()
	if m.cur.unit != "" && m.cur.total > 0 {
		// A fixed bar width per terminal keeps the numbers from jumping; the
		// widest numbers take about 60 columns.
		indicator = m.bar(m.cur, min(max(m.e.width-70, 8), 30))
	}
	fmt.Fprint(m.out, clearLine+m.fit("    "+indicator+"  "+m.numbers(now), m.e.width-1))
	m.live = true
}

// numbers is the phase's amount, rate, time left, detail and elapsed time.
func (m *meter) numbers(now time.Time) string {
	s := m.cur
	var parts []string
	switch {
	case s.unit != "" && s.total > 0:
		pct := min(float64(s.done)/float64(s.total), 1)
		parts = append(parts, fmt.Sprintf("%3.0f%%", pct*100), amount(s.unit, s.done)+" of "+amount(s.unit, s.total))
		if m.rate > 0 {
			parts = append(parts, perSecond(s.unit, m.rate))
			if now.Sub(m.phaseStart) >= 2*time.Second && s.done < s.total {
				parts = append(parts, "ETA "+formatElapsed(time.Duration(float64(s.total-s.done)/m.rate*float64(time.Second))))
			}
		}
	case s.unit != "" && s.done > 0:
		parts = append(parts, amount(s.unit, s.done)+", total unknown")
		if m.rate > 0 {
			parts = append(parts, perSecond(s.unit, m.rate))
		}
	}
	if s.detail != "" {
		parts = append(parts, s.detail)
	}
	return strings.Join(append(parts, formatElapsed(now.Sub(m.phaseStart))), "  ")
}

// fit cuts a line to the terminal width.
func (m *meter) fit(line string, width int) string {
	if width <= 8 || utf8.RuneCountInString(line) <= width {
		return line
	}
	return string([]rune(line)[:width-1]) + "…"
}

func (m *meter) bar(s sample, cells int) string {
	full, empty := "█", "░"
	if m.e.ascii {
		full, empty = "#", "-"
	}
	filled := min(int(float64(cells)*float64(s.done)/float64(s.total)), cells)
	return "[" + strings.Repeat(full, filled) + strings.Repeat(empty, cells-filled) + "]"
}

func (m *meter) spinner() string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	if m.e.ascii {
		frames = []string{"|", "/", "-", `\`}
	}
	return frames[m.spin%len(frames)]
}

func (m *meter) symbol(ok bool) string {
	switch {
	case ok && m.e.ascii:
		return "done"
	case ok:
		return "✓"
	case m.e.ascii:
		return "FAILED"
	}
	return "✗"
}

func amount(unit string, n int64) string {
	if unit == "rows" {
		return fmt.Sprintf("%d rows", n)
	}
	return formatBytes(n)
}

func perSecond(unit string, rate float64) string {
	if unit == "rows" {
		return fmt.Sprintf("%.0f rows/s", rate)
	}
	return formatBytes(int64(rate)) + "/s"
}

// utf8Locale reports whether the terminal expects UTF-8, so the bar and
// marks can use block characters.
func utf8Locale() bool {
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := strings.ToLower(os.Getenv(name)); v != "" {
			return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
		}
	}
	return false
}

// terminal reports whether w is an interactive terminal and how wide it is.
func terminal(w io.Writer) (bool, int) {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("TERM") == "dumb" {
		return false, 0
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false, 0
	}
	return true, terminalWidth(f)
}
