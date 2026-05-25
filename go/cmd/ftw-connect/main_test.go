package main

import (
	"strings"
	"testing"
)

func TestBuildPrompt(t *testing.T) {
	p := buildPrompt("write a goodwe driver", "3h45m")
	if !strings.Contains(p, "goodwe") {
		t.Fatal("intent missing")
	}
	if !strings.Contains(p, "3h45m") {
		t.Fatal("ttl missing")
	}
	if !strings.Contains(p, "ftw-remote") {
		t.Fatal("server name missing")
	}
}
