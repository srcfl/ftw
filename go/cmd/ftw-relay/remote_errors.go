package main

import (
	"log/slog"
	"net/http"
)

const (
	errRemoteP2POnly          = "FTW_REMOTE_P2P_ONLY"
	errRemoteAPIP2POnly       = "FTW_REMOTE_API_P2P_ONLY"
	errBootstrapStoreMissing  = "FTW_BOOTSTRAP_STORE_UNAVAILABLE"
	errBootstrapRateLimited   = "FTW_BOOTSTRAP_RATE_LIMITED"
	errBootstrapClaimRequired = "FTW_BOOTSTRAP_CLAIM_REQUIRED"
	errBootstrapNotLive       = "FTW_BOOTSTRAP_NOT_LIVE"
	errRemoteHomeOffline      = "FTW_REMOTE_HOME_OFFLINE"
	errRemoteRequestTooLarge  = "FTW_REMOTE_REQUEST_TOO_LARGE"
	errRemoteHomeNoResponse   = "FTW_REMOTE_HOME_NO_RESPONSE"
)

func writeRemoteError(w http.ResponseWriter, req *http.Request, status int, code, msg string, attrs ...any) {
	w.Header().Set("X-FTW-Error-Code", code)
	args := []any{
		"code", code,
		"status", status,
		"method", req.Method,
		"host", req.Host,
		"path", req.URL.Path,
	}
	args = append(args, attrs...)
	slog.Warn("ftw-relay request rejected", args...)
	http.Error(w, code+": "+msg, status)
}
