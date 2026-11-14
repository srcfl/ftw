package api

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
)

func agentReadRequest(path string, scopes ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	return r.WithContext(apiauth.WithCaller(r.Context(), apiauth.Caller{Kind: apiauth.KindAgent, Role: apiauth.RoleAgent, Subject: "agent:test", Scopes: apiauth.NewScopeSet(scopes...)}))
}
func diagnosticRead(t *testing.T, srv *Server, path string, read protocol.APIRead) (*httptest.ResponseRecorder, map[string]json.RawMessage) {
	t.Helper()
	answer := httptest.NewRecorder()
	srv.ServeAgentRead(answer, agentReadRequest(path, protocol.ScopeDiagRun), read)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(answer.Body.Bytes(), &doc); err != nil {
		t.Fatalf("invalid answer: %d %s", answer.Code, answer.Body.String())
	}
	return answer, doc
}
func TestAgentReadsEnergyEvidenceWithTechnicalIDs(t *testing.T) {
	st, base, asset := seedAPIEnergyHistory(t)
	t.Cleanup(func() { _ = st.Close() })
	srv := New(&Deps{State: st})
	catalog, _ := diagnosticRead(t, srv, "/api/energy/assets", protocol.APIRead{})
	if catalog.Code != 200 || !strings.Contains(catalog.Body.String(), "Site meter") || !strings.Contains(catalog.Body.String(), "maker:api-meter") {
		t.Fatalf("technical data lost: %s", catalog.Body.String())
	}
	q := url.Values{"scope": {"asset"}, "asset_id": {asset}, "since": {formatInt64(base)}, "until": {formatInt64(base + 300_000)}, "bucket": {"5m"}}
	answer, doc := diagnosticRead(t, srv, "/api/energy/history?"+q.Encode(), protocol.APIRead{})
	var body struct {
		Points []struct {
			AssetID    string  `json:"asset_id"`
			Energy     float64 `json:"energy_wh"`
			Quality    string  `json:"quality"`
			Source     string  `json:"source"`
			Provenance string  `json:"provenance"`
		} `json:"points"`
	}
	if answer.Code != 200 || json.Unmarshal(doc["body"], &body) != nil || len(body.Points) == 0 {
		t.Fatalf("ledger: %s", answer.Body.String())
	}
	var measured float64
	for _, p := range body.Points {
		if p.AssetID != asset {
			t.Fatal("id changed")
		}
		if p.Quality == "measured" {
			measured += p.Energy
			if p.Source != "hardware_counter" || p.Provenance != "counter" {
				t.Fatalf("provenance changed: %+v", p)
			}
		}
	}
	if measured != 12 {
		t.Fatalf("energy=%v, want 12 Wh", measured)
	}
	csv, doc := diagnosticRead(t, srv, "/api/energy/history.csv?"+q.Encode(), protocol.APIRead{})
	if csv.Code != 200 || !bytes.Contains(doc["body"], []byte("provenance")) || !bytes.Contains(doc["encoding"], []byte("utf-8")) {
		t.Fatalf("CSV: %s", csv.Body.String())
	}
}
func TestDiagnosticCatalogueCoversAllReadAndLocalDiagnosticViews(t *testing.T) {
	srv := New(&Deps{})
	catalog := map[string]bool{}
	for _, route := range srv.agentReadCatalog() {
		catalog[route.Path] = true
	}
	for pattern, mark := range srv.marks {
		fields := strings.Fields(pattern)
		if len(fields) != 2 || fields[0] != "GET" || mark.static {
			continue
		}
		path := fields[1]
		if mark.tier == Read && !catalog[path] {
			t.Errorf("missing Read route %s", path)
		}
		if mark.tier == Local && !catalog[path] && !strings.HasPrefix(path, "/api/oauth/") {
			t.Errorf("unclassified Local GET %s", path)
		}
	}
	for _, path := range []string{"/api/config", "/api/logs", "/api/drivers/{id}/source", "/api/support/dump", "/api/support/report", "/api/agents/audit", "/api/backups/{id}", "/api/loadpoints/{id}/battery_boost", "/api/research/load/dump"} {
		if !catalog[path] {
			t.Errorf("missing %s", path)
		}
	}
	for _, path := range []string{"/api/scan", "/api/version/check", "/api/oauth/myuplink/start", "/api/oauth/myuplink/callback"} {
		if catalog[path] {
			t.Errorf("side effect in catalogue: %s", path)
		}
		answer, _ := diagnosticRead(t, srv, path, protocol.APIRead{})
		if answer.Code != 403 {
			t.Errorf("side-effect route %s accepted: %d", path, answer.Code)
		}
	}
	srv.handle("GET /api/future/{id}", Read, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"id": r.PathValue("id"), "query": r.URL.Query().Get("new_parameter")})
	})
	answer, _ := diagnosticRead(t, srv, "/api/future/charger-192.168.1.9?new_parameter=yes", protocol.APIRead{})
	if answer.Code != 200 || !strings.Contains(answer.Body.String(), "charger-192.168.1.9") || !strings.Contains(answer.Body.String(), "yes") {
		t.Fatalf("future read lost data: %s", answer.Body.String())
	}
	for _, scope := range []string{"", protocol.ScopeHistoryRead} {
		denied := httptest.NewRecorder()
		srv.ServeAgentRead(denied, agentReadRequest("/api/future/x", scope), protocol.APIRead{})
		if denied.Code != 403 {
			t.Fatal("diagnostic grant not checked")
		}
	}
	mutation := agentReadRequest("/api/config", protocol.ScopeDiagRun)
	mutation.Method = http.MethodPost
	denied := httptest.NewRecorder()
	srv.ServeAgentRead(denied, mutation, protocol.APIRead{})
	if denied.Code != 403 {
		t.Fatal("write accepted")
	}
	t.Logf("Core exposes %d diagnostic routes", len(catalog))
}
func TestDiagnosticPagesRetainEveryByteOfLargeAnswers(t *testing.T) {
	srv := New(&Deps{})
	original := []byte(strings.Repeat("charger 192.168.1.9 9007199254740993\n", 40000) + "\x00\xff")
	current := original
	srv.handle("GET /api/test/export", Read, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(current)
	})
	sum := sha256.Sum256(original)
	digest := hex.EncodeToString(sum[:])
	var joined []byte
	var offset int64
	for {
		answer, doc := diagnosticRead(t, srv, "/api/test/export", protocol.APIRead{Offset: offset, MaxBytes: protocol.AgentAPIPageBytes, IfSHA256: digest})
		if answer.Code != 200 {
			t.Fatalf("page: %s", answer.Body.String())
		}
		var encoding, text string
		_ = json.Unmarshal(doc["encoding"], &encoding)
		_ = json.Unmarshal(doc["body"], &text)
		part := []byte(text)
		if encoding == "base64" {
			var err error
			part, err = base64.StdEncoding.DecodeString(text)
			if err != nil {
				t.Fatal(err)
			}
		}
		joined = append(joined, part...)
		var more bool
		_ = json.Unmarshal(doc["more"], &more)
		if !more {
			break
		}
		_ = json.Unmarshal(doc["next_offset"], &offset)
	}
	if !bytes.Equal(joined, original) {
		t.Fatal("paged bytes changed")
	}
	current = []byte("changed")
	answer, doc := diagnosticRead(t, srv, "/api/test/export", protocol.APIRead{Offset: 1, IfSHA256: digest})
	if answer.Code != 409 || doc["body"] != nil {
		t.Fatal("changed pages accepted")
	}
}

