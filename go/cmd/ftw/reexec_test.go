package main

import (
	"errors"
	"os"
	"testing"

	"github.com/srcfl/ftw/go/internal/components"
)

func TestRestartPlan(t *testing.T) {
	reexec, code := restartPlan(nil)
	if reexec || code != 1 {
		t.Fatalf("native install: reexec=%v exit=%d, want false/1", reexec, code)
	}
	reexec, code = restartPlan(&components.Bundle{Kind: "compose"})
	if reexec || code != 1 {
		t.Fatalf("compose bundle: reexec=%v exit=%d, want false/1", reexec, code)
	}
	reexec, code = restartPlan(&components.Bundle{Kind: components.KindHomeAssistantAddon})
	if !reexec || code != 0 {
		t.Fatalf("HA add-on: reexec=%v exit=%d, want true/0", reexec, code)
	}
}

func TestAfterShutdownNativeExit(t *testing.T) {
	var execs int
	gotExit := -1
	afterShutdown(false, 1, func(string, []string, []string) error {
		execs++
		return nil
	}, func(code int) { gotExit = code })
	if execs != 0 {
		t.Fatalf("native restart must not re-exec, called exec %d times", execs)
	}
	if gotExit != 1 {
		t.Fatalf("native restart exit=%d, want 1 so docker/systemd restart", gotExit)
	}
}

func TestAfterShutdownSignalDoesNotRestart(t *testing.T) {
	var execs, exits int
	afterShutdown(false, 0, func(string, []string, []string) error {
		execs++
		return nil
	}, func(int) { exits++ })
	if execs != 0 || exits != 0 {
		t.Fatalf("SIGTERM path must neither re-exec nor os.Exit, execs=%d exits=%d", execs, exits)
	}
}

func TestAfterShutdownReexecsThenFallsBack(t *testing.T) {
	var gotArgv0 string
	var gotArgv, gotEnv []string
	gotExit := -1
	afterShutdown(true, 0, func(argv0 string, argv []string, envv []string) error {
		gotArgv0 = argv0
		gotArgv = argv
		gotEnv = envv
		return errors.New("execve: refused")
	}, func(code int) { gotExit = code })
	if gotArgv0 != currentExecutable() {
		t.Fatalf("re-exec argv0=%q, want %q", gotArgv0, currentExecutable())
	}
	if len(gotArgv) != len(os.Args) || gotArgv[0] != os.Args[0] {
		t.Fatalf("re-exec argv=%v, want os.Args %v", gotArgv, os.Args)
	}
	if len(gotEnv) == 0 {
		t.Fatal("re-exec must pass the current environment")
	}
	if gotExit != 1 {
		t.Fatalf("failed re-exec exit=%d, want 1", gotExit)
	}
}

func TestExecCurrentProcessTreatsNilReturnAsFailure(t *testing.T) {
	err := execCurrentProcess(func(string, []string, []string) error { return nil })
	if err == nil {
		t.Fatal("exec that returns nil must still be an error; success never returns")
	}
}
