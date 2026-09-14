package state

import (
	"testing"
	"time"
)

func TestMaintenanceBackoffDoesNotPostponeRetry(t *testing.T) {
	s := freshStore(t)
	w := s.historyWriter // Idle: this test submits no ticks to the run loop.
	w.maintenanceRetryDelay = time.Minute
	w.maintenanceDue = time.Time{}
	w.maintainHistory(0)
	if st := s.HistoryWriterStatus(); st.MaintenanceRuns != 1 || st.MaintenanceError != "" {
		t.Fatalf("initial rotation failed: %+v", st)
	}
	deadline := w.maintenanceRetry
	if !deadline.After(time.Now()) {
		t.Fatal("successful rotation has no retry deadline")
	}
	for range 3 {
		w.maintenanceDue = time.Time{} // The same queued tick OOMs again.
		w.maintainHistory(0)
		if st := s.HistoryWriterStatus(); st.MaintenanceRuns != 1 {
			t.Fatalf("rotated inside backoff: %+v", st)
		}
		if !w.maintenanceRetry.Equal(deadline) {
			t.Fatal("skipped retry extended the deadline")
		}
	}
	w.maintenanceRetry = time.Now().Add(-time.Second)
	w.maintainHistory(0)
	if st := s.HistoryWriterStatus(); st.MaintenanceRuns != 2 {
		t.Fatalf("rotation did not resume after backoff: %+v", st)
	}
}
