// Package updateipc defines the read-only startup contract with the updater.
package updateipc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

const CapabilitiesPath = "/capabilities"

type Capabilities struct {
	Protocol                       int  `json:"protocol"`
	PreserveCoreOnReadinessFailure bool `json:"preserve_core_on_readiness_failure"`
}

// ServeCapabilities advertises the fixed updater's failure behavior. Reading
// it does not create a job, alter state.json or run a container command.
func ServeCapabilities(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(Capabilities{Protocol: 1, PreserveCoreOnReadinessFailure: true})
}

// RequireSafeUpdater runs before Core opens any persistent state. Only a
// positive capability response permits startup; an older sidecar can then
// revert a refused Core without any new data format having been written.
func RequireSafeUpdater(ctx context.Context, socket string) error {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix"+CapabilitiesPath, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			var capabilities Capabilities
			decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&capabilities)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && capabilities.Protocol == 1 && capabilities.PreserveCoreOnReadinessFailure {
				return nil
			}
			return fmt.Errorf("update ftw-updater first: the running updater does not confirm safe history upgrades (HTTP %d); Core has not opened its data", resp.StatusCode)
		}
		// Compose can start Core before the sidecar has bound its socket.
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("update ftw-updater first: cannot verify the updater at %s; Core has not opened its data: %w", socket, ctx.Err())
		case <-timer.C:
		}
	}
}
