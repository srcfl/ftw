package main

import "testing"

func TestRestoreRequiresExplicitConfirmation(t *testing.T) {
	if err := run([]string{"restore", "-archive", "backup.ftwbak", "-data", t.TempDir()}); err == nil {
		t.Fatal("restore ran without -yes")
	}
}

func TestRejectsUnknownCommand(t *testing.T) {
	if err := run([]string{"destroy"}); err == nil {
		t.Fatal("unknown command accepted")
	}
}
