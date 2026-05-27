package tunnel

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQueueEnqueuePollResponse(t *testing.T) {
	q := NewQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	respCh := make(chan TunneledResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		req := TunneledRequest{Method: "GET", Path: "/mcp"}
		resp, err := q.Enqueue(ctx, "host-a", req)
		if err != nil {
			errCh <- err
			return
		}
		respCh <- resp
	}()

	got, err := q.Poll(ctx, "host-a", 1*time.Second)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got.Method != "GET" || got.Path != "/mcp" {
		t.Fatalf("wrong request: %+v", got)
	}
	if got.ReqID == "" {
		t.Fatalf("queue must assign a req_id")
	}

	if err := q.PostResponse(got.ReqID, TunneledResponse{Status: 200, Body: []byte("hello")}); err != nil {
		t.Fatalf("post: %v", err)
	}

	select {
	case resp := <-respCh:
		if resp.Status != 200 || string(resp.Body) != "hello" {
			t.Fatalf("wrong response: %+v", resp)
		}
	case err := <-errCh:
		t.Fatalf("enqueue err: %v", err)
	case <-ctx.Done():
		t.Fatal("response never arrived")
	}
}

func TestQueuePollTimesOut(t *testing.T) {
	q := NewQueue()
	_, err := q.Poll(context.Background(), "host-empty", 100*time.Millisecond)
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("want ErrPollTimeout, got %v", err)
	}
}

func TestQueuePostResponseUnknownIDFails(t *testing.T) {
	q := NewQueue()
	err := q.PostResponse("nonexistent-id", TunneledResponse{Status: 200})
	if !errors.Is(err, ErrUnknownReqID) {
		t.Fatalf("want ErrUnknownReqID, got %v", err)
	}
}

func TestQueueEnqueueRespectsContextCancel(t *testing.T) {
	q := NewQueue()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := q.Enqueue(ctx, "host-x", TunneledRequest{Method: "GET", Path: "/x"})
		errCh <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("enqueue did not return after cancel")
	}
}

// TestQueuePendingDeliveredOnNextPoll exercises the path where the
// producer arrives before any waiter (request lands in pending), then
// a subsequent poll drains it.
func TestQueuePendingDeliveredOnNextPoll(t *testing.T) {
	q := NewQueue()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	respCh := make(chan TunneledResponse, 1)
	go func() {
		resp, _ := q.Enqueue(ctx, "host-z", TunneledRequest{Method: "POST", Path: "/x"})
		respCh <- resp
	}()
	// Give the producer time to enqueue before we poll.
	time.Sleep(20 * time.Millisecond)

	got, err := q.Poll(ctx, "host-z", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if got.Path != "/x" {
		t.Fatalf("wrong request: %+v", got)
	}
	_ = q.PostResponse(got.ReqID, TunneledResponse{Status: 201})
	resp := <-respCh
	if resp.Status != 201 {
		t.Fatalf("wrong response status: %d", resp.Status)
	}
}
