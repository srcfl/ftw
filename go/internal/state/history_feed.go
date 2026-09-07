package state

import (
	"sync"
	"time"
)

// CommittedHistory contains only the numeric site history from one successful
// live tick transaction. Sequence follows delivery after commit, including
// clock rollback; it is not a persistent database change cursor.
// SQL imports, retention and other tables are outside this feed's scope.
type CommittedHistory struct {
	Sequence          uint64
	CommittedAtMicros int64
	Point             HistoryPoint
}

type HistoryFeed struct {
	mu      sync.Mutex
	events  chan CommittedHistory
	offered uint64
	dropped uint64
}

type HistoryFeedStats struct {
	Offered uint64 `json:"offered_ticks"`
	Dropped uint64 `json:"dropped_ticks"`
	Queued  int    `json:"queued_ticks"`
}

// ObserveLiveHistory enables one bounded feed. Call before starting writers.
// The feed has no I/O, callback or backpressure path into a SQLite writer.
func (s *Store) ObserveLiveHistory() *HistoryFeed {
	feed := &HistoryFeed{events: make(chan CommittedHistory, 256)}
	s.historyFeedMu.Lock()
	s.historyFeed = feed
	s.historyFeedMu.Unlock()
	return feed
}

func (s *Store) offerCommittedHistory(p *HistoryPoint) {
	if p == nil {
		return
	}
	s.historyFeedMu.RLock()
	feed := s.historyFeed
	s.historyFeedMu.RUnlock()
	if feed == nil {
		return
	}
	point := *p
	point.JSON = "" // The beta copies numeric site history only.
	feed.mu.Lock()
	defer feed.mu.Unlock()
	feed.offered++
	select {
	case feed.events <- CommittedHistory{Sequence: feed.offered, CommittedAtMicros: time.Now().UnixMicro(), Point: point}:
	default:
		feed.dropped++
	}
}

func (f *HistoryFeed) Events() <-chan CommittedHistory { return f.events }

func (f *HistoryFeed) Stats() HistoryFeedStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return HistoryFeedStats{Offered: f.offered, Dropped: f.dropped, Queued: len(f.events)}
}

func (f *HistoryFeed) MarkDropped(count uint64) {
	f.mu.Lock()
	f.dropped += count
	f.mu.Unlock()
}
