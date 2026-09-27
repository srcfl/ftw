package nativeupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fixtureFile struct {
	name string
	data string
	mode int64
}

func writeArchive(t *testing.T, dir, version string, extra ...fixtureFile) (string, string) {
	t.Helper()
	archive := filepath.Join(dir, "ftw-linux-"+runtime.GOARCH+".tar.gz")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	files := []fixtureFile{
		{"ftw", "binary", 0o755},
		{"web/index.html", "web", 0o644},
		{"drivers/BUNDLED_SOURCE.json", "{}", 0o644},
		{"optimizer/native/bundle/manifest.json", "{}", 0o644},
		{packageVersionFile, fmt.Sprintf(`{"version":%q,"arch":%q,"state_schema":7}`, version, runtime.GOARCH), 0o644},
	}
	files = append(files, extra...)
	for _, entry := range files {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Size: int64(len(entry.data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(entry.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	checksum := archive + ".sha256"
	if err := os.WriteFile(checksum, []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), filepath.Base(archive))), 0o644); err != nil {
		t.Fatal(err)
	}
	return archive, checksum
}

func TestInstallArchiveAndSelect(t *testing.T) {
	root := t.TempDir()
	tag := "v0.131.0-beta.1"
	archive, checksum := writeArchive(t, root, tag)
	m := Manager{Root: root}
	if err := m.InstallArchive(context.Background(), tag, archive, checksum); err != nil {
		t.Fatal(err)
	}
	if err := m.Init(tag); err != nil {
		t.Fatal(err)
	}
	path, chosen, trial, err := m.Select()
	if err != nil || chosen != tag || trial || path != filepath.Join(root, "releases", tag) {
		t.Fatalf("selected %q %q %t: %v", path, chosen, trial, err)
	}
	if err := m.InstallArchive(context.Background(), tag, archive, checksum); err != nil {
		t.Fatalf("same archive must be idempotent: %v", err)
	}
}

// A receipt lost to a power cut after commit leaves the launcher unable to
// start or roll back. Publishing must wait until the receipt is on disk.
func TestInstallArchiveFlushesReceiptBeforePublishing(t *testing.T) {
	root := t.TempDir()
	tag := "v0.131.0-beta.1"
	archive, checksum := writeArchive(t, root, tag)
	synced := map[string]bool{}
	syncFile = func(f *os.File) error {
		synced[filepath.Base(f.Name())] = true
		return errors.New("flush failed")
	}
	t.Cleanup(func() { syncFile = (*os.File).Sync })
	m := Manager{Root: root}
	if err := m.InstallArchive(context.Background(), tag, archive, checksum); err == nil {
		t.Fatal("published a release whose receipt was not flushed")
	}
	if !synced[receiptFile] {
		t.Fatalf("receipt never flushed: %v", synced)
	}
	if _, err := os.Lstat(filepath.Join(root, "releases", tag)); !os.IsNotExist(err) {
		t.Fatalf("release was published: %v", err)
	}
	syncFile = (*os.File).Sync
	if err := m.InstallArchive(context.Background(), tag, archive, checksum); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReleaseDir(tag); err != nil {
		t.Fatal(err)
	}
}

func TestInstallArchiveRejectsBadInputWithoutPublishing(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		extra   []fixtureFile
		corrupt bool
	}{
		{name: "wrong version", version: "v0.131.1-beta.1"},
		{name: "path traversal", version: "v0.131.0-beta.1", extra: []fixtureFile{{"../outside", "bad", 0o644}}},
		{name: "duplicate", version: "v0.131.0-beta.1", extra: []fixtureFile{{"ftw", "other", 0o755}}},
		{name: "bad hash", version: "v0.131.0-beta.1", corrupt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			archive, checksum := writeArchive(t, root, test.version, test.extra...)
			if test.corrupt {
				if err := os.WriteFile(checksum, []byte(fmt.Sprintf("%s  %s\n", makeDigest('0'), filepath.Base(archive))), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			m := Manager{Root: root}
			if err := m.InstallArchive(context.Background(), "v0.131.0-beta.1", archive, checksum); err == nil {
				t.Fatal("bad package installed")
			}
			if _, err := os.Stat(filepath.Join(root, "releases", "v0.131.0-beta.1")); !os.IsNotExist(err) {
				t.Fatalf("release was published: %v", err)
			}
		})
	}
}

func TestDownloaderFetchesExactTagAndPublishesOnlyAfterChecksum(t *testing.T) {
	root := t.TempDir()
	tag := "v0.131.0-beta.1"
	archive, checksum := writeArchive(t, root, tag)
	archiveData, _ := os.ReadFile(archive)
	checksumData, _ := os.ReadFile(checksum)
	var requests []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.Path)
		var body []byte
		status := http.StatusOK
		switch r.URL.Path {
		case "/" + tag + "/" + filepath.Base(checksum):
			body = checksumData
		case "/" + tag + "/" + filepath.Base(archive):
			body = archiveData
		default:
			status = http.StatusNotFound
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, nil
	})}
	var copied int64
	d := Downloader{Manager: Manager{Root: root}, ReleaseBaseURL: "https://test.invalid",
		HTTPClient: client,
		Progress:   func(current, _ int64) { copied = current }}
	if err := d.Install(context.Background(), tag); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || copied != int64(len(archiveData)) {
		t.Fatalf("requests=%v copied=%d", requests, copied)
	}
	if _, err := d.Manager.ReleaseDir(tag); err != nil {
		t.Fatal(err)
	}
}

// A connection that stays open but sends nothing must end the download well
// before the update status turns stale, so a run reported failed has really
// stopped and cannot restart Core minutes later.
func TestDownloaderFailsASilentTransfer(t *testing.T) {
	old := fetchIdleTimeout
	fetchIdleTimeout = 100 * time.Millisecond
	t.Cleanup(func() { fetchIdleTimeout = old })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			// Silent before the response headers.
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		// Silent after the first bytes of the body.
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 512))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	root := t.TempDir()
	d := Downloader{Manager: Manager{Root: root}, ReleaseBaseURL: srv.URL, HTTPClient: srv.Client()}
	target := filepath.Join(root, "asset")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, asset := range []string{"ftw-linux-arm64.tar.gz.sha256", "ftw-linux-arm64.tar.gz"} {
		t.Run(asset, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := time.Now()
			err := d.fetch(ctx, "v0.131.0-beta.1", asset, target, 1<<20, nil)
			if !errors.Is(err, errFetchIdle) {
				t.Fatalf("silent transfer error = %v, want %v", err, errFetchIdle)
			}
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("silent transfer took %s to fail", elapsed)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
