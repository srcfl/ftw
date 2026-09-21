package backup

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestBackupMutexWaitCanBeCancelled(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var mu sync.Mutex
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Create(ctx, CreateOptions{State: s, StatePath: path, DataDir: root, OutputDir: filepath.Join(root, "backups"), Maintenance: &mu, Progress: func(p state.BackupProgress) {
			if p.Phase == "waiting_for_maintenance" {
				close(waiting)
			}
		}})
		done <- err
	}()
	<-waiting
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled backup still owns operation locks")
	}
}

func TestBackupVerificationCancellationDoesNotPublish(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := filepath.Join(root, "backups")
	_, err = Create(ctx, CreateOptions{State: s, StatePath: path, DataDir: root, OutputDir: output, Progress: func(p state.BackupProgress) {
		if p.Phase == "verifying_archive" {
			cancel()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("verification ignored cancellation: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(output, "*.ftwbak"))
	if err != nil || len(files) != 0 {
		t.Fatalf("published cancelled backup: %v %v", files, err)
	}
}
