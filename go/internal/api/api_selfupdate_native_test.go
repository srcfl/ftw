package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/srcfl/ftw/go/internal/nativeupdate"
	"github.com/srcfl/ftw/go/internal/selfupdate"
)

type nativeReleaseTransport func(*http.Request) (*http.Response, error)

func (f nativeReleaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func nativeTestArchive(t *testing.T, tag string) ([]byte, []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"ftw": "binary", "web/index.html": "web", "drivers/BUNDLED_SOURCE.json": "{}",
		"optimizer/native/bundle/manifest.json": "{}",
		"release-version.json":                  fmt.Sprintf(`{"version":%q,"arch":%q,"state_schema":7}`, tag, runtime.GOARCH),
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), []byte(fmt.Sprintf("%s  ftw-linux-%s.tar.gz\n", hex.EncodeToString(sum[:]), runtime.GOARCH))
}

func TestNativeUpdateTakesNoRollbackPoint(t *testing.T) {
	root := t.TempDir()
	current, next := "v0.131.0-beta.1", "v0.131.0-beta.2"
	dir := filepath.Join(root, "releases", current)
	for _, name := range []string{"ftw", "web/index.html", "drivers/BUNDLED_SOURCE.json", "optimizer/native/bundle/manifest.json"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("test"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	receipt := fmt.Sprintf(`{"tag":%q,"arch":%q,"archive_sha256":%q,"state_schema":7}`, current, runtime.GOARCH, strings.Repeat("a", 64))
	if err := os.WriteFile(filepath.Join(dir, ".ftw-release.json"), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (nativeupdate.Manager{Root: root}).Init(current); err != nil {
		t.Fatal(err)
	}
	archive, checksum := nativeTestArchive(t, next)
	asset := "ftw-linux-" + runtime.GOARCH + ".tar.gz"
	client := &http.Client{Transport: nativeReleaseTransport(func(r *http.Request) (*http.Response, error) {
		var body []byte
		switch r.URL.Path {
		case "/releases":
			body = []byte(fmt.Sprintf(`[{"tag_name":%q,"prerelease":true,"body":"<!-- ftw-state-schema-v2:7 -->","assets":[{"name":%q},{"name":%q}]}]`, next, asset, asset+".sha256"))
		case "/" + next + "/" + asset:
			body = archive
		case "/" + next + "/" + asset + ".sha256":
			body = checksum
		default:
			return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, nil
	})}
	restarted := make(chan struct{}, 1)
	checker := selfupdate.New(selfupdate.Config{
		CurrentVersion: current, CurrentStateSchema: 7, NativeRoot: root,
		StatusPath: filepath.Join(root, "update-status.json"), HTTPClient: client,
		NativeReleaseURL: "https://test.invalid", ReleasesURL: "https://test.invalid/releases",
		NativeRestart: func() error { restarted <- struct{}{}; return nil },
	}, newMemStore())
	if _, err := checker.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	snapshotDir := filepath.Join(root, "snapshots")
	srv := New(&Deps{SelfUpdate: checker, SnapshotDir: snapshotDir})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/version/update", nil))
	if rr.Code != http.StatusAccepted || strings.Contains(rr.Body.String(), "snapshot") {
		t.Fatalf("native update start: %d %s", rr.Code, rr.Body.String())
	}
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatalf("native update did not reach restart: %+v", checker.Status())
	}
	if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
		t.Fatalf("native update took a rollback point: %v", err)
	}
	if st := checker.Status(); st.State != "restarting" || st.Step != 3 || st.TotalSteps != 3 {
		t.Fatalf("native update status %+v", st)
	}

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/version/snapshots", nil))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "ftw backup") {
		t.Fatalf("native manual rollback point: %d %s", rr.Code, rr.Body.String())
	}
}
