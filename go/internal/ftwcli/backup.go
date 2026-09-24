package ftwcli

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type backupEntry struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Verified  bool   `json:"verified"`
}

type backupProgress struct {
	Phase          string `json:"phase"`
	CompletedBytes int64  `json:"completed_bytes"`
	TotalBytes     int64  `json:"total_bytes"`
	Table          string `json:"table"`
	RowsDone       int64  `json:"rows_done"`
}

func (p backupProgress) line() string {
	parts := []string{strings.ReplaceAll(p.Phase, "_", " ")}
	if p.Table != "" {
		parts = append(parts, p.Table)
	}
	if p.RowsDone > 0 {
		parts = append(parts, fmt.Sprintf("%d rows", p.RowsDone))
	}
	if p.TotalBytes > 0 {
		parts = append(parts, formatBytes(p.CompletedBytes)+" of "+formatBytes(p.TotalBytes))
	} else if p.CompletedBytes > 0 {
		parts = append(parts, formatBytes(p.CompletedBytes)+", total unknown")
	}
	return strings.Join(parts, " ")
}

func runBackup(args []string, out io.Writer, e env) error {
	var outputDir string
	base, err := parse(args, out, "ftw backup [--output-dir DIR] [--url URL]", func(fs *flag.FlagSet) {
		fs.StringVar(&outputDir, "output-dir", "", "")
	})
	if err != nil {
		return err
	}
	c := newClient(base, e)
	ctx := context.Background()
	fmt.Fprintln(out, "Making a full backup. Core keeps running; Ctrl-C stops the backup.")
	start := e.now()

	type result struct {
		body struct {
			Warning string      `json:"warning"`
			Backup  backupEntry `json:"backup"`
		}
		err error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		// No per-call timeout: Core answers only when the archive is verified.
		r.err = c.call(ctx, http.MethodPost, "/api/backups", map[string]any{}, &r.body, 0)
		done <- r
	}()
	var dir, last string
	lastPrinted := start
	for {
		select {
		case r := <-done:
			if r.err != nil {
				return fmt.Errorf("backup failed: %w", r.err)
			}
			if r.body.Warning != "" {
				fmt.Fprintln(out, "Warning:", r.body.Warning)
			}
			b := r.body.Backup
			if !b.Verified || !validBackupID(b.ID) || !validDigest(b.SHA256) {
				return errors.New("Core did not return a verified backup")
			}
			if dir == "" {
				var list struct {
					Dir string `json:"dir"`
				}
				_ = c.get(ctx, "/api/backups", &list)
				dir = list.Dir
			}
			where := b.ID
			if dir != "" {
				where = filepath.Join(dir, b.ID)
			}
			fmt.Fprintf(out, "Backup on the box: %s (%s, SHA-256 %s) after %s\n",
				where, formatBytes(b.SizeBytes), b.SHA256, formatElapsed(e.now().Sub(start)))
			if outputDir == "" {
				return nil
			}
			return c.copyBackup(ctx, out, outputDir, b)
		case <-time.After(e.pollInterval):
		}
		var list struct {
			Dir      string         `json:"dir"`
			Progress backupProgress `json:"progress"`
		}
		if c.get(ctx, "/api/backups", &list) != nil || list.Progress.Phase == "" {
			continue
		}
		dir = list.Dir
		now := e.now()
		if line := list.Progress.line(); line != last || now.Sub(lastPrinted) >= e.heartbeat {
			fmt.Fprintf(out, "[%s] %s\n", formatElapsed(now.Sub(start)), line)
			last, lastPrinted = line, now
		}
	}
}

// copyBackup copies a verified archive to dir and checks size and SHA-256
// before the file gets its final name.
func (c *client) copyBackup(ctx context.Context, out io.Writer, dir string, b backupEntry) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	target := filepath.Join(dir, b.ID)
	if sum, err := fileSHA256(target); err == nil {
		if sum != b.SHA256 {
			return fmt.Errorf("%s already exists and differs from Core's backup", target)
		}
		fmt.Fprintf(out, "Already saved: %s\n", target)
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pending := target + ".part"
	// A copy interrupted earlier leaves this name behind; it is ours.
	if err := os.Remove(pending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	resp, err := c.open(ctx, "/api/backups/"+b.ID)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(pending, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum), resp.Body)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && (hex.EncodeToString(sum.Sum(nil)) != b.SHA256 || (b.SizeBytes > 0 && n != b.SizeBytes)) {
		err = errors.New("the copy does not match Core's verified size and SHA-256")
	}
	if err == nil {
		err = os.Rename(pending, target)
	}
	if err != nil {
		_ = os.Remove(pending)
		return err
	}
	syncDir(dir)
	fmt.Fprintf(out, "Copied and checked: %s\n", target)
	return nil
}

func runSupport(args []string, out io.Writer, e env) error {
	var output string
	base, err := parse(args, out, "ftw support [--output FILE] [--url URL]", func(fs *flag.FlagSet) {
		fs.StringVar(&output, "output", "", "")
	})
	if err != nil {
		return err
	}
	if output == "" {
		output = "ftw-support-" + e.now().Format("20060102-150405") + ".zip"
	}
	c := newClient(base, e)
	ctx, cancel := context.WithTimeout(context.Background(), 2*e.requestTimeout)
	defer cancel()
	resp, err := c.open(ctx, "/api/support/dump")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		// Core streams the zip; a cut stream leaves an unreadable file.
		var r *zip.ReadCloser
		if r, err = zip.OpenReader(output); err == nil {
			_ = r.Close()
		}
	}
	if err != nil {
		_ = os.Remove(output)
		return fmt.Errorf("support file not written: %w", err)
	}
	fmt.Fprintf(out, "Support file: %s (%s)\nCore removes secrets from it; it still describes this site.\n", output, formatBytes(n))
	return nil
}

func validBackupID(id string) bool {
	return strings.HasPrefix(id, "ftw-full-backup-") && strings.HasSuffix(id, ".ftwbak") &&
		!strings.ContainsAny(id, `/\`) && !strings.Contains(id, "..")
}

func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func syncDir(path string) {
	if dir, err := os.Open(path); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
}
