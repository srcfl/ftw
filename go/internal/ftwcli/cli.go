// Package ftwcli is the ftw operator command for a native install. It runs
// the owner's steps against the Core on this machine through Core's HTTP API
// and never starts Core itself. Starting, stopping and logs stay with
// systemd.
package ftwcli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const defaultURL = "http://127.0.0.1:8080"

const usageText = `ftw runs FTW's operator steps against the Core on this machine.
It uses Core's local API and never starts Core itself.

Usage:
  ftw status                          version, releases, last update and health
  ftw update [--channel beta|stable]  install the next release; already current exits 0
             [--retry]                  also a release that failed here before
  ftw rollback                        return to the previous release
  ftw backup [--output-dir DIR]       make a verified full backup; DIR gets a checked copy
  ftw support [--output FILE]         write the redacted support file
  ftw help

Every command takes --url URL (default http://127.0.0.1:8080).
Exit status: 0 done, 1 the step failed, 2 the command line was wrong.

Starting, stopping and logs belong to systemd:
  sudo systemctl restart ftw
  journalctl -u ftw -n 100
`

const (
	exitOK     = 0
	exitFailed = 1
	exitUsage  = 2
)

type usageError string

func (e usageError) Error() string { return string(e) }

// env holds the clock and the limits, so tests can shorten them.
type env struct {
	now   func() time.Time
	sleep func(time.Duration)
	// requestTimeout bounds each API call except making and copying a backup.
	requestTimeout time.Duration
	// pollInterval is the gap between progress reads. A native download
	// and restart can both finish within a couple of seconds.
	pollInterval time.Duration
	// followLimit bounds waiting for an update or rollback. A native trial
	// may take six hours to become ready.
	followLimit time.Duration
	// logEvery repeats a progress line when the output is not a terminal,
	// so a log shows the command is alive without a line per poll.
	logEvery time.Duration
	// healthSettle is how long a new Core gets to read its devices before
	// its health is reported; healthSteady is how long ok must hold.
	healthSettle time.Duration
	healthSteady time.Duration
	// tty redraws one progress line in place; width is the terminal's.
	// ascii replaces block characters where the locale is not UTF-8.
	tty   bool
	width int
	ascii bool
	// systemd is false in a container, where the journalctl, systemctl and
	// launcher hints do not apply.
	systemd bool
}

func defaultEnv() env {
	return env{
		now: time.Now, sleep: time.Sleep,
		requestTimeout: 15 * time.Second, pollInterval: 500 * time.Millisecond,
		followLimit: 6*time.Hour + 15*time.Minute, logEvery: 10 * time.Second,
		healthSettle: 30 * time.Second, healthSteady: 5 * time.Second,
	}
}

// Run executes one ftw command and returns its exit status.
func Run(args []string, stdout, stderr io.Writer) int {
	e := defaultEnv()
	e.tty, e.width = terminal(stdout)
	e.ascii = !utf8Locale()
	_, err := os.Stat("/run/systemd/system")
	e.systemd = err == nil
	return run(args, stdout, stderr, e)
}

func run(args []string, stdout, stderr io.Writer, e env) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usageText)
		return exitOK
	}
	var err error
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usageText)
		return exitOK
	case "status":
		err = runStatus(args[1:], stdout, e)
	case "update":
		err = runUpdate(args[1:], stdout, e)
	case "rollback":
		err = runRollback(args[1:], stdout, e)
	case "backup":
		err = runBackup(args[1:], stdout, e)
	case "support":
		err = runSupport(args[1:], stdout, e)
	default:
		err = usageError("unknown command " + args[0])
	}
	return report(err, stderr)
}

func report(err error, stderr io.Writer) int {
	var usage usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.As(err, &usage):
		fmt.Fprintf(stderr, "ftw: %s\nRun ftw help for the commands.\n", usage)
		return exitUsage
	default:
		fmt.Fprintf(stderr, "ftw: %s\n", err)
		return exitFailed
	}
}

// parse reads one command's flags; --name value and --name=value both work.
// It returns the Core URL.
func parse(args []string, stdout io.Writer, usage string, define func(*flag.FlagSet)) (string, error) {
	fs := flag.NewFlagSet("ftw", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	url := fs.String("url", defaultURL, "")
	if define != nil {
		define(fs)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stdout, "usage: %s\n", usage)
			return "", err
		}
		return "", usageError(err.Error() + "; usage: " + usage)
	}
	if fs.NArg() > 0 {
		return "", usageError("unexpected argument " + fs.Arg(0) + "; usage: " + usage)
	}
	base := strings.TrimRight(*url, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return "", usageError("--url must start with http:// or https://")
	}
	return base, nil
}
