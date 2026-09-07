package state

import "testing"

func TestLiveHistoryFeedOnlyOffersCommittedRows(t *testing.T) {
	s := freshStore(t)
	feed := s.ObserveLiveHistory()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_tick BEFORE INSERT ON history_hot BEGIN SELECT RAISE(ABORT, 'disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTick(HistoryPoint{TsMs: 200, GridW: 42}, nil); err == nil {
		t.Fatal("write should fail")
	}
	if got := feed.Stats(); got.Offered != 0 {
		t.Fatalf("uncommitted row offered: %+v", got)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_tick`); err != nil {
		t.Fatal(err)
	}
	// Late and same-time writes have their own source sequence, independent of
	// wall-clock time. SQL retention and imports remain outside the live feed.
	for _, ts := range []int64{200, 100, 100} {
		if err := s.RecordTick(HistoryPoint{TsMs: ts, GridW: float64(ts), JSON: "private detail"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i, ts := range []int64{200, 100, 100} {
		tick := <-feed.Events()
		if tick.Sequence != uint64(i+1) || tick.Point.TsMs != ts || tick.Point.JSON != "" || tick.CommittedAtMicros == 0 {
			t.Fatalf("wrong committed point: %+v", tick)
		}
	}
	if err := s.RecordTickWithOptionalHistory(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := feed.Stats(); got.Offered != 3 {
		t.Fatal("missing history became a zero observation")
	}
}

func TestLiveHistoryFeedHasBoundedMemoryAndNoBackpressure(t *testing.T) {
	s := freshStore(t)
	feed := s.ObserveLiveHistory()
	for i := 0; i < 400; i++ {
		if err := s.RecordTick(HistoryPoint{TsMs: int64(i + 1)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	got := feed.Stats()
	if got.Queued != 256 || got.Dropped != 144 || got.Offered != 400 {
		t.Fatalf("unbounded or unreported gap: %+v", got)
	}
	rows, err := s.LoadHistory(0, 500, 0)
	if err != nil || len(rows) != 400 {
		t.Fatalf("shadow overload lost SQLite data: %d %v", len(rows), err)
	}
}

func TestLiveHistoryFeedStopKeepsQueueAndDoesNotStopSQLite(t *testing.T) {
	s := freshStore(t)
	feed := s.ObserveLiveHistory()
	if err := s.RecordTick(HistoryPoint{TsMs: 1, GridW: 42}, nil); err != nil {
		t.Fatal(err)
	}
	feed.Stop()
	feed.Stop()
	for i := 2; i <= 400; i++ {
		if err := s.RecordTick(HistoryPoint{TsMs: int64(i)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := feed.Stats(); got.Offered != 1 || got.Queued != 1 || got.Dropped != 0 {
		t.Fatalf("stopped session changed: %+v", got)
	}
	tick := <-feed.Events()
	if tick.Sequence != 1 || tick.Point.GridW != 42 {
		t.Fatalf("stop discarded the queue: %+v", tick)
	}
	rows, err := s.LoadHistory(0, 500, 0)
	if err != nil || len(rows) != 400 {
		t.Fatalf("stopping feed changed SQLite: %d %v", len(rows), err)
	}
}
