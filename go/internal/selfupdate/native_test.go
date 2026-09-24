package selfupdate

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
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/nativeupdate"
)

type nativeRoundTrip func(*http.Request) (*http.Response, error)

func (f nativeRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func nativeReleaseClient(t *testing.T, body string) *http.Client {
	t.Helper()
	return &http.Client{Transport: nativeRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://test.invalid/releases" {
			t.Fatalf("unexpected request: %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
}

func TestNativeCheckerSelectsOnlyPublished0xWithBothAssets(t *testing.T) {
	archive := "ftw-linux-" + runtime.GOARCH + ".tar.gz"
	client := nativeReleaseClient(t, `[
      {"tag_name":"v3.9.0-beta.1","prerelease":true},
      {"tag_name":"v0.131.0-beta.2","prerelease":true,"body":"<!-- ftw-state-schema-v2:7 -->",
       "assets":[{"name":"`+archive+`"},{"name":"`+archive+`.sha256"}]},
      {"tag_name":"v0.130.4","prerelease":false}
    ]`)
	c := New(Config{CurrentVersion: "v0.131.0-beta.1", CurrentStateSchema: 7,
		NativeRoot: t.TempDir(), HTTPClient: client, ReleasesURL: "https://test.invalid/releases"}, newMemStore())
	info, err := c.Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if info.Latest != "v0.131.0-beta.2" || !info.UpdateAvailable || info.FullBackupRequired {
		t.Fatalf("native release selected incorrectly: %+v", info)
	}
	if err := c.SetChannel(ChannelStable); err != nil {
		t.Fatal(err)
	}
	info, err = c.Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if info.UpdateAvailable {
		t.Fatalf("stable offered a downgrade: %+v", info)
	}
}

func TestNativeCheckerWaitsForChecksumAsset(t *testing.T) {
	archive := "ftw-linux-" + runtime.GOARCH + ".tar.gz"
	client := nativeReleaseClient(t, `[{"tag_name":"v0.131.0-beta.2","prerelease":true,
      "assets":[{"name":"`+archive+`"}]}]`)
	c := New(Config{CurrentVersion: "v0.131.0-beta.1", NativeRoot: t.TempDir(),
		HTTPClient: client, ReleasesURL: "https://test.invalid/releases"}, nil)
	info, err := c.Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if info.UpdateAvailable || info.Latest != "" {
		t.Fatalf("incomplete release offered: %+v", info)
	}
}

func TestNativeFailedCheckRetainsRuntimeCapabilities(t *testing.T) {
	root := t.TempDir()
	previous, current := "v0.131.0-beta.1", "v0.131.0-beta.2"
	nativeCurrentSlot(t, root, previous)
	nativeCurrentSlot(t, root, current)
	manager := nativeupdate.Manager{Root: root}
	if err := manager.Init(previous); err != nil {
		t.Fatal(err)
	}
	if err := manager.Prepare(current); err != nil {
		t.Fatal(err)
	}
	if _, selected, trial, err := manager.Select(); err != nil || selected != current || !trial {
		t.Fatalf("trial selection: %q %t %v", selected, trial, err)
	}
	if err := manager.Commit(current); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: nativeRoundTrip(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found"))}, nil
	})}
	c := New(Config{CurrentVersion: current, NativeRoot: root, HTTPClient: client,
		ReleasesURL: "https://test.invalid/releases"}, newMemStore())
	info, err := c.Check(context.Background(), true)
	if err == nil || !info.Native || !info.SidecarReady || info.Previous != previous {
		t.Fatalf("failed check lost native capabilities: info=%+v err=%v", info, err)
	}
	if cached := c.info; !cached.Native || !cached.SidecarReady || cached.Previous != previous {
		t.Fatalf("failed check did not retain capabilities in cache: %+v", cached)
	}
}

func TestDocker3xCheckerKeepsItsReleaseLine(t *testing.T) {
	client := nativeReleaseClient(t, `[
      {"tag_name":"v0.131.0","prerelease":false},
      {"tag_name":"v0.131.0-beta.2","prerelease":true},
      {"tag_name":"v3.8.1-beta.1","prerelease":true},
      {"tag_name":"v3.8.0","prerelease":false},
      {"tag_name":"v2.3.2","prerelease":false}
    ]`)
	c := New(Config{CurrentVersion: "v3.8.0-beta.1", HTTPClient: client,
		ReleasesURL: "https://test.invalid/releases"}, newMemStore())
	for _, tc := range []struct {
		channel Channel
		want    string
	}{
		{ChannelBeta, "v3.8.1-beta.1"},
		{ChannelStable, "v3.8.0"},
	} {
		release, deployable, err := c.resolveChannel(context.Background(), tc.channel, "v3.8.0-beta.1")
		if err != nil {
			t.Fatal(err)
		}
		if !deployable || release.TagName != tc.want {
			t.Fatalf("%s selected %q, want %q", tc.channel, release.TagName, tc.want)
		}
	}
}

func TestLegacyDockerDoesNotOffer3xOnEitherChannel(t *testing.T) {
	client := &http.Client{Transport: nativeRoundTrip(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.Path {
		case "/latest":
			body = `{"tag_name":"v3.8.0","prerelease":false}`
		case "/releases":
			body = `[{"tag_name":"v3.8.0-beta.1","prerelease":true}]`
		default:
			t.Fatalf("unexpected request: %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	for _, current := range []string{"v1.9.1-beta.1", "v2.3.2-beta.1", "v2.3.2"} {
		c := New(Config{CurrentVersion: current, HTTPClient: client,
			LatestReleaseURL: "https://test.invalid/latest", ReleasesURL: "https://test.invalid/releases"}, newMemStore())
		for _, channel := range []Channel{ChannelBeta, ChannelStable} {
			if err := c.SetChannel(channel); err != nil {
				t.Fatal(err)
			}
			info, err := c.Check(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			if info.UpdateAvailable || info.Latest != "" {
				t.Fatalf("%s %s offered a cross-line release: %+v", current, channel, info)
			}
		}
	}
}

func TestLegacyDockerRejectsDirect3xUpdate(t *testing.T) {
	c := New(Config{CurrentVersion: "v2.3.2", SocketPath: "/missing-updater"}, newMemStore())
	err := c.TriggerComponentAt(context.Background(), "update", "v3.8.0-beta.1", "core", time.Time{})
	if err == nil || !strings.Contains(err.Error(), "guided migration installer") {
		t.Fatalf("cross-line update was not blocked: %v", err)
	}
	if legacyCoreReleaseLocked("v2.3.2", "v2.3.3") {
		t.Fatal("same-line maintenance release was blocked")
	}
	if legacyCoreReleaseLocked("v0.131.0", "v0.131.1") {
		t.Fatal("native release was blocked as legacy Docker")
	}
}

func nativeArchive(t *testing.T, tag string) ([]byte, []byte) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{
		"ftw": "binary", "web/index.html": "web",
		"drivers/BUNDLED_SOURCE.json":           "{}",
		"optimizer/native/bundle/manifest.json": "{}",
		"release-version.json":                  fmt.Sprintf(`{"version":%q,"arch":%q,"state_schema":7}`, tag, runtime.GOARCH),
	}
	for name, body := range files {
		mode := int64(0o644)
		if name == "ftw" {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
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
	name := "ftw-linux-" + runtime.GOARCH + ".tar.gz"
	return buf.Bytes(), []byte(fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name))
}

func nativeCurrentSlot(t *testing.T, root, tag string) {
	t.Helper()
	dir := filepath.Join(root, "releases", tag)
	for _, name := range []string{"ftw", "web/index.html", "drivers/BUNDLED_SOURCE.json", "optimizer/native/bundle/manifest.json"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("test"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	receipt := fmt.Sprintf(`{"tag":%q,"arch":%q,"archive_sha256":%q,"state_schema":7}`, tag, runtime.GOARCH, strings.Repeat("a", 64))
	if err := os.WriteFile(filepath.Join(dir, ".ftw-release.json"), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeTriggerStagesThenRequestsRestart(t *testing.T) {
	root := t.TempDir()
	current, next := "v0.131.0-beta.1", "v0.131.0-beta.2"
	nativeCurrentSlot(t, root, current)
	manager := nativeupdate.Manager{Root: root}
	if err := manager.Init(current); err != nil {
		t.Fatal(err)
	}
	archive, checksum := nativeArchive(t, next)
	asset := "ftw-linux-" + runtime.GOARCH + ".tar.gz"
	client := &http.Client{Transport: nativeRoundTrip(func(req *http.Request) (*http.Response, error) {
		var body []byte
		switch req.URL.Path {
		case "/releases":
			body = []byte(fmt.Sprintf(`[{"tag_name":%q,"prerelease":true,"body":"<!-- ftw-state-schema-v2:7 -->","assets":[{"name":%q},{"name":%q}]}]`, next, asset, asset+".sha256"))
		case "/" + next + "/" + asset:
			body = archive
		case "/" + next + "/" + asset + ".sha256":
			body = checksum
		default:
			t.Fatalf("unexpected native request %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}, nil
	})}
	restarts := 0
	c := New(Config{CurrentVersion: current, CurrentStateSchema: 7, NativeRoot: root,
		NativeRestart: func() error { restarts++; return nil }, NativeReleaseURL: "https://test.invalid",
		StatusPath: filepath.Join(root, "update-status.json"), HTTPClient: client,
		ReleasesURL: "https://test.invalid/releases"}, newMemStore())
	if _, err := c.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := c.TriggerComponentAt(context.Background(), "update", next, "core", time.Now()); err != nil {
		t.Fatal(err)
	}
	state, err := manager.Read()
	if err != nil || state.Current != current || state.Next != next || restarts != 1 || c.Status().State != "restarting" {
		t.Fatalf("native update state=%+v err=%v restarts=%d status=%+v", state, err, restarts, c.Status())
	}
	if st := c.Status(); st.Step != NativeUpdateSteps || st.TotalSteps != NativeUpdateSteps {
		t.Fatalf("native update reports step %d/%d; it has no rollback point step", st.Step, st.TotalSteps)
	}
	if phases := c.Status().Phases; len(phases) != 2 || phases[0].Step != 1 || phases[0].Bytes != int64(len(archive)) ||
		phases[1].Step != 2 || phases[1].Message != "Unpacking and checking the release" ||
		phases[1].FinishedAt.Before(phases[1].StartedAt) || phases[1].StartedAt.Before(phases[0].FinishedAt) {
		t.Fatalf("finished phases %+v", phases)
	}
	if info := c.Info(); info.InstallRoot != root || info.InstallFreeBytes <= 0 || info.InstallNeedBytes <= 0 {
		t.Fatalf("install space not reported: %+v", info)
	}
	c.cfg.NativeRestart = func() error { return errors.New("restart refused") }
	if err := manager.CancelPrepared(next); err != nil {
		t.Fatal(err)
	}
	if err := c.TriggerComponentAt(context.Background(), "update", next, "core", time.Now()); err == nil {
		t.Fatal("failed restart reported success")
	}
	state, err = manager.Read()
	if err != nil || state.Next != "" || state.LastFailed != next {
		t.Fatalf("failed restart left a pending trial: %+v %v", state, err)
	}
}
