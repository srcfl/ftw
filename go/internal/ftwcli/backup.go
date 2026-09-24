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

type backupList struct {
	Dir       string         `json:"dir"`
	FreeBytes int64          `json:"free_bytes"`
	Progress  backupProgress `json:"progress"`
	Backups   []backupEntry  `json:"backups"`
}

// backupPhases names Core's backup phases for a person.
var backupPhases = map[string]string{
	"waiting_for_maintenance": "Pausing history maintenance",
	"checking_sources":        "Checking the files to back up",
	"copying_database":        "Copying the database",
	"compressing_database":    "Compressing the database",
	"packing_archive":         "Packing the archive",
	"verifying_archive":       "Verifying the archive",
	"syncing_backup":          "Writing the archive to disk",
}

func (p backupProgress) sample() (sample, bool) {
	if p.Phase == "" || p.Phase == "complete" || p.Phase == "failed" {
		return sample{}, false
	}
	label, ok := backupPhases[p.Phase]
	if !ok {
		label = strings.ReplaceAll(p.Phase, "_", " ")
	}
	s := sample{key: p.Phase, label: label, detail: p.Table}
	switch {
	case p.TotalBytes > 0 || p.CompletedBytes > 0:
		s.unit, s.done, s.total = "bytes", p.CompletedBytes, p.TotalBytes
	case p.RowsDone > 0:
		s.unit, s.done = "rows", p.RowsDone
	}
	return s, true
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
	var before backupList
	if c.get(ctx, "/api/backups", &before) == nil && before.Dir != "" {
		line := before.Dir
		if before.FreeBytes > 0 {
			line += " (" + formatBytes(before.FreeBytes) + " free)"
		}
		fmt.Fprintf(out, "Backups:  %s\n", line)
		if len(before.Backups) > 0 && before.FreeBytes > 0 && before.FreeBytes < before.Backups[0].SizeBytes {
			fmt.Fprintf(out, "Warning:  less space is free than the last backup took (%s)\n", formatBytes(before.Backups[0].SizeBytes))
		}
	}
	fmt.Fprintln(out, "Making a full backup. Core keeps running; Ctrl-C stops the backup.")
	start := e.now()
	m := newMeter(out, e)

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
	dir := before.Dir
	for {
		select {
		case r := <-done:
			m.finish(r.err == nil)
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
		var list backupList
		if c.get(ctx, "/api/backups", &list) != nil {
			continue
		}
		if list.Dir != "" {
			dir = list.Dir
		}
		if s, ok := list.Progress.sample(); ok {
			m.show(s)
		}
	}
}

// byteCounter reports how much of a copy has been written.
type byteCounter struct {
	n    int64
	tick func(int64)
}

func (b *byteCounter) Write(p []byte) (int, error) {
	b.n += int64(len(p))
	b.tick(b.n)
	return len(p), nil
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
	m := newMeter(out, c.env)
	counter := &byteCounter{tick: func(n int64) {
		m.show(sample{key: "copy", label: "Copying to " + dir, unit: "bytes", done: n, total: b.SizeBytes})
	}}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, sum, counter), resp.Body)
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
	m.finish(err == nil)
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
