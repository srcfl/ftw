package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/updateipc"
)

func TestUpdaterPreflightBeforePersistentState(t *testing.T) {
	if path := os.Getenv("FTW_PREFLIGHT_TEST_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("ftw", flag.ExitOnError)
		os.Args = []string{"ftw", "-config", path}
		main()
		return
	}
	for _, mode := range []string{"old", "fixed", "native"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "ftw-preflight-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "sock")
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "fixed" {
					updateipc.ServeCapabilities(w, r)
					return
				}
				http.NotFound(w, r)
			})}
			go srv.Serve(ln)
			defer srv.Close()
			data := t.TempDir()
			path := filepath.Join(data, "config.yaml")
			original := []byte("invalid: [yaml\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUpdaterPreflightBeforePersistentState$")
			enabled := "1"
			if mode == "native" {
				enabled = "0"
			}
			cmd.Env = append(os.Environ(), "FTW_PREFLIGHT_TEST_CONFIG="+path, "FTW_UPDATER_SOCKET="+socket, "FTW_SELFUPDATE_ENABLED="+enabled)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("unexpected startup success: %s", out)
			}
			guarded := strings.Contains(string(out), "update ftw-updater first")
			if guarded != (mode == "old") {
				t.Fatalf("wrong startup path: %s", out)
			}
			if mode != "old" && !strings.Contains(string(out), "load config") {
				t.Fatalf("did not pass preflight: %s", out)
			}
			files, err := os.ReadDir(data)
			if err != nil || len(files) != 1 {
				t.Fatalf("startup changed data: %v %v", files, err)
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != string(original) {
				t.Fatalf("startup changed config: %q %v", got, err)
			}
		})
	}
}
