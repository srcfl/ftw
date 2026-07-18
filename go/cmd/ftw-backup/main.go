// ftw-backup creates, verifies, inspects and restores portable FTW backups.
// Restore commands are intentionally offline: stop the FTW core before use.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	statePath := fs.String("state", "state.db", "path to state.db")
	dataDir := fs.String("data", "", "persistent data directory (default: state.db directory)")
	outputDir := fs.String("output", "", "backup destination (default: <data>/backups)")
	coreVersion := fs.String("core-version", Version, "core version recorded in component inventory")
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
	info, err := backup.Create(context.Background(), backup.CreateOptions{
		State: st, StatePath: absState, DataDir: *dataDir, OutputDir: *outputDir,
		Components: backup.ComponentInventory{Core: backup.ComponentVersion{Version: *coreVersion}},
	})
	if err != nil {
		return err
	}
	return printJSON(info)
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
