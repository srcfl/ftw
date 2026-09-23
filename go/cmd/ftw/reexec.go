package main

import (
	"errors"
	"log/slog"
	"os"
	"syscall"

	"github.com/srcfl/ftw/go/internal/components"
)

// execFunc is syscall.Exec (or a fake in tests). A successful call never
// returns: the process image is replaced.
type execFunc func(argv0 string, argv []string, envv []string) error

func currentExecutable() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return os.Args[0]
	}
	return exe
}

func execCurrentProcess(execFn execFunc) error {
	if execFn == nil {
		execFn = syscall.Exec
	}
	exe := currentExecutable()
	if exe == "" {
		return errors.New("cannot resolve current executable")
	}
	slog.Info("re-executing", "exe", exe)
	if err := execFn(exe, os.Args, os.Environ()); err != nil {
		return err
	}
	return errors.New("re-exec returned without replacing the process")
}

// restartPlan is the in-process fallback when the updater sidecar is absent.
// Home Assistant re-execs after shutdown; native installs exit 1 so docker
// (unless-stopped) and systemd (on-failure) bring the binary back.
func restartPlan(bundle *components.Bundle) (reexec bool, exitCode int) {
	if bundle.ReexecOnRestart() {
		return true, 0
	}
	return false, 1
}

// afterShutdown runs from main's first defer, after HTTP, SQLite and the
// other LIFO defers have already closed. SIGTERM uses reexec=false and
// exitCode=0 so the process actually ends.
func afterShutdown(reexec bool, exitCode int, execFn execFunc, exitFn func(int)) {
	if exitFn == nil {
		exitFn = os.Exit
	}
	if reexec {
		if err := execCurrentProcess(execFn); err != nil {
			slog.Error("re-exec failed", "err", err)
			exitFn(1)
		}
		return
	}
	if exitCode != 0 {
		exitFn(exitCode)
	}
}
