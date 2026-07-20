package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestWrapDoesNotAdvertiseWildcardCORS(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("proxied legacy response", func(t *testing.T) {
		rr := httptest.NewRecorder()
		Wrap(http.NotFoundHandler(), Config{Upstream: upstreamURL}).ServeHTTP(
			rr, httptest.NewRequest(http.MethodGet, "/api/status", nil),
		)
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	})

	t.Run("read-only rejection", func(t *testing.T) {
		rr := httptest.NewRecorder()
		Wrap(http.NotFoundHandler(), Config{Upstream: upstreamURL, ReadOnly: true}).ServeHTTP(
			rr, httptest.NewRequest(http.MethodPost, "/api/restart", nil),
		)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rr.Code)
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
		}
	})
}
