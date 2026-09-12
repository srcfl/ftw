// sim-ocpp: OCPP 1.6J / 2.0.1 charge-point simulator for Evify's in-stock
// home chargers. It dials FTW's built-in Central System the same way the
// hardware does: BootNotification, status, meter values, charging profiles.
//
//	go run ./cmd/sim-ocpp -list
//	go run ./cmd/sim-ocpp -all -plug
//	go run ./cmd/sim-ocpp -model charge-amps-aura -plug
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/srcfl/ftw/go/cmd/sim-ocpp/ocppcp"
)

func main() {
	cs := flag.String("cs", "ws://127.0.0.1:8887", "OCPP 1.6J Central System URL")
	cs201 := flag.String("cs-v201", "ws://127.0.0.1:8888", "OCPP 2.0.1 Central System URL")
	user := flag.String("user", "ftw", "basic-auth username (empty to skip)")
	pass := flag.String("pass", "sim-ocpp", "basic-auth password")
	model := flag.String("model", "", "catalog slug (see -list)")
	all := flag.Bool("all", false, "dial every OCPP model in the Evify inventory")
	plug := flag.Bool("plug", false, "plug a car in after boot")
	list := flag.Bool("list", false, "print the catalog and exit")
	control := flag.String("control", "127.0.0.1:8890", "control HTTP bind; empty to disable")
	tau := flag.Duration("tau", 500*time.Millisecond, "current-response lag")
	tick := flag.Duration("tick", time.Second, "meter interval")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if *list {
		printCatalog()
		return
	}

	var models []ocppcp.Model
	switch {
	case *all:
		models = ocppcp.OCPPModels()
	case *model != "":
		m, ok := ocppcp.Lookup(*model)
		if !ok {
			slog.Error("unknown model", "model", *model)
			os.Exit(2)
		}
		if !m.SpeaksOCPP() {
			slog.Error("this charger has no OCPP; FTW talks to it over local HTTP",
				"model", m.ID, "driver", "tesla_wall_connector")
			os.Exit(2)
		}
		models = []ocppcp.Model{m}
	default:
		fmt.Fprintln(os.Stderr, "need -all or -model; use -list to see the catalog")
		os.Exit(2)
	}

	opts := ocppcp.DialOpts{
		URL16:    *cs,
		URL201:   *cs201,
		Username: *user,
		Password: *pass,
		TauS:     tau.Seconds(),
	}

	sims := make([]*ocppcp.Sim, 0, len(models))
	for _, m := range models {
		sim := ocppcp.New(m)
		if err := sim.Dial(opts); err != nil {
			slog.Error("dial", "charger", m.ID, "err", err)
			os.Exit(1)
		}
		if err := sim.Boot(); err != nil {
			slog.Error("boot", "charger", m.ID, "err", err)
			os.Exit(1)
		}
		slog.Info("booted", "id", sim.DialID(), "vendor", m.Vendor, "model", m.Name, "protocol", m.Protocol)
		if *plug {
			if err := sim.Plug(); err != nil {
				slog.Error("plug", "charger", m.ID, "err", err)
				os.Exit(1)
			}
		}
		sims = append(sims, sim)
	}

	if *control != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
			marshalState(w, sims)
		})
		mux.HandleFunc("POST /charger/{id}/plug", func(w http.ResponseWriter, r *http.Request) {
			sim := findSim(sims, r.PathValue("id"))
			if sim == nil {
				http.NotFound(w, r)
				return
			}
			if err := sim.Plug(); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
		})
		mux.HandleFunc("POST /charger/{id}/unplug", func(w http.ResponseWriter, r *http.Request) {
			sim := findSim(sims, r.PathValue("id"))
			if sim == nil {
				http.NotFound(w, r)
				return
			}
			if err := sim.Unplug(); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(204)
		})
		go func() {
			slog.Info("control listening", "addr", *control)
			if err := http.ListenAndServe(*control, mux); err != nil {
				slog.Error("control server", "err", err)
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	tk := time.NewTicker(*tick)
	defer tk.Stop()
	last := time.Now()
	for {
		select {
		case <-stop:
			for _, sim := range sims {
				sim.Close()
			}
			return
		case now := <-tk.C:
			dt := now.Sub(last)
			last = now
			for _, sim := range sims {
				sim.Tick(dt)
				if err := sim.Report(); err != nil {
					slog.Warn("meter", "charger", sim.Model.ID, "err", err)
				}
			}
		}
	}
}

func findSim(sims []*ocppcp.Sim, id string) *ocppcp.Sim {
	for _, sim := range sims {
		if sim.Model.ID == id || sim.DialID() == id || sim.Model.Serial == id {
			return sim
		}
	}
	return nil
}

func marshalState(w http.ResponseWriter, sims []*ocppcp.Sim) {
	type row struct {
		ID       string  `json:"id"`
		DialID   string  `json:"dial_id"`
		Vendor   string  `json:"vendor"`
		Model    string  `json:"model"`
		Protocol string  `json:"protocol"`
		Plugged  bool    `json:"plugged"`
		PowerW   float64 `json:"power_w"`
		LimitA   float64 `json:"limit_a"`
	}
	out := make([]row, 0, len(sims))
	for _, sim := range sims {
		out = append(out, row{
			ID: sim.Model.ID, DialID: sim.DialID(),
			Vendor: sim.Model.Vendor, Model: sim.Model.Name,
			Protocol: string(sim.Model.Protocol),
			Plugged:  sim.Plugged(), PowerW: sim.PowerW(), LimitA: sim.LimitA(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func printCatalog() {
	fmt.Printf("%-24s %-14s %-18s %-8s %6s %s\n", "ID", "VENDOR", "MODEL", "OCPP", "kW", "IDENTITY")
	for _, m := range ocppcp.Inventory() {
		kw := m.MaxW / 1000
		fmt.Printf("%-24s %-14s %-18s %-8s %5.0f  %s\n", m.ID, m.Vendor, m.Name, m.Protocol, kw, m.DialID())
	}
	fmt.Println()
	fmt.Println("Tesla Wall Connector has no OCPP; skip it or use the tesla_wall_connector driver.")
}
