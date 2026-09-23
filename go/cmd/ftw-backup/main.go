// ftw-backup creates, verifies, inspects and restores portable FTW backups.
// Restore commands are intentionally offline: stop the FTW core before use.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/srcfl/ftw/go/internal/backup"
	"github.com/srcfl/ftw/go/internal/state"
)

var Version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ftw-backup:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ftw-backup create|verify|inspect|restore|revert [options]")
	}
	switch args[0] {
	case "create":
		return create(args[1:])
	case "verify":
		return verify(args[1:], false)
	case "inspect":
		return verify(args[1:], true)
	case "restore":
		return restore(args[1:])
	case "revert":
		return revert(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func create(args []string) error {
	return createWithProgress(args, os.Stderr)
}

func createWithProgress(args []string, progressOutput io.Writer) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	statePath := fs.String("state", "state.db", "path to state.db")
	configPath := fs.String("config", "", "config seed path (default: <data>/config.yaml)")
	dataDir := fs.String("data", "", "persistent data directory (default: state.db directory)")
	outputDir := fs.String("output", "", "backup destination (default: <data>/backups)")
	coreVersion := fs.String("core-version", Version, "core version recorded in component inventory")
	showProgress := fs.Bool("progress", false, "write JSON progress and periodic elapsed-time updates to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	absState, err := filepath.Abs(*statePath)
	if err != nil {
		return err
	}
	if *dataDir == "" {
		*dataDir = filepath.Dir(absState)
	}
	if *outputDir == "" {
		*outputDir = filepath.Join(*dataDir, "backups")
	}
	st, err := state.OpenBackupSource(absState)
	if err != nil {
		return err
	}
	defer st.Close()
	var report func(state.BackupProgress)
	if *showProgress {
		printer := newBackupProgressPrinter(progressOutput)
		defer printer.stop()
		report = printer.report
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	info, err := backup.Create(ctx, backup.CreateOptions{
		State: st, StatePath: absState, DataDir: *dataDir, OutputDir: *outputDir,
		ConfigPath: *configPath,
		Components: backup.ComponentInventory{Core: backup.ComponentVersion{Version: *coreVersion}},
		Progress:   report,
	})
	if err != nil {
		if report != nil {
			report(state.BackupProgress{Phase: "failed", Error: err.Error()})
		}
		return err
	}
	if report != nil {
		report(state.BackupProgress{Phase: "complete", CompletedBytes: info.SizeBytes, TotalBytes: info.SizeBytes})
	}
	return printJSON(info)
}

type backupProgressPrinter struct {
	mu      sync.Mutex
	output  *json.Encoder
	started time.Time
	latest  state.BackupProgress
	done    chan struct{}
	stopped chan struct{}
}

func newBackupProgressPrinter(output io.Writer) *backupProgressPrinter {
	p := &backupProgressPrinter{
		output: json.NewEncoder(output), started: time.Now(),
		done: make(chan struct{}), stopped: make(chan struct{}),
	}
	go func() {
		defer close(p.stopped)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.mu.Lock()
				p.write("heartbeat", p.latest)
				p.mu.Unlock()
			case <-p.done:
				return
			}
		}
	}()
	return p
}

func (p *backupProgressPrinter) report(progress state.BackupProgress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.latest = progress
	p.write("progress", progress)
}

func (p *backupProgressPrinter) write(event string, progress state.BackupProgress) {
	_ = p.output.Encode(struct {
		Event     string `json:"event"`
		ElapsedMS int64  `json:"elapsed_ms"`
		state.BackupProgress
	}{Event: event, ElapsedMS: time.Since(p.started).Milliseconds(), BackupProgress: progress})
}

func (p *backupProgressPrinter) stop() {
	close(p.done)
	<-p.stopped
}

func verify(args []string, includeManifest bool) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	archivePath := fs.String("archive", "", "path to .ftwbak archive")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *archivePath == "" && fs.NArg() == 1 {
		*archivePath = fs.Arg(0)
	}
	if *archivePath == "" {
		return errors.New("-archive is required")
	}
	manifest, info, err := backup.Inspect(context.Background(), *archivePath)
	if err != nil {
		return err
	}
	if includeManifest {
		return printJSON(struct {
			backup.Info
			Manifest backup.Manifest `json:"manifest"`
		}{Info: info, Manifest: manifest})
	}
	return printJSON(info)
}

func restore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	archivePath := fs.String("archive", "", "path to verified .ftwbak archive")
	dataDir := fs.String("data", "/app/data", "existing persistent data mount")
	yes := fs.Bool("yes", false, "confirm that FTW is stopped and perform restore")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *archivePath == "" || !*yes {
		return errors.New("restore requires -archive and -yes; stop FTW core first")
	}
	result, err := backup.RestoreContents(*archivePath, *dataDir, time.Now())
	if err != nil {
		return err
	}
	return printJSON(result)
}

func revert(args []string) error {
	fs := flag.NewFlagSet("revert", flag.ContinueOnError)
	dataDir := fs.String("data", "/app/data", "existing persistent data mount")
	safetyDir := fs.String("safety", "", "safety directory returned by restore")
	yes := fs.Bool("yes", false, "confirm that FTW is stopped and revert")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *safetyDir == "" || !*yes {
		return errors.New("revert requires -safety and -yes; stop FTW core first")
	}
	result, err := backup.RevertContents(*dataDir, *safetyDir)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func printJSON(value any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
