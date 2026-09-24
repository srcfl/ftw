package updatecli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpdateRefusesTheDockerUpdater(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/version/check" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"current":"v3.8.0-beta.1","native":false,"channel":"beta"}`))
	}))
	defer srv.Close()
	err := Update(t.Context(), Options{URL: srv.URL, Client: srv.Client(), Sleep: func(time.Duration) {}}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "does not start the Docker updater") {
		t.Fatal(err)
	}
}

func TestUpdatePrintsPhasesAndStopsWhenCurrent(t *testing.T) {
	var posts []string
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/version/channel":
			posts = append(posts, r.URL.Path)
			_, _ = w.Write([]byte(`{}`))
		case "/api/version/check":
			if step == 0 {
				_, _ = w.Write([]byte(`{"current":"v0.132.0-beta.1","native":true,"channel":"beta","latest":"v0.132.2-beta.1","update_available":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"current":"v0.132.2-beta.1","native":true,"channel":"beta"}`))
		case "/api/version/update":
			posts = append(posts, r.URL.Path)
			_, _ = w.Write([]byte(`{"target":"v0.132.2-beta.1"}`))
		case "/api/version/update/status":
			step++
			if step == 1 {
				_, _ = w.Write([]byte(`{"state":"pulling","message":"downloading","step":2,"total_steps":4,"progress_current":10,"progress_unit":"bytes"}`))
				return
			}
			_, _ = w.Write([]byte(`{"state":"done","message":"ready"}`))
		case "/api/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	var out strings.Builder
	err := Update(t.Context(), Options{
		URL: srv.URL, Channel: "beta", Client: srv.Client(),
		Now: func() time.Time { return time.Unix(0, 0) }, Sleep: func(time.Duration) {},
	}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(posts, ",") != "/api/version/channel,/api/version/update" {
		t.Fatalf("posts %v", posts)
	}
	text := out.String()
	if !strings.Contains(text, "Updating v0.132.0-beta.1 -> v0.132.2-beta.1") || !strings.Contains(text, "pulling") || !strings.Contains(text, "total unknown") || !strings.Contains(text, "Update complete: v0.132.2-beta.1") {
		t.Fatalf("output:\n%s", text)
	}
}

func TestHelpAndStartupAndChannelPrompt(t *testing.T) {
	var out strings.Builder
	if err := Run(nil, &out, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"ftw update", "ftw backup", "ftw doctor", "ftw support", "ftw startup", "--channel", "--port"} {
		if !strings.Contains(out.String(), part) {
			t.Fatalf("help missing %s\n%s", part, out.String())
		}
	}
	out.Reset()
	if err := Run([]string{"startup", "--port", "9090", "--root", "/opt/ftw-native"}, &out, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	script := out.String()
	for _, part := range []string{"sudo tee /etc/systemd/system/ftw.service", "ftw-launcher", "/opt/ftw-native", "run -port 9090", "systemctl enable --now"} {
		if !strings.Contains(script, part) {
			t.Fatalf("startup missing %s\n%s", part, script)
		}
	}
	chosen, err := promptChannel(strings.NewReader("\n"), &strings.Builder{}, "beta")
	if err != nil || chosen != "beta" {
		t.Fatalf("empty prompt = %s %v", chosen, err)
	}
	chosen, err = promptChannel(strings.NewReader("stable\n"), &strings.Builder{}, "beta")
	if err != nil || chosen != "stable" {
		t.Fatalf("stable prompt = %s %v", chosen, err)
	}
}