func TestDiagnosticStartIncludesVersionedSourceAndBugEvidence(t *testing.T) {
	srv := New(&Deps{Version: "0.151.0-beta.1"})
	answer, _ := diagnosticRead(t, srv, protocol.AgentAPICatalogPath, protocol.APIRead{})
	for _, text := range []string{"https://github.com/srcfl/ftw/tree/v0.151.0-beta.1", "https://github.com/srcfl/device-drivers", "https://github.com/srcfl/ftw-webapp-source-archive", "docs/protocol.md", "reproduction steps", "timezone", "real equipment", "/api/support/report"} {
		if !strings.Contains(answer.Body.String(), text) {
			t.Errorf("missing debugging context %q", text)
		}
	}
	// FTW Cloud stopped serving its source on 10 November 2026.
	if strings.Contains(answer.Body.String(), "cloud.ftw.energy/source") {
		t.Error("the catalog still sends agents to a Cloud source page that no longer exists")
	}
}

func TestDiagnosticArchiveMembersCanBeReadWithoutExternalTools(t *testing.T) {
	for _, kind := range []string{"zip", "tar.gz"} {
		t.Run(kind, func(t *testing.T) {
			var archive bytes.Buffer
			evidence := []byte("driver charger: 192.168.1.9; counter=9007199254740993\n")
			media := "application/zip"
			if kind == "zip" {
				writer := zip.NewWriter(&archive)
				file, err := writer.Create("ftw-support-stamp/logs/global.log")
				if err != nil {
					t.Fatal(err)
				}
				_, _ = file.Write(evidence)
				if err = writer.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				media = "application/gzip"
				gz := gzip.NewWriter(&archive)
				writer := tar.NewWriter(gz)
				if err := writer.WriteHeader(&tar.Header{Name: "ftw-research-stamp/logs/global.log", Size: int64(len(evidence)), Mode: 0600}); err != nil {
					t.Fatal(err)
				}
				_, _ = writer.Write(evidence)
				_ = writer.Close()
				_ = gz.Close()
			}
			srv := New(&Deps{})
			srv.handle("GET /api/test/archive", Read, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", media)
				_, _ = w.Write(archive.Bytes())
			})
			list, _ := diagnosticRead(t, srv, "/api/test/archive", protocol.APIRead{ArchiveMember: "."})
			if list.Code != 200 || !strings.Contains(list.Body.String(), "logs/global.log") {
				t.Fatalf("archive list: %s", list.Body.String())
			}
			answer, doc := diagnosticRead(t, srv, "/api/test/archive", protocol.APIRead{ArchiveMember: "logs/global.log"})
			var text string
			_ = json.Unmarshal(doc["body"], &text)
			if answer.Code != 200 || text != string(evidence) {
				t.Fatalf("archive evidence changed: %s", answer.Body.String())
			}
			absent, _ := diagnosticRead(t, srv, "/api/test/archive", protocol.APIRead{ArchiveMember: "../not-a-file"})
			if absent.Code != 404 {
				t.Fatal("archive member lookup escaped its file list")
			}
		})
	}
}

func TestDiagnosticArchiveCorruptionReturnsNoPartialMember(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	file, err := writer.CreateHeader(&zip.FileHeader{Name: "root/evidence.txt", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	evidence := []byte("original evidence must not leave as a successful partial read")
	_, _ = file.Write(evidence)
	_ = writer.Close()
	corrupt := append([]byte(nil), archive.Bytes()...)
	at := bytes.Index(corrupt, evidence)
	if at < 0 {
		t.Fatal("test member not found")
	}
	corrupt[at] ^= 0xff
	srv := New(&Deps{})
	srv.handle("GET /api/test/corrupt", Read, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(corrupt)
	})
	answer, doc := diagnosticRead(t, srv, "/api/test/corrupt", protocol.APIRead{ArchiveMember: "evidence.txt"})
	if answer.Code != 502 || doc["body"] != nil {
		t.Fatalf("corrupt member returned data: %s", answer.Body.String())
	}
}
