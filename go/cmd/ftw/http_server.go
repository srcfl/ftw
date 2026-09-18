package main

import (
	"net/http"
	"time"
)

const (
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 15 * time.Second
	// WriteTimeout must outlast assistant.Timeout (90s): Ask why streams SSE
	// on this listener, and a shorter write ceiling would cut the reply off.
	httpWriteTimeout = 2 * time.Minute
	httpIdleTimeout  = 60 * time.Second
)

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}
