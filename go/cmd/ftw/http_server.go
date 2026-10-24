package main

import (
	"net/http"
	"os"
	"time"

	"github.com/srcfl/ftw/go/internal/api"
)

const (
	httpReadHeaderTimeout = 10 * time.Second
	httpReadTimeout       = 15 * time.Second
	// WriteTimeout bounds how long one response may take to a slow client.
	httpWriteTimeout = 2 * time.Minute
	httpIdleTimeout  = 60 * time.Second
)

func homeAssistantIngressEnabled() bool {
	return os.Getenv("FTW_BUNDLE") == "home_assistant_addon" && os.Getenv("FTW_HA_INGRESS") == "1"
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	if homeAssistantIngressEnabled() {
		addr = ":18099"
		handler = api.WithHomeAssistantIngress(handler)
	}
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
	}
}
