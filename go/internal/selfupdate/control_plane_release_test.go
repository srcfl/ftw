package selfupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testCoreDigest    = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testUpdaterDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testRevision      = "0123456789abcdef0123456789abcdef01234567"
)

type pairReleaseFixture struct {
	manifest      controlPlaneReleaseManifest
	coreDigest    string
	updaterDigest string
}

func newPairReleaseFixture(tag string) pairReleaseFixture {
	var manifest controlPlaneReleaseManifest
	manifest.SchemaVersion = controlPlaneManifestSchema
	manifest.Release = tag
	manifest.Revision = testRevision
	manifest.Components.Core = controlPlaneReleaseComponent{Image: "ghcr.io/srcfl/ftw:" + tag, Digest: testCoreDigest}
	manifest.Components.Updater = controlPlaneReleaseComponent{Image: "ghcr.io/srcfl/ftw-updater:" + tag, Digest: testUpdaterDigest}
	return pairReleaseFixture{manifest: manifest, coreDigest: testCoreDigest, updaterDigest: testUpdaterDigest}
}

func pairReleaseServer(t *testing.T, tag string, fixture *pairReleaseFixture) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/release":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": tag, "draft": false, "prerelease": false,
				"assets": []map[string]string{{"name": "ftw-control-plane.json", "browser_download_url": srv.URL + "/asset"}},
			})
		case r.URL.Path == "/asset":
			_ = json.NewEncoder(w).Encode(fixture.manifest)
		case r.URL.Path == "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test"})
		case r.URL.Path == "/v2/srcfl/ftw/manifests/"+tag:
			if fixture.coreDigest == "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Docker-Content-Digest", fixture.coreDigest)
			_, _ = w.Write([]byte(`{"schemaVersion":2}`))
		case r.URL.Path == "/v2/srcfl/ftw-updater/manifests/"+tag:
			if fixture.updaterDigest == "" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Docker-Content-Digest", fixture.updaterDigest)
			_, _ = w.Write([]byte(`{"schemaVersion":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pairChecker(tag string, srv *httptest.Server) *Checker {
	return New(Config{
		Repo: "srcfl/ftw", Image: "srcfl/ftw", PairedImage: "srcfl/ftw-updater",
		PairManifestAsset: "ftw-control-plane.json", CurrentVersion: "v1.3.1",
		RegistryBaseURL: srv.URL, LatestReleaseURL: srv.URL + "/release",
		HTTPClient: srv.Client(),
	}, newMemStore())
}

func TestControlPlaneReleaseRequiresBothPinnedDigests(t *testing.T) {
	const tag = "v1.4.0"
	fixture := newPairReleaseFixture(tag)
	srv := pairReleaseServer(t, tag, &fixture)
	info, err := pairChecker(tag, srv).Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !info.UpdateAvailable || info.Latest != tag {
		t.Fatalf("pair release = %+v", info)
	}
}

func TestControlPlaneReleaseStaysHiddenUntilUpdaterDigestExists(t *testing.T) {
	const tag = "v1.4.0"
	fixture := newPairReleaseFixture(tag)
	fixture.updaterDigest = ""
	srv := pairReleaseServer(t, tag, &fixture)
	info, err := pairChecker(tag, srv).Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if info.UpdateAvailable || info.Latest != "" {
		t.Fatalf("partial pair must not be announced: %+v", info)
	}
}

func TestControlPlaneReleaseRejectsMovedTagDigest(t *testing.T) {
	const tag = "v1.4.0"
	fixture := newPairReleaseFixture(tag)
	fixture.coreDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	srv := pairReleaseServer(t, tag, &fixture)
	_, err := pairChecker(tag, srv).Check(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "does not match release manifest") {
		t.Fatalf("Check error = %v", err)
	}
}

func TestControlPlaneReleaseRejectsWrongReleaseIdentity(t *testing.T) {
	const tag = "v1.4.0"
	fixture := newPairReleaseFixture(tag)
	fixture.manifest.Release = "v1.4.1"
	srv := pairReleaseServer(t, tag, &fixture)
	_, err := pairChecker(tag, srv).Check(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Check error = %v", err)
	}
}
