package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/evcloud"
	"github.com/srcfl/ftw/go/internal/prices"
	"github.com/srcfl/ftw/go/internal/scanner"
)

// runBootstrap serves the setup wizard when config.yaml does not exist yet.
func runBootstrap(configPath, webDir, driverDir string) {
	slog.Info("no config found — starting setup wizard", "url", "http://localhost:8080/setup")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		serveStatic(w, r, webDir)
	})
	mux.HandleFunc("GET /setup", func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(webDir, "setup.html")
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		http.ServeFile(w, r, path)
	})
	mux.HandleFunc("GET /api/prices/zones", bootstrapPriceZones)
	mux.HandleFunc("GET /api/drivers/catalog", func(w http.ResponseWriter, _ *http.Request) {
		entries, err := drivers.LoadCatalogMulti(config.UserDriversDirOverride, driverDir)
		if err != nil {
			writeBootstrapJSON(w, http.StatusOK, map[string]any{
				"path": driverDir, "entries": []any{}, "error": err.Error(),
			})
			return
		}
		writeBootstrapJSON(w, http.StatusOK, map[string]any{"path": driverDir, "entries": entries})
	})
	mux.HandleFunc("GET /api/scan", func(w http.ResponseWriter, r *http.Request) {
		devices, err := scanner.Scan(r.Context())
		if err != nil {
			writeBootstrapJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeBootstrapJSON(w, http.StatusOK, devices)
	})
	mux.HandleFunc("GET /api/ev/providers", func(w http.ResponseWriter, _ *http.Request) {
		writeBootstrapJSON(w, http.StatusOK, evcloud.Describe())
	})
	mux.HandleFunc("POST /api/ev/chargers", func(w http.ResponseWriter, r *http.Request) {
		var cfg config.EVCharger
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&cfg); err != nil {
			writeBootstrapJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		cfg.Normalize()
		if cfg.Provider == "" {
			cfg.Provider = "easee"
		}
		p, err := evcloud.Get(cfg.Provider)
		if err != nil {
			writeBootstrapJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		desc := p.Describe()
		if err := cfg.Validate(); err != nil {
			writeBootstrapJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if desc.NeedsAuth && cfg.Password == "" {
			writeBootstrapJSON(w, http.StatusBadRequest, map[string]string{"error": "password required"})
			return
		}
		chargers, err := p.ListChargers(&cfg)
		if err != nil {
			writeBootstrapJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeBootstrapJSON(w, http.StatusOK, chargers)
	})
	mux.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body.Close()
		if err != nil {
			writeBootstrapJSON(w, http.StatusBadRequest, map[string]string{"error": "read body: " + err.Error()})
			return
		}
		var cfg config.Config
		if err := json.Unmarshal(body, &cfg); err != nil {
			writeBootstrapJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
			return
		}
		if err := cfg.Validate(); err != nil {
			writeBootstrapJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if err := config.SaveAtomic(configPath, &cfg); err != nil {
			writeBootstrapJSON(w, http.StatusInternalServerError, map[string]string{"error": "write config: " + err.Error()})
			return
		}
		slog.Info("config written by setup wizard — restarting", "path", configPath)
		writeBootstrapJSON(w, http.StatusOK, map[string]string{"status": "ok", "restart": "true"})
		go func() {
			if err := execCurrentProcess(nil); err != nil {
				slog.Error("bootstrap re-exec failed", "err", err)
			}
		}()
	})

	srv := newHTTPServer(":8080", secureBootstrapMutations(mux))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("bootstrap server", "err", err)
		os.Exit(1)
	}
}

// Bootstrap needs the same country/zone choices before any config exists.
func bootstrapPriceZones(w http.ResponseWriter, _ *http.Request) {
	zones := prices.Zones()
	items := make([]map[string]string, 0, len(zones))
	for _, z := range zones {
		items = append(items, map[string]string{
			"code": z.Code, "country": z.Country, "currency": z.Currency, "name": z.Name(),
		})
	}
	writeBootstrapJSON(w, http.StatusOK, map[string]any{"zones": items})
}

func secureBootstrapMutations(next http.Handler) http.Handler {
	return api.WithSecurityHeaders(api.Authenticate(next, apiMutationPolicy()))
}

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
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
