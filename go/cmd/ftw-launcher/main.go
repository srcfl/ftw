package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/srcfl/ftw/go/internal/nativeupdate"
)

func main() {
	root := flag.String("root", "/opt/ftw", "directory containing releases and slots.json")
	config := flag.String("config", "/var/lib/ftw/config.yaml", "persistent FTW config")
	userDrivers := flag.String("user-drivers", "/var/lib/ftw/drivers", "persistent driver overlay")
	flag.Parse()
	if err := run(*root, *config, *userDrivers, flag.Args(), syscall.Exec); err != nil {
		fmt.Fprintln(os.Stderr, "ftw-launcher:", err)
		os.Exit(1)
	}
}

type execFunc func(string, []string, []string) error

const usage = `usage: ftw-launcher [-root DIR] [-config FILE] [-user-drivers DIR] [run [CORE ARGS]]
       ftw-launcher [-root DIR] install TAG ARCHIVE CHECKSUM
       ftw-launcher [-root DIR] init TAG
       ftw-launcher [-root DIR] status
       ftw-launcher [-root DIR] prune
       ftw-launcher [-root DIR] rollback`

func run(root, config, userDrivers string, args []string, execFn execFunc) error {
	manager := nativeupdate.Manager{Root: root}
	command := "run"
	if len(args) > 0 {
		command = args[0]
	}
	if command != "status" {
		// Everything but status writes slot files.
		if err := manager.CheckOwner(); err != nil {
			return err
		}
	}
	switch command {
	case "install":
		if len(args) != 4 {
			return fmt.Errorf("usage: ftw-launcher install TAG ARCHIVE CHECKSUM")
		}
		return manager.InstallArchive(context.Background(), args[1], args[2], args[3])
	case "init":
		if len(args) != 2 {
			return fmt.Errorf("usage: ftw-launcher init TAG")
		}
		return manager.Init(args[1])
	case "status":
		if len(args) != 1 {
			return fmt.Errorf("usage: ftw-launcher status")
		}
		state, err := manager.Read()
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(state)
	case "prune":
		if len(args) != 1 {
			return fmt.Errorf("usage: ftw-launcher prune")
		}
		removed, err := manager.Prune()
		if err != nil {
			return err
		}
		for _, tag := range removed {
			fmt.Println(tag)
		}
		return nil
	case "rollback":
		// The way back when Core itself does not start: the next start runs
		// the previous release once, and it commits as any trial does.
		if len(args) != 1 {
			return fmt.Errorf("usage: ftw-launcher rollback")
		}
		previous, err := manager.PrepareRollback()
		if err != nil {
			return err
		}
		fmt.Printf("The next start runs %s once. Restart now with: sudo systemctl restart ftw\n", previous)
		return nil
	case "run":
		if len(args) > 0 {
			args = args[1:]
		}
		return launch(root, config, userDrivers, args, execFn)
	}
	return fmt.Errorf("unknown command %q\n%s", command, usage)
}

func launch(root, config, userDrivers string, extra []string, execFn execFunc) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	manager := nativeupdate.Manager{Root: root}
	path, tag, trial, err := manager.Select()
	if err != nil {
		return err
	}
	if err := execRelease(path, tag, trial, root, config, userDrivers, extra, execFn); err == nil {
		return nil // A real syscall.Exec never returns.
	} else if !trial {
		return err
	} else {
		fmt.Fprintf(os.Stderr, "ftw-launcher: trial %s failed to start: %v\n", tag, err)
		if failErr := manager.FailTrial(tag); failErr != nil {
			return fmt.Errorf("trial exec failed (%v) and fallback could not be recorded: %w", err, failErr)
		}
	}
	path, tag, trial, err = manager.Select()
	if err != nil {
		return err
	}
	if trial {
		return fmt.Errorf("fallback selected another trial %s", tag)
	}
	return execRelease(path, tag, false, root, config, userDrivers, extra, execFn)
}

func execRelease(path, tag string, trial bool, root, config, userDrivers string, extra []string, execFn execFunc) error {
	binary := filepath.Join(path, "ftw")
	args := []string{binary, "-config", config, "-web", filepath.Join(path, "web"), "-drivers", filepath.Join(path, "drivers")}
	if userDrivers != "" {
		args = append(args, "-user-drivers", userDrivers)
	}
	args = append(args, extra...)
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		if !strings.HasPrefix(item, "FTW_NATIVE_SLOT_ROOT=") && !strings.HasPrefix(item, "FTW_NATIVE_TRIAL_TAG=") {
			env = append(env, item)
		}
	}
	env = append(env, "FTW_NATIVE_SLOT_ROOT="+root)
	if trial {
		env = append(env, "FTW_NATIVE_TRIAL_TAG="+tag)
	}
	fmt.Fprintf(os.Stderr, "ftw-launcher: starting %s (trial=%t)\n", tag, trial)
	return execFn(binary, args, env)
}
