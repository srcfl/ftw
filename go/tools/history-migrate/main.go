// ftw-history-migrate is a one-off tool for DuckDB beta installations.
// It is deliberately a separate Go module, outside Core's build graph.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/duckdb/duckdb-go/v2"
	"github.com/srcfl/ftw/go/internal/state"
)

func main() {
	statePath := flag.String("state", "", "path to state.db; Core must be stopped")
	flag.Parse()
	if *statePath == "" {
		fmt.Fprintln(os.Stderr, "usage: ftw-history-migrate -state /path/to/state.db (stop Core first)")
		os.Exit(2)
	}
	path := state.BetaHistoryDatabasePath(*statePath)
	db, err := sql.Open("duckdb", path+"?access_mode=read_only&threads=1&memory_limit=256MB&max_temp_directory_size=512MB&autoload_known_extensions=false&autoinstall_known_extensions=false")
	if err == nil {
		db.SetMaxOpenConns(1)
		defer db.Close()
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		err = state.ConvertBetaHistory(ctx, *statePath, db, func(phase string) { fmt.Println(phase) })
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("Verified SQLite history selected. Original beta files remain unchanged.")
}
