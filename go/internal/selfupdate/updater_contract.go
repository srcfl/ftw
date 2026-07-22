package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

const (
	UpdaterProtocolVersion          = 2
	ControlPlanePairCapability      = "control-plane-pair-v1"
	updaterCompatibilityHTTPTimeout = 5 * time.Second
)

type UpdaterRuntimeInfo struct {
	ProtocolVersion int      `json:"protocol_version"`
	Version         string   `json:"updater_version"`
	Capabilities    []string `json:"capabilities"`
}

func updaterHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: updaterCompatibilityHTTPTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

func ProbeUpdater(ctx context.Context, socketPath string) (UpdaterRuntimeInfo, error) {
	if socketPath == "" {
		return UpdaterRuntimeInfo{}, errors.New("selfupdate: sidecar socket not configured")
	}
	fi, err := os.Stat(socketPath)
	if err != nil {
		return UpdaterRuntimeInfo{}, fmt.Errorf("selfupdate: updater socket: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return UpdaterRuntimeInfo{}, errors.New("selfupdate: updater path is not a Unix socket")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/status", nil)
	resp, err := updaterHTTPClient(socketPath).Do(req)
	if err != nil {
		return UpdaterRuntimeInfo{}, fmt.Errorf("selfupdate: updater handshake: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return UpdaterRuntimeInfo{}, fmt.Errorf("selfupdate: updater handshake HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var info UpdaterRuntimeInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&info); err != nil {
		return UpdaterRuntimeInfo{}, fmt.Errorf("selfupdate: decode updater handshake: %w", err)
	}
	if info.ProtocolVersion < UpdaterProtocolVersion || !slices.Contains(info.Capabilities, ControlPlanePairCapability) {
		return UpdaterRuntimeInfo{}, fmt.Errorf("selfupdate: updater lacks %s protocol %d", ControlPlanePairCapability, UpdaterProtocolVersion)
	}
	if strings.TrimSpace(info.Version) == "" {
		return UpdaterRuntimeInfo{}, errors.New("selfupdate: updater did not report its release version")
	}
	return info, nil
}

func RequireUpdaterRelease(ctx context.Context, socketPath, release string) error {
	info, err := ProbeUpdater(ctx, socketPath)
	if err != nil {
		return err
	}
	if release != "" && info.Version != release {
		return fmt.Errorf("selfupdate: updater release %s does not match Core release %s", info.Version, release)
	}
	return nil
}
