package state

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression for the intern-lock shape of the 2026-07-16 prune incident: the
// intern cache used to hold its exclusive lock across the allocating INSERT,
// so a slow disk write inside the five-second control tick parked every API
// reader behind it.
//
// The test makes the disk write slow the way a loaded Raspberry Pi does — by
// holding SQLite's write lock on another connection — and then requires the
// read-only intern surfaces to answer while the allocation is stuck.
func TestInternAllocationDoesNotBlockReaders(t *testing.T) {
	s := freshStore(t)

	// One recorded sample hydrates the cache and gives the readers something
	// to return, so the measurement is of lock waiting and nothing else.
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "grid_w", TsMs: 1, Value: 42, Unit: "W"}}); err != nil {
		t.Fatal(err)
	}

	// Take SQLite's write lock and keep it. Any INSERT on another connection
	// now waits on busy_timeout instead of returning.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO ts_drivers (name) VALUES ('lock-holder')`); err != nil {
		t.Fatal(err)
	}

	const held = 750 * time.Millisecond
	release := time.AfterFunc(held, func() { tx.Rollback() })
	defer release.Stop()

	allocDone := make(chan error, 2)
	go func() {
		_, err := s.metricID("battery_soc", "%")
		allocDone <- err
	}()
	go func() {
		_, err := s.driverID("battery")
		allocDone <- err
	}()

	// Let both allocators reach the blocked INSERT.
	time.Sleep(100 * time.Millisecond)

	// Every read-only surface that shares the intern lock.
	reads := []struct {
		name string
		fn   func() error
	}{
		{"MetricsCatalog", func() error { _, err := s.MetricsCatalog(); return err }},
		{"MetricNames", func() error { _, err := s.MetricNames(); return err }},
		{"DriverNames", func() error { _, err := s.DriverNames(); return err }},
		{"LoadSeries", func() error { _, err := s.LoadSeries("meter", "grid_w", 0, 1<<62, 0); return err }},
	}
	// A generous bound: the reads are in-memory map walks plus (for
	// LoadSeries) one indexed query on a three-row table. Anything near the
	// lock-hold window means the reader queued behind the stuck INSERT.
	const bound = held / 3
	for _, r := range reads {
		t0 := time.Now()
		if err := r.fn(); err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		if waited := time.Since(t0); waited > bound {
			t.Fatalf("%s took %s while an intern allocation was stuck on disk (bound %s) — reader queued behind the intern write lock", r.name, waited, bound)
		}
	}

	for i := 0; i < 2; i++ {
		if err := <-allocDone; err != nil {
			t.Fatalf("intern allocation: %v", err)
		}
	}
}

// Two callers interning the same new name concurrently must agree on one id
// and leave exactly one row behind. Interning no longer runs under a lock that
// makes this impossible by construction, so it is asserted instead.
func TestInternConcurrentSameNameYieldsOneID(t *testing.T) {
	s := freshStore(t)
	if err := s.hydrateIntern(); err != nil {
		t.Fatal(err)
	}

	const racers = 24
	for _, tc := range []struct {
		name  string
		table string
		alloc func() (int64, error)
	}{
		{"driver", "ts_drivers", func() (int64, error) { return s.driverID("inverter") }},
		{"metric", "ts_metrics", func() (int64, error) { return s.metricID("pv_w", "W") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids := make([]int64, racers)
			errs := make([]error, racers)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for i := 0; i < racers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					ids[i], errs[i] = tc.alloc()
				}(i)
			}
			close(start)
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Fatalf("racer %d: %v", i, err)
				}
			}
			for i, id := range ids {
				if id != ids[0] {
					t.Fatalf("racer %d got id %d, racer 0 got %d — concurrent interning split one name across ids", i, id, ids[0])
				}
			}
			var rows int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + tc.table).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 1 {
				t.Fatalf("%s has %d rows, want 1", tc.table, rows)
			}
		})
	}
}

