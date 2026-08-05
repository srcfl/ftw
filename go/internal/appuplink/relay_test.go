package appuplink

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// fakeRelay is the routing half of srcfl/ftw-webapp relay/src/server.ts: one
// box uplink and up to four browser streams meet under a handle, binary
// messages cross between them, and the relay says two words.
//
// It is a fake rather than the real server because the real one is TypeScript;
// the parts that matter to this package — the join path, the two control
// words and the close codes — are copied, not invented, and
// relay_contract_test.go pins them to the constants the app compiled against.
type fakeRelay struct {
	server *httptest.Server

	mu      sync.Mutex
	epoch   int64
	uplink  *websocket.Conn
	streams []*websocket.Conn
	handles []string
	// rotateTo, when set, closes the next accepted uplink with 4410 and this
	// epoch, which is what the relay does when the hour turns.
	rotateTo *int64
	// refuseEpoch, when set, closes with 4409 and this epoch, which is what
	// the relay does when a box guessed wrong.
	refuseEpoch *int64

	joined chan struct{}
}

func newFakeRelay(epoch int64) *fakeRelay {
	r := &fakeRelay{epoch: epoch, joined: make(chan struct{}, 16)}
	r.server = httptest.NewServer(http.HandlerFunc(r.handle))
	return r
}

func (r *fakeRelay) url() string {
	return "ws" + strings.TrimPrefix(r.server.URL, "http")
}

func (r *fakeRelay) close() { r.server.Close() }

func (r *fakeRelay) handle(w http.ResponseWriter, req *http.Request) {
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "r" {
		http.Error(w, "join", http.StatusBadRequest)
		return
	}
	epoch, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		http.Error(w, "join", http.StatusBadRequest)
		return
	}
	handle, role := parts[2], parts[3]

	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}

	r.mu.Lock()
	r.handles = append(r.handles, handle)
	refuse, rotate, want := r.refuseEpoch, r.rotateTo, r.epoch
	r.mu.Unlock()

	select {
	case r.joined <- struct{}{}:
	default:
	}

	switch {
	case refuse != nil:
		r.mu.Lock()
		r.refuseEpoch = nil
		r.mu.Unlock()
		closeWith(conn, CloseEpoch, strconv.FormatInt(*refuse, 10))
		return
	case rotate != nil:
		r.mu.Lock()
		r.rotateTo = nil
		r.epoch = *rotate
		r.mu.Unlock()
		closeWith(conn, CloseRotated, strconv.FormatInt(*rotate, 10))
		return
	case epoch != want:
		closeWith(conn, CloseEpoch, strconv.FormatInt(want, 10))
		return
	}

	if role == "box" {
		r.serveUplink(conn)
		return
	}
	r.serveStream(conn)
}

func (r *fakeRelay) serveUplink(conn *websocket.Conn) {
	r.mu.Lock()
	r.uplink = conn
	streams := append([]*websocket.Conn(nil), r.streams...)
	r.mu.Unlock()

	// The relay's only two words. Both sides hear them, which is how a peer
	// knows there is anybody to talk to.
	for _, s := range streams {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(CtrlReady))
		_ = s.WriteMessage(websocket.TextMessage, []byte(CtrlReady))
	}

	defer func() {
		r.mu.Lock()
		if r.uplink == conn {
			r.uplink = nil
		}
		r.mu.Unlock()
		_ = conn.Close()
	}()

	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			return
		}
		// Broadcast, because the relay cannot tell which stream a ciphertext
		// belongs to without reading it.
		r.mu.Lock()
		streams := append([]*websocket.Conn(nil), r.streams...)
		r.mu.Unlock()
		for _, s := range streams {
			_ = s.WriteMessage(websocket.BinaryMessage, message)
		}
	}
}

func (r *fakeRelay) serveStream(conn *websocket.Conn) {
	r.mu.Lock()
	r.streams = append(r.streams, conn)
	uplink := r.uplink
	r.mu.Unlock()

	if uplink != nil {
		_ = uplink.WriteMessage(websocket.TextMessage, []byte(CtrlReady))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(CtrlReady))
	}

	defer func() {
		r.mu.Lock()
		for i, s := range r.streams {
			if s == conn {
				r.streams = append(r.streams[:i], r.streams[i+1:]...)
				break
			}
		}
		empty := len(r.streams) == 0
		up := r.uplink
		r.mu.Unlock()
		if empty && up != nil {
			_ = up.WriteMessage(websocket.TextMessage, []byte(CtrlGone))
		}
		_ = conn.Close()
	}()

	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			return
		}
		r.mu.Lock()
		up := r.uplink
		r.mu.Unlock()
		if up != nil {
			_ = up.WriteMessage(websocket.BinaryMessage, message)
		}
	}
}

func (r *fakeRelay) seenHandles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.handles...)
}

func closeWith(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))
	_ = conn.Close()
}
