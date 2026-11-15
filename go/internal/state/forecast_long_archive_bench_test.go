package state

import (
	"context"
	"testing"
)

func BenchmarkForecastLongArchiveCopy(b *testing.B) {
	store, err := Open(b.TempDir() + "/state.db")
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	arch, err := store.OpenForecastLongArchive(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	base := longIssue("bench", longOrigin())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item := base
		item.ID = "bench-" + forecastLongItoa(i)
		if err := arch.ArchiveIssues(context.Background(), []ForecastArchiveIssue{{Issue: item, JSONBytes: 8000, GzipBytes: 2000}}); err != nil {
			b.Fatal(err)
		}
	}
}
