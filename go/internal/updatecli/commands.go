package updatecli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func runBackup(args []string, stdout io.Writer) error {
	if hasHelp(args) {
		fmt.Fprint(stdout, "usage: ftw backup [--output-dir DIR] [--url URL] [--port PORT]\n")
		return nil
	}
	opts, outputDir, err := commonOpts(args, map[string]bool{"--output-dir": true})
	if err != nil {
		return err
	}
	return Backup(context.Background(), opts, outputDir, stdout)
}

func runDoctor(args []string, stdout io.Writer) error {
	if hasHelp(args) {
		fmt.Fprint(stdout, "usage: ftw doctor [--url URL] [--port PORT]\n")
		return nil
	}
	opts, _, err := commonOpts(args, nil)
	if err != nil {
		return err
	}
	return Doctor(context.Background(), opts, stdout)
}

func runSupport(args []string, stdout io.Writer) error {
	if hasHelp(args) {
		fmt.Fprint(stdout, "usage: ftw support [--output FILE] [--report] [--url URL] [--port PORT]\n")
		return nil
	}
	opts, output, err := commonOpts(args, map[string]bool{"--output": true, "--report": true})
	if err != nil {
		return err
	}
	report := false
	for _, arg := range args {
		if arg == "--report" {
			report = true
		}
	}
	return Support(context.Background(), opts, output, report, stdout)
}

func runStartup(args []string, stdout io.Writer) error {
	if hasHelp(args) {
		fmt.Fprint(stdout, "usage: ftw startup [--root DIR] [--config FILE] [--user-drivers DIR] [--user USER] [--port PORT]\n")
		return nil
	}
	spec := startupSpec{Root: "/opt/ftw", Config: "/var/lib/ftw/config.yaml", UserDrivers: "/var/lib/ftw/drivers", User: "ftw"}
	for i := 0; i < len(args); i++ {
		take := func() (string, error) {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s needs a value", args[i-1])
			}
			return args[i], nil
		}
		switch args[i] {
		case "--root":
			v, err := take()
			if err != nil {
				return err
			}
			spec.Root = v
		case "--config":
			v, err := take()
			if err != nil {
				return err
			}
			spec.Config = v
		case "--user-drivers":
			v, err := take()
			if err != nil {
				return err
			}
			spec.UserDrivers = v
		case "--user":
			v, err := take()
			if err != nil {
				return err
			}
			spec.User = v
		case "--port":
			v, err := take()
			if err != nil {
				return err
			}
			spec.Port = v
		default:
			return fmt.Errorf("unknown argument %s", args[i])
		}
	}
	fmt.Fprint(stdout, startupScript(spec))
	return nil
}

type startupSpec struct {
	Root, Config, UserDrivers, User, Port string
}

func startupScript(spec startupSpec) string {
	exec := fmt.Sprintf("%s/ftw-launcher -root %s -config %s -user-drivers %s", spec.Root, spec.Root, spec.Config, spec.UserDrivers)
	if spec.Port != "" {
		exec += " run -port " + spec.Port
	}
	unit := fmt.Sprintf(`[Unit]
Description=FTW local energy runtime
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
Group=%s
WorkingDirectory=%s
ExecStart=%s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`, spec.User, spec.User, filepath.Dir(spec.Config), exec)
	return fmt.Sprintf(`# Review the paths, then paste this into a shell on the FTW machine.
# It installs a systemd service that keeps Core running. Updating is still: ftw update

sudo tee /etc/systemd/system/ftw.service >/dev/null <<'EOF'
%sEOF
sudo systemctl daemon-reload
sudo systemctl enable --now ftw.service
`, unit)
}

func commonOpts(args []string, extra map[string]bool) (Options, string, error) {
	if extra == nil {
		extra = map[string]bool{}
	}
	extra["--port"] = true
	opts := Options{URL: "http://127.0.0.1:8080", MaxWait: time.Hour}
	opts, err := parseCommon(args, opts, extra)
	if err != nil {
		return opts, "", err
	}
	port := flagValue(args, "--port")
	url := flagValue(args, "--url")
	if port != "" && url != "" {
		return opts, "", errors.New("use either --port or --url")
	}
	if port != "" {
		opts.URL = "http://127.0.0.1:" + port
	}
	output := flagValue(args, "--output-dir")
	if output == "" {
		output = flagValue(args, "--output")
	}
	return opts, output, nil
}

