package tunnel

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrPollTimeout — no request available within the poll deadline.
	// Hosts should re-poll immediately.
	ErrPollTimeout = errors.New("tunnel: poll timeout")
	// ErrUnknownReqID — the host POSTed a response for a request that
	// the queue doesn't know about (already responded, expired, or
	// fabricated). Indicates a host bug or a stale relay restart.
	ErrUnknownReqID = errors.New("tunnel: unknown req_id")
)

// Queue is the relay-internal request queue. One Queue per relay
// process; it indexes pending requests per host-id.
type Queue struct {
	mu       sync.Mutex
	pending  map[string][]TunneledRequest
	waiters  map[string][]chan TunneledRequest
	inflight map[string]chan TunneledResponse
}

// NewQueue returns an initialized Queue.
func NewQueue() *Queue {
	return &Queue{
		pending:  make(map[string][]TunneledRequest),
		waiters:  make(map[string][]chan TunneledRequest),
		inflight: make(map[string]chan TunneledResponse),
	}
}

// Enqueue submits a request for the given host and blocks until the
// host POSTs a response, the context is cancelled, or the request is
// purged. The req field's ReqID is overwritten with a fresh UUID.
func (q *Queue) Enqueue(ctx context.Context, hostID string, req TunneledRequest) (TunneledResponse, error) {
	req.ReqID = uuid.NewString()
	respCh := make(chan TunneledResponse, 1)

	q.mu.Lock()
	q.inflight[req.ReqID] = respCh
	// Hand off directly to a waiting poller if there is one.
	if waiters := q.waiters[hostID]; len(waiters) > 0 {
		next := waiters[0]
		q.waiters[hostID] = waiters[1:]
		q.mu.Unlock()
		next <- req
	} else {
		q.pending[hostID] = append(q.pending[hostID], req)
		q.mu.Unlock()
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		q.mu.Lock()
		delete(q.inflight, req.ReqID)
		q.mu.Unlock()
		return TunneledResponse{}, ctx.Err()
	}
}

// Poll long-polls for the next pending request for hostID, or returns
// ErrPollTimeout after the timeout. The returned request carries an
// assigned ReqID that the host must use in PostResponse.
func (q *Queue) Poll(ctx context.Context, hostID string, timeout time.Duration) (TunneledRequest, error) {
	q.mu.Lock()
	if pending := q.pending[hostID]; len(pending) > 0 {
		next := pending[0]
		q.pending[hostID] = pending[1:]
		q.mu.Unlock()
		return next, nil
	}
	wait := make(chan TunneledRequest, 1)
	q.waiters[hostID] = append(q.waiters[hostID], wait)
	q.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case req := <-wait:
		return req, nil
	case <-timer.C:
		return q.abandonWaiter(hostID, wait)
	case <-ctx.Done():
		if req, err := q.abandonWaiter(hostID, wait); err == nil {
			return req, nil
		}
		return TunneledRequest{}, ctx.Err()
	}
}

// abandonWaiter removes a timed-out / cancelled waiter from the host's waiter
// slice so a later Enqueue never hands a request off to a dead poller — which
// would silently drop the request and hang the caller forever. It drains the
// channel first, so a request handed off in the race window is never lost.
func (q *Queue) abandonWaiter(hostID string, wait chan TunneledRequest) (TunneledRequest, error) {
	q.mu.Lock()
	ws := q.waiters[hostID]
	for i, c := range ws {
		if c == wait {
			q.waiters[hostID] = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	q.mu.Unlock()
	select {
	case req := <-wait:
		return req, nil
	default:
		return TunneledRequest{}, ErrPollTimeout
	}
}

// PostResponse delivers a host's response to the waiting Enqueue call.
// Returns ErrUnknownReqID if the req_id isn't pending.
func (q *Queue) PostResponse(reqID string, resp TunneledResponse) error {
	q.mu.Lock()
	respCh, ok := q.inflight[reqID]
	if ok {
		delete(q.inflight, reqID)
	}
	q.mu.Unlock()
	if !ok {
		return ErrUnknownReqID
	}
	respCh <- resp
	return nil
}
