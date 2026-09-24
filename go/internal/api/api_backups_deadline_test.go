package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A backup answers only when it is done, long after the server's write
// timeout. The reply must still arrive; without lifting the deadline the
// client saw EOF although the archive had been made.
func TestSlowBackupReplyOutlastsTheWriteTimeout(t *testing.T) {
	for _, lift := range []bool{true, false} {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if lift {
				withoutWriteDeadline(w)
			}
			time.Sleep(300 * time.Millisecond)
			_, _ = io.WriteString(w, "created")
		}))
		srv.Config.WriteTimeout = 100 * time.Millisecond
		srv.Start()
		resp, err := http.Post(srv.URL, "application/json", nil)
		var body []byte
		if err == nil {
			body, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		srv.Close()
		got := err == nil && string(body) == "created"
		if got != lift {
			t.Fatalf("lift=%v: reply arrived=%v (err %v, body %q)", lift, got, err, body)
		}
	}
}
