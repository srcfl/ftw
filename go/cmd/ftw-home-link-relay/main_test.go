package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateListenRequiresTLSForNonLoopback(t *testing.T) {
	for _, test := range []struct {
		name      string
		address   string
		cert      string
		key       string
		proxy     bool
		wantError bool
	}{
		{name: "loopback v4", address: "127.0.0.1:8080"},
		{name: "loopback v6", address: "[::1]:8080"},
		{name: "public clear", address: "0.0.0.0:8080", wantError: true},
		{name: "trusted proxy", address: "0.0.0.0:8080", proxy: true},
		{name: "native tls", address: "0.0.0.0:8443", cert: "cert", key: "key"},
		{name: "missing key", address: "127.0.0.1:8080", cert: "cert", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateListen(test.address, test.cert, test.key, test.proxy)
			if (err != nil) != test.wantError {
				t.Fatalf("validateListen() = %v", err)
			}
		})
	}
}

func TestReadBoundedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invites.json")
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readBoundedFile(path, 16)
	if err != nil || string(data) != "[]" {
		t.Fatalf("readBoundedFile = %q, %v", data, err)
	}
	if _, err := readBoundedFile(path, 1); err == nil {
		t.Fatal("oversized file was accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(empty, 16); err == nil {
		t.Fatal("empty file was accepted")
	}
	directory := filepath.Join(t.TempDir(), "dir")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedFile(directory, 16); err == nil {
		t.Fatal("directory was accepted")
	}
}

func TestParseFlagsDoNotLeakInviteContent(t *testing.T) {
	if strings.Contains(strings.Join(os.Args, " "), "public_key") {
		t.Fatal("test process unexpectedly includes invite contents in arguments")
	}
}
