package state

import (
	"context"
	"io"
	"os"
)

// NewMaintenanceWriter bounds dirty backup/archive output between syncs.
// The caller still owns the file and must sync the final tail before publishing.
// It must not wrap goal/session writes: those must never wait for this pacing.
func NewMaintenanceWriter(ctx context.Context, f *os.File) io.Writer {
	return NewMaintenanceWriterPaced(ctx, f, true)
}

// NewMaintenanceWriterPaced still syncs each bounded dirty batch. Live Core
// backups then pause so goal writes can catch up; the offline helper does not.
func NewMaintenanceWriterPaced(ctx context.Context, f *os.File, live bool) io.Writer {
	pause := func(context.Context) error { return nil }
	if live {
		pause = pauseMaintenance
	}
	return &maintenanceWriter{ctx: ctx, file: f, limit: 1 << 20, pause: pause}
}

type maintenanceWriteSyncer interface {
	io.Writer
	Sync() error
}

type maintenanceWriter struct {
	ctx     context.Context
	file    maintenanceWriteSyncer
	limit   int
	pending int
	pause   func(context.Context) error
}

func (w *maintenanceWriter) Write(p []byte) (int, error) {
	var total int
	for len(p) > 0 {
		if err := w.ctx.Err(); err != nil {
			return total, err
		}
		size := min(len(p), w.limit-w.pending)
		n, err := w.file.Write(p[:size])
		total += n
		w.pending += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n != size {
			return total, io.ErrShortWrite
		}
		if w.pending == w.limit {
			if err := w.file.Sync(); err != nil {
				return total, err
			}
			w.pending = 0
			if err := w.pause(w.ctx); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}
