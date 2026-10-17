package state

import (
	"fmt"
	"strings"
	"time"
)

// Keep bounded timings without logging successful writes or sample values.
// Capture a failure before rollback, which may take time of its own.
type historyWriteTiming struct {
	started time.Time
	step    time.Time
	phase   string
	phases  [12]struct {
		name    string
		elapsed time.Duration
		max     time.Duration
		steps   int
	}
	n int
}

func newHistoryWriteTiming(phase string) historyWriteTiming {
	now := time.Now()
	return historyWriteTiming{started: now, step: now, phase: phase}
}

func (t *historyWriteTiming) next(phase string) {
	now := time.Now()
	t.add(t.phase, now.Sub(t.step))
	t.phase, t.step = phase, now
}

func (t *historyWriteTiming) add(phase string, elapsed time.Duration) {
	for i := 0; i < t.n; i++ {
		if t.phases[i].name == phase {
			t.phases[i].elapsed += elapsed
			t.phases[i].max = max(t.phases[i].max, elapsed)
			t.phases[i].steps++
			return
		}
	}
	if t.n < len(t.phases) {
		t.phases[t.n].name, t.phases[t.n].elapsed = phase, elapsed
		t.phases[t.n].max, t.phases[t.n].steps = elapsed, 1
		t.n++
	}
}

func (t *historyWriteTiming) failure(err error) error {
	if err == nil {
		return nil
	}
	now := time.Now()
	t.add(t.phase, now.Sub(t.step))
	var details strings.Builder
	for i := 0; i < t.n; i++ {
		p := &t.phases[i]
		fmt.Fprintf(&details, " %s_ms=%d %s_max_ms=%d %s_steps=%d", p.name, p.elapsed.Milliseconds(), p.name, p.max.Milliseconds(), p.name, p.steps)
	}
	return fmt.Errorf("%w [elapsed_ms=%d phase=%s step_ms=%d%s]", err, now.Sub(t.started).Milliseconds(), t.phase, now.Sub(t.step).Milliseconds(), details.String())
}
