// Package updatecli is the `ftw update` command. It asks the running native
// Core to download the next release and swap to it. It does not start a
// Docker updater.
package updatecli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Options controls one update.
type Options struct {
	URL     string
	Channel string
	Token   string
	MaxWait time.Duration
	Now     func() time.Time
	Sleep   func(time.Duration)
	Client  *http.Client
}

type versionInfo struct {
	Current            string `json:"current"`
	Native             bool   `json:"native"`
	Channel            string `json:"channel"`
	Latest             string `json:"latest"`
	UpdateAvailable    bool   `json:"update_available"`
	FullBackupRequired bool   `json:"full_backup_required"`
	Err                string `json:"err"`
}

type updateStatus struct {
	State           string `json:"state"`
	Message         string `json:"message"`
	Target          string `json:"target"`
	Step            int    `json:"step"`
	TotalSteps      int    `json:"total_steps"`
	ProgressCurrent int64  `json:"progress_current"`
	ProgressTotal   int64  `json:"progress_total"`
	ProgressUnit    string `json:"progress_unit"`
}

// Update talks to a running Core. A non-native install is refused so this
// command cannot start the old Docker updater.
func Update(ctx context.Context, opts Options, out io.Writer) error {
	if opts.URL == "" {
		opts.URL = "http://127.0.0.1:8080"
	}
	if opts.MaxWait <= 0 {
		opts.MaxWait = time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = time.Sleep
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	if opts.Channel != "" {
		if err := postJSON(ctx, client, opts, "/api/version/channel", map[string]string{"channel": opts.Channel}); err != nil {
			return err
		}
	}
	var info versionInfo
	if err := getJSON(ctx, client, opts, "/api/version/check?force=1", &info); err != nil {
		return err
	}
	if info.Err != "" {
		return errors.New(info.Err)
	}
	if !info.Native {
		return errors.New("this Core is not a native install; ftw update does not start the Docker updater")
	}
	if !info.UpdateAvailable {
		fmt.Fprintf(out, "Already current on %s: %s\n", info.Channel, info.Current)
		return nil
	}
	if info.FullBackupRequired {
		return fmt.Errorf("updating to %s changes stored data; take a verified full backup off the machine before ftw update", info.Latest)
	}
	fmt.Fprintf(out, "Updating %s -> %s on %s\n", info.Current, info.Latest, info.Channel)
	var started struct {
		Target string `json:"target"`
	}
	if err := postJSON(ctx, client, opts, "/api/version/update", map[string]any{}, &started); err != nil {
		return err
	}
	if started.Target != "" && started.Target != info.Latest {
		return fmt.Errorf("Core accepted %s, not %s", started.Target, info.Latest)
	}
	start := opts.Now()
	deadline := start.Add(opts.MaxWait)
	var last string
	for {
		now := opts.Now()
		if now.After(deadline) {
			return errors.New("update did not finish; Core may still be working")
		}
		var st updateStatus
		if err := getJSON(ctx, client, opts, "/api/version/update/status", &st); err != nil {
			st.State = "restarting"
			st.Message = err.Error()
		}
		line := formatStatus(st, now.Sub(start))
		if line != last {
			fmt.Fprintln(out, line)
			last = line
		}
		switch st.State {
		case "done":
			var health struct {
				Status string `json:"status"`
			}
			var after versionInfo
			if err := getJSON(ctx, client, opts, "/api/health", &health); err != nil {
				return err
			}
			if err := getJSON(ctx, client, opts, "/api/version/check", &after); err != nil {
				return err
			}
			if after.Current != info.Latest || health.Status != "ok" {
				return fmt.Errorf("update ended as %s, health %s", after.Current, health.Status)
			}
			fmt.Fprintf(out, "Update complete: %s\n", after.Current)
			return nil
		case "failed":
			if st.Message == "" {
				st.Message = "update failed"
			}
			return errors.New(st.Message)
		}
		opts.Sleep(2 * time.Second)
	}
}

func formatStatus(st updateStatus, elapsed time.Duration) string {
	parts := []string{fmt.Sprintf("[%s]", elapsed.Round(time.Second)), st.State}
	if st.Step > 0 && st.TotalSteps > 0 {
		parts = append(parts, fmt.Sprintf("step %d/%d", st.Step, st.TotalSteps))
	}
	if st.ProgressUnit == "bytes" && (st.ProgressCurrent > 0 || st.ProgressTotal > 0) {
		if st.ProgressTotal > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d bytes", st.ProgressCurrent, st.ProgressTotal))
		} else {
			parts = append(parts, fmt.Sprintf("%d bytes, total unknown", st.ProgressCurrent))
		}
	}
	if st.Message != "" {
		parts = append(parts, st.Message)
	}
	return strings.Join(parts, " ")
}

func prepare(opts *Options) {
	if opts.URL == "" {
		opts.URL = "http://127.0.0.1:8080"
	}
	if opts.Client == nil {
		opts.Client = http.DefaultClient
	}
}

func getJSON(ctx context.Context, client *http.Client, opts Options, path string, dest any) error {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL+path, nil)
	if err != nil {
		return err
	}
	return doJSON(client, opts, req, dest)
}

func postJSON(ctx context.Context, client *http.Client, opts Options, path string, body any, dest ...any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.URL+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	var out any
	if len(dest) > 0 {
		out = dest[0]
	}
	return doJSON(client, opts, req, out)
}

func doJSON(client *http.Client, opts Options, req *http.Request, dest any) error {
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(payload, &apiErr)
		if apiErr.Error == "" {
			apiErr.Error = strings.TrimSpace(string(payload))
		}
		return fmt.Errorf("%s %s: %s", req.Method, req.URL.Path, apiErr.Error)
	}
	if dest == nil || len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, dest)
}
