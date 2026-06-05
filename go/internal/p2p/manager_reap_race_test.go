package p2p

import (
	"sync"
	"testing"
	"time"
)

// manager_reap_race_test.go — FIX-D: the unauth reap and markAuthed must not
// race. A peer that authenticates concurrently with (or a hair before) its grace
// reap must SURVIVE — the authed check and the close are now under the same lock
// hold. Run with -race to catch both the data race and the lost-update.

// TestReapIfUnauthed_RaceWithMarkAuthed fires reapIfUnauthed and markAuthed at the
// same instant on the same peer, many times. THE INVARIANT: once markAuthed has
// returned (the peer is authenticated), the reap must NOT have closed it. We
// observe markAuthed's effect: if markAuthed reports it set the flag on a present
// session, that session must still exist afterwards. With the TOCTOU bug a reap
// that read !authed, then let markAuthed land, then ran its unconditional remove,
// would delete an authenticated peer — caught here and by -race.
func TestReapIfUnauthed_RaceWithMarkAuthed(t *testing.T) {
	mgr := NewManager(nil, nil)
	defer mgr.Close()

	const iters = 500
	lostUpdates := 0
	for i := 0; i < iters; i++ {
		id := "race-" + time.Now().Format("150405.000000000") + "-" + fmtID("n", i%10)
		mgr.inject(t, id, unauthReapAfter+time.Second, false) // past grace, unauth → reap-eligible

		var wg sync.WaitGroup
		var authedOK bool // markAuthed set the flag AND the session was still present at that instant
		wg.Add(2)
		go func() {
			defer wg.Done()
			mgr.mu.Lock()
			if s := mgr.sessions[id]; s != nil {
				s.authed = true
				authedOK = true // we authenticated a live session; it must survive
			}
			mgr.mu.Unlock()
		}()
		go func() { defer wg.Done(); mgr.reapIfUnauthed(id) }()
		wg.Wait()

		mgr.mu.Lock()
		_, present := mgr.sessions[id]
		mgr.mu.Unlock()

		// If markAuthed committed authed=true while the session was present, a
		// correct reap (same-lock re-check) must have seen authed and kept it. If
		// it's gone, the reap removed a peer that had been authenticated — the
		// lost update.
		if authedOK && !present {
			lostUpdates++
		}
		if present {
			mgr.remove(id) // tidy up between rounds
		}
	}
	if lostUpdates != 0 {
		t.Fatalf("FIX-D: %d/%d rounds closed a peer AFTER it was authenticated (TOCTOU lost update)", lostUpdates, iters)
	}
}

// TestMarkAuthedThenReap_AuthedSurvives is the deterministic ordering guard: a
// peer authenticated BEFORE its grace timer fires must always survive the reap,
// even though it is past the unauth grace age.
func TestMarkAuthedThenReap_AuthedSurvives(t *testing.T) {
	mgr := NewManager(nil, nil)
	defer mgr.Close()

	mgr.inject(t, "authed-past-grace", unauthReapAfter+time.Second, false)
	mgr.markAuthed("authed-past-grace") // logs in just before the timer
	mgr.reapIfUnauthed("authed-past-grace")

	mgr.mu.Lock()
	_, present := mgr.sessions["authed-past-grace"]
	mgr.mu.Unlock()
	if !present {
		t.Fatal("a peer authenticated before its grace reap was still closed (FIX-D regression)")
	}
}
