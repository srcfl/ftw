package updatecli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

const usageText = `ftw talks to the Core running on this machine. It does not start a Docker updater.

Usage:
  ftw update [--channel beta|stable] [--url URL] [--port PORT]
  ftw backup [--output-dir DIR] [--url URL] [--port PORT]
  ftw doctor [--url URL] [--port PORT]
  ftw support [--output FILE] [--report] [--url URL] [--port PORT]
  ftw startup [--root DIR] [--config FILE] [--user-drivers DIR] [--user USER] [--port PORT]
  ftw help

update downloads the next native release and swaps to it.
With no --channel, ftw asks beta or stable. Enter keeps the channel
the box already uses. A closed terminal also keeps that channel.

backup makes a verified full archive while Core keeps running.
--output-dir copies it off the machine and checks the SHA-256.
Without --output-dir the archive stays on the box.

doctor reports whether Core, history and drivers are fit to run.

support writes Core's redacted diagnostic zip. --report writes the
shorter Markdown report instead. The file mode is 0600.

startup prints a systemd unit and the shell lines that install it.
It does not change the machine. Paste the lines after you have read them.

--port chooses the HTTP port. --url is the full address of a running Core.
Use one of them.
`

// Run is the ftw command line.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, usageText)
		return nil
	}
	switch args[0] {
	case "update":
		return runUpdate(args[1:], stdout, stderr)
	case "backup":
		return runBackup(args[1:], stdout)
	case "doctor":
		return runDoctor(args[1:], stdout)
	case "support":
		return runSupport(args[1:], stdout)
	case "startup":
		return runStartup(args[1:], stdout)
	default:
		fmt.Fprint(stderr, usageText)
		return fmt.Errorf("unknown command %s", args[0])
	}
}

func runUpdate(args []string, stdout, stderr io.Writer) error {
	if hasHelp(args) {
		fmt.Fprint(stdout, "usage: ftw update [--channel beta|stable] [--url http://127.0.0.1:8080]\n")
		return nil
	}
	opts, _, err := commonOpts(args, map[string]bool{"--channel": true})
	if err != nil {
		return err
	}
	channel := flagValue(args, "--channel")
	if channel == "" {
		info, err := peekVersion(opts)
		if err != nil {
			return err
		}
		if !info.Native {
			return notNative
		}
		chosen, err := promptChannel(os.Stdin, stderr, info.Channel)
		if err != nil {
			return err
		}
		channel = chosen
	}
	if channel != "" && channel != "beta" && channel != "stable" {
		return fmt.Errorf("--channel must be beta or stable, got %s", channel)
	}
	opts.Channel = channel
	return Update(context.Background(), opts, stdout)
}

func promptChannel(in io.Reader, out io.Writer, current string) (string, error) {
	if current != "beta" && current != "stable" {
		current = "beta"
	}
	fmt.Fprintf(out, "Release channel: beta or stable [%s]: ", current)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		fmt.Fprintf(out, "Keeping %s.\n", current)
		return current, nil
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return current, nil
	}
	if line != "beta" && line != "stable" {
		return "", fmt.Errorf("channel must be beta or stable, got %s", line)
	}
	return line, nil
}

func peekVersion(opts Options) (versionInfo, error) {
	prepare(&opts)
	var info versionInfo
	err := getJSON(context.Background(), opts.Client, opts, "/api/version/check", &info)
	return info, err
}

func hasHelp(args []string) bool {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func flagValue(args []string, name string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func parseCommon(args []string, opts Options, extra map[string]bool) (Options, error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--url":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("--url needs an address")
			}
			opts.URL = stringsTrimRightSlash(args[i])
		case "--port":
			if !extra["--port"] {
				return opts, fmt.Errorf("unknown argument --port")
			}
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("--port needs a port")
			}
		case "--channel":
			if !extra["--channel"] {
				return opts, fmt.Errorf("unknown argument %s", args[i])
			}
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("--channel needs beta or stable")
			}
		case "--output-dir", "--output":
			if !extra[args[i]] {
				return opts, fmt.Errorf("unknown argument %s", args[i])
			}
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("%s needs a path", args[i-1])
			}
		case "--report":
			if !extra["--report"] {
				return opts, fmt.Errorf("unknown argument --report")
			}
		case "--help", "-h":
		default:
			return opts, fmt.Errorf("unknown argument %s", args[i])
		}
	}
	return opts, nil
}

func stringsTrimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

var notNative = errNative{}

type errNative struct{}

func (errNative) Error() string {
	return "this Core is not a native install; ftw update does not start the Docker updater"
}
