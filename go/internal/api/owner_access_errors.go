package api

import (
	"log/slog"
	"net/http"
)

const (
	errEnrollPinLANOnly       = "FTW_ENROLL_PIN_LAN_ONLY"
	errRemoteAccessOff        = "FTW_REMOTE_ACCESS_OFF"
	errRemoteRestartRequired  = "FTW_REMOTE_RESTART_REQUIRED"
	errTrustedDevicesRead     = "FTW_TRUSTED_DEVICES_READ_FAILED"
	errFirstSetupClosed       = "FTW_FIRST_SETUP_CLOSED"
	errEnrollPinMintFailed    = "FTW_ENROLL_PIN_MINT_FAILED"
	errBootstrapPublishFailed = "FTW_BOOTSTRAP_PUBLISH_FAILED"
	errEnrollPinRequired      = "FTW_ENROLL_PIN_REQUIRED"
	errEnrollLANRequired      = "FTW_ENROLL_LAN_REQUIRED"
	errEnrollSessionRequired  = "FTW_ENROLL_SESSION_REQUIRED"
	errBootstrapProofRequired = "FTW_BOOTSTRAP_PROOF_REQUIRED"
	errEnrollWindowClosed     = "FTW_ENROLL_WINDOW_CLOSED"
)

type ownerAccessHTTPError struct {
	status int
	code   string
	msg    string
}

func (e ownerAccessHTTPError) Error() string {
	return e.msg
}

func ownerAccessError(status int, code, msg string) error {
	return ownerAccessHTTPError{status: status, code: code, msg: msg}
}

func writeOwnerAccessError(w http.ResponseWriter, r *http.Request, status int, code, msg string, attrs ...any) {
	w.Header().Set("X-FTW-Error-Code", code)
	args := []any{
		"code", code,
		"status", status,
		"method", r.Method,
		"path", r.URL.Path,
		"remote", r.RemoteAddr,
	}
	args = append(args, attrs...)
	slog.Warn("owner-access request rejected", args...)
	http.Error(w, code+": "+msg, status)
}
