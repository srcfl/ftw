package nativeupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const defaultReleaseBaseURL = "https://github.com/srcfl/ftw/releases/download"

type Downloader struct {
	Manager        Manager
	ReleaseBaseURL string
	HTTPClient     *http.Client
	Progress       func(copied, total int64)
	// Downloaded runs once the archive is on disk, before it is verified
	// and unpacked.
	Downloaded func()
}

// Install downloads one published tag's archive and checksum. The caller
// stages the result separately with Manager.Prepare after its data snapshot.
func (d Downloader) Install(ctx context.Context, tag string) error {
	if !ValidTag(tag) {
		return fmt.Errorf("invalid release tag %q", tag)
	}
	arch := runtime.GOARCH
	if arch != "arm64" && arch != "amd64" {
		return fmt.Errorf("unsupported host architecture %s", arch)
	}
	name := "ftw-linux-" + arch + ".tar.gz"
	dir := filepath.Join(d.Manager.Root, ".downloads")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	archive, err := os.CreateTemp(dir, ".archive-*")
	if err != nil {
		return err
	}
	archivePath := archive.Name()
	archive.Close()
	defer os.Remove(archivePath)
	checksum, err := os.CreateTemp(dir, ".checksum-*")
	if err != nil {
		return err
	}
	checksumPath := checksum.Name()
	checksum.Close()
	defer os.Remove(checksumPath)
	if err := d.fetch(ctx, tag, name+".sha256", checksumPath, 512, nil); err != nil {
		return err
	}
	if err := d.fetch(ctx, tag, name, archivePath, maxArchiveBytes, d.Progress); err != nil {
		return err
	}
	if d.Downloaded != nil {
		d.Downloaded()
	}
	return d.Manager.InstallArchive(ctx, tag, archivePath, checksumPath)
}

func (d Downloader) fetch(ctx context.Context, tag, asset, target string, limit int64, progress func(int64, int64)) error {
	base := d.ReleaseBaseURL
	if base == "" {
		base = defaultReleaseBaseURL
	}
	url := strings.TrimRight(base, "/") + "/" + tag + "/" + asset
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := d.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("release asset %s: HTTP %d", asset, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("release asset %s exceeds size limit", asset)
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	var writer io.Writer = f
	if progress != nil {
		writer = &progressWriter{writer: f, total: resp.ContentLength, publish: progress}
	}
	n, copyErr := io.Copy(writer, io.LimitReader(resp.Body, limit+1))
	if copyErr == nil && n > limit {
		copyErr = errors.New("release asset exceeds size limit")
	}
	if copyErr == nil {
		copyErr = f.Sync()
	}
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

type progressWriter struct {
	writer  io.Writer
	total   int64
	copied  int64
	publish func(int64, int64)
}

func (w *progressWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.copied += int64(n)
	w.publish(w.copied, w.total)
	return n, err
}
