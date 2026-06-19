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
	// ErrTooManyWaiters — the global live-waiter cap is reached. /tunnel/next
	// is unauthenticated, so this bounds the goroutines/channels an attacker can
	// pin by long-polling many host_ids. Hosts back off and retry.
	ErrTooManyWaiters = errors.New("tunnel: too many waiters")
)

// defaultMaxWaiters bounds the total number of concurrently parked long-poll
// waiters across all host_ids. Legitimate hosts hold ~1 in-flight poll each, so
// this is far above real usage; it exists purely to cap an unauthenticated flood.
const defaultMaxWaiters = 1024

// Queue is the relay-internal request queue. One Queue per relay
// process; it indexes pending requests per host-id.
type Queue struct {
	mu          sync.Mutex
	pending     map[string][]TunneledRequest
	waiters     map[string][]chan TunneledRequest
	inflight    map[string]chan TunneledResponse
	waiterCount int // total live waiters across all hosts (bounded by maxWaiters)
	maxWaiters  int // global parked-waiter ceiling (defaultMaxWaiters)
}

// NewQueue returns an initialized Queue.
func NewQueue() *Queue {
	return &Queue{
		pending:    make(map[string][]TunneledRequest),
		waiters:    make(map[string][]chan TunneledRequest),
		inflight:   make(map[string]chan TunneledResponse),
		maxWaiters: defaultMaxWaiters,
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
		q.waiterCount--
		if len(q.waiters[hostID]) == 0 {
			delete(q.waiters, hostID) // don't leak an empty-slice entry per host_id
		}
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
		// The caller gave up (client disconnect or a deadline — see the relay's
		// per-request timeout for a dead-but-registered host). Drop both the
		// inflight channel and any still-queued copy of this request so a host
		// that never polls can't make the pending slice grow without bound.
		q.mu.Lock()
		delete(q.inflight, req.ReqID)
		q.removePendingLocked(hostID, req.ReqID)
		q.mu.Unlock()
		return TunneledResponse{}, ctx.Err()
	}
}

// removePendingLocked drops a queued request by ReqID. Caller holds q.mu.
func (q *Queue) removePendingLocked(hostID, reqID string) {
	ps := q.pending[hostID]
	for i, r := range ps {
		if r.ReqID == reqID {
			q.pending[hostID] = append(ps[:i], ps[i+1:]...)
			break
		}
	}
	if len(q.pending[hostID]) == 0 {
		delete(q.pending, hostID)
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
	// Bound the total parked waiters so an unauthenticated flood of long-polls
	// (across arbitrary host_ids) can't pin unbounded goroutines/channels.
	if q.waiterCount >= q.maxWaiters {
		q.mu.Unlock()
		return TunneledRequest{}, ErrTooManyWaiters
	}
	wait := make(chan TunneledRequest, 1)
	q.waiters[hostID] = append(q.waiters[hostID], wait)
	q.waiterCount++
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
	found := false
	for i, c := range ws {
		if c == wait {
			q.waiters[hostID] = append(ws[:i], ws[i+1:]...)
			found = true
			break
		}
	}
	// Only decrement if WE removed it — if Enqueue already handed this waiter a
	// request (the race the select below catches), it already decremented.
	if found {
		q.waiterCount--
	}
	// An unauthenticated client can long-poll arbitrary host_ids; without this
	// the waiters map keeps one empty-slice entry per distinct host_id forever.
	if len(q.waiters[hostID]) == 0 {
		delete(q.waiters, hostID)
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