// The post-2026-07-16 rule: database work is volume-tested with simultaneous
// writers. Writers stream samples across a realistic metric catalog — a
// hundred-odd names, most of them new — while readers poll the same intern
// cache. Both must finish clean, readers must keep making progress, and every
// name must have exactly one row.
func TestInternUnderConcurrentReadersAndWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("volume test")
	}
	s := freshStore(t)
	if err := s.hydrateIntern(); err != nil {
		t.Fatal(err)
	}

	const (
		writers      = 4
		readers      = 4
		ticks        = 60
		driversPer   = 6
		metricsPerTk = 8
	)

	stop := make(chan struct{})
	var readOK, readErrs atomic.Int64
	var maxReadNs atomic.Int64
	var readerWG sync.WaitGroup
	for r := 0; r < readers; r++ {
		readerWG.Add(1)
		go func(r int) {
			defer readerWG.Done()
			probes := []func() error{
				func() error { _, err := s.MetricsCatalog(); return err },
				func() error { _, err := s.MetricNames(); return err },
				func() error { _, err := s.DriverNames(); return err },
			}
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				t0 := time.Now()
				err := probes[i%len(probes)]()
				waited := time.Since(t0).Nanoseconds()
				for {
					prev := maxReadNs.Load()
					if waited <= prev || maxReadNs.CompareAndSwap(prev, waited) {
						break
					}
				}
				if err != nil {
					readErrs.Add(1)
				} else {
					readOK.Add(1)
				}
				i++
				time.Sleep(time.Millisecond)
			}
		}(r)
	}

	var writeErrs atomic.Int64
	var writerWG sync.WaitGroup
	t0 := time.Now()
	for w := 0; w < writers; w++ {
		writerWG.Add(1)
		go func(w int) {
			defer writerWG.Done()
			for tick := 0; tick < ticks; tick++ {
				batch := make([]Sample, 0, driversPer*metricsPerTk)
				ts := time.Now().UnixMilli() + int64(tick)
				for d := 0; d < driversPer; d++ {
					for m := 0; m < metricsPerTk; m++ {
						// Names overlap across writers on purpose: the
						// interesting case is several goroutines allocating
						// the same new name in the same tick.
						batch = append(batch, Sample{
							Driver: fmt.Sprintf("driver_%d", (w+d)%driversPer),
							Metric: fmt.Sprintf("metric_%d_%d", tick%ticks, m),
							TsMs:   ts,
							Value:  float64(m),
							Unit:   "W",
						})
					}
				}
				if err := s.RecordSamples(batch); err != nil {
					writeErrs.Add(1)
				}
			}
		}(w)
	}
	writerWG.Wait()
	elapsed := time.Since(t0)
	close(stop)
	readerWG.Wait()

	maxRead := time.Duration(maxReadNs.Load())
	t.Logf("writers finished in %s; reads ok=%d failed=%d slowest=%s", elapsed, readOK.Load(), readErrs.Load(), maxRead)

	if writeErrs.Load() > 0 {
		t.Fatalf("%d sample batches failed", writeErrs.Load())
	}
	if readErrs.Load() > 0 {
		t.Fatalf("%d intern reads failed", readErrs.Load())
	}
	if readOK.Load() == 0 {
		t.Fatal("readers never ran — test setup broken")
	}
	// No reader may be parked for a human-noticeable time by interning.
	if maxRead > time.Second {
		t.Fatalf("slowest intern read took %s — readers are starving behind allocation", maxRead)
	}

	// Every name interned exactly once, whichever writer got there first.
	for _, q := range []string{
		`SELECT COUNT(*) FROM (SELECT name FROM ts_drivers GROUP BY name HAVING COUNT(*) > 1)`,
		`SELECT COUNT(*) FROM (SELECT name FROM ts_metrics GROUP BY name HAVING COUNT(*) > 1)`,
	} {
		var dupes int
		if err := s.db.QueryRow(q).Scan(&dupes); err != nil {
			t.Fatal(err)
		}
		if dupes != 0 {
			t.Fatalf("%d duplicated intern names (%s)", dupes, q)
		}
	}
	var drivers, metrics int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ts_drivers`).Scan(&drivers); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ts_metrics`).Scan(&metrics); err != nil {
		t.Fatal(err)
	}
	if drivers != driversPer {
		t.Fatalf("ts_drivers has %d rows, want %d", drivers, driversPer)
	}
	if metrics != ticks*metricsPerTk {
		t.Fatalf("ts_metrics has %d rows, want %d", metrics, ticks*metricsPerTk)
	}
}
