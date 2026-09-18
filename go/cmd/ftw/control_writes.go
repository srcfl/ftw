package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func closeControlWrites(w *state.ControlWrites, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		slog.Error("pending control state could not be committed before shutdown", "state", name, "err", err)
	}
}