func Backup(ctx context.Context, opts Options, outputDir string, out io.Writer) error {
	prepare(&opts)
	fmt.Fprintln(out, "Starting full backup. Core stays running.")
	type created struct {
		Warning string `json:"warning"`
		Backup  struct {
			ID        string `json:"id"`
			SHA256    string `json:"sha256"`
			SizeBytes int64  `json:"size_bytes"`
			Verified  bool   `json:"verified"`
		} `json:"backup"`
	}
	done := make(chan error, 1)
	var body created
	go func() {
		done <- postJSON(ctx, opts.Client, opts, "/api/backups", map[string]any{}, &body)
	}()
	var last string
	start := time.Now()
	if opts.Now != nil {
		start = opts.Now()
	}
	for {
		select {
		case err := <-done:
			if err != nil {
				return err
			}
			if body.Warning != "" {
				fmt.Fprintln(out, body.Warning)
			}
			if !body.Backup.Verified || !validBackupID(body.Backup.ID) || len(body.Backup.SHA256) != 64 {
				return errors.New("Core did not return a verified backup")
			}
			fmt.Fprintf(out, "Backup on the box: %s (%d bytes, SHA-256 %s)\n", body.Backup.ID, body.Backup.SizeBytes, body.Backup.SHA256)
			if outputDir == "" {
				return nil
			}
			return downloadBackup(ctx, opts, outputDir, body.Backup.ID, body.Backup.SHA256, body.Backup.SizeBytes, out)
		case <-time.After(2 * time.Second):
			var list struct {
				Progress struct {
					Phase          string `json:"phase"`
					CompletedBytes int64  `json:"completed_bytes"`
					TotalBytes     int64  `json:"total_bytes"`
					Table          string `json:"table"`
					RowsDone       int64  `json:"rows_done"`
					Error          string `json:"error"`
				} `json:"progress"`
			}
			if err := getJSON(ctx, opts.Client, opts, "/api/backups", &list); err != nil {
				continue
			}
			now := time.Now()
			if opts.Now != nil {
				now = opts.Now()
			}
			line := backupLine(list.Progress.Phase, list.Progress.Table, list.Progress.RowsDone, list.Progress.CompletedBytes, list.Progress.TotalBytes, now.Sub(start))
			if list.Progress.Error != "" {
				return errors.New(list.Progress.Error)
			}
			if line != last && list.Progress.Phase != "" {
				fmt.Fprintln(out, line)
				last = line
			}
		}
	}
}

func backupLine(phase, table string, rows, done, total int64, elapsed time.Duration) string {
	parts := []string{fmt.Sprintf("[%s]", elapsed.Round(time.Second)), "backup:", phase}
	if table != "" {
		parts = append(parts, table)
	}
	if rows > 0 {
		parts = append(parts, fmt.Sprintf("%d rows", rows))
	}
	if done > 0 || total > 0 {
		if total > 0 {
			parts = append(parts, fmt.Sprintf("%d/%d bytes", done, total))
		} else {
			parts = append(parts, fmt.Sprintf("%d bytes, total unknown", done))
		}
	}
	return strings.Join(parts, " ")
}

func validBackupID(id string) bool {
	return strings.HasPrefix(id, "ftw-full-backup-") && strings.HasSuffix(id, ".ftwbak") && !strings.Contains(id, "/") && !strings.Contains(id, "..")
}

func downloadBackup(ctx context.Context, opts Options, outputDir, id, expect string, size int64, out io.Writer) error {
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return err
	}
	target := filepath.Join(outputDir, id)
	pending := target + ".part"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL+"/api/backups/"+id, nil)
	if err != nil {
		return err
	}
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	resp, err := opts.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("download %s", resp.Status)
	}
	f, err := os.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	sum := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, sum), resp.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(pending)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if got != expect || (size > 0 && n != size) {
		_ = os.Remove(pending)
		return errors.New("downloaded backup does not match Core's verified size and SHA-256")
	}
	if err := os.Rename(pending, target); err != nil {
		_ = os.Remove(pending)
		return err
	}
	fmt.Fprintf(out, "Backup saved: %s\nSHA-256 %s\n", target, expect)
	return nil
}

func Doctor(ctx context.Context, opts Options, out io.Writer) error {
	prepare(&opts)
	var info versionInfo
	var health struct {
		Status         string `json:"status"`
		DriversOK      int    `json:"drivers_ok"`
		DriversOffline int    `json:"drivers_offline"`
		DriversFaulted int    `json:"drivers_faulted"`
		History        struct {
			Migration struct {
				State string `json:"state"`
			} `json:"migration"`
			Writer struct {
				CommitFailures int64 `json:"commit_failures"`
			} `json:"writer"`
		} `json:"history_storage"`
	}
	if err := getJSON(ctx, opts.Client, opts, "/api/version/check", &info); err != nil {
		fmt.Fprintf(out, "core: not reachable (%s)\n", err)
		return err
	}
	if err := getJSON(ctx, opts.Client, opts, "/api/health", &health); err != nil {
		fmt.Fprintf(out, "health: not readable (%s)\n", err)
		return err
	}
	native := "no"
	if info.Native {
		native = "yes"
	}
	fmt.Fprintf(out, "core: reachable\nversion: %s\nnative: %s\nchannel: %s\nhealth: %s\ndrivers: %d ok, %d offline, %d faulted\nhistory: %s\nwrite failures: %d\n",
		info.Current, native, info.Channel, health.Status, health.DriversOK, health.DriversOffline, health.DriversFaulted,
		health.History.Migration.State, health.History.Writer.CommitFailures)
	if info.UpdateAvailable {
		fmt.Fprintf(out, "update: %s is published. Run ftw update\n", info.Latest)
	} else {
		fmt.Fprintln(out, "update: current")
	}
	if !info.Native {
		fmt.Fprintln(out, "note: this is not a native install")
	}
	if health.Status != "ok" || health.DriversFaulted > 0 {
		return errors.New("doctor found a problem")
	}
	return nil
}

func Support(ctx context.Context, opts Options, output string, report bool, out io.Writer) error {
	prepare(&opts)
	path := "/api/support/dump"
	name := "ftw-support.zip"
	if report {
		path = "/api/support/report"
		name = "ftw-support.md"
	}
	if output == "" {
		output = name
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL+path, nil)
	if err != nil {
		return err
	}
	if opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+opts.Token)
	}
	resp, err := opts.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("support: %s", strings.TrimSpace(string(raw)))
	}
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(output)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	fmt.Fprintf(out, "Support file: %s (%d bytes)\nCore redacts secrets before writing this. It still describes the site.\n", output, n)
	return nil
}
