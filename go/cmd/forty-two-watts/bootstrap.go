package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/drivers"
	"github.com/frahlg/forty-two-watts/go/internal/evcloud"
	"github.com/frahlg/forty-two-watts/go/internal/scanner"
	"github.com/frahlg/forty-two-watts/go/internal/selfupdate"
)

// runBootstrap starts a minimal HTTP server that serves the setup wizard.
// It is called when config.Load fails (no config.yaml yet). The wizard
// collects initial configuration, writes it to disk, and restarts the
// process so the normal startup path takes over.
func runBootstrap(configPath, webDir, driverDir string) {
	slog.Info("no config found — starting setup wizard", "url", "http://localhost:8080/setup")

	// Self-update checker runs here too — <ftw-update-check> on the setup
	// welcome step is useless without /api/version/check, and the whole
	// point of that banner is to let the operator pull-and-refresh BEFORE
	// committing config on an outdated release. Store is nil because there
	// is no SQLite yet; skip persistence falls back to no-op which is fine
	// for one-shot setup (dismiss is session-only anyway).
	var selfUpdater *selfupdate.Checker
	// See main.go for the rationale; mirror the dev-implicit-enable so
	// the setup wizard also reports "Update available" when the operator
	// is running an old dev binary against a new release.
	if envBool("FTW_SELFUPDATE_ENABLED") || Version == "dev" {
		current := Version
		if v, ok := os.LookupEnv("FTW_SELFUPDATE_CURRENT_VERSION"); ok && v != "" {
			current = v
			slog.Warn("selfupdate: CurrentVersion overridden for testing",
				"real_version", Version, "reported_version", current,
				"env", "FTW_SELFUPDATE_CURRENT_VERSION")
		}
		selfUpdater = selfupdate.New(selfupdate.Config{
			CurrentVersion: current,
			SocketPath:     envOr("FTW_UPDATER_SOCKET", "/run/ftw-update/sock"),
			StatusPath:     envOr("FTW_UPDATER_STATUS", "/run/ftw-update/state.json"),
		}, nil)
		selfUpdater.Start(context.Background())
		slog.Info("selfupdate enabled in bootstrap", "socket", envOr("FTW_UPDATER_SOCKET", "/run/ftw-update/sock"))
	}

	mux := http.NewServeMux()

	// GET / → redirect to setup wizard
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		// Serve static files from web dir.
		serveStatic(w, r, webDir)
	})

	// GET /setup → serve setup.html
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(webDir, "setup.html")
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		http.ServeFile(w, r, path)
	})

	// GET /api/drivers/catalog → scan Lua drivers
	mux.HandleFunc("GET /api/drivers/catalog", func(w http.ResponseWriter, r *http.Request) {
		entries, err := drivers.LoadCatalog(driverDir)
		if err != nil {
			writeBootstrapJSON(w, 200, map[string]any{
				"path":    driverDir,
				"entries": []any{},
				"error":   err.Error(),
			})
			return
		}
		writeBootstrapJSON(w, 200, map[string]any{
			"path":    driverDir,
			"entries": entries,
		})
	})

	// GET /api/scan → network scanner
	mux.HandleFunc("GET /api/scan", func(w http.ResponseWriter, r *http.Request) {
		devices, err := scanner.Scan(r.Context())
		if err != nil {
			writeBootstrapJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeBootstrapJSON(w, 200, devices)
	})

	// Self-update routes — mirror the subset of /api/version/* that the
	// setup-wizard's <ftw-update-check> component needs: check, update,
	// status. Skip/unskip/restart aren't needed from the wizard (dismiss
	// is session-only; Restart is a dashboard affordance) so we don't
	// register them — POST hits land on the default 404 path.
	mux.HandleFunc("GET /api/version/check", func(w http.ResponseWriter, r *http.Request) {
		if selfUpdater == nil {
			writeBootstrapJSON(w, 503, map[string]string{"error": "self-update disabled"})
			return
		}
		if r.URL.Query().Get("force") == "1" {
			info, err := selfUpdater.Check(r.Context(), true)
			if err != nil {
				info.Err = err.Error()
				writeBootstrapJSON(w, 502, info)
				return
			}
		}
		writeBootstrapJSON(w, 200, selfUpdater.Info())
	})

	mux.HandleFunc("POST /api/version/update", func(w http.ResponseWriter, r *http.Request) {
		if selfUpdater == nil {
			writeBootstrapJSON(w, 503, map[string]string{"error": "self-update disabled"})
			return
		}
		info := selfUpdater.Info()
		if err := selfUpdater.Trigger(r.Context(), "update", info.Latest); err != nil {
			writeBootstrapJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		writeBootstrapJSON(w, 202, map[string]any{"status": "started", "action": "update", "target": info.Latest})
	})

	mux.HandleFunc("GET /api/version/update/status", func(w http.ResponseWriter, r *http.Request) {
		if selfUpdater == nil {
			writeBootstrapJSON(w, 503, map[string]string{"error": "self-update disabled"})
			return
		}
		writeBootstrapJSON(w, 200, selfUpdater.Status())
	})

	// GET /api/ev/providers — descriptor list for the wizard's field
	// renderer. Mirrors the full-app endpoint.
	mux.HandleFunc("GET /api/ev/providers", func(w http.ResponseWriter, r *http.Request) {
		writeBootstrapJSON(w, 200, evcloud.Describe())
	})

	// POST /api/ev/chargers — probe a provider for the chargers reachable
	// from the supplied config. Mirror of the full-app handler so the
	// setup wizard can offer a picker instead of asking the operator to
	// transcribe a serial. No state store here, so password (when
	// required) comes only from the body — no fallback to the persisted
	// ev_charger_password.
	mux.HandleFunc("POST /api/ev/chargers", func(w http.ResponseWriter, r *http.Request) {
		var cfg config.EVCharger
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&cfg); err != nil {
			writeBootstrapJSON(w, 400, map[string]string{"error": "invalid request"})
			return
		}
		cfg.Normalize()
		if cfg.Provider == "" {
			cfg.Provider = "easee"
		}
		p, err := evcloud.Get(cfg.Provider)
		if err != nil {
			writeBootstrapJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		desc := p.Describe()
		if err := cfg.Validate(); err != nil {
			writeBootstrapJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if desc.NeedsAuth && cfg.Password == "" {
			writeBootstrapJSON(w, 400, map[string]string{"error": "password required"})
			return
		}
		chargers, err := p.ListChargers(&cfg)
		if err != nil {
			writeBootstrapJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		writeBootstrapJSON(w, 200, chargers)
	})

	// POST /api/config → validate, write, restart
	mux.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body.Close()
		if err != nil {
			writeBootstrapJSON(w, 400, map[string]string{"error": "read body: " + err.Error()})
			return
		}

		var cfg config.Config
		if err := json.Unmarshal(body, &cfg); err != nil {
			writeBootstrapJSON(w, 400, map[string]string{"error": "invalid json: " + err.Error()})
			return
		}

		if err := cfg.Validate(); err != nil {
			writeBootstrapJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}

		if err := config.SaveAtomic(configPath, &cfg); err != nil {
			writeBootstrapJSON(w, 500, map[string]string{"error": "write config: " + err.Error()})
			return
		}

		slog.Info("config written by setup wizard — restarting", "path", configPath)
		writeBootstrapJSON(w, 200, map[string]string{"status": "ok", "restart": "true"})

		// Restart the process so the normal startup path picks up the new config.
		go func() {
			exe, err := os.Executable()
			if err != nil {
				exe = os.Args[0]
			}
			_ = syscall.Exec(exe, os.Args, os.Environ())
		}()
	})

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("bootstrap server", "err", err)
		os.Exit(1)
	}
}

// serveStatic serves files from webDir with path-traversal protection.
func serveStatic(w http.ResponseWriter, r *http.Request, webDir string) {
	clean := filepath.Clean(filepath.Join(webDir, r.URL.Path))
	absWeb, _ := filepath.Abs(webDir)
	absPath, _ := filepath.Abs(clean)
	if !strings.HasPrefix(absPath, absWeb+string(filepath.Separator)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, clean)
}

func writeBootstrapJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
