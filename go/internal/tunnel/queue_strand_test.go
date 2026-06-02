package tunnel

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPollTimeoutDoesNotStrandEnqueue is the regression for the relay hang
// that broke help-a-friend end-to-end: a Poll that times out must not leave
// its waiter channel registered. If it does, the next Enqueue hands the
// request to that abandoned (buffered, never-read) channel and the friend's
// request is lost forever — it hangs until the client gives up.
//
// In practice a friend approves the session, then takes more than one
// poll-timeout (25s in prod) to paste the `claude mcp add` command, so the
// FIRST real MCP request always lands after at least one poll has timed out.
func TestPollTimeoutDoesNotStrandEnqueue(t *testing.T) {
	q := NewQueue()
	ctx := context.Background()

	// 1. A poll times out with no request available.
	if _, err := q.Poll(ctx, "h1", 10*time.Millisecond); !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("first poll: want ErrPollTimeout, got %v", err)
	}

	// 2. A friend request is enqueued; a live poller must be able to pick it up.
	done := make(chan error, 1)
	go func() {
		_, err := q.Enqueue(ctx, "h1", TunneledRequest{Method: "POST", Path: "/mcp"})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond) // let Enqueue register the request

	// 3. A fresh poll must deliver the enqueued request.
	req, err := q.Poll(ctx, "h1", time.Second)
	if err != nil {
		t.Fatalf("live poll after a timed-out poll: request was stranded: %v", err)
	}
	if err := q.PostResponse(req.ReqID, TunneledResponse{Status: 200}); err != nil {
		t.Fatalf("post response: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("enqueue never completed — request lost to a stale waiter")
	}
}

// TestPollCtxCancelDoesNotStrandEnqueue is the same invariant for the
// context-cancellation exit path (host shutdown / client disconnect): a
// cancelled Poll must also deregister its waiter.
func TestPollCtxCancelDoesNotStrandEnqueue(t *testing.T) {
	q := NewQueue()

	cctx, ccancel := context.WithCancel(context.Background())
	pollErr := make(chan error, 1)
	go func() {
		_, err := q.Poll(cctx, "h1", time.Second)
		pollErr <- err
	}()
	time.Sleep(20 * time.Millisecond)
	ccancel()
	if err := <-pollErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled poll: want context.Canceled, got %v", err)
	}

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := q.Enqueue(ctx, "h1", TunneledRequest{Method: "POST", Path: "/mcp"})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)

	req, err := q.Poll(ctx, "h1", time.Second)
	if err != nil {
		t.Fatalf("live poll after a cancelled poll: request was stranded: %v", err)
	}
	if err := q.PostResponse(req.ReqID, TunneledResponse{Status: 200}); err != nil {
		t.Fatalf("post response: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("enqueue never completed — request lost to a stale waiter")
	}
}

// TestPollHandoffRaceDeliversRequest covers the race where Enqueue claims a
// waiter at the same instant that waiter's Poll hits its deadline: the
// request must still be delivered (to the timing-out poller), never dropped.
func TestPollHandoffRaceDeliversRequest(t *testing.T) {
	for i := 0; i < 200; i++ {
		q := NewQueue()
		ctx := context.Background()

		gotReq := make(chan TunneledRequest, 1)
		go func() {
			// Very short deadline so this poll is racing the enqueue below.
			req, err := q.Poll(ctx, "h1", time.Millisecond)
			if err == nil {
				gotReq <- req
			}
		}()

		enqDone := make(chan TunneledResponse, 1)
		go func() {
			resp, _ := q.Enqueue(ctx, "h1", TunneledRequest{Method: "GET", Path: "/x"})
			enqDone <- resp
		}()

		// Drain: whoever ends up holding the request answers it. If the poll
		// won the race it delivers via gotReq; otherwise a follow-up poll does.
		go func() {
			select {
			case req := <-gotReq:
				_ = q.PostResponse(req.ReqID, TunneledResponse{Status: 200})
			case <-time.After(500 * time.Millisecond):
			}
		}()
		go func() {
			// Backup poller in case the racing poll timed out empty-handed.
			req, err := q.Poll(ctx, "h1", 300*time.Millisecond)
			if err == nil {
				_ = q.PostResponse(req.ReqID, TunneledResponse{Status: 200})
			}
		}()

		select {
		case <-enqDone:
		case <-time.After(time.Second):
			t.Fatalf("iter %d: request dropped in poll/enqueue handoff race", i)
		}
	}
}
