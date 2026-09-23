package prices

import (
	"context"
	"log/slog"
	"time"
)

// fallbackProvider tries primary first. If that day is empty (not yet in
// the harvest cache), it asks secondary. Name stays the primary so a
// configured sourceful box still reports sourceful unless the fallback
// actually supplied the rows — Fetch logs that case.
type fallbackProvider struct {
	primary, secondary Provider
}

func withFallback(primary, secondary Provider) Provider {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	return fallbackProvider{primary: primary, secondary: secondary}
}

func (f fallbackProvider) Name() string { return f.primary.Name() }

func (f fallbackProvider) Fetch(ctx context.Context, zone string, day time.Time) ([]RawPrice, error) {
	rows, err := f.primary.Fetch(ctx, zone, day)
	if err == nil && len(rows) > 0 {
		return rows, nil
	}
	rows2, err2 := f.secondary.Fetch(ctx, zone, day)
	if err2 != nil {
		if err != nil {
			return nil, err
		}
		return nil, err2
	}
	if len(rows2) > 0 {
		slog.Info("price: primary empty, using Nord Pool day-ahead",
			"primary", f.primary.Name(),
			"zone", zone,
			"day", day.Format("2006-01-02"),
			"slots", len(rows2))
	}
	return rows2, nil
}
