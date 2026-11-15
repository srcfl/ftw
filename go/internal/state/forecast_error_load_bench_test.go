package state

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func BenchmarkForecastErrorLoad(b *testing.B) {
	ctx := context.Background()
	s, err := Open(filepath.Join(b.TempDir(), "state.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	if err := s.InitForecastArchive(ctx); err != nil {
		b.Fatal(err)
	}
	const hour int64 = 3600000
	const until = 30000 * hour
	const since = until - 720*hour
	tx, err := s.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO forecast_errors VALUES(?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		b.Fatal(err)
	}
	var payloads [][]byte
	for i := range 63635 {
		slot := int64(i % 2800)
		if i >= 15743 && slot%4 == 0 {
			slot++
		}
		if i < 15743 {
			slot = int64(i%700) * 4
		}
		start := since + slot*hour/4
		e := archiveError(strings.Repeat("i", 64), start-hour, start-hour, start)
		e.Series = fmt.Sprintf("series-%d", i%8)
		e.ConfigVersion = fmt.Sprintf("%064d", i)
		data, err := json.Marshal(e)
		if err != nil {
			b.Fatal(err)
		}
		data = append(data, strings.Repeat(" ", 1054-len(data))...)
		if _, err := stmt.Exec(e.Series, e.ConfigVersion, e.Lead, start, e.EndMS, e.OriginMS, e.IssuedAtMS, e.IssueID, e.AvailableAtMS, string(data)); err != nil {
			b.Fatal(err)
		}
		if i < 15743 {
			payloads = append(payloads, data)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	if err := s.EnsureForecastHourlyIndex(ctx); err != nil {
		b.Fatal(err)
	}
	b.Run("upgrade-index", func(b *testing.B) {
		for b.Loop() {
			b.StopTimer()
			if _, err := s.db.Exec("DROP INDEX forecast_errors_hourly"); err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if err := s.EnsureForecastHourlyIndex(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})

	for _, candidate := range []struct{ name, index, from, filter string }{
		{"baseline", "", "forecast_errors INDEXED BY forecast_errors_time", "AND (?=0 OR start_ms%3600000=0)"},
		{"ordered-full", "CREATE INDEX bench_full ON forecast_errors(start_ms, series DESC, lead DESC)", "forecast_errors INDEXED BY bench_full", "AND (?=0 OR start_ms%3600000=0)"},
		{"ordered-hourly", "", "forecast_errors", "AND start_ms%3600000=0"},
	} {
		if candidate.index != "" {
			if _, err := s.db.Exec(candidate.index); err != nil {
				b.Fatal(err)
			}
		}
		query := "SELECT payload FROM " + candidate.from + " WHERE start_ms>=? AND end_ms<=? AND available_at_ms<=? " + candidate.filter + " ORDER BY start_ms DESC,series,lead LIMIT ?"
		args := []any{since, until, until}
		if candidate.name != "ordered-hourly" {
			args = append(args, true)
		}
		args = append(args, MaxForecastErrors+1)
		plan, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			b.Fatal(err)
		}
		for plan.Next() {
			var a, c, d int
			var detail string
			if err := plan.Scan(&a, &c, &d, &detail); err != nil {
				b.Fatal(err)
			}
			b.Log(candidate.name, detail)
		}
		if err := plan.Err(); err != nil {
			b.Fatal(err)
		}
		plan.Close()
		for _, decode := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/decode=%t", candidate.name, decode), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					rows, err := s.db.QueryContext(ctx, query, args...)
					if err != nil {
						b.Fatal(err)
					}
					n := 0
					for rows.Next() {
						var data string
						if err := rows.Scan(&data); err != nil {
							b.Fatal(err)
						}
						if decode {
							var e forecasting.ErrorSample
							if err := json.Unmarshal([]byte(data), &e); err != nil {
								b.Fatal(err)
							}
							if err := e.Validate(); err != nil {
								b.Fatal(err)
							}
						}
						n++
					}
					if err := rows.Err(); err != nil {
						b.Fatal(err)
					}
					if n != len(payloads) {
						b.Fatalf("loaded %d", n)
					}
				}
			})
		}
		if candidate.name == "ordered-full" {
			if _, err := s.db.Exec("DROP INDEX bench_full"); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.Run("decode-only", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, data := range payloads {
				var e forecasting.ErrorSample
				if err := json.Unmarshal(data, &e); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	var samples []forecasting.ErrorSample
	for _, data := range payloads {
		var e forecasting.ErrorSample
		if err := json.Unmarshal(data, &e); err != nil {
			b.Fatal(err)
		}
		samples = append(samples, e)
	}
	b.Run("validate-only", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, e := range samples {
				if err := e.Validate(); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("production-loader", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			out, err := s.LoadForecastErrors(ctx, since, until, true)
			if err != nil {
				b.Fatal(err)
			}
			if len(out) != len(payloads) {
				b.Fatalf("loaded %d", len(out))
			}
		}
	})
}
