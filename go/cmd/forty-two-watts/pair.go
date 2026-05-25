package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// runPair is the entry point for `forty-two-watts pair`.
//
//	forty-two-watts pair                       # 4h session
//	forty-two-watts pair --ttl 2h
//	forty-two-watts pair --intent "..." --as "@erikarenhill"
//	forty-two-watts pair --abort               # signal the running sidecar to exit
func runPair(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	ttl := fs.String("ttl", "4h", "Session TTL (Go duration)")
	intent := fs.String("intent", "", "Free-form description of what the friend should help with")
	as := fs.String("as", "", "Optional friend identity")
	abort := fs.Bool("abort", false, "Abort the active session and exit")
	bin := fs.String("bin", "", "Path to ftw-pair binary (default: sibling of forty-two-watts)")
	_ = fs.Parse(args)

	if *abort {
		resp, err := http.Post("http://127.0.0.1:8080/api/pair/abort", "application/json", bytes.NewReader(nil))
		if err != nil {
			fmt.Fprintf(os.Stderr, "abort: %v\n", err)
			os.Exit(1)
		}
		resp.Body.Close()
		fmt.Println("abort signaled — sidecar will exit on its next poll")
		return
	}

	pairBin := *bin
	if pairBin == "" {
		self, _ := os.Executable()
		pairBin = filepath.Join(filepath.Dir(self), "ftw-pair")
	}
	if _, err := os.Stat(pairBin); err != nil {
		fmt.Fprintf(os.Stderr, "ftw-pair binary not found at %s\n", pairBin)
		os.Exit(1)
	}

	cmdArgs := []string{
		"-ttl", *ttl,
		"-intent", *intent,
		"-as", *as,
	}
	cmd := exec.Command(pairBin, cmdArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
}
