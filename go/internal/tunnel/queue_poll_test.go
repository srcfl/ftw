package tunnel

import (
	"context"
	"testing"
	"time"
)

// Regression for the "poll-waiter" bug: a timed-out Poll must not leave a dead
// waiter in the queue, or a later Enqueue hands its request off to that dead
// poller — silently dropping it and stranding the caller forever.
func TestPollTimeoutDoesNotStrandEnqueue(t *testing.T) {
	q := NewQueue()
	const host = "h1"

	// (1) A poll that finds nothing times out — and must leave no stale waiter.
	if _, err := q.Poll(context.Background(), host, 10*time.Millisecond); err != ErrPollTimeout {
		t.Fatalf("want ErrPollTimeout, got %v", err)
	}

	// (2) A fresh poller drains the request a moment later.
	go func() {
		time.Sleep(20 * time.Millisecond)
		req, err := q.Poll(context.Background(), host, time.Second)
		if err != nil {
			return
		}
		_ = q.PostResponse(req.ReqID, TunneledResponse{Status: 200, Body: []byte("ok")})
	}()

	// (3) The request must reach that fresh poller, not vanish into a dead one.
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	resp, err := q.Enqueue(ctx, host, TunneledRequest{Method: "GET", Path: "/"})
	if err != nil {
		t.Fatalf("Enqueue stranded by a dead waiter (poll-waiter bug): %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("want 200, got %d", resp.Status)
	}
}

// Many idle Poll timeouts must not accumulate dead waiters.
func TestPollTimeoutsDoNotAccumulateWaiters(t *testing.T) {
	q := NewQueue()
	const host = "h2"
	for i := 0; i < 5; i++ {
		_, _ = q.Poll(context.Background(), host, 5*time.Millisecond)
	}
	q.mu.Lock()
	n := len(q.waiters[host])
	q.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected 0 lingering waiters after timeouts, got %d", n)
	}
}
