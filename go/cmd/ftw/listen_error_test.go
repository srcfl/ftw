package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
)

func TestExplainBindErrorNamesATakenPortThatIsNotFTW(t *testing.T) {
	err := explainBindError("127.0.0.1:1", &net.OpError{Err: syscall.EADDRINUSE})
	if err == nil || !strings.Contains(err.Error(), "does not answer as FTW") || !strings.Contains(err.Error(), "api.port") {
		t.Fatal(err)
	}
}

func TestExplainBindErrorRecognisesAStartingFTW(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"phase":"initializing state","status":"starting"}`))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	err := explainBindError(":"+port, &net.OpError{Err: syscall.EADDRINUSE})
	if err == nil || !strings.Contains(err.Error(), "FTW is already running") ||
		!strings.Contains(err.Error(), "ftw status --url http://127.0.0.1:"+port) {
		t.Fatal(err)
	}
}

func TestExplainBindErrorKeepsOtherFailures(t *testing.T) {
	cause := &net.OpError{Err: syscall.EACCES}
	if err := explainBindError(":80", cause); !errors.Is(err, syscall.EACCES) {
		t.Fatal(err)
	}
}
