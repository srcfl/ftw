package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestFullBackupRestoresCurrentSQLiteConfigAtANewPath(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(data, "state.db")
	seed := filepath.Join(data, "config.yaml")
	st, err := state.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := config.Parse([]byte("site:\n  name: Imported\nfuse:\n  max_amps: 16\n"), data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = config.InitializeStorage(seed, database, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Site.Name = "Latest committed settings"
	if err := config.SaveStored(st, seed, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := Create(context.Background(), CreateOptions{State: st, StatePath: database, DataDir: data, OutputDir: filepath.Join(root, "backups"), ConfigPath: seed})
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "restored")
	if _, err := Restore(info.Path, destination, time.Now()); err != nil {
		t.Fatal(err)
	}
	restored, err := config.Load(filepath.Join(destination, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if restored.Site.Name != cfg.Site.Name || restored.Revision != cfg.Revision {
		t.Fatal("restore used the old import config")
	}
	raw, err := os.ReadFile(filepath.Join(destination, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Latest committed settings") {
		t.Fatal("older Core would read stale YAML")
	}
	if restored.ConfigDatabase != filepath.Join(destination, "state.db") {
		t.Fatalf("backup still points at original database: %s", restored.ConfigDatabase)
	}
}
